use rcd_communicate::{
    config,
    frame::{self, FrameDecoder, Limits},
    points::{PointStore, Quality, Value},
    protocol::{DeviceProtocol, Response},
    unit_report::{Layout, ReportProtocol},
};

const LIMITS: Limits = Limits {
    frame: 4096,
    payload: 4096,
};

#[test]
fn original_document_names_are_exact_for_receive_and_transmit() {
    for (layout, document) in [
        (
            Layout::Unit1To9,
            include_str!("../test_data/FromUnit1ToUnit9.Txt"),
        ),
        (
            Layout::Unit9To1,
            include_str!("../test_data/FromUnit9ToUnit1.Txt"),
        ),
    ] {
        let names: Vec<_> = document
            .lines()
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .collect();
        let receive = layout.definitions("any-internal-task-name");
        assert_eq!(
            receive.iter().map(|p| p.name.as_str()).collect::<Vec<_>>(),
            names
        );
        let transmit = layout.transmit_definitions("any-internal-port-name");
        assert_eq!(
            transmit.iter().map(|p| p.name.as_str()).collect::<Vec<_>>(),
            names[1..]
        );
        assert!(receive
            .iter()
            .filter(|p| p.name.ends_with(".Fire"))
            .all(|p| p.data_type == rcd_communicate::points::DataType::UInt32
                && p.description.contains("8=闭锁")));
    }
    // The supplied 9->2 list has different fields. It must never be silently
    // interpreted using 9->1 byte offsets while its full layout is unavailable.
    assert!(Layout::from_protocol("unit9-to2-v1").is_none());
}

#[test]
fn fire_enumeration_encodes_exact_workbook_bytes_and_rejects_unknown() {
    use rcd_communicate::unit_report::{fire_bytes, fire_code};
    let cases = [
        (1, [10, 133, 0, 6, 0]),
        (2, [1, 136, 0, 7, 0]),
        (3, [1, 200, 0, 7, 0]),
        (4, [5, 134, 0, 12, 0]),
        (5, [5, 166, 0, 12, 0]),
        (6, [6, 130, 0, 18, 0]),
        (7, [6, 194, 0, 18, 0]),
        (8, [9, 133, 0, 4, 0]),
    ];
    for (code, bytes) in cases {
        assert_eq!(fire_code(&bytes), code);
        assert_eq!(fire_bytes(code), Some(bytes));
        for (layout, name, offset) in [
            (Layout::Unit1To9, "DATA.Buildings.11UBE.Fire", 981),
            (Layout::Unit9To1, "DATA.Buildings.91UBH.Fire", 50),
        ] {
            let mut payload = layout.empty_transmit_payload();
            layout
                .write_transmit_field("sync", name, &Value::UInt32(code), &mut payload)
                .unwrap();
            assert_eq!(payload[offset..offset + 5], bytes);
            assert_eq!(
                layout
                    .decode("rx", &payload)
                    .unwrap()
                    .iter()
                    .find(|p| p.name == name)
                    .unwrap()
                    .value,
                Value::UInt32(code)
            );
            let accepted = payload.clone();
            for bad in [0, 9, u32::MAX] {
                assert!(layout
                    .write_transmit_field("sync", name, &Value::UInt32(bad), &mut payload)
                    .is_err());
                assert_eq!(payload, accepted);
            }
            assert!(layout
                .write_transmit_field("sync", name, &Value::String("1".into()), &mut payload)
                .is_err());
        }
    }
    assert_eq!(fire_code(&[0xff; 5]), 0);
}

#[test]
fn client_document_complete_json_example_encodes_original_fields() {
    let document = include_str!("../docs/opc-client-guide.md");
    let json = document
        .split("<!-- example: unit9-to1 -->")
        .nth(1)
        .unwrap()
        .split("```json\n")
        .nth(1)
        .unwrap()
        .split("```")
        .next()
        .unwrap();
    let request: serde_json::Value = serde_json::from_str(json).unwrap();
    let fields = request["fields"].as_object().unwrap();
    assert_eq!(fields.len(), 14);
    let mut payload = Layout::Unit9To1.empty_transmit_payload();
    for field in Layout::Unit9To1.transmit_definitions("sync") {
        let raw = &fields[&field.name];
        let value = if field.data_type == rcd_communicate::points::DataType::UInt32 {
            Value::UInt32(raw.as_u64().unwrap().try_into().unwrap())
        } else {
            Value::String(raw.as_str().unwrap().into())
        };
        Layout::Unit9To1
            .write_transmit_field("sync", &field.name, &value, &mut payload)
            .unwrap();
    }
    assert_eq!(payload[50..55], [1, 0x88, 0, 7, 0]);
    assert_eq!(payload[55..60], [0x0a, 0x85, 0, 6, 0]);
}

#[test]
fn client_document_unit1_complete_json_example_encodes_all_39_inputs() {
    let document = include_str!("../docs/opc-client-guide.md");
    let json = document
        .split("<!-- example: unit1-to9 -->")
        .nth(1)
        .unwrap()
        .split("```json\n")
        .nth(1)
        .unwrap()
        .split("```")
        .next()
        .unwrap();
    let request: serde_json::Value = serde_json::from_str(json).unwrap();
    let fields = request["fields"].as_object().unwrap();
    assert_eq!(fields.len(), 39);
    assert!(!fields.contains_key("DATA.Header.nBlock.z"));
    let mut payload = Layout::Unit1To9.empty_transmit_payload();
    for field in Layout::Unit1To9.transmit_definitions("sync") {
        let raw = &fields[&field.name];
        let value = if field.data_type == rcd_communicate::points::DataType::UInt32 {
            Value::UInt32(raw.as_u64().unwrap().try_into().unwrap())
        } else {
            Value::String(raw.as_str().unwrap().into())
        };
        Layout::Unit1To9
            .write_transmit_field("sync", &field.name, &value, &mut payload)
            .unwrap();
    }
    assert_eq!(payload.len(), 1156);
    assert_eq!(payload[..4], [1, 0, 20, 21]);
    assert_eq!(payload[981..986], [1, 0x88, 0, 7, 0]);
    assert_eq!(payload[986..991], [0x0a, 0x85, 0, 6, 0]);
}
fn bytes(hex: &str) -> Vec<u8> {
    (0..hex.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&hex[i..i + 2], 16).unwrap())
        .collect()
}

// Verbatim wire bytes from 91BAOWEIFENXI.xls Sheet1 columns A,D,G,J,M,P,S,AN.
const UNIT9: &[(&str,&str)] = &[
    ("2c00690022007801e364606a650082a52002ccc2051881122c7c5c8c5cad0c6c0ca4130068a4084007f654d5", "正常"),
    ("3000690026007801e364606a650082a52002ccc2051881122c7c5c8c8c1d0cec0c5cad0c6ca41100678b083b647f7321", "红色报警"),
    ("3000690026007801e364606a650082a52002ccc2051881122c7c5c8c8c2718d819b85a19d8482300750b087be63a6297", "红色报警闪"),
    ("3100690027007801e364606a650082a52002ccc2059899181898f8b81859db187818b85a19d8482300698308442d4ab785", "黄色报警"),
    ("3200690028007801e364606a650082a52002ccc2059899181898f8b818599731f03070b532b091460000704308644a2cf9f9", "黄色报警闪"),
    ("3200690028007801e364606a650082a52002ccc2059899181898f8b818d90e31083170b532b091460000779a0887c6248b85", "故障报警闪（细分原因未定义）"),
    ("3100690027007801e364606a650082a52002ccc2059899181898f8b818d99a188418b85a19d84823006a1a08470f9a1053", "故障报警（细分原因未定义）"),
    ("3100690027007801e364606a650082a52002ccc2059899181898f8b818395b195818b85a19d84823006889083f95d7fcae", "闭锁"),
];
// 01BAOWEIFENXI.xlsx Sheet1 column E: 11UBE HJS.
const UNIT1: &str="5e0084045400785e6364606038c9ccc5c0c7f0929d1dc86666e0626067e0669061fef77f148e86009610f8870b00930fd1009719234a9ce8d022522133130303131f1723e309601ee66a65601ba404432bb09c411000b3d84879f854c1df";

#[test]
fn workbook_golden_frames_decode_all_alarm_states_and_offsets() {
    for (index, (hex, state)) in UNIT9.iter().enumerate() {
        let raw = frame::decode(&bytes(hex), LIMITS).unwrap();
        let values = Layout::Unit9To1.decode("rx9", &raw).unwrap();
        assert_eq!(values.len(), 15);
        assert_eq!(
            rcd_communicate::unit_report::fire_state(&raw[50..55]),
            *state
        );
        assert_eq!(
            values
                .iter()
                .find(|p| p.name.ends_with("91UBH.Fire"))
                .unwrap()
                .value,
            Value::UInt32([1, 2, 3, 4, 5, 7, 6, 8][index])
        );
        assert_eq!(
            values
                .iter()
                .find(|p| p.name.ends_with("91UGM.Fire"))
                .unwrap()
                .value,
            Value::UInt32(1)
        );
        assert_eq!(
            frame::decode(&Layout::Unit9To1.encode(&raw, LIMITS).unwrap(), LIMITS).unwrap(),
            raw
        );
    }
    let raw = frame::decode(&bytes(UNIT1), LIMITS).unwrap();
    let values = Layout::Unit1To9.decode("rx1", &raw).unwrap();
    assert_eq!(values.len(), 40);
    assert_eq!(
        values
            .iter()
            .find(|p| p.name.ends_with("11UBE.Fire"))
            .unwrap()
            .value,
        Value::UInt32(3)
    );
    assert_eq!(
        values
            .iter()
            .find(|p| p.name.ends_with("1JMF34.Fire"))
            .unwrap()
            .value,
        Value::UInt32(0)
    );
}

#[test]
fn workbook_uncompressed_prefix_frames_are_transport_valid_but_not_full_snapshots() {
    for hex in [
        "0c00020002000900ef188e04",
        "110007000700090002850000002d11601e",
        "0c00020002000100e79257cc",
        "0d000300030001000083f9cc60",
        "0e0004000400010000004c73245a",
    ] {
        let wire = bytes(hex);
        let raw = frame::decode(&wire, LIMITS).unwrap();
        assert_eq!(frame::encode(&raw, LIMITS).unwrap(), wire);
        assert!(Layout::Unit1To9.decode("rx1", &raw).is_err());
        assert!(Layout::Unit9To1.decode("rx9", &raw).is_err());
    }
}

#[test]
fn all_golden_frame_split_boundaries_crc_and_concatenation() {
    for hex in UNIT9.iter().map(|r| r.0).chain([UNIT1]) {
        let wire = bytes(hex);
        let raw = frame::decode(&wire, LIMITS).unwrap();
        for split in 0..=wire.len() {
            let mut decoder = FrameDecoder::new(LIMITS).unwrap();
            let mut got = decoder.feed(&wire[..split]).unwrap().payloads;
            got.extend(decoder.feed(&wire[split..]).unwrap().payloads);
            assert_eq!(got, vec![raw.clone()]);
        }
        let mut corrupt = wire.clone();
        corrupt[7] ^= 1;
        assert!(frame::decode(&corrupt, LIMITS).is_err());
        let joined = [wire.clone(), wire].concat();
        assert_eq!(
            FrameDecoder::new(LIMITS)
                .unwrap()
                .feed(&joined)
                .unwrap()
                .payloads
                .len(),
            2
        );
    }
}

#[test]
fn identity_length_unknown_state_and_point_table_are_strict() {
    let mut raw = frame::decode(&bytes(UNIT9[0].0), LIMITS).unwrap();
    for n in 0..raw.len() {
        assert!(Layout::Unit9To1.decode("rx9", &raw[..n]).is_err());
    }
    raw[0] = 1;
    assert!(Layout::Unit9To1.decode("rx9", &raw).is_err());
    raw[0] = 9;
    raw[50] = 0xff;
    let values = Layout::Unit9To1.decode("rx9", &raw).unwrap();
    let state = values
        .iter()
        .find(|p| p.name.ends_with("91UBH.Fire"))
        .unwrap();
    assert_eq!(state.quality, Quality::Good);
    assert_eq!(state.value, Value::UInt32(0));
    for name in ["unit1-receiver", "unit9-receiver"] {
        let path =
            std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(format!("examples/{name}.toml"));
        let loaded = config::load(&path).unwrap();
        config::validate_adapters(&loaded.config).unwrap();
        let missing = loaded
            .points_text
            .lines()
            .take(2)
            .collect::<Vec<_>>()
            .join("\n");
        assert!(config::parse(&loaded.config_text, &missing).is_err());
    }
}

#[test]
fn unsolicited_protocol_has_no_query_and_opc_preserves_alarm_vs_quality() {
    let path =
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("examples/unit1-receiver.toml");
    let loaded = config::load(&path).unwrap();
    let store = PointStore::new(&loaded.config, &loaded.points);
    let task = &loaded.config.ports[0].tasks[0];
    let mut protocol = ReportProtocol(Layout::Unit9To1);
    assert!(protocol.unsolicited());
    assert!(protocol.request(task).is_err());
    let raw = frame::decode(&bytes(UNIT9[2].0), LIMITS).unwrap();
    let Response::Matched(values) = protocol.response(task, &raw).unwrap() else {
        panic!("not matched")
    };
    store
        .publish(&task.id, &values, chrono::Utc::now())
        .unwrap();
    let name = "DATA.Buildings.91UBH.Fire";
    let sample = store.sample(name).unwrap();
    assert_eq!(sample.value, Value::UInt32(3));
    assert_eq!(sample.quality, Quality::Good);
    assert_eq!(
        rcd_communicate::opc::data_value(&sample).status,
        Some(opcua::types::StatusCode::Good)
    );
    store
        .fail(&task.id, Quality::NoCommunication, "peer silent")
        .unwrap();
    let failed = store.sample(name).unwrap();
    assert_eq!(failed.value, Value::UInt32(0));
    assert_eq!(failed.source_timestamp, sample.source_timestamp);
}
