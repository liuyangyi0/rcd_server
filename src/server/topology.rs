//! 增量设备拓扑：为旧的扁平 OPC 点表提供串口、设备及计算依赖归属。
//!
//! 数据节点仍由 `urn:SerialServer` 管理；本命名空间只保存只读元数据和引用，
//! 不复制实时值、不改变旧 NodeId/访问级别/命令方法。

use std::collections::{BTreeMap, BTreeSet};
use std::io;
use std::sync::Arc;

use opcua::nodes::AccessLevel;
use opcua::server::address_space::{AddressSpace, ReferenceDirection, Variable};
use opcua::server::node_manager::memory::SimpleNodeManager;
use opcua::types::{
    DataTypeId, NodeId, QualifiedName, ReferenceTypeId, UAString, VariableTypeId, Variant,
};
use serde::Serialize;

use crate::calc_engine::engine::CalcEngine;
use crate::serial::port_config::SerialPortConfig;

use super::opcua::{opc_port_stem, opc_send_method_name, opc_send_node_name};

pub const TOPOLOGY_NAMESPACE_URI: &str = "urn:SerialServer:topology";
pub const TOPOLOGY_MANIFEST_NODE_ID: &str = "Topology/Manifest";
pub const TOPOLOGY_SCHEMA_VERSION: u32 = 1;

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TopologyManifest {
    pub schema_version: u32,
    pub ports: Vec<TopologyPort>,
    pub calculations: Vec<TopologyCalculation>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TopologyPort {
    pub port_id: String,
    pub port_name: String,
    pub send_node_id: String,
    pub send_method_id: String,
    pub devices: Vec<TopologyDevice>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TopologyDevice {
    pub device_id: String,
    pub device_address: u8,
    pub name: String,
    pub kks_prefix: String,
    pub node_ids: Vec<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Serialize)]
pub struct DeviceOwner {
    pub port_id: String,
    pub device_id: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TopologyCalculation {
    pub node_id: String,
    pub dependency_node_ids: Vec<String>,
    pub owners: Vec<DeviceOwner>,
}

impl TopologyManifest {
    /// 从实际已加载配置构建完整、确定性排序的静态拓扑。
    pub fn build(serial_ports: &[SerialPortConfig], engine: &CalcEngine) -> io::Result<Self> {
        let mut ports = BTreeMap::new();
        let mut point_owners = BTreeMap::new();

        for port in serial_ports {
            validate_identifier(&port.port_number, "串口")?;
            if ports.contains_key(&port.port_number) {
                return Err(invalid(format!("拓扑串口重复: {}", port.port_number)));
            }
            let port_name = opc_port_stem(&port.port_number).to_string();
            validate_identifier(&port_name, "串口显示名")?;
            let mut devices = BTreeMap::new();

            for device in &port.devices {
                if device.config.com != port.port_number {
                    return Err(invalid(format!(
                        "拓扑设备串口不一致: 设备配置 {}:{} 位于 {}",
                        device.config.com, device.config.device_id, port.port_number,
                    )));
                }
                // RCD 地址是完整的 u8，不能套用其他协议的广播/站地址限制。
                // CSV 解析阶段已拒绝不在 0..=255 的值。
                let device_id = device.config.device_id.to_string();
                if devices.contains_key(&device_id) {
                    return Err(invalid(format!(
                        "拓扑设备重复: {}:{}",
                        port.port_number, device_id,
                    )));
                }
                let owner = DeviceOwner {
                    port_id: port.port_number.clone(),
                    device_id: device_id.clone(),
                };
                let mut node_ids = Vec::with_capacity(device.records.len());
                for record in &device.records {
                    validate_identifier(&record.kks, "点位")?;
                    if let Some(previous) = point_owners.insert(record.kks.clone(), owner.clone()) {
                        return Err(invalid(format!(
                            "拓扑点位归属重复: {} 属于 {}:{} 和 {}:{}",
                            record.kks,
                            previous.port_id,
                            previous.device_id,
                            owner.port_id,
                            owner.device_id,
                        )));
                    }
                    node_ids.push(record.kks.clone());
                }
                node_ids.sort();
                devices.insert(
                    device_id.clone(),
                    TopologyDevice {
                        device_id,
                        device_address: device.config.device_id,
                        name: format!("设备 {}", device.config.device_id),
                        kks_prefix: device.config.kks_prefix.clone(),
                        node_ids,
                    },
                );
            }

            ports.insert(
                port.port_number.clone(),
                TopologyPort {
                    port_id: port.port_number.clone(),
                    port_name,
                    send_node_id: opc_send_node_name(&port.port_number),
                    send_method_id: opc_send_method_name(&port.port_number),
                    devices: devices.into_values().collect(),
                },
            );
        }

        // 与 owners 一起保留“全部依赖是否已知”，避免 unknown + A 被错误归为 A，
        // 并将该不确定性沿计算 DAG 传播。常量本身是已知但没有设备归属。
        let mut resolved_calculations: BTreeMap<String, (BTreeSet<DeviceOwner>, bool)> =
            BTreeMap::new();
        let mut calculations = BTreeMap::new();
        for (node_id, dependencies) in engine.output_dependencies() {
            validate_identifier(node_id, "计算点")?;
            if point_owners.contains_key(node_id) || calculations.contains_key(node_id) {
                return Err(invalid(format!("拓扑计算点归属重复: {node_id}")));
            }
            let mut owners = BTreeSet::new();
            let mut fully_resolved = true;
            let dependency_node_ids: Vec<String> = dependencies
                .iter()
                .cloned()
                .collect::<BTreeSet<_>>()
                .into_iter()
                .collect();
            for dependency in &dependency_node_ids {
                if let Some(owner) = point_owners.get(dependency) {
                    owners.insert(owner.clone());
                } else if let Some((inherited, resolved)) = resolved_calculations.get(dependency) {
                    owners.extend(inherited.iter().cloned());
                    fully_resolved &= *resolved;
                } else {
                    fully_resolved = false;
                }
            }
            if !fully_resolved {
                owners.clear();
            }
            resolved_calculations.insert(node_id.to_string(), (owners.clone(), fully_resolved));
            calculations.insert(
                node_id.to_string(),
                TopologyCalculation {
                    node_id: node_id.to_string(),
                    dependency_node_ids,
                    owners: owners.into_iter().collect(),
                },
            );
        }

        Ok(Self {
            schema_version: TOPOLOGY_SCHEMA_VERSION,
            ports: ports.into_values().collect(),
            calculations: calculations.into_values().collect(),
        })
    }

    pub fn to_json(&self) -> io::Result<String> {
        serde_json::to_string(self)
            .map_err(|error| invalid(format!("拓扑 manifest 序列化失败: {error}")))
    }

    pub fn device_count(&self) -> usize {
        self.ports.iter().map(|port| port.devices.len()).sum()
    }

    pub(super) fn metadata_variable_count(&self) -> usize {
        2 + self.ports.len() * 5 + self.device_count() * 4 + self.calculations.len() * 2
    }

    /// 每个采集点和计算点恰好列出一次；命令节点单独由端口标识。
    fn data_node_ids(&self) -> impl Iterator<Item = &str> {
        self.ports
            .iter()
            .flat_map(|port| &port.devices)
            .flat_map(|device| &device.node_ids)
            .map(String::as_str)
            .chain(self.calculations.iter().map(|calc| calc.node_id.as_str()))
    }
}

/// 在独立节点管理器发布元数据。提前核对所有外部节点，失败时不发布半份拓扑。
pub(super) fn publish_topology(
    topology_manager: &Arc<SimpleNodeManager>,
    business_manager: &Arc<SimpleNodeManager>,
    topology_ns: u16,
    business_ns: u16,
    manifest: &TopologyManifest,
    manifest_json: &str,
) -> io::Result<()> {
    {
        let business = business_manager.address_space().read();
        for identifier in manifest.data_node_ids().chain(
            manifest
                .ports
                .iter()
                .flat_map(|port| [port.send_node_id.as_str(), port.send_method_id.as_str()]),
        ) {
            if !business.node_exists(&NodeId::new(business_ns, identifier)) {
                return Err(invalid(format!("拓扑引用的业务节点不存在: {identifier}")));
            }
        }
    }
    let mut address_space = topology_manager.address_space().write();
    create_topology_nodes(
        &mut address_space,
        topology_ns,
        business_ns,
        manifest,
        manifest_json,
    )
}

fn create_topology_nodes(
    address_space: &mut AddressSpace,
    ns: u16,
    business_ns: u16,
    manifest: &TopologyManifest,
    manifest_json: &str,
) -> io::Result<()> {
    let known_nodes: BTreeSet<&str> = manifest.data_node_ids().collect();
    let root = add_folder(
        address_space,
        ns,
        "Topology",
        "Topology",
        "Topology",
        &NodeId::objects_folder_id(),
    )?;
    add_scalar(
        address_space,
        ns,
        TOPOLOGY_MANIFEST_NODE_ID,
        "Manifest",
        DataTypeId::String,
        Variant::String(manifest_json.into()),
        &root,
    )?;
    add_scalar(
        address_space,
        ns,
        "Topology/SchemaVersion",
        "SchemaVersion",
        DataTypeId::UInt32,
        Variant::UInt32(manifest.schema_version),
        &root,
    )?;
    let ports_root = add_folder(address_space, ns, "Topology/Ports", "Ports", "Ports", &root)?;

    for port in &manifest.ports {
        let path = format!("Topology/Ports/{}", path_component(&port.port_id));
        let port_node = add_folder(
            address_space,
            ns,
            &path,
            &port.port_id,
            &port.port_name,
            &ports_root,
        )?;
        for (name, value) in [
            ("PortId", port.port_id.as_str()),
            ("PortName", port.port_name.as_str()),
            ("SendNodeId", port.send_node_id.as_str()),
            ("SendMethodId", port.send_method_id.as_str()),
            ("SendMethodObjectId", "Methods"),
        ] {
            add_string(address_space, ns, &path, name, value, &port_node)?;
        }
        let commands = add_folder(
            address_space,
            ns,
            &format!("{path}/Commands"),
            "Commands",
            "Commands",
            &port_node,
        )?;
        address_space.insert_reference(
            &commands,
            &NodeId::new(business_ns, port.send_node_id.as_str()),
            ReferenceTypeId::Organizes,
        );
        // Method 仍只从原 Methods 对象调用。不能发布一个看似可调用的新父对象：
        // Call 服务验证 HasComponent，Organizes 别名不能替代原 ObjectId。
        let devices_root = add_folder(
            address_space,
            ns,
            &format!("{path}/Devices"),
            "Devices",
            "Devices",
            &port_node,
        )?;
        for device in &port.devices {
            let device_path = format!("{path}/Devices/{}", device.device_id);
            let device_node = add_folder(
                address_space,
                ns,
                &device_path,
                &device.device_id,
                &device.name,
                &devices_root,
            )?;
            for (name, value) in [
                ("DeviceId", device.device_id.as_str()),
                ("Name", device.name.as_str()),
                ("KksPrefix", device.kks_prefix.as_str()),
            ] {
                add_string(address_space, ns, &device_path, name, value, &device_node)?;
            }
            add_scalar(
                address_space,
                ns,
                &format!("{device_path}/DeviceAddress"),
                "DeviceAddress",
                DataTypeId::Byte,
                Variant::Byte(device.device_address),
                &device_node,
            )?;
            let points = add_folder(
                address_space,
                ns,
                &format!("{device_path}/Points"),
                "Points",
                "Points",
                &device_node,
            )?;
            for identifier in &device.node_ids {
                address_space.insert_reference(
                    &points,
                    &NodeId::new(business_ns, identifier.as_str()),
                    ReferenceTypeId::Organizes,
                );
            }
        }
    }

    let calculations_root = add_folder(
        address_space,
        ns,
        "Topology/Calculations",
        "Calculations",
        "Calculations",
        &root,
    )?;
    for calc in &manifest.calculations {
        let path = format!("Topology/Calculations/{}", path_component(&calc.node_id));
        let calculation = add_folder(
            address_space,
            ns,
            &path,
            &calc.node_id,
            &calc.node_id,
            &calculations_root,
        )?;
        add_string(
            address_space,
            ns,
            &path,
            "NodeId",
            &calc.node_id,
            &calculation,
        )?;
        add_strings(
            address_space,
            ns,
            &format!("{path}/DependencyNodeIds"),
            "DependencyNodeIds",
            &calc.dependency_node_ids,
            &calculation,
        )?;
        let result = add_folder(
            address_space,
            ns,
            &format!("{path}/Result"),
            "Result",
            "Result",
            &calculation,
        )?;
        address_space.insert_reference(
            &result,
            &NodeId::new(business_ns, calc.node_id.as_str()),
            ReferenceTypeId::Organizes,
        );
        let dependencies = add_folder(
            address_space,
            ns,
            &format!("{path}/Dependencies"),
            "Dependencies",
            "Dependencies",
            &calculation,
        )?;
        // 未知依赖保存在 DependencyNodeIds，但不能建立指向不存在节点的 Browse 引用。
        for identifier in &calc.dependency_node_ids {
            if known_nodes.contains(identifier.as_str()) {
                address_space.insert_reference(
                    &dependencies,
                    &NodeId::new(business_ns, identifier.as_str()),
                    ReferenceTypeId::Organizes,
                );
            }
        }
        let owners = add_folder(
            address_space,
            ns,
            &format!("{path}/Owners"),
            "Owners",
            "Owners",
            &calculation,
        )?;
        for owner in &calc.owners {
            let owner_path = format!(
                "Topology/Ports/{}/Devices/{}",
                path_component(&owner.port_id),
                owner.device_id
            );
            address_space.insert_reference(
                &owners,
                &NodeId::new(ns, owner_path),
                ReferenceTypeId::Organizes,
            );
        }
        // 单设备计算同样出现在该设备的 Points 内；跨设备和常量只在计算分支展示。
        if let [owner] = calc.owners.as_slice() {
            let points = NodeId::new(
                ns,
                format!(
                    "Topology/Ports/{}/Devices/{}/Points",
                    path_component(&owner.port_id),
                    owner.device_id
                ),
            );
            address_space.insert_reference(
                &points,
                &NodeId::new(business_ns, calc.node_id.as_str()),
                ReferenceTypeId::Organizes,
            );
        }
    }
    Ok(())
}

fn add_folder(
    address_space: &mut AddressSpace,
    ns: u16,
    identifier: &str,
    browse_name: &str,
    display_name: &str,
    parent: &NodeId,
) -> io::Result<NodeId> {
    let node_id = NodeId::new(ns, identifier);
    if !address_space.add_folder(
        &node_id,
        QualifiedName::new(ns, browse_name),
        display_name,
        parent,
    ) {
        return Err(invalid(format!("拓扑文件夹创建失败: {identifier}")));
    }
    Ok(node_id)
}

fn add_string(
    address_space: &mut AddressSpace,
    ns: u16,
    parent_path: &str,
    name: &str,
    value: &str,
    parent: &NodeId,
) -> io::Result<()> {
    add_scalar(
        address_space,
        ns,
        &format!("{parent_path}/{name}"),
        name,
        DataTypeId::String,
        Variant::String(value.into()),
        parent,
    )
}

fn add_scalar(
    address_space: &mut AddressSpace,
    ns: u16,
    identifier: &str,
    name: &str,
    data_type: DataTypeId,
    value: Variant,
    parent: &NodeId,
) -> io::Result<()> {
    add_variable(
        address_space,
        ns,
        identifier,
        name,
        data_type,
        -1,
        value,
        parent,
    )
}

fn add_strings(
    address_space: &mut AddressSpace,
    ns: u16,
    identifier: &str,
    name: &str,
    values: &[String],
    parent: &NodeId,
) -> io::Result<()> {
    let value = values
        .iter()
        .map(|value| UAString::from(value.as_str()))
        .collect::<Vec<_>>()
        .into();
    add_variable(
        address_space,
        ns,
        identifier,
        name,
        DataTypeId::String,
        1,
        value,
        parent,
    )
}

#[allow(clippy::too_many_arguments)]
fn add_variable(
    address_space: &mut AddressSpace,
    ns: u16,
    identifier: &str,
    name: &str,
    data_type: DataTypeId,
    value_rank: i32,
    value: Variant,
    parent: &NodeId,
) -> io::Result<()> {
    let node_id = NodeId::new(ns, identifier);
    let mut variable = Variable::new_data_value(
        &node_id,
        QualifiedName::new(ns, name),
        name,
        NodeId::from(data_type),
        Some(value_rank),
        None,
        value,
    );
    variable.set_access_level(AccessLevel::CURRENT_READ);
    variable.set_user_access_level(AccessLevel::CURRENT_READ);
    if !address_space.insert(
        variable,
        Some(&[(
            parent,
            &ReferenceTypeId::HasComponent,
            ReferenceDirection::Inverse,
        )]),
    ) {
        return Err(invalid(format!("拓扑只读变量创建失败: {identifier}")));
    }
    address_space.insert_reference(
        &node_id,
        &NodeId::from(VariableTypeId::BaseDataVariableType),
        ReferenceTypeId::HasTypeDefinition,
    );
    Ok(())
}

/// 对完整 UTF-8 字节做可逆路径编码，避免 /、%、反斜线及 Unicode 导致层级冲突。
fn path_component(value: &str) -> String {
    const HEX: &[u8; 16] = b"0123456789ABCDEF";
    let mut output = String::with_capacity(value.len());
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b'~') {
            output.push(char::from(byte));
        } else {
            output.push('%');
            output.push(char::from(HEX[usize::from(byte >> 4)]));
            output.push(char::from(HEX[usize::from(byte & 15)]));
        }
    }
    output
}

fn validate_identifier(value: &str, kind: &str) -> io::Result<()> {
    if value.trim().is_empty() || value.chars().any(char::is_control) {
        return Err(invalid(format!("拓扑{kind}标识非法: {value:?}")));
    }
    Ok(())
}

fn invalid(message: String) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::calc_engine::config::CalcRule;
    use crate::csv_parser::{BitIndex, Config, DeviceConfig, Record};
    use opcua::server::address_space::{NodeBase, NodeType};
    use opcua::types::{DataEncoding, NumericRange, TimestampsToReturn};

    fn device(port: &str, address: u8, nodes: &[&str]) -> DeviceConfig {
        DeviceConfig::new(
            Config {
                com: port.into(),
                baud_rate: 115200,
                is_special: false,
                com_index: 1,
                device_id: address,
                data_len: 8,
                kks_prefix: "UNIT".into(),
            },
            nodes
                .iter()
                .map(|node| Record {
                    kks: (*node).into(),
                    type_: "uint".into(),
                    _f_type: String::new(),
                    byte_index: 1,
                    bit_index: BitIndex::Single(0),
                    _def: 0,
                    _max: 0,
                    _min: 0,
                    byte_order: 0,
                })
                .collect(),
        )
    }

    fn port(name: &str, devices: &[(u8, &[&str])]) -> SerialPortConfig {
        SerialPortConfig::new(
            name.into(),
            115200,
            false,
            devices
                .iter()
                .map(|(address, nodes)| device(name, *address, nodes))
                .collect(),
        )
    }

    fn engine(rules: &[(&str, &str)]) -> CalcEngine {
        CalcEngine::new(
            rules
                .iter()
                .map(|(node, expression)| CalcRule {
                    kks_calc: (*node).into(),
                    data_type: "uint".into(),
                    expression: (*expression).into(),
                    max_limit: f64::MAX,
                    min_limit: f64::MIN,
                    default_value: 0.0,
                })
                .collect(),
        )
        .unwrap()
    }

    fn calculation<'a>(manifest: &'a TopologyManifest, node: &str) -> &'a TopologyCalculation {
        manifest
            .calculations
            .iter()
            .find(|calc| calc.node_id == node)
            .unwrap()
    }

    #[test]
    fn topology_manifest_matches_wire_contract_and_is_deterministic() {
        let ports = vec![
            port("/dev/ttyS1", &[(2, &["Z", "Y"])]),
            port("COM1", &[(9, &["B"]), (1, &["A"])]),
        ];
        let manifest = TopologyManifest::build(&ports, &engine(&[("CALC", "A + A")])).unwrap();
        let mut reordered = ports.clone();
        reordered.reverse();
        for port in &mut reordered {
            port.devices.reverse();
            for device in &mut port.devices {
                device.records.reverse();
            }
        }
        let other = TopologyManifest::build(&reordered, &engine(&[("CALC", "A + A")])).unwrap();
        assert_eq!(manifest.to_json().unwrap(), other.to_json().unwrap());
        let value: serde_json::Value = serde_json::from_str(&manifest.to_json().unwrap()).unwrap();
        assert_eq!(value["schema_version"], 1);
        assert_eq!(value["ports"][0]["port_id"], "/dev/ttyS1");
        assert_eq!(value["ports"][0]["port_name"], "ttyS1");
        assert_eq!(value["ports"][0]["send_node_id"], "ttyS1_Send");
        assert_eq!(value["ports"][0]["send_method_id"], "ttyS1_SendCommand");
        assert_eq!(value["ports"][0]["devices"][0]["device_id"], "2");
        assert_eq!(value["ports"][0]["devices"][0]["device_address"], 2);
        assert_eq!(value["ports"][0]["devices"][0]["name"], "设备 2");
        assert_eq!(
            value["ports"][0]["devices"][0]["node_ids"],
            serde_json::json!(["Y", "Z"])
        );
        assert_eq!(
            value["calculations"][0],
            serde_json::json!({
                "node_id": "CALC", "dependency_node_ids": ["A"],
                "owners": [{"port_id": "COM1", "device_id": "1"}]
            })
        );
        assert_eq!(manifest.device_count(), 3);
        assert_eq!(manifest.data_node_ids().count(), 5);
        assert_eq!(manifest.metadata_variable_count(), 26);
    }

    #[test]
    fn topology_calculation_owners_follow_recursive_dag_without_runtime_mixing() {
        let ports = vec![port("COM1", &[(1, &["A", "A2"]), (2, &["B"])])];
        let manifest = TopologyManifest::build(
            &ports,
            &engine(&[
                ("CHAIN", "SINGLE + 2"),
                ("CROSS", "CHAIN + B"),
                ("SINGLE", "A + A2"),
                ("CONSTANT", "42"),
                ("WITH_CONSTANT", "SINGLE + CONSTANT"),
            ]),
        )
        .unwrap();
        let owner = DeviceOwner {
            port_id: "COM1".into(),
            device_id: "1".into(),
        };
        assert_eq!(calculation(&manifest, "SINGLE").owners, vec![owner.clone()]);
        assert_eq!(calculation(&manifest, "CHAIN").owners, vec![owner.clone()]);
        assert_eq!(calculation(&manifest, "WITH_CONSTANT").owners, vec![owner]);
        assert_eq!(
            calculation(&manifest, "CHAIN").dependency_node_ids,
            vec!["SINGLE"]
        );
        assert_eq!(calculation(&manifest, "CROSS").owners.len(), 2);
        assert!(calculation(&manifest, "CONSTANT").owners.is_empty());
        assert!(calculation(&manifest, "CONSTANT")
            .dependency_node_ids
            .is_empty());
    }

    #[test]
    fn topology_unresolved_dependency_prevents_partial_owner_and_propagates() {
        let manifest = TopologyManifest::build(
            &[port("COM1", &[(1, &["A"])])],
            &engine(&[
                ("BROKEN", "A + MISSING"),
                ("CHILD", "BROKEN + A"),
                ("ORPHAN", "MISSING + 1"),
            ]),
        )
        .unwrap();
        for calc in &manifest.calculations {
            assert!(calc.owners.is_empty(), "{calc:?}");
        }
        assert_eq!(
            calculation(&manifest, "BROKEN").dependency_node_ids,
            vec!["A", "MISSING"]
        );
    }

    #[test]
    fn topology_cycles_skipped_by_engine_do_not_create_phantom_ownership() {
        let manifest = TopologyManifest::build(
            &[port("COM1", &[(1, &["A"])])],
            &engine(&[
                ("FIRST", "SECOND + A"),
                ("SECOND", "FIRST + 1"),
                ("GOOD", "A + 1"),
            ]),
        )
        .unwrap();
        assert_eq!(manifest.calculations.len(), 1);
        assert_eq!(manifest.calculations[0].node_id, "GOOD");
    }

    #[test]
    fn topology_duplicate_device_and_point_owners_are_rejected() {
        for ports in [
            vec![port("COM1", &[(1, &["A"]), (1, &["B"])])],
            vec![port("COM1", &[(1, &["A", "A"])])],
            vec![port("COM1", &[(1, &["A"])]), port("COM2", &[(1, &["A"])])],
            vec![port("COM1", &[(1, &["A"])]), port("COM1", &[(2, &["B"])])],
        ] {
            assert!(TopologyManifest::build(&ports, &engine(&[])).is_err());
        }
        assert!(
            TopologyManifest::build(&[port("COM1", &[(1, &["A"])])], &engine(&[("A", "42")]))
                .is_err()
        );
    }

    #[test]
    fn topology_invalid_identifiers_or_mismatched_parent_are_rejected() {
        for ports in [
            vec![port("", &[])],
            vec![port("COM1", &[(1, &[" "])])],
            vec![port("COM1", &[(1, &["A\0B"])])],
        ] {
            assert!(TopologyManifest::build(&ports, &engine(&[])).is_err());
        }
        let mismatched = SerialPortConfig::new(
            "COM1".into(),
            115200,
            false,
            vec![device("COM2", 1, &["A"])],
        );
        assert!(TopologyManifest::build(&[mismatched], &engine(&[])).is_err());
    }

    #[test]
    fn topology_preserves_full_rcd_address_range_and_escapes_paths() {
        let manifest = TopologyManifest::build(
            &[port(r"\\.\COM3", &[(0, &["A"]), (255, &["B"])])],
            &engine(&[]),
        )
        .unwrap();
        assert_eq!(manifest.ports[0].port_name, "COM3");
        assert_eq!(manifest.ports[0].devices[0].device_address, 0);
        assert_eq!(manifest.ports[0].devices[1].device_address, 255);
        assert_eq!(path_component("/dev/ttyS0"), "%2Fdev%2FttyS0");
        assert_ne!(
            path_component("/dev/ttyS0"),
            path_component("%2Fdev%2FttyS0")
        );
        let json = TopologyManifest::build(&[port("端口\"A", &[(1, &["KKS\\\"A"])])], &engine(&[]))
            .unwrap()
            .to_json()
            .unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed["ports"][0]["devices"][0]["node_ids"][0], "KKS\\\"A");
    }

    #[test]
    fn topology_nodes_are_read_only_and_reference_unchanged_business_ids() {
        let manifest = TopologyManifest::build(
            &[port("/dev/ttyS0", &[(1, &["A"])])],
            &engine(&[("CALC", "A + 1")]),
        )
        .unwrap();
        let json = manifest.to_json().unwrap();
        let mut address_space = AddressSpace::new();
        address_space.add_namespace(TOPOLOGY_NAMESPACE_URI, 7);
        create_topology_nodes(&mut address_space, 7, 4, &manifest, &json).unwrap();
        let manifest_id = NodeId::new(7, TOPOLOGY_MANIFEST_NODE_ID);
        let Some(NodeType::Variable(variable)) = address_space.find_node(&manifest_id) else {
            panic!("missing manifest")
        };
        assert_eq!(variable.data_type(), NodeId::from(DataTypeId::String));
        assert_eq!(variable.value_rank(), -1);
        assert_eq!(
            variable.access_level().bits(),
            AccessLevel::CURRENT_READ.bits()
        );
        assert_eq!(
            variable.user_access_level().bits(),
            AccessLevel::CURRENT_READ.bits()
        );
        assert_eq!(
            variable
                .value(
                    TimestampsToReturn::Neither,
                    &NumericRange::None,
                    &DataEncoding::Binary,
                    0.0
                )
                .value,
            Some(Variant::String(json.into()))
        );
        let points = NodeId::new(7, "Topology/Ports/%2Fdev%2FttyS0/Devices/1/Points");
        for identifier in ["A", "CALC"] {
            let old_id = NodeId::new(4, identifier);
            assert!(address_space.has_reference(&points, &old_id, ReferenceTypeId::Organizes));
            assert!(
                !address_space.node_exists(&old_id),
                "实时业务节点不得复制到拓扑管理器"
            );
        }
        let command_folder = NodeId::new(7, "Topology/Ports/%2Fdev%2FttyS0/Commands");
        assert!(address_space.has_reference(
            &command_folder,
            &NodeId::new(4, "ttyS0_Send"),
            ReferenceTypeId::Organizes
        ));
        assert!(!address_space.has_reference(
            &command_folder,
            &NodeId::new(4, "ttyS0_SendCommand"),
            ReferenceTypeId::Organizes
        ));
        let object_id = NodeId::new(7, "Topology/Ports/%2Fdev%2FttyS0/SendMethodObjectId");
        let Some(NodeType::Variable(variable)) = address_space.find_node(&object_id) else {
            panic!("missing method object identifier")
        };
        assert_eq!(
            variable.browse_name(),
            &QualifiedName::new(7, "SendMethodObjectId")
        );
        assert_eq!(
            variable
                .value(
                    TimestampsToReturn::Neither,
                    &NumericRange::None,
                    &DataEncoding::Binary,
                    0.0,
                )
                .value,
            Some(Variant::String("Methods".into()))
        );
    }
}
