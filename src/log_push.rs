//! Non-blocking HTTP delivery for lower-gateway log records.
//!
//! The logger only calls `try_send`; network and spool I/O are isolated on a
//! dedicated OS thread. Local rolling files remain the authoritative fallback.

use std::collections::VecDeque;
use std::fs::{self, File};
use std::io::{self, BufRead, BufReader, Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::path::{Path, PathBuf};
use std::sync::mpsc::{self, Receiver, SyncSender, TryRecvError, TrySendError};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use chrono::{SecondsFormat, Utc};
use log::{Level, Record};
use serde::{Deserialize, Serialize};

const PUSH_URL_ENV: &str = "RCD_LOG_PUSH_URL";
const PUSH_SOURCE_ENV: &str = "RCD_LOG_PUSH_SOURCE_ID";
const SPOOL_FILE: &str = "http-log-spool.jsonl";
const MAX_MESSAGE_BYTES: usize = 16 * 1024;
const HIGH_PRIORITY_CAPACITY: usize = 512;

#[derive(Deserialize, Debug, Clone)]
#[serde(default)]
pub struct HttpLogPushConfig {
    /// Disabled by default so an existing deployment is unchanged until the
    /// source identity and manager URL are explicitly configured.
    pub enabled: bool,
    /// Full manager ingest URL, for example
    /// `http://192.168.10.46:8087/api/v1/lower-ingest/logs`.
    pub url: String,
    /// Stable source id configured for this lower in RCD Manager.
    pub source_id: String,
    pub batch_interval_ms: u64,
    pub batch_size: usize,
    pub queue_capacity: usize,
    pub spool_directory: Option<String>,
    pub spool_max_bytes: u64,
    pub request_timeout_ms: u64,
}

impl Default for HttpLogPushConfig {
    fn default() -> Self {
        Self {
            enabled: false,
            url: String::new(),
            source_id: String::new(),
            batch_interval_ms: 500,
            batch_size: 100,
            queue_capacity: 10_000,
            spool_directory: None,
            spool_max_bytes: 16 * 1024 * 1024,
            request_timeout_ms: 3_000,
        }
    }
}

impl HttpLogPushConfig {
    fn resolved(mut self, log_dir: &Path) -> Result<ResolvedConfig, String> {
        if let Ok(value) = std::env::var(PUSH_URL_ENV) {
            if !value.trim().is_empty() {
                self.enabled = true;
                self.url = value;
            }
        }
        if let Ok(value) = std::env::var(PUSH_SOURCE_ENV) {
            if !value.trim().is_empty() {
                self.source_id = value;
            }
        }
        if !self.enabled {
            return Err("disabled".to_string());
        }
        let endpoint = HttpEndpoint::parse(self.url.trim())?;
        let source_id = self.source_id.trim().to_string();
        if source_id.is_empty() || source_id.len() > 128 {
            return Err("logging.http_push.source_id must contain 1..128 characters".to_string());
        }
        if source_id.chars().any(char::is_control) {
            return Err(
                "logging.http_push.source_id must not contain control characters".to_string(),
            );
        }
        let spool_dir = match self.spool_directory.as_deref().map(str::trim) {
            Some(value) if !value.is_empty() => {
                let path = PathBuf::from(value);
                if path.is_absolute() {
                    path
                } else {
                    log_dir.join(path)
                }
            }
            _ => log_dir.join("push-spool"),
        };
        Ok(ResolvedConfig {
            endpoint,
            source_id,
            batch_interval: Duration::from_millis(self.batch_interval_ms.clamp(50, 60_000)),
            batch_size: self.batch_size.clamp(1, 500),
            queue_capacity: self.queue_capacity.clamp(100, 100_000),
            spool_path: spool_dir.join(SPOOL_FILE),
            spool_max_bytes: self.spool_max_bytes.clamp(1024 * 1024, 512 * 1024 * 1024),
            request_timeout: Duration::from_millis(self.request_timeout_ms.clamp(250, 30_000)),
        })
    }
}

#[derive(Debug, Clone)]
struct ResolvedConfig {
    endpoint: HttpEndpoint,
    source_id: String,
    batch_interval: Duration,
    batch_size: usize,
    queue_capacity: usize,
    spool_path: PathBuf,
    spool_max_bytes: u64,
    request_timeout: Duration,
}

#[derive(Debug, Clone)]
struct PendingRecord {
    timestamp: String,
    level: String,
    component: String,
    message: String,
    urgent: bool,
}

#[derive(Debug, Clone)]
pub(crate) struct HttpLogSink {
    normal: SyncSender<PendingRecord>,
    high: SyncSender<PendingRecord>,
}

impl HttpLogSink {
    pub(crate) fn enqueue(&self, record: &Record<'_>) {
        let level = record.level();
        let pending = PendingRecord {
            timestamp: Utc::now().to_rfc3339_opts(SecondsFormat::Millis, true),
            level: level.to_string(),
            component: truncate_utf8(record.target(), 256),
            message: truncate_utf8(&record.args().to_string(), MAX_MESSAGE_BYTES),
            urgent: matches!(level, Level::Warn | Level::Error),
        };
        if pending.urgent {
            match self.high.try_send(pending) {
                Ok(()) => (),
                Err(TrySendError::Full(value)) => {
                    let _ = self.normal.try_send(value);
                }
                Err(TrySendError::Disconnected(_)) => {}
            }
        } else {
            let _ = self.normal.try_send(pending);
        }
    }
}

pub(crate) struct HttpLogPushGuard {
    shutdown: mpsc::Sender<()>,
    worker: Option<JoinHandle<()>>,
}

impl HttpLogPushGuard {
    pub(crate) fn start(
        config: HttpLogPushConfig,
        log_dir: &Path,
    ) -> Result<(HttpLogSink, Self), String> {
        let resolved = config.resolved(log_dir)?;
        if let Some(parent) = resolved.spool_path.parent() {
            fs::create_dir_all(parent)
                .map_err(|error| format!("create HTTP log spool directory {parent:?}: {error}"))?;
        }
        let (normal_tx, normal_rx) = mpsc::sync_channel(resolved.queue_capacity);
        let (high_tx, high_rx) = mpsc::sync_channel(HIGH_PRIORITY_CAPACITY);
        let (shutdown_tx, shutdown_rx) = mpsc::channel();
        let worker = thread::Builder::new()
            .name("rcd-http-log-push".to_string())
            .spawn(move || run_worker(resolved, normal_rx, high_rx, shutdown_rx))
            .map_err(|error| format!("start HTTP log push worker: {error}"))?;
        Ok((
            HttpLogSink {
                normal: normal_tx,
                high: high_tx,
            },
            Self {
                shutdown: shutdown_tx,
                worker: Some(worker),
            },
        ))
    }

    pub(crate) fn shutdown(&mut self) {
        let _ = self.shutdown.send(());
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

impl Drop for HttpLogPushGuard {
    fn drop(&mut self) {
        self.shutdown();
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct StoredLogEvent {
    source_id: String,
    boot_id: String,
    sequence: u64,
    timestamp: String,
    level: String,
    component: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    port_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    device_id: Option<String>,
    message: String,
}

#[derive(Serialize)]
struct LogBatch<'a> {
    source_id: &'a str,
    boot_id: &'a str,
    first_sequence: u64,
    events: &'a [StoredLogEvent],
}

#[derive(Deserialize)]
struct LogAck {
    source_id: String,
    boot_id: String,
    accepted_through: u64,
}

fn run_worker(
    config: ResolvedConfig,
    normal_rx: Receiver<PendingRecord>,
    high_rx: Receiver<PendingRecord>,
    shutdown_rx: Receiver<()>,
) {
    let mut pending = load_spool(&config.spool_path, &config.source_id);
    let mut boot_id = new_boot_id();
    let mut next_sequence = 0_u64;
    let mut spool_dirty = false;
    let mut last_batch_at = Instant::now();
    let mut next_attempt_at = Instant::now();
    let mut retry_delay = Duration::from_secs(1);
    let mut stopping = false;
    let mut force_flush = false;

    loop {
        if shutdown_rx.try_recv().is_ok() {
            stopping = true;
        }
        let mut urgent = force_flush;
        force_flush = false;
        for record in drain_records(&high_rx, &normal_rx, 512) {
            urgent |= record.urgent;
            next_sequence = next_sequence.saturating_add(1);
            let (port_id, device_id) = extract_log_context(&record.message);
            pending.push_back(StoredLogEvent {
                source_id: config.source_id.clone(),
                boot_id: boot_id.clone(),
                sequence: next_sequence,
                timestamp: record.timestamp,
                level: record.level,
                component: record.component,
                port_id,
                device_id,
                message: record.message,
            });
            spool_dirty = true;
        }

        if spool_dirty
            && (urgent
                || pending.len() >= config.batch_size
                || last_batch_at.elapsed() >= config.batch_interval
                || stopping)
        {
            enforce_spool_limit(
                &mut pending,
                &mut boot_id,
                &mut next_sequence,
                config.spool_max_bytes,
            );
            if let Err(error) = save_spool(&config.spool_path, &pending) {
                eprintln!("HTTP 日志补发文件写入失败: {error}");
            } else {
                spool_dirty = false;
            }
        }

        let should_send = !pending.is_empty()
            && Instant::now() >= next_attempt_at
            && (urgent
                || pending.len() >= config.batch_size
                || last_batch_at.elapsed() >= config.batch_interval
                || stopping);
        if should_send {
            let batch = next_batch(&pending, config.batch_size);
            match post_batch(&config, &batch) {
                Ok(accepted_through) => {
                    let source_id = batch[0].source_id.clone();
                    let accepted_boot = batch[0].boot_id.clone();
                    while matches!(pending.front(), Some(event)
                        if event.source_id == source_id
                            && event.boot_id == accepted_boot
                            && event.sequence <= accepted_through)
                    {
                        pending.pop_front();
                    }
                    if let Err(error) = save_spool(&config.spool_path, &pending) {
                        eprintln!("HTTP 日志 ACK 后压缩补发文件失败: {error}");
                        spool_dirty = true;
                    } else {
                        spool_dirty = false;
                    }
                    retry_delay = Duration::from_secs(1);
                    next_attempt_at = Instant::now();
                    last_batch_at = Instant::now();
                }
                Err(error) => {
                    if spool_dirty {
                        let _ = save_spool(&config.spool_path, &pending);
                        spool_dirty = false;
                    }
                    if retry_delay == Duration::from_secs(1) {
                        eprintln!("HTTP 日志推送暂不可用，将在后台补发: {error}");
                    }
                    next_attempt_at = Instant::now() + retry_delay;
                    retry_delay = (retry_delay * 2).min(Duration::from_secs(30));
                }
            }
        }

        if stopping {
            // One bounded final attempt is enough. The durable spool is the
            // next process's responsibility if the manager is unavailable.
            if spool_dirty {
                let _ = save_spool(&config.spool_path, &pending);
            }
            return;
        }

        if let Ok(record) = high_rx.recv_timeout(Duration::from_millis(25)) {
            force_flush = record.urgent;
            next_sequence = next_sequence.saturating_add(1);
            let (port_id, device_id) = extract_log_context(&record.message);
            pending.push_back(StoredLogEvent {
                source_id: config.source_id.clone(),
                boot_id: boot_id.clone(),
                sequence: next_sequence,
                timestamp: record.timestamp,
                level: record.level,
                component: record.component,
                port_id,
                device_id,
                message: record.message,
            });
            spool_dirty = true;
        }
    }
}

fn drain_records(
    high_rx: &Receiver<PendingRecord>,
    normal_rx: &Receiver<PendingRecord>,
    limit: usize,
) -> Vec<PendingRecord> {
    let mut values = Vec::with_capacity(limit.min(512));
    while values.len() < limit {
        match high_rx.try_recv() {
            Ok(value) => values.push(value),
            Err(TryRecvError::Empty) => break,
            Err(TryRecvError::Disconnected) => break,
        }
    }
    while values.len() < limit {
        match normal_rx.try_recv() {
            Ok(value) => values.push(value),
            Err(TryRecvError::Empty) => break,
            Err(TryRecvError::Disconnected) => break,
        }
    }
    values
}

fn next_batch(pending: &VecDeque<StoredLogEvent>, max: usize) -> Vec<StoredLogEvent> {
    let Some(first) = pending.front() else {
        return Vec::new();
    };
    pending
        .iter()
        .take_while(|event| event.source_id == first.source_id && event.boot_id == first.boot_id)
        .take(max)
        .cloned()
        .collect()
}

fn post_batch(config: &ResolvedConfig, events: &[StoredLogEvent]) -> Result<u64, String> {
    let first = events
        .first()
        .ok_or_else(|| "empty log batch".to_string())?;
    let body = serde_json::to_vec(&LogBatch {
        source_id: &first.source_id,
        boot_id: &first.boot_id,
        first_sequence: first.sequence,
        events,
    })
    .map_err(|error| format!("encode log batch: {error}"))?;
    let response = http_post_json(&config.endpoint, &body, config.request_timeout)?;
    let ack: LogAck = serde_json::from_slice(&response)
        .map_err(|error| format!("decode manager log ACK: {error}"))?;
    if ack.source_id != first.source_id || ack.boot_id != first.boot_id {
        return Err("manager log ACK identity does not match request".to_string());
    }
    if ack.accepted_through < first.sequence {
        return Err("manager log ACK did not accept the first event".to_string());
    }
    if ack.accepted_through > events.last().map_or(first.sequence, |event| event.sequence) {
        return Err("manager log ACK exceeds the submitted batch".to_string());
    }
    Ok(ack.accepted_through)
}

fn load_spool(path: &Path, source_id: &str) -> VecDeque<StoredLogEvent> {
    let Ok(file) = File::open(path) else {
        return VecDeque::new();
    };
    let mut result = VecDeque::new();
    for line in BufReader::new(file).lines().map_while(Result::ok) {
        if let Ok(event) = serde_json::from_str::<StoredLogEvent>(&line) {
            if event.source_id == source_id && event.sequence > 0 {
                result.push_back(event);
            }
        }
    }
    result
}

fn save_spool(path: &Path, events: &VecDeque<StoredLogEvent>) -> io::Result<()> {
    let parent = path.parent().unwrap_or_else(|| Path::new("."));
    fs::create_dir_all(parent)?;
    let temporary = path.with_extension(format!("tmp-{}", std::process::id()));
    {
        let mut file = File::create(&temporary)?;
        for event in events {
            serde_json::to_writer(&mut file, event).map_err(io::Error::other)?;
            file.write_all(b"\n")?;
        }
        file.sync_all()?;
    }
    if cfg!(windows) && path.exists() {
        fs::remove_file(path)?;
    }
    fs::rename(temporary, path)
}

fn enforce_spool_limit(
    events: &mut VecDeque<StoredLogEvent>,
    boot_id: &mut String,
    next_sequence: &mut u64,
    limit: u64,
) {
    let mut bytes = estimated_spool_bytes(events);
    if bytes <= limit {
        return;
    }
    while bytes > limit {
        let candidate = events
            .iter()
            .position(|event| matches!(event.level.as_str(), "TRACE" | "DEBUG" | "INFO"));
        let Some(index) = candidate.or_else(|| (!events.is_empty()).then_some(0)) else {
            break;
        };
        events.remove(index);
        bytes = estimated_spool_bytes(events);
    }
    // Removing an unacknowledged low-priority record would create a sequence
    // gap. Rebase the retained backlog as one new boot stream instead.
    let replacement = new_boot_id();
    for (index, event) in events.iter_mut().enumerate() {
        event.boot_id = replacement.clone();
        event.sequence = index as u64 + 1;
    }
    *next_sequence = events.len() as u64;
    *boot_id = replacement;
}

fn estimated_spool_bytes(events: &VecDeque<StoredLogEvent>) -> u64 {
    events
        .iter()
        .map(|event| {
            serde_json::to_vec(event)
                .map(|value| value.len() as u64 + 1)
                .unwrap_or(0)
        })
        .sum()
}

fn new_boot_id() -> String {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    format!("{:x}-{:x}", nanos, std::process::id())
}

fn truncate_utf8(value: &str, max_bytes: usize) -> String {
    if value.len() <= max_bytes {
        return value.to_string();
    }
    let mut end = max_bytes;
    while !value.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &value[..end])
}

fn extract_log_context(message: &str) -> (Option<String>, Option<String>) {
    let port = extract_token(message, &["port_id=", "port=", "串口="]).or_else(|| {
        message
            .split_whitespace()
            .find(|part| part.starts_with("/dev/tty"))
            .map(clean_token)
    });
    let device = extract_token(message, &["device_id=", "device=", "站点="]);
    (port, device)
}

fn extract_token(message: &str, prefixes: &[&str]) -> Option<String> {
    prefixes.iter().find_map(|prefix| {
        let start = message.find(prefix)? + prefix.len();
        let value = message[start..]
            .split_whitespace()
            .next()
            .map(clean_token)?;
        (!value.is_empty()).then_some(value)
    })
}

fn clean_token(value: &str) -> String {
    value
        .trim_matches(|character: char| matches!(character, ',' | ';' | ']' | ')' | '"' | '\''))
        .chars()
        .take(128)
        .collect()
}

#[derive(Debug, Clone)]
struct HttpEndpoint {
    host: String,
    port: u16,
    path: String,
    host_header: String,
}

impl HttpEndpoint {
    fn parse(value: &str) -> Result<Self, String> {
        let rest = value.strip_prefix("http://").ok_or_else(|| {
            "logging.http_push.url must use http:// while TLS is disabled".to_string()
        })?;
        let (authority, path) = match rest.find('/') {
            Some(index) => (&rest[..index], &rest[index..]),
            None => (rest, "/"),
        };
        if authority.is_empty() || authority.contains('@') || path.contains('#') {
            return Err("logging.http_push.url is invalid".to_string());
        }
        let (host, port) = if let Some(bracketed) = authority.strip_prefix('[') {
            let end = bracketed
                .find(']')
                .ok_or_else(|| "invalid IPv6 log push URL".to_string())?;
            let host = &bracketed[..end];
            let suffix = &bracketed[end + 1..];
            let port = suffix
                .strip_prefix(':')
                .unwrap_or("80")
                .parse::<u16>()
                .map_err(|_| "invalid log push URL port".to_string())?;
            (host.to_string(), port)
        } else if let Some((host, port)) = authority.rsplit_once(':') {
            (
                host.to_string(),
                port.parse::<u16>()
                    .map_err(|_| "invalid log push URL port".to_string())?,
            )
        } else {
            (authority.to_string(), 80)
        };
        if host.trim().is_empty() {
            return Err("log push URL host is empty".to_string());
        }
        Ok(Self {
            host,
            port,
            path: path.to_string(),
            host_header: authority.to_string(),
        })
    }
}

fn http_post_json(
    endpoint: &HttpEndpoint,
    body: &[u8],
    timeout: Duration,
) -> Result<Vec<u8>, String> {
    let address = (endpoint.host.as_str(), endpoint.port)
        .to_socket_addrs()
        .map_err(|error| format!("resolve manager address: {error}"))?
        .next()
        .ok_or_else(|| "manager address did not resolve".to_string())?;
    let mut stream = TcpStream::connect_timeout(&address, timeout)
        .map_err(|error| format!("connect manager: {error}"))?;
    stream
        .set_read_timeout(Some(timeout))
        .map_err(|error| error.to_string())?;
    stream
        .set_write_timeout(Some(timeout))
        .map_err(|error| error.to_string())?;
    write!(stream,
        "POST {} HTTP/1.1\r\nHost: {}\r\nContent-Type: application/json\r\nAccept: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        endpoint.path, endpoint.host_header, body.len())
        .and_then(|_| stream.write_all(body))
        .and_then(|_| stream.flush())
        .map_err(|error| format!("write manager request: {error}"))?;
    let mut response = Vec::new();
    stream
        .read_to_end(&mut response)
        .map_err(|error| format!("read manager response: {error}"))?;
    parse_http_response(&response)
}

fn parse_http_response(response: &[u8]) -> Result<Vec<u8>, String> {
    let split = response
        .windows(4)
        .position(|window| window == b"\r\n\r\n")
        .ok_or_else(|| "manager returned an invalid HTTP response".to_string())?;
    let headers = std::str::from_utf8(&response[..split])
        .map_err(|_| "manager returned non-UTF8 HTTP headers".to_string())?;
    let status = headers
        .lines()
        .next()
        .and_then(|line| line.split_whitespace().nth(1))
        .and_then(|value| value.parse::<u16>().ok())
        .ok_or_else(|| "manager HTTP status is invalid".to_string())?;
    let mut body = response[split + 4..].to_vec();
    let chunked = headers.lines().skip(1).any(|line| {
        line.split_once(':').is_some_and(|(name, value)| {
            name.eq_ignore_ascii_case("transfer-encoding")
                && value.to_ascii_lowercase().contains("chunked")
        })
    });
    if chunked {
        body = decode_chunked(&body)?;
    }
    if !(200..300).contains(&status) {
        return Err(format!(
            "manager returned HTTP {status}: {}",
            truncate_utf8(&String::from_utf8_lossy(&body), 512)
        ));
    }
    Ok(body)
}

fn decode_chunked(body: &[u8]) -> Result<Vec<u8>, String> {
    let mut result = Vec::new();
    let mut offset = 0;
    loop {
        let line_end = body[offset..]
            .windows(2)
            .position(|window| window == b"\r\n")
            .map(|index| offset + index)
            .ok_or_else(|| "invalid chunked response".to_string())?;
        let size_text = std::str::from_utf8(&body[offset..line_end])
            .map_err(|_| "invalid chunk size".to_string())?;
        let size = usize::from_str_radix(size_text.split(';').next().unwrap_or(""), 16)
            .map_err(|_| "invalid chunk size".to_string())?;
        offset = line_end + 2;
        if size == 0 {
            return Ok(result);
        }
        if offset + size + 2 > body.len() {
            return Err("truncated chunked response".to_string());
        }
        result.extend_from_slice(&body[offset..offset + size]);
        offset += size + 2;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_manager_http_url() {
        let endpoint =
            HttpEndpoint::parse("http://192.168.10.46:8087/api/v1/lower-ingest/logs").unwrap();
        assert_eq!(endpoint.host, "192.168.10.46");
        assert_eq!(endpoint.port, 8087);
        assert_eq!(endpoint.path, "/api/v1/lower-ingest/logs");
        assert!(HttpEndpoint::parse("https://manager/logs").is_err());
    }

    #[test]
    fn chunked_ack_is_decoded() {
        let response =
            b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4\r\ntest\r\n0\r\n\r\n";
        assert_eq!(parse_http_response(response).unwrap(), b"test");
    }

    #[test]
    fn overflowing_spool_rebases_retained_events_without_gaps() {
        let mut values = VecDeque::from([
            StoredLogEvent {
                source_id: "s".into(),
                boot_id: "old".into(),
                sequence: 1,
                timestamp: "t".into(),
                level: "INFO".into(),
                component: "c".into(),
                port_id: None,
                device_id: None,
                message: "x".repeat(1024),
            },
            StoredLogEvent {
                source_id: "s".into(),
                boot_id: "old".into(),
                sequence: 2,
                timestamp: "t".into(),
                level: "ERROR".into(),
                component: "c".into(),
                port_id: None,
                device_id: None,
                message: "important".into(),
            },
        ]);
        let mut boot = "old".to_string();
        let mut sequence = 2;
        enforce_spool_limit(&mut values, &mut boot, &mut sequence, 500);
        assert_eq!(values.len(), 1);
        assert_eq!(values[0].level, "ERROR");
        assert_eq!(values[0].sequence, 1);
        assert_eq!(sequence, 1);
        assert_eq!(values[0].boot_id, boot);
    }

    #[test]
    fn extracts_common_serial_context() {
        let (port, device) =
            extract_log_context("poll failed port_id=/dev/ttyWCH0 device_id=13 timeout");
        assert_eq!(port.as_deref(), Some("/dev/ttyWCH0"));
        assert_eq!(device.as_deref(), Some("13"));
    }
}
