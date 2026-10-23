//! Lossless, read-only draft preparation shared by both lower programs.
//! No filesystem access: saving and full cross-file validation remain the
//! responsibility of the owning program. Unknown TOML/comments survive edits.
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::collections::HashSet;
use toml_edit::{DocumentMut, Item, Table};

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Profile {
    Acquisition,
    Communicate,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq, Hash)]
#[serde(untagged)]
pub enum Segment {
    Key(String),
    Index(usize),
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Edit {
    pub path: Vec<Segment>,
    pub value: Value,
}

#[derive(Serialize)]
pub struct Field {
    path: Vec<Segment>,
    key: String,
    label: String,
    hint: String,
    kind: &'static str,
    value: Value,
    options: Vec<Value>,
    inherited: bool,
    readonly: bool,
}

#[derive(Serialize)]
pub struct Form {
    pub content: String,
    fields: Vec<Field>,
}

fn scalar(item: &Item) -> Option<(&'static str, Value)> {
    if let Some(v) = item.as_integer() {
        Some(("integer", json!(v)))
    } else if let Some(v) = item.as_bool() {
        Some(("boolean", json!(v)))
    } else {
        item.as_str().map(|v| ("string", json!(v)))
    }
}

fn metadata(profile: Profile, key: &str) -> (&str, &str, Vec<Value>, bool) {
    let leaf = key.rsplit('.').next().unwrap_or(key);
    let options = |values: &[&str]| values.iter().map(|v| json!(v)).collect();
    match key {
        "settings.command_retry_count" => (
            "命令最大下发次数（含首次）",
            "3 表示最多发送 3 次，不是额外重试 3 次。收到有效 ACK 后立即停止。保存后重启生效。",
            vec![],
            false,
        ),
        "settings.command_interval_ms" => (
            "控制命令最小间隔（ms）",
            "0 关闭；范围 0–60000。各物理串口独立，从上一条控制事务结束到下一条首次发送；不改变普通轮询或同条命令重试。保存后重启生效。",
            vec![],
            false,
        ),
        "settings.query_retry_count" => (
            "轮询失败补发次数",
            "0 不补发；1 失败后最多补发一次。",
            vec![json!(0), json!(1)],
            false,
        ),
        "settings.timeout" => (
            "串口读取超时（ms）",
            "增大后设备无响应时等待更久。",
            vec![],
            false,
        ),
        "settings.poll_interval_ms" => ("轮询间隔（ms）", "不会改变命令 ACK 规则。", vec![], false),
        "settings.parity_switch_delay_ms" => (
            "校验位切换保护（ms）",
            "范围 0–100；影响串口设备时序，请按设备要求设置。",
            vec![],
            false,
        ),
        "settings.transmit_settle_guard_ms" => (
            "发送完成保护（ms）",
            "范围 0–100；线速发送完成后追加等待。",
            vec![],
            false,
        ),
        "server.primary_ip" => (
            "主机地址",
            "含原 TCP 端口；更改地址可能导致管理端断连。",
            vec![],
            false,
        ),
        "server.secondary_ip" => ("备机地址", "含原 TCP 端口。", vec![], false),
        "server.run_on" | "server.current_run" => (
            "运行角色",
            "保存后重启生效。",
            options(&["primary", "secondary"]),
            false,
        ),
        "serial.baud_rate" => (
            "全局默认波特率",
            "各串口 CSV 中的波特率可覆盖此项；请同时核对对应 CSV。",
            vec![],
            false,
        ),
        "serial.data_bits" => (
            "数据位",
            "全局串口参数。",
            options(&["Five", "Six", "Seven", "Eight"]),
            false,
        ),
        "serial.stop_bits" => ("停止位", "全局串口参数。", options(&["One", "Two"]), false),
        "serial.parity" => (
            "校验位",
            "轮询协议的 Mark/Space 切换行为仍由协议决定。",
            options(&["None", "Odd", "Even", "Mark", "Space"]),
            false,
        ),
        "serial.flow_control" => (
            "流控（预留，未生效）",
            "程序目前未使用此字段，不将它显示为可生效参数。",
            vec![],
            true,
        ),
        "calculation.mode" => (
            "计算执行位置",
            "local 下级计算；manager 由管理端计算。需重启。",
            options(&["local", "manager"]),
            false,
        ),
        "logging.level" => (
            "日志等级",
            "修改文件后需重启生效；模块级过滤表达式可在原文中编辑。",
            options(&["error", "warn", "info", "debug", "trace"]),
            false,
        ),
        "logging.rotate_age" => (
            "日志按时间轮转",
            "与大小轮转条件共同生效。",
            options(&["day", "hour", "minute", "second", "none"]),
            false,
        ),
        "logging.write_mode" => (
            "日志写入方式",
            "direct 直接写；buffer_and_flush 缓冲并刷新。",
            options(&["direct", "buffer_dont_flush", "buffer_and_flush"]),
            false,
        ),
        "logging.directory" => (
            "日志目录",
            "相对路径按下级可执行文件目录解析，不是管理端目录。",
            vec![],
            false,
        ),
        "logging.basename" => (
            "日志文件名前缀",
            "只能为单个文件名，不含目录。",
            vec![],
            false,
        ),
        "logging.keep_files" => (
            "日志保留文件数",
            "0 表示不自动清理，请关注磁盘空间。",
            vec![],
            false,
        ),
        "logging.rotate_size_bytes" => (
            "日志轮转大小（字节）",
            "正整数；未配置时仅按时间轮转。",
            vec![],
            false,
        ),
        "logging.serial_frame_logs" => (
            "记录串口原始帧",
            "会显著增加日志量。保存后重启生效。",
            vec![],
            false,
        ),
        "logging.opcua_transport_errors" => {
            ("记录 OPC 传输错误", "用于排查客户端断连。", vec![], false)
        }
        "opc.bind_ip" => (
            "OPC 监听地址",
            "修改可能使管理端无法再连接，请核对访问地址。",
            vec![],
            false,
        ),
        "opc.advertised_host" => (
            "OPC 广告主机地址",
            "填写客户端可访问的主机名或 IPv4 地址，不带协议和端口。",
            vec![],
            false,
        ),
        "opc.port" => (
            "OPC 服务端口",
            "1–65535；不能与已有程序占用相同端口。",
            vec![],
            false,
        ),
        "schema_version" | "instance_id" | "points_file" if profile == Profile::Communicate => {
            (key, "实例身份和配置文件绑定不允许远程更换。", vec![], true)
        }
        _ if profile == Profile::Communicate => match leaf {
            "enabled" => (
                "启用",
                "尚未实现的协议不能启用；保存时由下级再次校验。",
                vec![],
                false,
            ),
            "device" => (
                "串口设备路径",
                "请使用该来源机器的设备路径，勿与其它程序争用串口。",
                vec![],
                false,
            ),
            "baud_rate" => ("波特率", "范围 300–4000000。", vec![], false),
            "parity" => ("校验位", "", options(&["none", "even", "odd"]), false),
            "data_bits" => (
                "数据位",
                "",
                vec![json!(5), json!(6), json!(7), json!(8)],
                false,
            ),
            "stop_bits" => ("停止位", "", vec![json!(1), json!(2)], false),
            "first_byte_timeout_ms" => ("首字节超时（ms）", "1–60000。", vec![], false),
            "frame_timeout_ms" => ("完整帧超时（ms）", "不得小于首字节超时。", vec![], false),
            "write_timeout_ms" => ("发送超时（ms）", "1–60000。", vec![], false),
            "late_frame_guard_ms" => (
                "迟到帧隔离（ms）",
                "1–60000；不把旧响应计入下一次采集。",
                vec![],
                false,
            ),
            "interval_ms" => (
                "任务轮询间隔（ms）",
                "10–3600000；查询型任务生效，机组同步使用独立发送周期。",
                vec![],
                false,
            ),
            "reconnect_min_ms" => ("重连最短间隔（ms）", "不小于 100。", vec![], false),
            "reconnect_max_ms" => (
                "重连最长间隔（ms）",
                "不超过 60000，且不小于最短间隔。",
                vec![],
                false,
            ),
            "max_frame_bytes" => ("最大帧字节数", "18–65535。", vec![], false),
            "max_payload_bytes" => ("最大载荷字节数", "1–65535。", vec![], false),
            "protocol" => (
                "协议适配器",
                "同步协议按对方发送方向选择；不能把数据同步当作查询。",
                options(&["pending", "unit1-to9-v1", "unit9-to1-v1"]),
                false,
            ),
            "transmit_protocol" => (
                "本方发送布局",
                "须与接收方向相反。",
                options(&["unit1-to9-v1", "unit9-to1-v1"]),
                false,
            ),
            "transmit_enabled" => (
                "启用周期同步发送",
                "没有有效本方快照时不发送，不使用样例代替现场值。",
                vec![],
                false,
            ),
            "send_interval_ms" => (
                "同步发送周期（ms）",
                "文档示例5秒；不补发积压周期。",
                vec![],
                false,
            ),
            "initial_delay_ms" => (
                "启动错峰延时（ms）",
                "双方应错开配置，且小于发送周期。",
                vec![],
                false,
            ),
            "idle_gap_ms" => (
                "发送前静默间隔（ms）",
                "接收中让路，串口持续有数据时不强行发送。",
                vec![],
                false,
            ),
            "receive_timeout_ms" => (
                "对方数据过期时间（ms）",
                "超时将接收点清零并标记通信中断。",
                vec![],
                false,
            ),
            "snapshot_max_age_ms" => (
                "OPC输入超时（ms）",
                "默认15000；任一必需输入超时停止发送，须大于刷新间隔。",
                vec![],
                false,
            ),
            "input_refresh_interval_ms" => (
                "OPC客户端刷新间隔（ms）",
                "默认5000；客户端须按此周期刷新全部发送输入，变化立即写入；不会自动配置外部客户端。",
                vec![],
                false,
            ),
            _ => (
                key,
                "保存后重启生效；网络参数变化可能使当前连接中断。",
                vec![],
                false,
            ),
        },
        _ => (
            key,
            "保存后重启生效；完整校验以下级返回为准。",
            vec![],
            false,
        ),
    }
}

fn walk(
    table: &Table,
    path: &mut Vec<Segment>,
    fields: &mut Vec<Field>,
    profile: Profile,
) -> Result<(), String> {
    if path.len() > 24 {
        return Err("参数嵌套过深，请使用配置原文编辑".into());
    }
    for (name, item) in table.iter() {
        path.push(Segment::Key(name.into()));
        if let Some((kind, value)) = scalar(item) {
            let key = path
                .iter()
                .map(|part| match part {
                    Segment::Key(k) => k.clone(),
                    Segment::Index(i) => (i + 1).to_string(),
                })
                .collect::<Vec<_>>()
                .join(".");
            // Secrets remain in the existing advanced file editor; do not copy
            // them into a second form representation or metadata payload.
            let sensitive = ["password", "secret", "token", "private_key"]
                .iter()
                .any(|v| name.to_ascii_lowercase().contains(v));
            if !sensitive {
                let (label, hint, options, readonly) = metadata(profile, &key);
                fields.push(Field {
                    path: path.clone(),
                    label: label.into(),
                    hint: hint.into(),
                    key,
                    kind,
                    value,
                    options,
                    inherited: false,
                    readonly,
                });
            }
        } else if let Some(child) = item.as_table() {
            walk(child, path, fields, profile)?;
        } else if let Some(array) = item.as_array_of_tables() {
            for (index, child) in array.iter().enumerate() {
                path.push(Segment::Index(index));
                walk(child, path, fields, profile)?;
                path.pop();
            }
        }
        path.pop();
        if fields.len() > 4096 {
            return Err("参数过多，请使用配置原文编辑".into());
        }
    }
    Ok(())
}

fn fields(doc: &DocumentMut, profile: Profile) -> Result<Vec<Field>, String> {
    let mut fields = Vec::new();
    walk(doc.as_table(), &mut Vec::new(), &mut fields, profile)?;
    let mut keys = HashSet::new();
    if fields.iter().any(|field| !keys.insert(field.key.clone())) {
        return Err("参数显示路径重名，请使用配置原文编辑".into());
    }
    if profile == Profile::Acquisition {
        for (name, value) in [
            ("poll_interval_ms", 1),
            ("command_retry_count", 3),
            ("command_interval_ms", 0),
            ("query_retry_count", 0),
            ("parity_switch_delay_ms", 1),
            ("transmit_settle_guard_ms", 1),
        ] {
            let key = format!("settings.{name}");
            if fields.iter().any(|f| f.key == key) {
                continue;
            }
            let (label, hint, options, readonly) = metadata(profile, &key);
            fields.push(Field {
                path: vec![Segment::Key("settings".into()), Segment::Key(name.into())],
                label: label.into(),
                hint: hint.into(),
                key,
                kind: "integer",
                value: json!(value),
                options,
                inherited: true,
                readonly,
            });
        }
    }
    if profile == Profile::Communicate {
        if let Some(ports) = doc.get("ports").and_then(Item::as_array_of_tables) {
            for (index, port) in ports.iter().enumerate() {
                let Some(sync) = port.get("synchronization").and_then(Item::as_table) else {
                    continue;
                };
                for (name, value) in [
                    ("input_refresh_interval_ms", 5000),
                    ("snapshot_max_age_ms", 15000),
                ] {
                    if sync.contains_key(name) {
                        continue;
                    }
                    let key = format!("ports.{}.synchronization.{name}", index + 1);
                    let (label, hint, options, readonly) = metadata(profile, &key);
                    fields.push(Field {
                        path: vec![
                            Segment::Key("ports".into()),
                            Segment::Index(index),
                            Segment::Key("synchronization".into()),
                            Segment::Key(name.into()),
                        ],
                        label: label.into(),
                        hint: hint.into(),
                        key,
                        kind: "integer",
                        value: json!(value),
                        options,
                        inherited: true,
                        readonly,
                    });
                }
            }
        }
    }
    if fields.len() > 4096 {
        return Err("参数过多，请使用配置原文编辑".into());
    }
    Ok(fields)
}

fn target<'a>(table: &'a mut Table, path: &[Segment]) -> Result<&'a mut Item, String> {
    let Some(Segment::Key(key)) = path.first() else {
        return Err("无效参数路径".into());
    };
    if path.len() == 1 {
        return Ok(table.entry(key).or_insert(Item::None));
    }
    let item = table.get_mut(key).ok_or("参数表不存在")?;
    if let Some(Segment::Index(index)) = path.get(1) {
        let child = item
            .as_array_of_tables_mut()
            .and_then(|v| v.get_mut(*index))
            .ok_or("参数索引无效")?;
        target(child, &path[2..])
    } else {
        target(item.as_table_mut().ok_or("参数不是表")?, &path[1..])
    }
}

pub fn prepare(content: &str, edits: &[Edit], profile: Profile) -> Result<Form, String> {
    if content.len() > 2 * 1024 * 1024 || edits.len() > 4096 {
        return Err("配置草稿超限".into());
    }
    let mut doc: DocumentMut = content
        .parse()
        .map_err(|_| "TOML 语法无效，请先在配置原文中修正")?;
    let available = fields(&doc, profile)?;
    let mut seen = HashSet::new();
    for edit in edits {
        if !seen.insert(&edit.path) {
            return Err("重复的参数修改".into());
        }
        let field = available
            .iter()
            .find(|field| field.path == edit.path)
            .ok_or("不支持的参数路径")?;
        if field.readonly {
            return Err(format!("{} 不允许修改", field.label));
        }
        if profile == Profile::Acquisition
            && field.key == "settings.command_interval_ms"
            && !edit.value.as_u64().is_some_and(|value| value <= 60_000)
        {
            return Err("控制命令最小间隔必须为 0–60000 毫秒的整数".into());
        }
        let mut value = match field.kind {
            "integer" => toml_edit::Value::from(edit.value.as_i64().ok_or("需要整数")?),
            "boolean" => toml_edit::Value::from(edit.value.as_bool().ok_or("需要布尔值")?),
            _ => toml_edit::Value::from(
                edit.value
                    .as_str()
                    .filter(|s| s.len() <= 8192 && !s.contains('\0'))
                    .ok_or("需要有界字符串")?,
            ),
        };
        if !field.options.is_empty() && !field.options.contains(&edit.value) {
            return Err(format!("{} 选项无效", field.label));
        }
        let item = target(doc.as_table_mut(), &edit.path)?;
        if let Some(old) = item.as_value() {
            *value.decor_mut() = old.decor().clone();
        }
        *item = Item::Value(value);
    }
    let fields = fields(&doc, profile)?;
    let content = if edits.is_empty() {
        content.into()
    } else {
        doc.to_string()
    };
    if content.len() > 2 * 1024 * 1024 {
        return Err("生成的配置草稿超限，参数未保存".into());
    }
    Ok(Form { content, fields })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn acquisition_command_interval_default_is_visible_and_edits_preserve_comments() {
        let text = "# current configuration\n[settings]\ntimeout=50 # preserve\n";
        let initial = prepare(text, &[], Profile::Acquisition).unwrap();
        assert_eq!(initial.content, text);
        let interval = initial
            .fields
            .iter()
            .find(|f| f.key == "settings.command_interval_ms")
            .unwrap();
        assert_eq!(interval.value, json!(0));
        assert!(interval.inherited);
        assert!(interval.hint.contains("不改变普通轮询"));
        for value in [0, 500, 60_000] {
            let edit = serde_json::from_value(
                json!({"path":["settings","command_interval_ms"],"value":value}),
            )
            .unwrap();
            let changed = prepare(text, &[edit], Profile::Acquisition).unwrap();
            assert!(changed.content.contains("# preserve"));
            assert!(changed
                .content
                .contains(&format!("command_interval_ms = {value}")));
        }
        let other = prepare("[settings]\ntimeout=50\n", &[], Profile::Communicate).unwrap();
        assert!(!other
            .fields
            .iter()
            .any(|f| f.key == "settings.command_interval_ms"));
    }

    #[test]
    fn acquisition_command_interval_prepare_rejects_invalid_values() {
        for value in [
            json!(-1),
            json!(60_001),
            json!(1.5),
            json!("500"),
            json!(true),
        ] {
            let edit = serde_json::from_value(
                json!({"path":["settings","command_interval_ms"],"value":value}),
            )
            .unwrap();
            assert!(prepare("[settings]\ntimeout=50\n", &[edit], Profile::Acquisition).is_err());
        }
    }

    #[test]
    fn communicate_refresh_defaults_are_visible_and_editable_without_read_mutation() {
        let text = "[[ports]]\nid='sync'\n[ports.synchronization]\ntransmit_enabled=false # keep\n";
        let form = prepare(text, &[], Profile::Communicate).unwrap();
        assert_eq!(form.content, text);
        let refresh = form
            .fields
            .iter()
            .find(|f| f.key.ends_with(".input_refresh_interval_ms"))
            .unwrap();
        assert!(refresh.inherited);
        assert_eq!(refresh.value, json!(5000));
        let timeout = form
            .fields
            .iter()
            .find(|f| f.key.ends_with(".snapshot_max_age_ms"))
            .unwrap();
        assert_eq!(timeout.value, json!(15000));
        let edit: Edit = serde_json::from_value(
            json!({"path":["ports",0,"synchronization","input_refresh_interval_ms"],"value":4000}),
        )
        .unwrap();
        let changed = prepare(text, &[edit], Profile::Communicate).unwrap();
        assert!(changed.content.contains("input_refresh_interval_ms = 4000"));
        assert!(changed.content.contains("# keep"));
    }
    #[test]
    fn preserves_comments_and_does_not_materialize_defaults_on_read() {
        let text = "# 现场配置\n[settings]\ntimeout = 50 # 保留\n";
        let initial = prepare(text, &[], Profile::Acquisition).unwrap();
        assert_eq!(initial.content, text);
        let edit: Edit =
            serde_json::from_value(json!({"path":["settings","command_retry_count"],"value":5}))
                .unwrap();
        let result = prepare(text, &[edit], Profile::Acquisition).unwrap();
        assert!(result.content.contains("timeout = 50 # 保留"));
        assert!(result.content.contains("command_retry_count = 5"));
        assert!(!result.content.contains("query_retry_count ="));
    }
    #[test]
    fn array_edits_target_one_port_and_keep_inline_comments() {
        let text = "[[ports]]\nparity='even' # A\n[[ports]]\nparity='odd' # B\n";
        let edit: Edit =
            serde_json::from_value(json!({"path":["ports",1,"parity"],"value":"none"})).unwrap();
        let result = prepare(text, &[edit], Profile::Communicate).unwrap();
        assert!(result.content.contains("parity='even' # A"));
        assert!(result.content.contains("\"none\" # B"));
    }
    #[test]
    fn rejects_unknown_readonly_and_wrong_typed_edits() {
        for (path, value) in [
            (json!(["instance_id"]), json!("other")),
            (json!(["unknown"]), json!(1)),
            (json!(["port"]), json!("wrong")),
        ] {
            let edit = serde_json::from_value(json!({"path":path,"value":value})).unwrap();
            assert!(prepare("instance_id='x'\nport=42\n", &[edit], Profile::Communicate).is_err());
        }
    }

    #[test]
    fn rejects_expanded_draft_over_transport_limit() {
        let text = format!("#{}\nname='x'\n", "x".repeat(2 * 1024 * 1024 - 32));
        let edit =
            serde_json::from_value(json!({"path":["name"],"value":"x".repeat(8192)})).unwrap();
        assert!(prepare(&text, &[edit], Profile::Communicate).is_err());
    }
}
