//! Atomic publication boundary: validate an entire response before updating any
//! point. OPC readers only receive owned snapshots, never mutable cache entries.
use std::collections::{BTreeMap, HashSet};
use std::sync::RwLock;

use chrono::{DateTime, Utc};
use serde::Deserialize;
use thiserror::Error;

use crate::config::{Config, PointDefinition};

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
pub enum DataType {
    Boolean,
    UInt32,
    Float,
    Double,
    String,
}

#[derive(Clone, Debug, PartialEq)]
pub enum Value {
    Boolean(bool),
    UInt32(u32),
    Float(f32),
    Double(f64),
    String(String),
}

impl DataType {
    pub fn zero(self) -> Value {
        match self {
            Self::Boolean => Value::Boolean(false),
            Self::UInt32 => Value::UInt32(0),
            Self::Float => Value::Float(0.0),
            Self::Double => Value::Double(0.0),
            Self::String => Value::String(String::new()),
        }
    }
}
impl Value {
    pub fn data_type(&self) -> DataType {
        match self {
            Self::Boolean(_) => DataType::Boolean,
            Self::UInt32(_) => DataType::UInt32,
            Self::Float(_) => DataType::Float,
            Self::Double(_) => DataType::Double,
            Self::String(_) => DataType::String,
        }
    }
    fn valid(&self) -> bool {
        match self {
            Self::Float(v) => v.is_finite(),
            Self::Double(v) => v.is_finite(),
            Self::String(v) => v.len() <= 16 * 1024,
            _ => true,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Quality {
    Good,
    Waiting,
    NoCommunication,
    Configuration,
    OutOfService,
}

#[derive(Clone, Debug)]
pub struct Sample {
    pub value: Value,
    pub quality: Quality,
    pub source_timestamp: Option<DateTime<Utc>>,
    pub error: String,
}

#[derive(Clone, Debug)]
pub struct PointUpdate {
    pub name: String,
    pub value: Value,
    pub quality: Quality,
    pub error: String,
}

#[derive(Clone, Debug, Default)]
pub struct PortSnapshot {
    pub state: String,
    pub last_error: String,
    pub last_attempt_at: Option<DateTime<Utc>>,
    pub last_success_at: Option<DateTime<Utc>>,
    pub attempts: u32,
    pub successes: u32,
    pub failures: u32,
    pub reconnects: u32,
    pub rejected_frames: u32,
    pub discarded_bytes: u32,
    pub partial_timeouts: u32,
}

#[derive(Debug, Error)]
pub enum StoreError {
    #[error("point store lock poisoned")]
    Poisoned,
    #[error("unknown task or port: {0}")]
    Unknown(String),
    #[error("response does not match the configured task point set or type: {0}")]
    InvalidBatch(String),
}

struct State {
    samples: BTreeMap<String, Sample>,
    ports: BTreeMap<String, PortSnapshot>,
}

pub struct PointStore {
    pub synchronization: crate::synchronization::Mailboxes,
    definitions: BTreeMap<String, PointDefinition>,
    tasks: BTreeMap<String, Vec<String>>,
    task_ports: BTreeMap<String, String>,
    state: RwLock<State>,
}

fn short_error(value: &str) -> String {
    value.chars().take(1024).collect()
}

impl PointStore {
    pub fn new(config: &Config, definitions: &[PointDefinition]) -> Self {
        let mut tasks = BTreeMap::new();
        let mut task_ports = BTreeMap::new();
        let mut initial = BTreeMap::new();
        let mut ports = BTreeMap::new();
        for port in &config.ports {
            let active = port.enabled && port.tasks.iter().any(|t| t.enabled);
            ports.insert(
                port.id.clone(),
                PortSnapshot {
                    state: if active { "Waiting" } else { "Disabled" }.into(),
                    ..Default::default()
                },
            );
            for task in &port.tasks {
                tasks.insert(task.id.clone(), Vec::new());
                task_ports.insert(task.id.clone(), port.id.clone());
                initial.insert(
                    task.id.clone(),
                    if !port.enabled || !task.enabled {
                        Quality::OutOfService
                    } else if task.protocol == "pending" {
                        Quality::Configuration
                    } else {
                        Quality::Waiting
                    },
                );
            }
        }
        let mut samples = BTreeMap::new();
        for point in definitions {
            if let Some(names) = tasks.get_mut(&point.task_id) {
                names.push(point.name.clone());
            }
            let quality = initial
                .get(&point.task_id)
                .copied()
                .unwrap_or(Quality::Configuration);
            samples.insert(
                point.name.clone(),
                Sample {
                    value: point.data_type.zero(),
                    quality,
                    source_timestamp: None,
                    error: if quality == Quality::OutOfService {
                        "acquisition disabled".into()
                    } else {
                        String::new()
                    },
                },
            );
        }
        Self {
            synchronization: crate::synchronization::Mailboxes::new(config),
            definitions: definitions
                .iter()
                .map(|d| (d.name.clone(), d.clone()))
                .collect(),
            tasks,
            task_ports,
            state: RwLock::new(State { samples, ports }),
        }
    }

    pub fn definitions(&self) -> impl Iterator<Item = &PointDefinition> {
        self.definitions.values()
    }
    pub fn sample(&self, name: &str) -> Result<Sample, StoreError> {
        self.state
            .read()
            .map_err(|_| StoreError::Poisoned)?
            .samples
            .get(name)
            .cloned()
            .ok_or_else(|| StoreError::Unknown(name.into()))
    }
    pub fn port(&self, id: &str) -> Result<PortSnapshot, StoreError> {
        self.state
            .read()
            .map_err(|_| StoreError::Poisoned)?
            .ports
            .get(id)
            .cloned()
            .ok_or_else(|| StoreError::Unknown(id.into()))
    }
    pub fn port_state(
        &self,
        id: &str,
        status: &str,
        error: &str,
        reconnected: bool,
    ) -> Result<(), StoreError> {
        let mut state = self.state.write().map_err(|_| StoreError::Poisoned)?;
        let port = state
            .ports
            .get_mut(id)
            .ok_or_else(|| StoreError::Unknown(id.into()))?;
        port.state = status.into();
        port.last_error = short_error(error);
        if reconnected {
            port.reconnects = port.reconnects.saturating_add(1);
        }
        Ok(())
    }
    pub fn attempt(&self, task: &str) -> Result<(), StoreError> {
        let id = self
            .task_ports
            .get(task)
            .ok_or_else(|| StoreError::Unknown(task.into()))?;
        let mut state = self.state.write().map_err(|_| StoreError::Poisoned)?;
        let port = state
            .ports
            .get_mut(id)
            .ok_or_else(|| StoreError::Unknown(id.clone()))?;
        port.attempts = port.attempts.saturating_add(1);
        port.last_attempt_at = Some(Utc::now());
        Ok(())
    }
    pub fn record_frame_errors(
        &self,
        id: &str,
        rejected: u64,
        discarded: u64,
        expired: bool,
    ) -> Result<(), StoreError> {
        let mut state = self.state.write().map_err(|_| StoreError::Poisoned)?;
        let port = state
            .ports
            .get_mut(id)
            .ok_or_else(|| StoreError::Unknown(id.into()))?;
        port.rejected_frames = port
            .rejected_frames
            .saturating_add(rejected.min(u32::MAX as u64) as u32);
        port.discarded_bytes = port
            .discarded_bytes
            .saturating_add(discarded.min(u32::MAX as u64) as u32);
        port.partial_timeouts = port.partial_timeouts.saturating_add(u32::from(expired));
        Ok(())
    }
    pub fn publish(
        &self,
        task: &str,
        updates: &[PointUpdate],
        source_time: DateTime<Utc>,
    ) -> Result<(), StoreError> {
        let names = self
            .tasks
            .get(task)
            .ok_or_else(|| StoreError::Unknown(task.into()))?;
        let mut seen = HashSet::new();
        if names.len() != updates.len() {
            return Err(StoreError::InvalidBatch(task.into()));
        }
        for update in updates {
            let definition = self
                .definitions
                .get(&update.name)
                .ok_or_else(|| StoreError::InvalidBatch(update.name.clone()))?;
            if definition.task_id != task
                || definition.data_type != update.value.data_type()
                || !update.value.valid()
                || !seen.insert(&update.name)
            {
                return Err(StoreError::InvalidBatch(update.name.clone()));
            }
        }
        let mut state = self.state.write().map_err(|_| StoreError::Poisoned)?;
        for update in updates {
            state.samples.insert(
                update.name.clone(),
                Sample {
                    value: if update.quality == Quality::Good {
                        update.value.clone()
                    } else {
                        update.value.data_type().zero()
                    },
                    quality: update.quality,
                    source_timestamp: Some(source_time),
                    error: short_error(&update.error),
                },
            );
        }
        if let Some(port) = self
            .task_ports
            .get(task)
            .and_then(|id| state.ports.get_mut(id))
        {
            port.successes = port.successes.saturating_add(1);
            port.last_success_at = Some(Utc::now());
        }
        Ok(())
    }
    pub fn fail(&self, task: &str, quality: Quality, error: &str) -> Result<(), StoreError> {
        if quality == Quality::Good {
            return Err(StoreError::InvalidBatch("failure cannot be Good".into()));
        }
        let names = self
            .tasks
            .get(task)
            .ok_or_else(|| StoreError::Unknown(task.into()))?;
        let mut state = self.state.write().map_err(|_| StoreError::Poisoned)?;
        for name in names {
            if let Some(sample) = state.samples.get_mut(name) {
                sample.value = sample.value.data_type().zero();
                sample.quality = quality;
                sample.error = short_error(error);
                // Keep the last valid response time; failure is not a new sample.
            }
        }
        if let Some(port) = self
            .task_ports
            .get(task)
            .and_then(|id| state.ports.get_mut(id))
        {
            port.failures = port.failures.saturating_add(1);
            port.last_error = short_error(error);
        }
        Ok(())
    }
}
