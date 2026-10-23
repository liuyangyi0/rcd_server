use chrono::Utc;
use rcd_communicate::{
    config::{self, PointDefinition},
    points::{DataType, PointStore, PointUpdate, Quality, Value},
};

fn loaded() -> config::LoadedConfig {
    config::load(&std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("examples/config.toml"))
        .unwrap()
}

#[test]
fn example_is_parseable_but_cannot_enable_unknown_protocol() {
    let mut value = loaded();
    config::validate_adapters(&value.config).unwrap();
    value.config.ports[0].enabled = true;
    value.config.ports[0].tasks[0].enabled = true;
    assert!(config::validate_adapters(&value.config)
        .unwrap_err()
        .to_string()
        .contains("pending"));
}

#[test]
fn rejects_unknown_fields_duplicates_references_types_and_unsafe_endpoint() {
    let raw = include_str!("../examples/config.toml").replace(
        "schema_version = 1",
        "schema_version = 1\nunrecognized = true",
    );
    assert!(toml::from_str::<config::Config>(&raw).is_err());
    let mut value = loaded();
    value.points.push(value.points[0].clone());
    assert!(config::validate(&value.config, &value.points).is_err());
    value.points.pop();
    value.points[0].task_id = "missing".into();
    assert!(config::validate(&value.config, &value.points).is_err());
    value = loaded();
    value.config.opc.advertised_host = "0.0.0.0".into();
    assert!(config::validate(&value.config, &value.points).is_err());
    value = loaded();
    value.config.ports.push(value.config.ports[0].clone());
    value.config.ports[1].id = "another".into();
    assert!(config::validate(&value.config, &value.points).is_err());
}

#[test]
fn bounded_csv_rejects_unknown_columns_and_bad_types() {
    let dir = tempfile::tempdir().unwrap();
    let file = dir.path().join("config.toml");
    std::fs::write(&file, include_str!("../examples/config.toml")).unwrap();
    let csv = dir.path().join("points.csv");
    std::fs::write(&csv, "name,task_id,data_type,description,extra\n").unwrap();
    assert!(config::load(&file).is_err());
    std::fs::write(
        &csv,
        "name,task_id,data_type,description\nA,example-task,Int128,test\n",
    )
    .unwrap();
    assert!(config::load(&file).is_err());
}

fn update(name: &str, value: Value) -> PointUpdate {
    PointUpdate {
        name: name.into(),
        value,
        quality: Quality::Good,
        error: String::new(),
    }
}

#[test]
fn publication_is_atomic_and_failure_zeros_without_forging_source_time() {
    let value = loaded();
    let store = PointStore::new(&value.config, &value.points);
    assert_eq!(
        store.sample("COMM_EXAMPLE_VALUE").unwrap().quality,
        Quality::OutOfService
    );
    let at = Utc::now();
    let values = [
        update("COMM_EXAMPLE_VALUE", Value::Double(12.5)),
        update("COMM_EXAMPLE_STATE", Value::Boolean(true)),
    ];
    store.publish("example-task", &values, at).unwrap();
    let invalid = [
        update("COMM_EXAMPLE_VALUE", Value::Double(999.0)),
        update("COMM_EXAMPLE_STATE", Value::UInt32(1)),
    ];
    assert!(store.publish("example-task", &invalid, at).is_err());
    assert_eq!(
        store.sample("COMM_EXAMPLE_VALUE").unwrap().value,
        Value::Double(12.5)
    );
    assert!(store.publish("example-task", &values[..1], at).is_err());
    store
        .fail("example-task", Quality::NoCommunication, "timeout")
        .unwrap();
    let sample = store.sample("COMM_EXAMPLE_VALUE").unwrap();
    assert_eq!(sample.value, Value::Double(0.0));
    assert_eq!(sample.quality, Quality::NoCommunication);
    assert_eq!(sample.source_timestamp, Some(at));
    assert_eq!(
        store.sample("COMM_EXAMPLE_STATE").unwrap().value,
        Value::Boolean(false)
    );
}

#[test]
fn nan_and_duplicate_response_entries_never_publish() {
    let value = loaded();
    let store = PointStore::new(&value.config, &value.points);
    let at = Utc::now();
    assert!(store
        .publish(
            "example-task",
            &[
                update("COMM_EXAMPLE_VALUE", Value::Double(f64::NAN)),
                update("COMM_EXAMPLE_STATE", Value::Boolean(true))
            ],
            at
        )
        .is_err());
    assert!(store
        .publish(
            "example-task",
            &[
                update("COMM_EXAMPLE_VALUE", Value::Double(1.0)),
                update("COMM_EXAMPLE_VALUE", Value::Double(2.0))
            ],
            at
        )
        .is_err());
    assert!(store
        .sample("COMM_EXAMPLE_VALUE")
        .unwrap()
        .source_timestamp
        .is_none());
}

#[test]
fn bad_quality_payload_is_zero_and_other_tasks_are_unchanged() {
    let mut value = loaded();
    let mut task = value.config.ports[0].tasks[0].clone();
    task.id = "other".into();
    value.config.ports[0].tasks.push(task);
    value.points.push(PointDefinition {
        name: "OTHER".into(),
        task_id: "other".into(),
        data_type: DataType::UInt32,
        description: String::new(),
    });
    let store = PointStore::new(&value.config, &value.points);
    let mut bad = update("OTHER", Value::UInt32(99));
    bad.quality = Quality::Configuration;
    store.publish("other", &[bad], Utc::now()).unwrap();
    assert_eq!(store.sample("OTHER").unwrap().value, Value::UInt32(0));
    store
        .fail("example-task", Quality::NoCommunication, "timeout")
        .unwrap();
    assert_eq!(
        store.sample("OTHER").unwrap().quality,
        Quality::Configuration
    );
}
