//! OPC transport only: immutable point snapshots become read-only DataValues.
//! No raw send node, fabricated topology or unsupported maintenance capability
//! is advertised while the device protocol is pending.
use std::io;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use opcua::nodes::AccessLevel;
use opcua::server::address_space::{
    AddressSpace, MethodBuilder, ReferenceDirection, VariableBuilder,
};
use opcua::server::diagnostics::NamespaceMetadata;
use opcua::server::node_manager::memory::{simple_node_manager, SimpleNodeManager};
use opcua::server::{ServerBuilder, ServerEndpoint, ANONYMOUS_USER_TOKEN_ID};
use opcua::types::{
    DataTypeId, DataValue, DateTime, NodeId, NumericRange, QualifiedName, ReferenceTypeId,
    StatusCode, TimestampsToReturn, VariableTypeId, Variant,
};
use tokio::net::TcpListener;
use tokio::sync::oneshot;
use tokio_util::sync::CancellationToken;

use crate::config::Config;
use crate::points::{DataType, PointStore, Quality, Sample, Value};

pub const BUSINESS_URI: &str = "urn:SerialServer";
pub const DIAGNOSTICS_URI: &str = "urn:RcdCommunicate:diagnostics";

fn configure_limits(builder: &mut ServerBuilder, point_count: usize) {
    let limits = builder.limits_mut();
    // The simple node manager polls no faster than 100 ms. Advertise that real
    // floor instead of promising a 50 ms sample that the manager cannot supply.
    limits.subscriptions.min_sampling_interval_ms = 100.0;
    limits.subscriptions.min_publishing_interval_ms = 50.0;
    limits.subscriptions.max_monitored_items_per_sub = point_count.saturating_add(512).max(1000);
    // Requests remain batched (1000 items/references by default), not unbounded.
    limits.max_message_size = 16 * 1024 * 1024;
    limits.max_string_length = limits.max_string_length.max(64 * 1024);
    limits.max_chunk_count = 256;
}

fn variant(value: &Value) -> Variant {
    match value {
        Value::Boolean(v) => Variant::Boolean(*v),
        Value::UInt32(v) => Variant::UInt32(*v),
        Value::Float(v) => Variant::Float(*v),
        Value::Double(v) => Variant::Double(*v),
        Value::String(v) => Variant::String(v.as_str().into()),
    }
}
fn data_type(value: DataType) -> DataTypeId {
    match value {
        DataType::Boolean => DataTypeId::Boolean,
        DataType::UInt32 => DataTypeId::UInt32,
        DataType::Float => DataTypeId::Float,
        DataType::Double => DataTypeId::Double,
        DataType::String => DataTypeId::String,
    }
}
pub fn status(quality: Quality) -> StatusCode {
    match quality {
        Quality::Good => StatusCode::Good,
        Quality::Waiting => StatusCode::BadWaitingForInitialData,
        Quality::NoCommunication => StatusCode::BadNoCommunication,
        Quality::Configuration => StatusCode::BadConfigurationError,
        Quality::OutOfService => StatusCode::BadOutOfService,
    }
}
pub fn data_value(sample: &Sample) -> DataValue {
    DataValue {
        value: Some(variant(&sample.value)),
        status: Some(status(sample.quality)),
        source_timestamp: sample.source_timestamp.map(DateTime::from),
        server_timestamp: Some(DateTime::now()),
        ..Default::default()
    }
}
fn good(value: Value) -> DataValue {
    DataValue {
        value: Some(variant(&value)),
        status: Some(StatusCode::Good),
        server_timestamp: Some(DateTime::now()),
        ..Default::default()
    }
}
fn folder(
    space: &mut AddressSpace,
    ns: u16,
    id: &str,
    name: &str,
    parent: &NodeId,
) -> io::Result<NodeId> {
    let id = NodeId::new(ns, id);
    if !space.add_folder(&id, QualifiedName::new(ns, name), name, parent) {
        return Err(io::Error::other("duplicate OPC folder"));
    }
    Ok(id)
}
fn variable(
    space: &mut AddressSpace,
    ns: u16,
    id: &str,
    name: &str,
    initial: &Value,
    parent: &NodeId,
) -> io::Result<NodeId> {
    variable_access(space, ns, id, name, initial, parent, false)
}
fn variable_access(
    space: &mut AddressSpace,
    ns: u16,
    id: &str,
    name: &str,
    initial: &Value,
    parent: &NodeId,
    writable: bool,
) -> io::Result<NodeId> {
    let id = NodeId::new(ns, id);
    let description = if name.starts_with("DATA.Buildings.") && name.ends_with(".Fire") {
        crate::unit_report::FIRE_DESCRIPTION
    } else {
        name
    };
    let mut var = VariableBuilder::new(&id, QualifiedName::new(ns, name), name)
        .description(description)
        .data_type(NodeId::from(data_type(initial.data_type())))
        .value_rank(-1)
        .value(variant(initial))
        .build();
    let access = if writable {
        AccessLevel::CURRENT_READ | AccessLevel::CURRENT_WRITE
    } else {
        AccessLevel::CURRENT_READ
    };
    var.set_access_level(access);
    var.set_user_access_level(access);
    if !space.insert(
        var,
        Some(&[(
            parent,
            &ReferenceTypeId::HasComponent,
            ReferenceDirection::Inverse,
        )]),
    ) {
        return Err(io::Error::other("duplicate OPC variable"));
    }
    space.insert_reference(
        &id,
        &NodeId::from(VariableTypeId::BaseDataVariableType),
        ReferenceTypeId::HasTypeDefinition,
    );
    Ok(id)
}
fn read_callback(
    manager: &SimpleNodeManager,
    id: &NodeId,
    read: impl Fn() -> Result<DataValue, StatusCode> + Send + Sync + 'static,
) {
    manager
        .inner()
        .add_read_callback(id.clone(), move |range, timestamps, _| {
            if !matches!(range, NumericRange::None) {
                return Err(StatusCode::BadIndexRangeNoData);
            }
            Ok(filter_timestamps(read()?, timestamps))
        });
}

fn filter_timestamps(mut value: DataValue, timestamps: TimestampsToReturn) -> DataValue {
    if !matches!(
        timestamps,
        TimestampsToReturn::Source | TimestampsToReturn::Both
    ) {
        value.source_timestamp = None;
        value.source_picoseconds = None;
    }
    if !matches!(
        timestamps,
        TimestampsToReturn::Server | TimestampsToReturn::Both
    ) {
        value.server_timestamp = None;
        value.server_picoseconds = None;
    }
    value
}

pub async fn serve(
    config: &Config,
    store: Arc<PointStore>,
    pki: &Path,
    cancel: CancellationToken,
    ready: Option<oneshot::Sender<()>>,
    operations: Option<crate::operations::Operations>,
) -> io::Result<()> {
    let uri = format!("urn:RcdCommunicate:{}", config.instance_id);
    let endpoint = format!(
        "opc.tcp://{}:{}/",
        config.opc.advertised_host, config.opc.port
    );
    let (server, handle) = {
        let mut builder = ServerBuilder::new()
            .host(&config.opc.advertised_host)
            .port(config.opc.port)
            .discovery_urls(vec![endpoint.clone()])
            .application_name("RCD Communicate")
            .application_uri(&uri)
            .product_uri("urn:RcdCommunicate")
            .pki_dir(pki)
            .create_sample_keypair(true)
            .trust_client_certs(config.opc.trust_client_certificates)
            .subscription_poll_interval_ms(50)
            .with_node_manager(simple_node_manager(
                NamespaceMetadata {
                    namespace_uri: BUSINESS_URI.into(),
                    ..Default::default()
                },
                "business",
            ))
            .with_node_manager(simple_node_manager(
                NamespaceMetadata {
                    namespace_uri: DIAGNOSTICS_URI.into(),
                    ..Default::default()
                },
                "communicate_diagnostics",
            ));
        let tokens = [ANONYMOUS_USER_TOKEN_ID.to_string()];
        if operations.is_some() {
            builder = builder.with_node_manager(simple_node_manager(
                NamespaceMetadata {
                    namespace_uri: crate::operations::URI.into(),
                    ..Default::default()
                },
                "communicate_operations",
            ));
        }
        if config.opc.allow_anonymous_none {
            builder = builder
                .add_endpoint("none", ServerEndpoint::new_none("/", &tokens))
                .default_endpoint("none");
        }
        if config.opc.allow_anonymous_secure {
            builder = builder
                .add_endpoint(
                    "secure",
                    ServerEndpoint::new_basic256sha256_sign_encrypt("/", &tokens),
                )
                .default_endpoint("secure");
        }
        let transmit_count: usize = config
            .ports
            .iter()
            .filter_map(|p| {
                p.synchronization
                    .as_ref()
                    .and_then(|s| crate::unit_report::Layout::from_protocol(&s.transmit_protocol))
                    .map(|layout| layout.transmit_definitions(&p.id).len() + 4)
            })
            .sum();
        configure_limits(&mut builder, store.definitions().count() + transmit_count);
        builder
            .build()
            .map_err(|e| io::Error::other(format!("OPC build: {e}")))?
    };
    let manager = handle
        .node_managers()
        .get_by_name::<SimpleNodeManager>("business")
        .ok_or_else(|| io::Error::other("missing business namespace"))?;
    if let Some(operations) = operations {
        let operations_manager = handle
            .node_managers()
            .get_by_name::<SimpleNodeManager>("communicate_operations")
            .ok_or_else(|| io::Error::other("missing operations namespace"))?;
        let operations_ns = handle
            .get_namespace_index(crate::operations::URI)
            .ok_or_else(|| io::Error::other("missing operations namespace index"))?;
        publish_operations(&operations_manager, operations_ns, operations)?;
    }
    let ns = handle
        .get_namespace_index(BUSINESS_URI)
        .ok_or_else(|| io::Error::other("business namespace not registered"))?;
    {
        let mut space = manager.address_space().write();
        let devices = folder(
            &mut space,
            ns,
            "Devices",
            "Devices",
            &NodeId::objects_folder_id(),
        )?;
        folder(
            &mut space,
            ns,
            "Methods",
            "Methods",
            &NodeId::objects_folder_id(),
        )?;
        for definition in store.definitions() {
            let id = variable(
                &mut space,
                ns,
                &definition.name,
                &definition.name,
                &definition.data_type.zero(),
                &devices,
            )?;
            let store = store.clone();
            let name = definition.name.clone();
            read_callback(&manager, &id, move || {
                store
                    .sample(&name)
                    .map(|s| data_value(&s))
                    .map_err(|_| StatusCode::BadInternalError)
            });
        }
        publish_transmit_inputs(&mut space, &manager, ns, &devices, config, &store)?;
    }
    let diagnostics = handle
        .node_managers()
        .get_by_name::<SimpleNodeManager>("communicate_diagnostics")
        .ok_or_else(|| io::Error::other("missing diagnostics namespace"))?;
    let ns = handle
        .get_namespace_index(DIAGNOSTICS_URI)
        .ok_or_else(|| io::Error::other("diagnostics namespace not registered"))?;
    {
        let mut space = diagnostics.address_space().write();
        let root = folder(
            &mut space,
            ns,
            "Diagnostics",
            "Diagnostics",
            &NodeId::objects_folder_id(),
        )?;
        let tasks: Vec<_> = config.ports.iter().flat_map(|p| &p.tasks).collect();
        let protocol_ready = !tasks.is_empty() && tasks.iter().all(|t| t.protocol != "pending");
        for (name, value) in [
            (
                "SoftwareVersion",
                Value::String(env!("CARGO_PKG_VERSION").into()),
            ),
            ("InstanceId", Value::String(config.instance_id.clone())),
            ("ProtocolReady", Value::Boolean(protocol_ready)),
            (
                "AcquisitionState",
                Value::String(
                    if protocol_ready {
                        "Configured"
                    } else {
                        "AwaitingProtocol"
                    }
                    .into(),
                ),
            ),
            (
                "ConfiguredPointCount",
                Value::UInt32(store.definitions().count() as u32),
            ),
        ] {
            let id = variable(
                &mut space,
                ns,
                &format!("Diagnostics/{name}"),
                name,
                &value,
                &root,
            )?;
            read_callback(&diagnostics, &id, move || Ok(good(value.clone())));
        }
        let ports = folder(&mut space, ns, "Diagnostics/Ports", "Ports", &root)?;
        for port in &config.ports {
            let base = format!("Diagnostics/Ports/{}", port.id);
            let parent = folder(&mut space, ns, &base, &port.id, &ports)?;
            if port.synchronization.is_some() {
                let status = variable(
                    &mut space,
                    ns,
                    &format!("{base}/Synchronization"),
                    "Synchronization",
                    &Value::String(String::new()),
                    &parent,
                )?;
                let status_store = store.clone();
                let port_id = port.id.clone();
                read_callback(&diagnostics, &status, move || {
                    status_store
                        .synchronization
                        .status(&port_id)
                        .map(|v| good(Value::String(v)))
                        .map_err(|_| StatusCode::BadInternalError)
                });
                let method = NodeId::new(ns, format!("{base}/SetTransmitSnapshot"));
                if !MethodBuilder::new(
                    &method,
                    QualifiedName::new(ns, "SetTransmitSnapshot"),
                    "SetTransmitSnapshot",
                )
                .executable(true)
                .user_executable(true)
                .component_of(parent.clone())
                .input_args(
                    &mut *space,
                    &NodeId::new(ns, format!("{base}/SetTransmitSnapshot/InputArguments")),
                    &[("Request", DataTypeId::String).into()],
                )
                .output_args(
                    &mut *space,
                    &NodeId::new(ns, format!("{base}/SetTransmitSnapshot/OutputArguments")),
                    &[("Response", DataTypeId::String).into()],
                )
                .insert(&mut *space)
                {
                    return Err(io::Error::other("cannot publish sync snapshot method"));
                }
                let snapshot_store = store.clone();
                let port_id = port.id.clone();
                diagnostics
                    .inner()
                    .add_method_callback(method, move |args| match args {
                        [Variant::String(request)] => Ok(vec![Variant::String(
                            snapshot_store
                                .synchronization
                                .submit_json(&port_id, request.as_ref())
                                .into(),
                        )]),
                        _ => Err(StatusCode::BadInvalidArgument),
                    });
            }
            for field in [
                "State",
                "LastError",
                "LastAttemptAt",
                "LastSuccessAt",
                "Attempts",
                "Successes",
                "Failures",
                "Reconnects",
                "RejectedFrames",
                "DiscardedBytes",
                "PartialTimeouts",
            ] {
                let initial = if [
                    "Attempts",
                    "Successes",
                    "Failures",
                    "Reconnects",
                    "RejectedFrames",
                    "DiscardedBytes",
                    "PartialTimeouts",
                ]
                .contains(&field)
                {
                    Value::UInt32(0)
                } else {
                    Value::String(String::new())
                };
                let id = variable(
                    &mut space,
                    ns,
                    &format!("{base}/{field}"),
                    field,
                    &initial,
                    &parent,
                )?;
                let store = store.clone();
                let port_id = port.id.clone();
                read_callback(&diagnostics, &id, move || {
                    let p = store
                        .port(&port_id)
                        .map_err(|_| StatusCode::BadInternalError)?;
                    Ok(good(match field {
                        "State" => Value::String(p.state),
                        "LastError" => Value::String(p.last_error),
                        "LastAttemptAt" => Value::String(
                            p.last_attempt_at
                                .map(|v| v.to_rfc3339())
                                .unwrap_or_default(),
                        ),
                        "LastSuccessAt" => Value::String(
                            p.last_success_at
                                .map(|v| v.to_rfc3339())
                                .unwrap_or_default(),
                        ),
                        "Attempts" => Value::UInt32(p.attempts),
                        "Successes" => Value::UInt32(p.successes),
                        "Failures" => Value::UInt32(p.failures),
                        "RejectedFrames" => Value::UInt32(p.rejected_frames),
                        "DiscardedBytes" => Value::UInt32(p.discarded_bytes),
                        "PartialTimeouts" => Value::UInt32(p.partial_timeouts),
                        _ => Value::UInt32(p.reconnects),
                    }))
                });
            }
        }
    }
    let listener = TcpListener::bind((config.opc.bind_ip, config.opc.port)).await?;
    log::info!("OPC ready: {}", endpoint);
    if let Some(ready) = ready {
        let _ = ready.send(());
    }
    let run = server.run_with(listener);
    tokio::pin!(run);
    tokio::select! {
        result = &mut run => result.map_err(|e| io::Error::other(format!("OPC server: {e}"))),
        _ = cancel.cancelled() => {
            handle.cancel();
            tokio::time::timeout(Duration::from_secs(5), run).await
                .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "OPC shutdown timed out"))?
                .map_err(|e| io::Error::other(format!("OPC shutdown: {e}")))
        }
    }
}

fn publish_transmit_inputs(
    space: &mut AddressSpace,
    manager: &SimpleNodeManager,
    ns: u16,
    devices: &NodeId,
    config: &Config,
    store: &Arc<PointStore>,
) -> io::Result<()> {
    for port in &config.ports {
        let Some(settings) = &port.synchronization else {
            continue;
        };
        let Some(layout) = crate::unit_report::Layout::from_protocol(&settings.transmit_protocol)
        else {
            continue;
        };
        for definition in layout.transmit_definitions(&port.id) {
            let id = variable_access(
                space,
                ns,
                &definition.name,
                &definition.name,
                &definition.data_type.zero(),
                devices,
                true,
            )?;
            let reader = store.clone();
            let name = definition.name.clone();
            let port_id = port.id.clone();
            read_callback(manager, &id, move || {
                reader
                    .synchronization
                    .field_sample(&port_id, &name, definition.data_type)
                    .map(|s| data_value(&s))
                    .map_err(|_| StatusCode::BadInternalError)
            });
            let writer = store.clone();
            let name = definition.name;
            let port_id = port.id.clone();
            manager.inner().add_write_callback(id, move |data, range| {
                if !matches!(range, NumericRange::None) {
                    return StatusCode::BadIndexRangeInvalid;
                }
                if data.status.is_some_and(|s| !s.is_good()) {
                    return StatusCode::BadInvalidArgument;
                }
                let value = match data.value {
                    Some(Variant::UInt32(v)) => Value::UInt32(v),
                    Some(Variant::String(v)) => Value::String(v.as_ref().to_string()),
                    _ => return StatusCode::BadTypeMismatch,
                };
                match writer.synchronization.write_field(&port_id, &name, value) {
                    Ok(()) => StatusCode::Good,
                    Err(e) if e == "transmission disabled" => StatusCode::BadOutOfService,
                    Err(_) => StatusCode::BadInvalidArgument,
                }
            });
        }
        let snapshot_key = "TransmitSnapshot".to_string();
        let id = variable_access(
            space,
            ns,
            &snapshot_key,
            &snapshot_key,
            &Value::String(String::new()),
            devices,
            true,
        )?;
        let reader = store.clone();
        let port_id = port.id.clone();
        read_callback(manager, &id, move || {
            reader
                .synchronization
                .status(&port_id)
                .map(|v| good(Value::String(v)))
                .map_err(|_| StatusCode::BadInternalError)
        });
        let writer = store.clone();
        let port_id = port.id.clone();
        manager.inner().add_write_callback(id, move |data, range| {
            if !matches!(range, NumericRange::None) {
                return StatusCode::BadIndexRangeInvalid;
            }
            if data.status.is_some_and(|s| !s.is_good()) {
                return StatusCode::BadInvalidArgument;
            }
            let Some(Variant::String(request)) = data.value else {
                return StatusCode::BadTypeMismatch;
            };
            match writer
                .synchronization
                .submit_fields_json(&port_id, request.as_ref())
            {
                Ok(_) => StatusCode::Good,
                Err(e) if e == "transmission disabled" => StatusCode::BadOutOfService,
                Err(e) if e == "snapshot revision conflict" => StatusCode::BadInvalidState,
                Err(_) => StatusCode::BadInvalidArgument,
            }
        });
        for (name, value) in [
            (
                "InputRefreshIntervalMs",
                Value::UInt32(settings.input_refresh_interval_ms as u32),
            ),
            (
                "InputTimeoutMs",
                Value::UInt32(settings.snapshot_max_age_ms as u32),
            ),
            ("TransmitStatus", Value::String(String::new())),
        ] {
            let key = name.to_string();
            let id = variable(space, ns, &key, &key, &value, devices)?;
            let store = store.clone();
            let port_id = port.id.clone();
            read_callback(manager, &id, move || {
                if name == "TransmitStatus" {
                    store
                        .synchronization
                        .status(&port_id)
                        .map(|v| good(Value::String(v)))
                        .map_err(|_| StatusCode::BadInternalError)
                } else {
                    Ok(good(value.clone()))
                }
            });
        }
    }
    Ok(())
}

fn publish_operations(
    manager: &SimpleNodeManager,
    ns: u16,
    operations: crate::operations::Operations,
) -> io::Result<()> {
    let method = NodeId::new(ns, "Operations/Execute");
    {
        let mut space = manager.address_space().write();
        let root = folder(
            &mut space,
            ns,
            "Operations",
            "Operations",
            &NodeId::from(opcua::types::ObjectId::ObjectsFolder),
        )?;
        variable(
            &mut space,
            ns,
            "Operations/Capabilities",
            "Capabilities",
            &Value::String(crate::operations::Operations::capabilities().to_string()),
            &root,
        )?;
        variable(
            &mut space,
            ns,
            "Operations/SoftwareVersion",
            "SoftwareVersion",
            &Value::String(env!("CARGO_PKG_VERSION").into()),
            &root,
        )?;
        if !MethodBuilder::new(&method, QualifiedName::new(ns, "Execute"), "Execute")
            .executable(true)
            .user_executable(true)
            .component_of(root)
            .input_args(
                &mut *space,
                &NodeId::new(ns, "Operations/Execute/InputArguments"),
                &[("Request", DataTypeId::String).into()],
            )
            .output_args(
                &mut *space,
                &NodeId::new(ns, "Operations/Execute/OutputArguments"),
                &[("Response", DataTypeId::String).into()],
            )
            .insert(&mut *space)
        {
            return Err(io::Error::other("could not publish configuration method"));
        }
    }
    manager.inner().add_method_callback(method, move |args| {
        let response = match args {
            [Variant::String(request)] => operations.execute_json(request.as_ref()),
            _ => serde_json::json!({"ok":false,"error":{"code":"invalid_request","message":"Execute expects one String Request"}}).to_string(),
        };
        Ok(vec![Variant::String(response.into())])
    });
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn subscription_capacity_covers_the_configured_point_table() {
        let mut builder = ServerBuilder::new();
        configure_limits(&mut builder, 25_000);
        let limits = builder.limits_mut();
        assert_eq!(limits.subscriptions.max_monitored_items_per_sub, 25_512);
        assert_eq!(limits.subscriptions.min_sampling_interval_ms, 100.0);
        assert_eq!(limits.operational.max_monitored_items_per_call, 1000);
    }

    #[test]
    fn callback_respects_requested_timestamps_without_changing_value_or_quality() {
        let sample = Sample {
            value: Value::Double(0.0),
            quality: Quality::NoCommunication,
            source_timestamp: Some(chrono::Utc::now()),
            error: "timeout".into(),
        };
        for (selection, source, server) in [
            (TimestampsToReturn::Neither, false, false),
            (TimestampsToReturn::Source, true, false),
            (TimestampsToReturn::Server, false, true),
            (TimestampsToReturn::Both, true, true),
        ] {
            let result = filter_timestamps(data_value(&sample), selection);
            assert_eq!(result.source_timestamp.is_some(), source);
            assert_eq!(result.server_timestamp.is_some(), server);
            assert_eq!(result.value, Some(Variant::Double(0.0)));
            assert_eq!(result.status, Some(StatusCode::BadNoCommunication));
        }
    }
}
