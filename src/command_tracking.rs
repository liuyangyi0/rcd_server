//! Command journal. Serial workers only update bounded memory; a dedicated
//! thread owns SQLite writes. This is evidence, never a replay queue.
use chrono::Utc;
use rusqlite::{params, Connection, OptionalExtension};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, HashMap, HashSet};
use std::path::PathBuf;
use std::sync::{mpsc, Arc, Mutex, Weak};
use std::time::{Duration, Instant};

const CACHE_LIMIT: usize = 10_000;
const KEEP_COUNT: i64 = 100_000;
const WAIT: Duration = Duration::from_millis(100);

#[cfg(test)]
#[path = "command_tracking_tests.rs"]
mod tests;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Attempt {
    pub attempt: u32,
    pub sent: bool,
    pub result: String,
    pub at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Snapshot {
    pub schema_version: u32,
    pub manager_instance_id: String,
    pub command_id: String,
    pub gateway_instance_id: String,
    pub boot_id: String,
    pub port_id: String,
    pub ack_id: String,
    pub request_hash: String,
    pub request_preview: String,
    pub device_id: String,
    pub accepted: bool,
    pub status_code: u32,
    pub phase: String,
    pub attempt_count: u32,
    pub sent_count: u32,
    pub attempts: Vec<Attempt>,
    pub error: String,
    pub revision: u64,
    pub started_at: String,
    pub updated_at: String,
    pub audit_incomplete: bool,
}

impl Snapshot {
    pub fn terminal(&self) -> bool {
        matches!(
            self.phase.as_str(),
            "AckReceived" | "Failed" | "Expired" | "Unknown" | "SentUnconfirmed"
        )
    }
    fn key(&self) -> String {
        key(&self.manager_instance_id, &self.command_id)
    }
}

fn key(manager: &str, command: &str) -> String {
    format!("{manager}\0{command}")
}
fn now() -> String {
    Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true)
}
fn new_id() -> String {
    format!(
        "{:x}-{}",
        Utc::now().timestamp_nanos_opt().unwrap_or_default(),
        std::process::id()
    )
}

struct State {
    entries: HashMap<String, Snapshot>,
    ack: HashMap<(String, u64), String>,
    dirty: BTreeMap<String, Snapshot>,
    revision: u64,
    persisted: HashMap<String, u64>,
    error: String,
    gap: bool,
    // A bounded tombstone filter prevents replay after degraded cache eviction.
    // False positives reject an unknown duplicate; they never execute it twice.
    seen: Vec<u8>,
}
struct Inner {
    state: Mutex<State>,
    gateway: String,
    boot: String,
    // A read-only startup inventory distinguishes a genuinely new identity
    // from a historical command when a later point lookup becomes unavailable.
    historical_ids: HashSet<[u8; 32]>,
    history_known: bool,
    // Admission is serialized separately from the worker's short memory lock.
    admission: Mutex<()>,
    requests: mpsc::SyncSender<Request>,
    lookups: mpsc::SyncSender<Lookup>,
}
struct Lookup {
    manager: String,
    command: String,
    reply: mpsc::Sender<Result<Option<Snapshot>, String>>,
}
enum Request {
    Events(String, u64, usize, mpsc::Sender<Result<Value, String>>),
    Wake,
    #[cfg(test)]
    Pause(mpsc::Sender<()>, mpsc::Receiver<()>),
}

#[derive(Clone)]
pub struct Tracker(Arc<Inner>);

impl Tracker {
    pub fn legacy_ack_id(&self) -> u64 {
        static IDS: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1 << 63);
        IDS.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
    }
    pub fn open(path: PathBuf) -> Self {
        let boot = new_id();
        let opened = open_database(&path, &boot, None);
        let (gateway, revision, database, error) = match opened {
            Ok((db, id, rev)) => (id, rev, Some(db), String::new()),
            Err(err) => (
                // Read-only identity lookup can still work when writes fail
                // (disk full/read-only media). Never invent a new identity if
                // the durable one remains readable.
                read_gateway_identity(&path).unwrap_or_else(|| format!("volatile-{boot}")),
                0,
                None,
                err,
            ),
        };
        let history = historical_command_ids(&path, &gateway);
        let history_known = history.is_ok();
        let historical_ids = history.unwrap_or_default();
        let (tx, rx) = mpsc::sync_channel(64);
        let (lookup_tx, lookup_rx) = mpsc::sync_channel(64);
        let inner = Arc::new(Inner {
            state: Mutex::new(State {
                entries: HashMap::new(),
                ack: HashMap::new(),
                dirty: BTreeMap::new(),
                revision,
                persisted: HashMap::new(),
                error,
                gap: false,
                seen: vec![0; 128 * 1024],
            }),
            gateway,
            boot,
            historical_ids,
            history_known,
            admission: Mutex::new(()),
            requests: tx,
            lookups: lookup_tx,
        });
        let weak = Arc::downgrade(&inner);
        let writer_path = path.clone();
        if let Err(error) = std::thread::Builder::new()
            .name("command-journal".into())
            .spawn(move || writer(weak, writer_path, database, rx))
        {
            inner.state.lock().unwrap_or_else(|e| e.into_inner()).error =
                format!("journal thread unavailable: {error}");
        }
        let gateway = inner.gateway.clone();
        if let Err(error) = std::thread::Builder::new()
            .name("command-journal-reader".into())
            .spawn(move || lookup_reader(path, gateway, lookup_rx))
        {
            inner.state.lock().unwrap_or_else(|e| e.into_inner()).error =
                format!("journal reader unavailable: {error}");
        }
        Self(inner)
    }

    pub fn identity(&self) -> Value {
        let s = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        json!({"gateway_instance_id":self.0.gateway,"boot_id":self.0.boot,"revision":s.revision.to_string(),
            "audit_degraded":!s.error.is_empty(),"audit_error":s.error,"history_gap":s.gap,
            "retention_days":7,"max_completed_commands":KEEP_COUNT})
    }

    pub fn revision(&self) -> String {
        self.0
            .state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .revision
            .to_string()
    }

    pub fn flush(&self, timeout: Duration) -> bool {
        let start = Instant::now();
        let _ = self.0.requests.try_send(Request::Wake);
        while start.elapsed() < timeout {
            if self
                .0
                .state
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .dirty
                .is_empty()
            {
                return true;
            }
            std::thread::sleep(Duration::from_millis(2));
        }
        false
    }

    pub fn validate_identity(&self, gateway: &str, boot: &str) -> Result<(), String> {
        if gateway != self.0.gateway || boot != self.0.boot {
            return Err("lower instance changed; do not replay the command".into());
        }
        Ok(())
    }

    pub fn get(&self, manager: &str, command: &str) -> Result<Option<Snapshot>, String> {
        {
            let s = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(value) = s.entries.get(&key(manager, command)) {
                let mut value = value.clone();
                value.audit_incomplete |= !s.error.is_empty();
                return Ok(Some(value));
            }
        }
        let (tx, rx) = mpsc::channel();
        self.0
            .lookups
            .try_send(Lookup {
                manager: manager.into(),
                command: command.into(),
                reply: tx,
            })
            .map_err(|_| "journal busy".to_string())?;
        rx.recv_timeout(WAIT)
            .map_err(|_| "journal lookup timeout".to_string())?
    }

    // Returns (snapshot, newly reserved). Repeated IDs never enqueue again.
    pub fn begin(
        &self,
        manager: &str,
        command: &str,
        port: &str,
        ack: u64,
        payload: &str,
    ) -> Result<(Snapshot, bool), String> {
        if [manager, command, port]
            .iter()
            .any(|v| v.is_empty() || v.len() > 256 || v.contains('\0'))
        {
            return Err("invalid command identity".into());
        }
        let normalized: String = payload
            .chars()
            .filter(|c| !c.is_ascii_whitespace())
            .flat_map(char::to_uppercase)
            .collect();
        if normalized.len() > 65_536 {
            return Err("command payload too large".into());
        }
        let hash = format!(
            "{:x}",
            Sha256::digest(format!("{port}\0{normalized}").as_bytes())
        );
        let _guard = self.0.admission.lock().unwrap_or_else(|e| e.into_inner());
        let start = Instant::now();
        match self.get(manager, command) {
            Ok(Some(existing)) => {
                if existing.request_hash != hash {
                    return Err("command ID reused with different content".into());
                }
                return Ok((existing, false));
            }
            Ok(None) => (),
            Err(error) => {
                self.degrade(error);
                // Continuing after failed audit writes is allowed. Treating an
                // unreadable old identity as an absent command is not. A new
                // command may proceed only when the startup inventory proves
                // it did not belong to a prior process; this process's bounded
                // tombstones below still protect subsequent admissions.
                if !self.0.history_known {
                    return Err(
                        "command history unavailable; identity cannot be confirmed; never replayed"
                            .into(),
                    );
                }
            }
        }
        if self
            .0
            .historical_ids
            .contains(&command_identity_digest(manager, command))
        {
            return Err("historical command evidence unavailable; never replayed".into());
        }
        let timestamp = now();
        let mut s = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        let digest = Sha256::digest(key(manager, command).as_bytes());
        let indexes: Vec<usize> = digest
            .chunks_exact(4)
            .take(4)
            .map(|b| u32::from_le_bytes(b.try_into().unwrap()) as usize % (s.seen.len() * 8))
            .collect();
        if indexes.iter().all(|i| s.seen[i / 8] & (1 << (i % 8)) != 0) {
            return Err("command identity was admitted but its evidence is no longer available; never replayed".into());
        }
        if s.entries.len() >= CACHE_LIMIT {
            if let Some(old) = s
                .entries
                .iter()
                .filter(|(_, v)| v.terminal())
                .min_by_key(|(_, v)| v.revision)
                .map(|(k, _)| k.clone())
            {
                if s.dirty.remove(&old).is_some() {
                    s.gap = true;
                }
                s.entries.remove(&old);
                s.persisted.remove(&old);
                s.ack.retain(|_, v| v != &old);
            } else {
                return Err("too many active tracked commands".into());
            }
        }
        // Only remember an admitted identity. A capacity rejection must not
        // permanently tombstone a request that never entered the port queue.
        for i in indexes {
            s.seen[i / 8] |= 1 << (i % 8);
        }
        s.revision += 1;
        let value = Snapshot {
            schema_version: 1,
            manager_instance_id: manager.into(),
            command_id: command.into(),
            gateway_instance_id: self.0.gateway.clone(),
            boot_id: self.0.boot.clone(),
            port_id: port.into(),
            ack_id: ack.to_string(),
            request_hash: hash,
            request_preview: normalized.chars().take(512).collect(),
            device_id: normalized
                .get(..2)
                .and_then(|s| u8::from_str_radix(s, 16).ok())
                .map(|v| v.to_string())
                .unwrap_or_default(),
            accepted: false,
            status_code: 0,
            phase: "Accepted".into(),
            attempt_count: 0,
            sent_count: 0,
            attempts: Vec::new(),
            error: String::new(),
            revision: s.revision,
            started_at: timestamp.clone(),
            updated_at: timestamp,
            audit_incomplete: !s.error.is_empty(),
        };
        let k = value.key();
        s.ack.insert((port.into(), ack), k.clone());
        s.entries.insert(k.clone(), value.clone());
        s.dirty.insert(k.clone(), value.clone());
        drop(s);
        let _ = self.0.requests.try_send(Request::Wake);
        while start.elapsed() < WAIT {
            let s = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
            if s.persisted.get(&k).is_some_and(|r| *r >= value.revision) {
                return Ok((value, true));
            }
            if !s.error.is_empty() {
                break;
            }
            drop(s);
            std::thread::sleep(Duration::from_millis(1));
        }
        self.degrade("command acceptance not durably confirmed within 100 ms".into());
        self.update(port, Some(ack), |v| v.audit_incomplete = true);
        Ok((self.get(manager, command)?.unwrap_or(value), true))
    }

    /// In-memory evidence only; admission must additionally acquire the physical
    /// command permit, which remains held until the worker has actually exited.
    pub fn is_terminal_ack(&self, port: &str, ack: u64) -> bool {
        let s = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        s.ack
            .get(&(port.into(), ack))
            .and_then(|key| s.entries.get(key))
            .is_some_and(Snapshot::terminal)
    }

    pub fn update(&self, port: &str, ack: Option<u64>, change: impl FnOnce(&mut Snapshot)) {
        let Some(ack) = ack else { return };
        let mut s = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        let Some(k) = s.ack.get(&(port.into(), ack)).cloned() else {
            return;
        };
        s.revision += 1;
        let rev = s.revision;
        let Some(v) = s.entries.get_mut(&k) else {
            return;
        };
        change(v);
        v.revision = rev;
        v.updated_at = now();
        if v.attempts.len() > 256 {
            v.attempts.drain(..v.attempts.len() - 256);
            v.audit_incomplete = true;
        }
        let snapshot = v.clone();
        s.dirty.insert(k, snapshot);
        drop(s);
        let _ = self.0.requests.try_send(Request::Wake);
    }

    pub fn events(&self, manager: &str, after: u64, limit: usize) -> Result<Value, String> {
        let (tx, rx) = mpsc::channel();
        self.0
            .requests
            .try_send(Request::Events(
                manager.into(),
                after,
                limit.clamp(1, 200),
                tx,
            ))
            .map_err(|_| "journal busy".to_string())?;
        let mut value = rx
            .recv_timeout(Duration::from_secs(2))
            .map_err(|_| "journal query timeout".to_string())??;
        value["health"] = self.identity();
        Ok(value)
    }
    fn degrade(&self, error: String) {
        let mut s = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        if s.error != error {
            log::warn!("[命令审计降级] {error}; control remains enabled");
        }
        s.error = error;
    }
}

fn read_gateway_identity(path: &PathBuf) -> Option<String> {
    let db = Connection::open_with_flags(path, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY).ok()?;
    db.query_row("SELECT value FROM identity WHERE id=1", [], |r| r.get(0))
        .ok()
}

fn command_identity_digest(manager: &str, command: &str) -> [u8; 32] {
    Sha256::digest(key(manager, command).as_bytes()).into()
}

fn historical_command_ids(path: &PathBuf, gateway: &str) -> Result<HashSet<[u8; 32]>, String> {
    if !path.try_exists().map_err(|e| e.to_string())? {
        return Ok(HashSet::new()); // No historical file existed before this process.
    }
    let db = Connection::open_with_flags(path, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY)
        .map_err(|e| e.to_string())?;
    db.busy_timeout(Duration::from_millis(50))
        .map_err(|e| e.to_string())?;
    let tx = db.unchecked_transaction().map_err(|e| e.to_string())?;
    let actual: String = tx
        .query_row("SELECT value FROM identity WHERE id=1", [], |r| r.get(0))
        .map_err(|e| e.to_string())?;
    if actual != gateway {
        return Err("historical journal identity mismatch".into());
    }
    let mut stmt = tx
        .prepare("SELECT manager,id FROM commands")
        .map_err(|e| e.to_string())?;
    let rows = stmt
        .query_map([], |r| Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?)))
        .map_err(|e| e.to_string())?;
    let mut ids = HashSet::new();
    for row in rows {
        let (manager, command) = row.map_err(|e| e.to_string())?;
        if ids.len() >= KEEP_COUNT as usize + CACHE_LIMIT {
            return Err("historical identity inventory exceeds bounded limit".into());
        }
        ids.insert(command_identity_digest(&manager, &command));
    }
    Ok(ids)
}

// Historical point lookups must not queue behind FULL-synchronous writes or
// retention work. WAL permits this separate reader while the writer is busy.
// Keep the same bounded request queue and 100 ms caller budget; a read failure
// still remains an audit failure, never a synthetic "record not found".
fn lookup_reader(path: PathBuf, gateway: String, rx: mpsc::Receiver<Lookup>) {
    while let Ok(request) = rx.recv() {
        let result = lookup_journal(&path, &gateway, &request.manager, &request.command);
        let _ = request.reply.send(result);
    }
}

fn lookup_journal(
    path: &PathBuf,
    gateway: &str,
    manager: &str,
    command: &str,
) -> Result<Option<Snapshot>, String> {
    // Reopen for each cold lookup so a replaced or recovered database cannot
    // silently keep serving the previous file or a different gateway's data.
    let db = Connection::open_with_flags(path, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY)
        .map_err(|e| e.to_string())?;
    db.busy_timeout(Duration::from_millis(50))
        .map_err(|e| e.to_string())?;
    let tx = db.unchecked_transaction().map_err(|e| e.to_string())?;
    let version: u32 = tx
        .pragma_query_value(None, "user_version", |r| r.get(0))
        .map_err(|e| e.to_string())?;
    if version != 1 {
        return Err(format!("unsupported command journal schema {version}"));
    }
    let actual: String = tx
        .query_row("SELECT value FROM identity WHERE id=1", [], |r| r.get(0))
        .map_err(|e| e.to_string())?;
    if actual != gateway {
        return Err("command journal instance mismatch; operator recovery required".into());
    }
    let raw: Option<String> = tx
        .query_row(
            "SELECT data FROM commands WHERE manager=? AND id=?",
            params![manager, command],
            |r| r.get(0),
        )
        .optional()
        .map_err(|e| e.to_string())?;
    raw.map(|v| serde_json::from_str(&v).map_err(|e| e.to_string()))
        .transpose()
}

fn open_database(
    path: &PathBuf,
    boot: &str,
    expected_gateway: Option<&str>,
) -> Result<(Connection, String, u64), String> {
    let db = Connection::open(path).map_err(|e| e.to_string())?;
    db.busy_timeout(Duration::from_millis(50))
        .map_err(|e| e.to_string())?;
    let version: u32 = db
        .pragma_query_value(None, "user_version", |r| r.get(0))
        .map_err(|e| e.to_string())?;
    if version > 1 {
        return Err(format!(
            "unsupported command journal schema {version}; control remains enabled"
        ));
    }
    let has_identity: bool = db
        .query_row(
            "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='identity')",
            [],
            |r| r.get(0),
        )
        .map_err(|e| e.to_string())?;
    if has_identity {
        let existing: Option<String> = db
            .query_row("SELECT value FROM identity WHERE id=1", [], |r| r.get(0))
            .optional()
            .map_err(|e| e.to_string())?;
        if let (Some(expected), Some(actual)) = (expected_gateway, existing.as_deref()) {
            if expected != actual {
                // A replacement/unreadable-at-startup database must not mix
                // evidence from two instances or rewrite its old identity.
                return Err("command journal instance mismatch; evidence remains in degraded memory; operator recovery required".into());
            }
        }
    }
    db.execute_batch("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
        CREATE TABLE IF NOT EXISTS journal_integrity(id INTEGER PRIMARY KEY CHECK(id=1),history_gap INTEGER NOT NULL);
        INSERT OR IGNORE INTO journal_integrity VALUES(1,0);
        CREATE TABLE IF NOT EXISTS journal_meta(id INTEGER PRIMARY KEY CHECK(id=1),head INTEGER NOT NULL);
        INSERT OR IGNORE INTO journal_meta VALUES(1,0);
        CREATE TABLE IF NOT EXISTS identity(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL);
        CREATE TABLE IF NOT EXISTS commands(manager TEXT NOT NULL,id TEXT NOT NULL,revision INTEGER NOT NULL,finished INTEGER NOT NULL,updated TEXT NOT NULL,data TEXT NOT NULL,PRIMARY KEY(manager,id));
        CREATE TABLE IF NOT EXISTS events(revision INTEGER PRIMARY KEY,manager TEXT NOT NULL,id TEXT NOT NULL,data TEXT NOT NULL);
        CREATE INDEX IF NOT EXISTS events_manager ON events(manager,revision);").map_err(|e|e.to_string())?;
    db.execute(
        "INSERT OR IGNORE INTO identity VALUES(1,?)",
        [expected_gateway
            .map(str::to_owned)
            .unwrap_or_else(|| format!("gateway-{}", new_id()))],
    )
    .map_err(|e| e.to_string())?;
    let gateway: String = db
        .query_row("SELECT value FROM identity WHERE id=1", [], |r| r.get(0))
        .map_err(|e| e.to_string())?;
    db.pragma_update(None, "user_version", 1)
        .map_err(|e| e.to_string())?;
    let mut revision:u64=db.query_row("SELECT MAX((SELECT head FROM journal_meta WHERE id=1),COALESCE(MAX(revision),0)) FROM events",[],|r|r.get(0)).map_err(|e|e.to_string())?;
    let values: Vec<String> = db
        .prepare("SELECT data FROM commands WHERE finished=0")
        .map_err(|e| e.to_string())?
        .query_map([], |r| r.get(0))
        .map_err(|e| e.to_string())?
        .collect::<Result<_, _>>()
        .map_err(|e| e.to_string())?;
    for raw in values {
        let mut v: Snapshot = serde_json::from_str(&raw).map_err(|e| e.to_string())?;
        if v.boot_id == boot {
            continue;
        }
        revision += 1;
        v.revision = revision;
        v.phase = "Unknown".into();
        v.error = "lower restarted before durable completion; never replayed".into();
        v.audit_incomplete = true;
        v.updated_at = now();
        save(&db, &v)?;
    }
    Ok((db, gateway, revision))
}

fn save(db: &Connection, v: &Snapshot) -> Result<(), String> {
    let raw = serde_json::to_string(v).map_err(|e| e.to_string())?;
    let tx = db.unchecked_transaction().map_err(|e| e.to_string())?;
    tx.execute("INSERT INTO commands VALUES(?,?,?,?,?,?) ON CONFLICT(manager,id) DO UPDATE SET revision=excluded.revision,finished=excluded.finished,updated=excluded.updated,data=excluded.data WHERE excluded.revision>commands.revision",
        params![v.manager_instance_id,v.command_id,v.revision,v.terminal(),v.updated_at,raw]).map_err(|e|e.to_string())?;
    tx.execute(
        "INSERT OR IGNORE INTO events VALUES(?,?,?,?)",
        params![v.revision, v.manager_instance_id, v.command_id, raw],
    )
    .map_err(|e| e.to_string())?;
    tx.execute(
        "UPDATE journal_meta SET head=MAX(head,?) WHERE id=1",
        [v.revision],
    )
    .map_err(|e| e.to_string())?;
    tx.commit().map_err(|e| e.to_string())
}

fn prune_journal(db: &Connection, cutoff: &str, keep_count: i64) -> Result<(), String> {
    let tx = db.unchecked_transaction().map_err(|e| e.to_string())?;
    tx.execute("DELETE FROM commands WHERE finished=1 AND (updated<? OR rowid IN (SELECT rowid FROM commands WHERE finished=1 ORDER BY revision DESC LIMIT -1 OFFSET ?))", params![cutoff, keep_count]).map_err(|e| e.to_string())?;
    tx.execute("DELETE FROM events WHERE NOT EXISTS(SELECT 1 FROM commands c WHERE c.manager=events.manager AND c.id=events.id)", []).map_err(|e| e.to_string())?;
    // journal_meta.head deliberately survives pruning, so cursors and future
    // events never reuse a removed revision, even when all rows expire.
    tx.commit().map_err(|e| e.to_string())
}

fn writer(
    weak: Weak<Inner>,
    path: PathBuf,
    mut db: Option<Connection>,
    rx: mpsc::Receiver<Request>,
) {
    let mut last_cleanup = Instant::now();
    loop {
        let request = rx.recv_timeout(Duration::from_millis(20));
        let Some(inner) = weak.upgrade() else { break };
        if db.is_none() {
            match open_database(&path, &inner.boot, Some(&inner.gateway)) {
                Ok((connection, _, revision)) => {
                    let mut s = inner.state.lock().unwrap_or_else(|e| e.into_inner());
                    s.revision = s.revision.max(revision);
                    // A previously inaccessible database may have a higher head.
                    // Rebase buffered snapshots before writing; never collide with
                    // an old event or silently discard the new command evidence.
                    let keys: Vec<String> = s.dirty.keys().cloned().collect();
                    for k in keys {
                        s.revision += 1;
                        let rev = s.revision;
                        if let Some(v) = s.entries.get_mut(&k) {
                            v.revision = rev;
                            v.audit_incomplete = true;
                            let value = v.clone();
                            s.dirty.insert(k, value);
                        }
                    }
                    db = Some(connection);
                }
                Err(error) => {
                    inner.state.lock().unwrap_or_else(|e| e.into_inner()).error = error;
                }
            }
        }
        let values: Vec<Snapshot> = inner
            .state
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .dirty
            .values()
            .take(256)
            .cloned()
            .collect();
        if let Some(connection) = &db {
            let gap = inner.state.lock().unwrap_or_else(|e| e.into_inner()).gap;
            if gap {
                let _ =
                    connection.execute("UPDATE journal_integrity SET history_gap=1 WHERE id=1", []);
            } else if connection
                .query_row(
                    "SELECT history_gap FROM journal_integrity WHERE id=1",
                    [],
                    |r| r.get::<_, bool>(0),
                )
                .unwrap_or(false)
            {
                inner.state.lock().unwrap_or_else(|e| e.into_inner()).gap = true;
            }
            for v in values {
                match save(connection, &v) {
                    Ok(()) => {
                        let mut s = inner.state.lock().unwrap_or_else(|e| e.into_inner());
                        let k = v.key();
                        s.persisted.insert(k.clone(), v.revision);
                        if s.dirty.get(&k).is_some_and(|x| x.revision == v.revision) {
                            s.dirty.remove(&k);
                        }
                        if s.dirty.is_empty() {
                            s.error.clear();
                        }
                    }
                    Err(error) => {
                        let mut s = inner.state.lock().unwrap_or_else(|e| e.into_inner());
                        if s.error != error {
                            log::warn!("[命令审计降级] {error}; control remains enabled");
                        }
                        s.error = error;
                        let keys: Vec<String> = s.dirty.keys().cloned().collect();
                        for k in keys {
                            if let Some(v) = s.entries.get_mut(&k) {
                                v.audit_incomplete = true;
                                let value = v.clone();
                                s.dirty.insert(k, value);
                            }
                        }
                        break;
                    }
                }
            }
            if last_cleanup.elapsed() > Duration::from_secs(60) {
                let cutoff = (Utc::now() - chrono::Duration::days(7)).to_rfc3339();
                if let Err(error) = prune_journal(connection, &cutoff, KEEP_COUNT) {
                    let mut s = inner.state.lock().unwrap_or_else(|e| e.into_inner());
                    s.error = format!("command journal retention failed: {error}");
                    log::warn!("[命令审计降级] {}; control remains enabled", s.error);
                }
                last_cleanup = Instant::now();
            }
        }
        match request {
            #[cfg(test)]
            Ok(Request::Pause(ready, resume)) => {
                let _ = ready.send(());
                let _ = resume.recv();
            }
            Ok(Request::Events(manager, after, limit, reply)) => {
                let result=db.as_ref().ok_or_else(||"journal unavailable".to_string()).and_then(|db|{
                let oldest:u64=db.query_row("SELECT COALESCE(MIN(revision),0) FROM events",[],|r|r.get(0)).map_err(|e|e.to_string())?;
                // Do not advance past older dirty evidence that can commit later.
                let ceiling={let s=inner.state.lock().unwrap_or_else(|e|e.into_inner());s.dirty.values().map(|v|v.revision.saturating_sub(1)).min().unwrap_or(s.revision)};
                let mut stmt=db.prepare("SELECT data FROM events WHERE manager=? AND revision>? AND revision<=? ORDER BY revision LIMIT ?").map_err(|e|e.to_string())?;
                let raw=stmt.query_map(params![manager,after,ceiling,limit as i64],|r|r.get::<_,String>(0)).map_err(|e|e.to_string())?;
                let mut items=Vec::new();let mut cursor=after;let mut bytes=0;
                for row in raw{let raw=row.map_err(|e|e.to_string())?;if !items.is_empty() && bytes+raw.len()>1024*1024{break}bytes+=raw.len();let v:Snapshot=serde_json::from_str(&raw).map_err(|e|e.to_string())?;cursor=v.revision;items.push(v);}
                let gap=inner.state.lock().unwrap_or_else(|e|e.into_inner()).gap;
                Ok(json!({"items":items,"next_cursor":cursor.to_string(),"oldest_revision":oldest.to_string(),"history_gap":gap || (after>0 && after<oldest.saturating_sub(1))}))
            });
                let _ = reply.send(result);
            }
            _ => (),
        }
    }
}
