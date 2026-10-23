//! Additive operations namespace; existing business and topology IDs stay put.

use std::io;
use std::sync::Arc;

use opcua::nodes::AccessLevel;
use opcua::server::address_space::{
    EventNotifier, MethodBuilder, ObjectBuilder, ReferenceDirection, Variable,
};
use opcua::server::node_manager::memory::SimpleNodeManager;
use opcua::types::{
    DataTypeId, DataValue, DateTime as UaDateTime, NodeId, ObjectId, ObjectTypeId, QualifiedName,
    ReferenceTypeId, StatusCode, UAString, VariableTypeId, Variant,
};

use crate::operations::{
    build_commit, capabilities, software_version, OperationError, Operations,
    StationVersionsSnapshot,
};

pub const OPERATIONS_NAMESPACE_URI: &str = "urn:SerialServer:operations";
pub const OPERATIONS_OBJECT_ID: &str = "Operations";
pub const EXECUTE_NODE_ID: &str = "Operations/Execute";

pub(super) fn publish_operations(
    manager: &Arc<SimpleNodeManager>,
    ns: u16,
    operations: Operations,
) -> io::Result<()> {
    let root = NodeId::new(ns, OPERATIONS_OBJECT_ID);
    let method = NodeId::new(ns, EXECUTE_NODE_ID);
    {
        let mut address_space = manager.address_space().write();
        if !ObjectBuilder::new(&root, QualifiedName::new(ns, "Operations"), "Operations")
            .event_notifier(EventNotifier::empty())
            .has_type_definition(ObjectTypeId::BaseObjectType)
            .organized_by(ObjectId::ObjectsFolder)
            .insert(&mut *address_space)
        {
            return Err(io::Error::other("could not create operations object"));
        }
        let id = NodeId::new(ns, "Operations/Capabilities");
        let mut variable = Variable::new_data_value(
            &id,
            QualifiedName::new(ns, "Capabilities"),
            "Capabilities",
            NodeId::from(DataTypeId::String),
            Some(-1),
            None,
            Variant::String(capabilities().to_string().into()),
        );
        variable.set_access_level(AccessLevel::CURRENT_READ);
        variable.set_user_access_level(AccessLevel::CURRENT_READ);
        if !address_space.insert(
            variable,
            Some(&[(
                &root,
                &ReferenceTypeId::HasComponent,
                ReferenceDirection::Inverse,
            )]),
        ) {
            return Err(io::Error::other("could not create operations capabilities"));
        }
        address_space.insert_reference(
            &id,
            &NodeId::from(VariableTypeId::BaseDataVariableType),
            ReferenceTypeId::HasTypeDefinition,
        );
        let initial_station_revision = operations.station_versions_snapshot().station_revision;
        for (name, value) in [
            ("SoftwareVersion", software_version()),
            ("BuildCommit", build_commit()),
            ("StationVersionRevision", initial_station_revision.as_str()),
            ("CommandRevision", "0"),
        ] {
            add_read_only_string(&mut address_space, ns, &root, name, value)?;
        }
        if !MethodBuilder::new(&method, QualifiedName::new(ns, "Execute"), "Execute")
            .executable(true)
            .user_executable(true)
            .component_of(root)
            .input_args(
                &mut *address_space,
                &NodeId::new(ns, "Operations/Execute/InputArguments"),
                &[("Request", DataTypeId::String).into()],
            )
            .output_args(
                &mut *address_space,
                &NodeId::new(ns, "Operations/Execute/OutputArguments"),
                &[("Response", DataTypeId::String).into()],
            )
            .insert(&mut *address_space)
        {
            return Err(io::Error::other(
                "could not create operations Execute method",
            ));
        }
    }
    manager.inner().add_method_callback(method, move |args| {
        let response = match args {
            [Variant::String(request)] => operations.execute_json(request.as_ref()),
            _ => serde_json::json!({"ok":false,"error":OperationError::new("invalid_request", "Execute expects exactly one String Request")}).to_string(),
        };
        // Business errors are JSON results, not bad UA service results that
        // might incorrectly make clients tear down their healthy Session.
        Ok(vec![Variant::String(UAString::from(response))])
    });
    Ok(())
}

fn add_read_only_string(
    address_space: &mut opcua::server::address_space::AddressSpace,
    ns: u16,
    parent: &NodeId,
    name: &str,
    value: &str,
) -> io::Result<()> {
    let id = NodeId::new(ns, format!("Operations/{name}"));
    let mut variable = Variable::new_data_value(
        &id,
        QualifiedName::new(ns, name),
        name,
        NodeId::from(DataTypeId::String),
        Some(-1),
        None,
        Variant::String(value.into()),
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
        return Err(io::Error::other(format!(
            "could not create operations {name}"
        )));
    }
    address_space.insert_reference(
        &id,
        &NodeId::from(VariableTypeId::BaseDataVariableType),
        ReferenceTypeId::HasTypeDefinition,
    );
    Ok(())
}

pub(super) fn operations_version_updates(
    ns: u16,
    snapshot: &StationVersionsSnapshot,
) -> Vec<(NodeId, DataValue)> {
    [
        ("SoftwareVersion", snapshot.software_version.as_str()),
        ("BuildCommit", snapshot.build_commit.as_str()),
        ("StationVersionRevision", snapshot.station_revision.as_str()),
    ]
    .into_iter()
    .map(|(name, value)| {
        (
            NodeId::new(ns, format!("Operations/{name}")),
            DataValue {
                value: Some(Variant::String(value.into())),
                status: Some(StatusCode::Good),
                source_timestamp: None,
                source_picoseconds: None,
                server_timestamp: Some(UaDateTime::now()),
                server_picoseconds: Some(0),
            },
        )
    })
    .collect()
}
