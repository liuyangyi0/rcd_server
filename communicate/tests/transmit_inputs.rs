use rcd_communicate::{
    config,
    points::{PointStore, Quality, Value},
    unit_report::Layout,
};
use std::time::{Duration, Instant};

fn transmit_payload(layout: Layout, seed: u8) -> Vec<u8> {
    let mut raw = layout.empty_transmit_payload();
    raw[2..].fill(seed);
    let (offset, count) = if layout == Layout::Unit1To9 {
        (981, 35)
    } else {
        (50, 11)
    };
    let state = if seed.is_multiple_of(2) {
        [9, 0x85, 0, 4, 0]
    } else {
        [6, 0xc2, 0, 0x12, 0]
    };
    for i in 0..count {
        raw[offset + i * 5..offset + (i + 1) * 5].copy_from_slice(&state);
    }
    raw
}

fn loaded() -> config::LoadedConfig {
    let mut c = config::load(
        &std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("examples/unit1-receiver.toml"),
    )
    .unwrap();
    c.config.ports[0].enabled = true;
    c.config.ports[0].tasks[0].enabled = true;
    c.config.ports[0]
        .synchronization
        .as_mut()
        .unwrap()
        .transmit_enabled = true;
    c
}

#[test]
fn client_fields_assemble_complete_snapshot_and_expire_individually() {
    for layout in [Layout::Unit1To9, Layout::Unit9To1] {
        let mut c = loaded();
        c.config.ports[0]
            .synchronization
            .as_mut()
            .unwrap()
            .transmit_protocol = match layout {
            Layout::Unit1To9 => "unit1-to9-v1",
            Layout::Unit9To1 => "unit9-to1-v1",
        }
        .into();
        let store = PointStore::new(&c.config, &c.points);
        let raw = transmit_payload(layout, 7);
        let decoded = layout.decode("source", &raw).unwrap();
        let definitions = layout.transmit_definitions("sync");
        for (i, d) in definitions.iter().enumerate() {
            assert!(store
                .synchronization
                .current("sync", Instant::now())
                .is_none());
            let value = decoded
                .iter()
                .find(|v| v.name == d.name)
                .unwrap()
                .value
                .clone();
            store
                .synchronization
                .write_field("sync", &d.name, value.clone())
                .unwrap();
            let before = store
                .synchronization
                .field_sample("sync", &d.name, d.data_type)
                .unwrap();
            store
                .synchronization
                .write_field("sync", &d.name, value)
                .unwrap();
            let after = store
                .synchronization
                .field_sample("sync", &d.name, d.data_type)
                .unwrap();
            assert_eq!(after.quality, Quality::Good);
            assert!(after.source_timestamp >= before.source_timestamp);
            if i + 1 == definitions.len() {
                assert_eq!(
                    store
                        .synchronization
                        .current("sync", Instant::now())
                        .unwrap(),
                    raw
                );
            }
        }
        let frozen = store
            .synchronization
            .current("sync", Instant::now())
            .unwrap();
        assert!(store
            .synchronization
            .current("sync", Instant::now() + Duration::from_secs(16))
            .is_none());
        // An unrelated input refresh must not revive the remaining stale ones.
        let d = &definitions[0];
        let previous = store
            .synchronization
            .field_sample("sync", &d.name, d.data_type)
            .unwrap()
            .value;
        store
            .synchronization
            .write_field("sync", &d.name, previous)
            .unwrap();
        assert_eq!(frozen, raw);
        assert!(store
            .synchronization
            .current("sync", Instant::now() + Duration::from_secs(16))
            .is_none());
    }
}

#[test]
fn invalid_inputs_disabled_and_restart_never_seed_a_snapshot() {
    let c = loaded();
    let store = PointStore::new(&c.config, &c.points);
    let name = "DATA.Header.detectorTemp3";
    for v in [Value::UInt32(256), Value::String("7".into())] {
        assert!(store.synchronization.write_field("sync", name, v).is_err());
    }
    assert!(store
        .synchronization
        .write_field("sync", "DATA.Header.nBlock.z", Value::UInt32(9))
        .is_err());
    assert!(store
        .synchronization
        .write_field("sync", "DATA.PumpStationCtrl", Value::String("00".into()))
        .is_err());
    store
        .synchronization
        .write_field("sync", name, Value::UInt32(0))
        .unwrap();
    assert!(store
        .synchronization
        .current("sync", Instant::now())
        .is_none());
    let restart = PointStore::new(&c.config, &c.points);
    assert_eq!(
        restart
            .synchronization
            .field_sample("sync", name, rcd_communicate::points::DataType::UInt32)
            .unwrap()
            .quality,
        Quality::Waiting
    );
    let mut disabled = c.config.clone();
    disabled.ports[0].enabled = false;
    assert!(PointStore::new(&disabled, &c.points)
        .synchronization
        .write_field("sync", name, Value::UInt32(1))
        .is_err());
}

#[test]
fn refresh_and_timeout_configuration_are_validated() {
    let c = loaded();
    let settings = c.config.ports[0].synchronization.as_ref().unwrap();
    assert_eq!(settings.input_refresh_interval_ms, 5000);
    assert_eq!(settings.snapshot_max_age_ms, 15000);
    let mut port = c.config.ports[0].clone();
    port.synchronization
        .as_mut()
        .unwrap()
        .input_refresh_interval_ms = 15000;
    assert!(rcd_communicate::synchronization::validate_port(&port).is_err());
}

fn atomic_request(layout: Layout, byte: u8, revision: u64) -> serde_json::Value {
    let payload = transmit_payload(layout, byte);
    let decoded = layout.decode("source", &payload).unwrap();
    let mut fields = serde_json::Map::new();
    for definition in layout.transmit_definitions("sync") {
        let name = definition.name.clone();
        let value = &decoded.iter().find(|u| u.name == name).unwrap().value;
        fields.insert(
            definition.name,
            match value {
                Value::UInt32(v) => serde_json::json!(v),
                Value::String(v) => serde_json::json!(v),
                _ => panic!("unexpected type"),
            },
        );
    }
    serde_json::json!({"revision":revision,"fields":fields})
}

#[test]
fn atomic_fields_reject_partial_invalid_conflicting_and_mixed_writers() {
    for layout in [Layout::Unit1To9, Layout::Unit9To1] {
        let mut c = loaded();
        c.config.ports[0]
            .synchronization
            .as_mut()
            .unwrap()
            .transmit_protocol = if layout == Layout::Unit1To9 {
            "unit1-to9-v1"
        } else {
            "unit9-to1-v1"
        }
        .into();
        let store = PointStore::new(&c.config, &c.points);
        let request = atomic_request(layout, 7, 0);
        assert_eq!(
            store
                .synchronization
                .submit_fields_json("sync", &request.to_string())
                .unwrap(),
            1
        );
        let original = store
            .synchronization
            .current("sync", Instant::now())
            .unwrap();
        let duplicate = atomic_request(layout, 8, 1).to_string();
        let definition = layout.transmit_definitions("sync").remove(0);
        let duplicate = duplicate.replacen(
            "\"fields\":{",
            &format!("\"fields\":{{\"{}\":0,", definition.name),
            1,
        );
        assert!(store
            .synchronization
            .submit_fields_json("sync", &duplicate)
            .is_err());
        assert_eq!(
            store
                .synchronization
                .current("sync", Instant::now())
                .unwrap(),
            original
        );
        for mode in 0..4 {
            let mut bad = atomic_request(layout, 8, 1);
            let fields = bad["fields"].as_object_mut().unwrap();
            let key = fields.keys().next().unwrap().clone();
            match mode {
                0 => {
                    fields.remove(&key);
                }
                1 => {
                    fields.insert(key, serde_json::json!(-1));
                }
                2 => {
                    fields.insert("unknown".into(), serde_json::json!(0));
                }
                _ => {
                    bad["revision"] = serde_json::json!(0);
                }
            }
            assert!(store
                .synchronization
                .submit_fields_json("sync", &bad.to_string())
                .is_err());
            assert_eq!(
                store
                    .synchronization
                    .current("sync", Instant::now())
                    .unwrap(),
                original
            );
        }
        let definition = layout.transmit_definitions("sync").remove(0);
        assert!(store
            .synchronization
            .write_field("sync", &definition.name, definition.data_type.zero())
            .is_err());
        assert_eq!(
            store
                .synchronization
                .submit_fields_json("sync", &atomic_request(layout, 8, 1).to_string())
                .unwrap(),
            2
        );
        let new = store
            .synchronization
            .current("sync", Instant::now())
            .unwrap();
        assert_eq!(new, transmit_payload(layout, 8));
        assert!(store
            .synchronization
            .current("sync", Instant::now() + Duration::from_secs(16))
            .is_none());
    }
}

#[test]
fn concurrent_snapshot_reader_never_observes_mixed_payload() {
    let c = loaded();
    let store = std::sync::Arc::new(PointStore::new(&c.config, &c.points));
    store
        .synchronization
        .submit_fields_json("sync", &atomic_request(Layout::Unit1To9, 7, 0).to_string())
        .unwrap();
    std::thread::scope(|scope| {
        let s = store.clone();
        scope.spawn(move || {
            for i in 1..101 {
                s.synchronization
                    .submit_fields_json(
                        "sync",
                        &atomic_request(Layout::Unit1To9, if i % 2 == 0 { 7 } else { 8 }, i)
                            .to_string(),
                    )
                    .unwrap();
            }
        });
        for _ in 0..2000 {
            let payload = store
                .synchronization
                .current("sync", Instant::now())
                .unwrap();
            assert!(
                payload == transmit_payload(Layout::Unit1To9, 7)
                    || payload == transmit_payload(Layout::Unit1To9, 8)
            );
        }
    });
}

#[test]
fn atomic_competitors_only_one_commit_and_disabled_port_rejects() {
    let mut c = loaded();
    let store = std::sync::Arc::new(PointStore::new(&c.config, &c.points));
    let barrier = std::sync::Arc::new(std::sync::Barrier::new(2));
    let successes = std::thread::scope(|scope| {
        let handles: Vec<_> = [7, 8]
            .into_iter()
            .map(|byte| {
                let store = store.clone();
                let barrier = barrier.clone();
                scope.spawn(move || {
                    let request = atomic_request(Layout::Unit1To9, byte, 0).to_string();
                    barrier.wait();
                    store
                        .synchronization
                        .submit_fields_json("sync", &request)
                        .is_ok()
                })
            })
            .collect();
        handles
            .into_iter()
            .map(|h| usize::from(h.join().unwrap()))
            .sum::<usize>()
    });
    assert_eq!(successes, 1);
    c.config.ports[0].enabled = false;
    let disabled = PointStore::new(&c.config, &c.points);
    assert!(disabled
        .synchronization
        .submit_fields_json("sync", &atomic_request(Layout::Unit1To9, 7, 0).to_string())
        .is_err());
    assert!(disabled
        .synchronization
        .current("sync", Instant::now())
        .is_none());
}
