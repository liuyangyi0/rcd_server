//! Protocol boundary shared by request/response and unsolicited synchronization.
use crate::{config::TaskConfig, points::PointUpdate};
use crate::{
    config::{Config, ConfigError},
    poller::PollTask,
};
use std::collections::BTreeMap;
use thiserror::Error;

#[derive(Debug, Error)]
#[error("device protocol: {0}")]
pub struct ProtocolError(pub String);

pub enum Response {
    /// Valid envelope, but for another address/function/transaction.
    Ignore,
    /// A complete set of points owned by this task, validated atomically later.
    Matched(Vec<PointUpdate>),
}

/// Implementations must be bounded, nonblocking and have no I/O. Request bytes
/// are uncompressed payloads; the transport applies the shared wire envelope.
/// Correlation is device-specific: CRC validity alone is never a match.
pub trait DeviceProtocol: Send {
    /// Unsolicited snapshots must use a continuous stream, never query/ACK I/O.
    fn unsolicited(&self) -> bool {
        false
    }
    fn request(&mut self, task: &TaskConfig) -> Result<Vec<u8>, ProtocolError>;
    fn response(&mut self, task: &TaskConfig, payload: &[u8]) -> Result<Response, ProtocolError>;
}

type Factory =
    Box<dyn Fn(&TaskConfig) -> Result<Box<dyn DeviceProtocol>, ProtocolError> + Send + Sync>;

/// Compile-time registration, not dynamically loaded scripts or HTTP commands.
/// Known unit snapshots are registered here; other protocols remain explicit.
#[derive(Default)]
pub struct Registry {
    factories: BTreeMap<String, Factory>,
}

/// One registration point shared by configuration checks and the executable.
/// Add only implemented, regression-tested production protocols here.
pub fn production_registry() -> Registry {
    let mut registry = Registry::default();
    for name in ["unit1-to9-v1", "unit9-to1-v1"] {
        registry.factories.insert(
            name.into(),
            Box::new(move |_| {
                Ok(Box::new(crate::unit_report::ReportProtocol(
                    crate::unit_report::Layout::from_protocol(name)
                        .expect("constant protocol name"),
                )))
            }),
        );
    }
    registry
}

impl Registry {
    pub fn register(
        &mut self,
        name: &str,
        factory: impl Fn(&TaskConfig) -> Result<Box<dyn DeviceProtocol>, ProtocolError>
            + Send
            + Sync
            + 'static,
    ) -> Result<(), ProtocolError> {
        if !crate::config::valid_id(name) || name == "pending" || self.factories.contains_key(name)
        {
            return Err(ProtocolError("invalid or duplicate protocol name".into()));
        }
        self.factories.insert(name.into(), Box::new(factory));
        Ok(())
    }

    /// Prepare all adapters before opening any network or serial endpoint.
    pub fn prepare(&self, config: &Config) -> Result<Vec<Vec<PollTask>>, ConfigError> {
        let mut ports = Vec::new();
        for port in &config.ports {
            let mut tasks = Vec::new();
            for task in &port.tasks {
                if task.protocol != "pending" && !self.factories.contains_key(&task.protocol) {
                    return Err(ConfigError::Invalid(format!(
                        "task {}: unsupported protocol {}",
                        task.id, task.protocol
                    )));
                }
                if port.enabled && task.enabled {
                    let factory = self.factories.get(&task.protocol).ok_or_else(|| {
                        ConfigError::Invalid(format!(
                            "task {}: request and decoder pending; acquisition cannot be enabled",
                            task.id
                        ))
                    })?;
                    tasks.push(PollTask {
                        config: task.clone(),
                        protocol: factory(task).map_err(|e| ConfigError::Invalid(e.to_string()))?,
                    });
                }
            }
            if tasks.iter().any(|t| t.protocol.unsolicited()) && tasks.len() != 1 {
                return Err(ConfigError::Invalid(
                    "unit synchronization requires one receive layout per physical port".into(),
                ));
            }
            ports.push(tasks);
        }
        Ok(ports)
    }
}
