//! Persistent command admission fence. This is the lower half of the deployment
//! handshake, not an agent job or permission to stop the serial worker.
use super::OperationError;
use rusqlite::{params, Connection};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Record {
    schema_version: u32,
    gateway_instance_id: String,
    fencing_token: u64,
    deployment_id: String,
    active: bool,
}

struct State {
    record: Record,
    pending: usize,
    durable: bool,
    error: String,
}

struct Inner {
    path: PathBuf,
    // Disk work never holds the admission/state lock or Operations configuration lock.
    transition: Mutex<()>,
    state: Mutex<State>,
}

#[derive(Clone)]
pub(super) struct Gate(Arc<Inner>);

/// A short in-memory reservation; the serial worker never writes this database.
pub(crate) struct Admission(Arc<Inner>);

impl Drop for Admission {
    fn drop(&mut self) {
        let mut state = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        state.pending -= 1;
    }
}

impl Gate {
    pub(super) fn open(path: PathBuf, gateway: &str) -> Self {
        let initial = Record {
            schema_version: 1,
            gateway_instance_id: gateway.into(),
            fencing_token: 0,
            deployment_id: String::new(),
            active: false,
        };
        let (record, durable, error) = match load(&path, &initial) {
            Ok(record) => (record, true, String::new()),
            Err(error) => {
                log::error!("maintenance state unavailable; control admission blocked: {error}");
                // Acquisition/status stay available, but never guess an unknown fence is open.
                (initial, false, error.to_string())
            }
        };
        Self(Arc::new(Inner {
            path,
            transition: Mutex::new(()),
            state: Mutex::new(State {
                record,
                pending: 0,
                durable,
                error,
            }),
        }))
    }

    pub(super) fn admit(&self) -> Result<Admission, OperationError> {
        let mut state = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        if state.record.active || !state.durable {
            return Err(OperationError::new(
                "busy",
                "gateway maintenance blocks new mutations",
            ));
        }
        state.pending = state
            .pending
            .checked_add(1)
            .ok_or_else(|| OperationError::new("busy", "admission counter exhausted"))?;
        Ok(Admission(self.0.clone()))
    }

    pub(super) fn snapshot(&self, pending_ports: impl FnOnce() -> usize) -> Value {
        let state = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        let pending_ports = pending_ports();
        json!({
            "schema_version":1,
            "gateway_instance_id":state.record.gateway_instance_id,
            "deployment_id":state.record.deployment_id,
            "fencing_token":state.record.fencing_token.to_string(),
            "active":state.record.active,
            "admission_blocked":state.record.active || !state.durable,
            "durable":state.durable,
            "pending_admissions":state.pending,
            "pending_port_operations":pending_ports,
            "drained":state.record.active && state.durable && state.pending == 0 && pending_ports == 0,
            "error":state.error
        })
    }

    pub(super) fn enter(&self, deployment: &str, token: u64) -> Result<(), OperationError> {
        validate_owner(deployment, token)?;
        let _transition = self.0.transition.lock().unwrap_or_else(|e| e.into_inner());
        let next = {
            let mut state = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
            if !state.durable && !state.record.active {
                return Err(OperationError::new(
                    "persistence_failed",
                    "unknown maintenance state; reconcile storage before restarting",
                ));
            }
            if state.record.active {
                if state.record.deployment_id != deployment || state.record.fencing_token != token {
                    return Err(OperationError::new(
                        "conflict",
                        "another maintenance fence owns this gateway",
                    ));
                }
                if state.durable {
                    return Ok(());
                }
            } else if state.record.fencing_token.checked_add(1) != Some(token) {
                return Err(OperationError::new(
                    "conflict",
                    "fencing token must be the next generation",
                ));
            }
            let mut next = state.record.clone();
            next.active = true;
            next.deployment_id = deployment.into();
            next.fencing_token = token;
            // Block atomically with admission before persistence; no acknowledgement until durable.
            state.record = next.clone();
            state.durable = false;
            next
        };
        self.persist(next)
    }

    pub(super) fn leave(
        &self,
        deployment: &str,
        token: u64,
        pending_ports: impl FnOnce() -> usize,
    ) -> Result<(), OperationError> {
        validate_owner(deployment, token)?;
        let _transition = self.0.transition.lock().unwrap_or_else(|e| e.into_inner());
        let next = {
            let state = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
            if state.record.deployment_id != deployment || state.record.fencing_token != token {
                return Err(OperationError::new(
                    "conflict",
                    "maintenance fence owner changed",
                ));
            }
            if !state.durable {
                return Err(OperationError::new(
                    "persistence_failed",
                    "maintenance state is not durable",
                ));
            }
            if !state.record.active {
                return Ok(());
            }
            if state.pending != 0 || pending_ports() != 0 {
                return Err(OperationError::new(
                    "busy",
                    "accepted operations have not drained",
                ));
            }
            let mut next = state.record.clone();
            next.active = false;
            next
        };
        // Admission remains blocked until the release commit is confirmed.
        self.persist(next)
    }

    fn persist(&self, next: Record) -> Result<(), OperationError> {
        let result = save(&self.0.path, &next);
        let mut state = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        match result {
            Ok(()) => {
                state.record = next;
                state.durable = true;
                state.error.clear();
                Ok(())
            }
            Err(error) => {
                state.durable = false;
                state.error = error.to_string();
                log::error!(
                    "maintenance transition unconfirmed; admission remains blocked: {error}"
                );
                Err(OperationError::new(
                    "persistence_failed",
                    state.error.clone(),
                ))
            }
        }
    }
}

fn validate_owner(deployment: &str, token: u64) -> Result<(), OperationError> {
    if token == 0
        || deployment.is_empty()
        || deployment.len() > 128
        || !deployment
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"-_.".contains(&b))
    {
        return Err(OperationError::new(
            "invalid_request",
            "invalid deployment_id or fencing_token",
        ));
    }
    Ok(())
}

fn connect(path: &Path) -> Result<Connection, Box<dyn std::error::Error>> {
    if let Ok(meta) = std::fs::symlink_metadata(path) {
        if !meta.is_file() || meta.file_type().is_symlink() {
            return Err("maintenance database is not a regular file".into());
        }
    }
    let db = Connection::open(path)?;
    db.busy_timeout(Duration::from_millis(100))?;
    db.pragma_update(None, "journal_mode", "WAL")?;
    db.pragma_update(None, "synchronous", "FULL")?;
    Ok(db)
}

fn load(path: &Path, initial: &Record) -> Result<Record, Box<dyn std::error::Error>> {
    let fresh = !path.try_exists()?;
    let mut db = connect(path)?;
    let transaction = db.transaction()?;
    if fresh {
        transaction.execute_batch("CREATE TABLE maintenance_state (id INTEGER PRIMARY KEY CHECK(id=1), record TEXT NOT NULL);")?;
        transaction.execute(
            "INSERT INTO maintenance_state(id,record) VALUES(1,?1)",
            [serde_json::to_string(initial)?],
        )?;
    }
    let text: String = transaction.query_row(
        "SELECT record FROM maintenance_state WHERE id=1",
        [],
        |row| row.get(0),
    )?;
    let record: Record = serde_json::from_str(&text)?;
    if record.schema_version != 1
        || record.gateway_instance_id != initial.gateway_instance_id
        || (record.fencing_token == 0 && (record.active || !record.deployment_id.is_empty()))
        || (record.fencing_token != 0
            && validate_owner(&record.deployment_id, record.fencing_token).is_err())
    {
        return Err("invalid maintenance identity/schema/fencing generation".into());
    }
    transaction.commit()?;
    Ok(record)
}

fn save(path: &Path, record: &Record) -> Result<(), Box<dyn std::error::Error>> {
    let db = connect(path)?;
    let count = db.execute(
        "UPDATE maintenance_state SET record=?1 WHERE id=1",
        params![serde_json::to_string(record)?],
    )?;
    if count != 1 {
        return Err("maintenance record missing".into());
    }
    Ok(())
}

#[cfg(test)]
#[path = "maintenance_tests.rs"]
mod tests;
