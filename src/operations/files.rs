//! Bounded, restart-only configuration and log access for the operations RPC.
//! All names are resource IDs within the two startup directories, never paths.

use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet, VecDeque};
use std::fs::{self, File, Metadata, OpenOptions};
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

use chrono::{DateTime, Utc};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};

use super::OperationError;

const MAX_CONFIG_BYTES: usize = 2 * 1024 * 1024;
const MAX_LOG_SCAN: usize = 256 * 1024 - 2048;
const MAX_LOG_TEXT: usize = 64 * 1024;
// Matching context is a separate, bounded backward read. It never advances the
// returned byte cursor or repeats text already returned by an earlier page.
const MAX_LOG_LOOKBEHIND: usize = 256 * 1024;
const MAX_BACKUPS: usize = 200;
const MAX_VALIDATION_TEXT: usize = 12 * 1024;
const BACKUP_DIRECTORY: &str = ".rcd-operations-backups";
const WRITE_LOCK: &str = ".rcd-operations.lock";
static FILE_COUNTER: AtomicU64 = AtomicU64::new(0);

type OperationResult<T> = Result<T, OperationError>;

pub struct FileRepository {
    config_dir: PathBuf,
    log_dir: PathBuf,
    log_basename: String,
    active_revisions: BTreeMap<String, String>,
    log_generations: HashMap<String, LogGeneration>,
}

struct ConfigFile {
    bytes: Vec<u8>,
    revision: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct FileIdentity(String);

struct LogGeneration {
    token: String,
    identity: FileIdentity,
    size: u64,
    prefix: Vec<u8>,
    anchors: VecDeque<(u64, Vec<u8>)>,
}

#[derive(Default)]
struct Validation {
    errors: Vec<String>,
    warnings: Vec<String>,
    error_bytes: usize,
    warning_bytes: usize,
    errors_truncated: bool,
    warnings_truncated: bool,
}

impl FileRepository {
    pub fn new(config_dir: PathBuf, log_dir: PathBuf, log_basename: String) -> io::Result<Self> {
        if !safe_name(&log_basename) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "非法日志 basename",
            ));
        }
        let config_dir = canonical_directory(&config_dir)?;
        let log_dir = canonical_directory(&log_dir)?;
        let mut active_revisions = BTreeMap::new();
        for name in config_names(&config_dir).map_err(operation_io)? {
            let file = read_config_file(&config_dir, &name).map_err(operation_io)?;
            active_revisions.insert(name, file.revision);
        }
        Ok(Self {
            config_dir,
            log_dir,
            log_basename,
            active_revisions,
            log_generations: HashMap::new(),
        })
    }

    pub fn handle(&mut self, request: &Value) -> OperationResult<Value> {
        match required_string(request, "action")? {
            "list_configs" => self.list_configs(),
            "read_config" => self.read_config(request),
            "validate_config" => self.validate_config(request),
            "write_config" => self.write_config(request),
            "list_logs" => self.list_logs(),
            "read_log" => self.read_log(request),
            _ => Err(OperationError::new("unsupported", "不支持的文件操作")),
        }
    }

    fn config_metadata(&self, name: &str, file: &ConfigFile) -> Value {
        let active = self
            .active_revisions
            .get(name)
            .map(String::as_str)
            .unwrap_or("");
        json!({
            "name": name,
            "kind": if name == "config.toml" { "application" } else if name == "calc.toml" { "calculation" } else { "device_csv" },
            "size_bytes": file.bytes.len(),
            "revision": file.revision,
            "active_revision": active,
            "restart_required": active != file.revision,
        })
    }

    fn list_configs(&self) -> OperationResult<Value> {
        let mut items = Vec::new();
        for name in config_names(&self.config_dir)? {
            let file = read_config_file(&self.config_dir, &name)?;
            items.push(self.config_metadata(&name, &file));
        }
        Ok(json!({ "items": items }))
    }

    fn read_config(&self, request: &Value) -> OperationResult<Value> {
        let name = config_name(request)?;
        let current = read_config_file(&self.config_dir, name)?;
        let mut result = self.config_metadata(name, &current);
        let bytes = if let Some(backup_id) = optional_string(request, "backup_id")? {
            validate_backup_id(backup_id)?;
            let directory = self.backup_directory(name, false)?;
            let backup = read_config_file(&directory, backup_id)?;
            result["viewing_backup_id"] = json!(backup_id);
            result["backup_revision"] = json!(backup.revision);
            backup.bytes
        } else {
            current.bytes
        };
        result["content"] = json!(String::from_utf8(bytes)
            .map_err(|_| OperationError::new("invalid_request", "配置必须为 UTF-8 文本"))?);
        let (backups, truncated) = self.list_backups(name)?;
        result["backups"] = json!(backups);
        result["backups_truncated"] = json!(truncated);
        Ok(result)
    }

    fn config_set(&self) -> OperationResult<BTreeMap<String, ConfigFile>> {
        let mut files = BTreeMap::new();
        for name in config_names(&self.config_dir)? {
            files.insert(name.clone(), read_config_file(&self.config_dir, &name)?);
        }
        Ok(files)
    }

    fn validate_config(&self, request: &Value) -> OperationResult<Value> {
        let name = config_name(request)?;
        let content = config_content(request)?;
        let mut files = self.config_set()?;
        if !files.contains_key(name) {
            return Err(OperationError::new(
                "not_found",
                "配置文件不存在，不支持创建任意配置文件",
            ));
        }
        files.insert(
            name.to_string(),
            config_from_bytes(content.as_bytes().to_vec()),
        );
        let validation = validate_set(&files);
        Ok(json!({
            "valid": validation.errors.is_empty(),
            "errors": validation.errors,
            "warnings": validation.warnings,
            "errors_truncated": validation.errors_truncated,
            "warnings_truncated": validation.warnings_truncated,
            "restart_required": true,
        }))
    }

    fn write_config(&self, request: &Value) -> OperationResult<Value> {
        let name = config_name(request)?;
        let content = config_content(request)?;
        let expected = required_string(request, "expected_revision")?;
        if expected.len() != 64 || !expected.bytes().all(|byte| byte.is_ascii_hexdigit()) {
            return Err(OperationError::new(
                "invalid_request",
                "expected_revision 必须为内容 SHA256",
            ));
        }
        let _lock = config_write_lock(&self.config_dir)?;
        let original = self.config_set()?;
        let current = original
            .get(name)
            .ok_or_else(|| OperationError::new("not_found", "配置文件不存在"))?;
        if current.revision != expected {
            return Err(OperationError::new("conflict", "配置已被修改，请重新读取")
                .with_revision(current.revision.clone()));
        }
        let mut candidate = BTreeMap::new();
        for (file_name, file) in &original {
            candidate.insert(
                file_name.clone(),
                config_from_bytes(if file_name == name {
                    content.as_bytes().to_vec()
                } else {
                    file.bytes.clone()
                }),
            );
        }
        let validation = validate_set(&candidate);
        if !validation.errors.is_empty() {
            return Err(OperationError::new(
                "invalid_request",
                validation.errors.join("\n"),
            ));
        }
        self.ensure_config_set_unchanged(&original)?;
        if content.as_bytes() == current.bytes.as_slice() {
            return self.read_config(&json!({ "name": name }));
        }
        self.backup(name, &current.bytes)?;
        // A local editor need not honour our process lock. Recheck all files after
        // validation and backup, immediately before the atomic replacement.
        self.ensure_config_set_unchanged(&original)?;
        atomic_replace(&self.config_dir, name, content.as_bytes())?;
        self.read_config(&json!({ "name": name }))
    }

    fn ensure_config_set_unchanged(
        &self,
        expected: &BTreeMap<String, ConfigFile>,
    ) -> OperationResult<()> {
        let current = self.config_set()?;
        if current.len() != expected.len() {
            return Err(OperationError::new(
                "conflict",
                "校验期间配置文件集合发生变化",
            ));
        }
        for (name, file) in &current {
            if expected.get(name).map(|entry| &entry.revision) != Some(&file.revision) {
                return Err(OperationError::new(
                    "conflict",
                    format!("校验期间配置 {name} 发生变化"),
                )
                .with_revision(file.revision.clone()));
            }
        }
        Ok(())
    }

    fn backup_directory(&self, name: &str, create: bool) -> OperationResult<PathBuf> {
        let root = child_directory(&self.config_dir, BACKUP_DIRECTORY, create)?;
        child_directory(&root, name, create)
    }

    fn backup(&self, name: &str, bytes: &[u8]) -> OperationResult<()> {
        let directory = self.backup_directory(name, true)?;
        let id = format!("{}-{}", unique_id(), digest(bytes));
        let pending = directory.join(format!(".pending-{id}.tmp"));
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&pending)
            .map_err(io_error)?;
        let mut temporary = TemporaryFile(Some(pending.clone()));
        file.write_all(bytes).map_err(io_error)?;
        file.sync_all().map_err(io_error)?;
        drop(file);
        // A failed write or interrupted process must not expose a partial backup
        // under a valid resource ID. Pending files are never listed or readable.
        fs::rename(&pending, directory.join(&id)).map_err(io_error)?;
        temporary.0 = None;
        sync_directory(&directory).map_err(io_error)
    }

    fn list_backups(&self, name: &str) -> OperationResult<(Vec<Value>, bool)> {
        let directory = match self.backup_directory(name, false) {
            Ok(directory) => directory,
            Err(error) if error.code == "not_found" => return Ok((Vec::new(), false)),
            Err(error) => return Err(error),
        };
        let mut newest = BTreeSet::new();
        let mut truncated = false;
        for entry in fs::read_dir(&directory).map_err(io_error)? {
            let entry = entry.map_err(io_error)?;
            let Some(id) = entry.file_name().to_str().map(str::to_string) else {
                continue;
            };
            if validate_backup_id(&id).is_err() || !entry.file_type().map_err(io_error)?.is_file() {
                continue;
            }
            newest.insert(id);
            if newest.len() > MAX_BACKUPS {
                newest.pop_first();
                truncated = true;
            }
        }
        // Directory enumeration is necessary to find the newest IDs, but memory,
        // metadata reads and the response are bounded; older backups stay on disk.
        let mut items = Vec::with_capacity(newest.len());
        for id in newest.into_iter().rev() {
            let (_, metadata) = open_regular(&directory, &id, false)?;
            items.push(json!({
                "id": id, "created_at": timestamp(metadata.modified().unwrap_or(UNIX_EPOCH)),
                "size_bytes": metadata.len(),
            }));
        }
        Ok((items, truncated))
    }
}

fn required_string<'a>(request: &'a Value, key: &str) -> OperationResult<&'a str> {
    request
        .get(key)
        .and_then(Value::as_str)
        .ok_or_else(|| OperationError::new("invalid_request", format!("{key} 必须为字符串")))
}

fn optional_string<'a>(request: &'a Value, key: &str) -> OperationResult<Option<&'a str>> {
    match request.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => value
            .as_str()
            .map(Some)
            .ok_or_else(|| OperationError::new("invalid_request", format!("{key} 必须为字符串"))),
    }
}

fn safe_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 255
        && name != "."
        && name != ".."
        && !name.ends_with(['.', ' '])
        && !name
            .chars()
            .any(|ch| ch.is_control() || matches!(ch, '/' | '\\' | ':'))
}

fn allowed_config(name: &str) -> bool {
    safe_name(name)
        && (name == "config.toml"
            || name == "calc.toml"
            || (name.starts_with("rcd") && name.ends_with(".csv")))
}

fn config_name(request: &Value) -> OperationResult<&str> {
    let name = required_string(request, "name")?;
    if !allowed_config(name) {
        return Err(OperationError::new(
            "invalid_request",
            "仅允许已有 config.toml、calc.toml、rcd*.csv 文件",
        ));
    }
    Ok(name)
}

fn config_content(request: &Value) -> OperationResult<&str> {
    let content = required_string(request, "content")?;
    if content.len() > MAX_CONFIG_BYTES {
        return Err(OperationError::new("too_large", "配置文件最大为 2 MiB"));
    }
    Ok(content)
}

fn validate_backup_id(id: &str) -> OperationResult<()> {
    if id.len() != 114 {
        return Err(OperationError::new("invalid_request", "非法备份资源 ID"));
    }
    let parts: Vec<_> = id.split('-').collect();
    if parts.len() != 3
        || parts.iter().map(|part| part.len()).collect::<Vec<_>>() != [32, 16, 64]
        || !parts
            .iter()
            .all(|part| part.bytes().all(|byte| byte.is_ascii_hexdigit()))
    {
        return Err(OperationError::new("invalid_request", "非法备份资源 ID"));
    }
    Ok(())
}

fn canonical_directory(path: &Path) -> io::Result<PathBuf> {
    let metadata = fs::symlink_metadata(path)?;
    if is_link(&metadata) || !metadata.is_dir() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "目录必须为非链接的实际目录",
        ));
    }
    fs::canonicalize(path)
}

fn child_directory(parent: &Path, name: &str, create: bool) -> OperationResult<PathBuf> {
    canonical_directory(parent).map_err(io_error)?;
    let child = parent.join(name);
    if create {
        match fs::create_dir(&child) {
            Ok(()) => sync_directory(parent).map_err(io_error)?,
            Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {}
            Err(error) => return Err(io_error(error)),
        }
    }
    let canonical = canonical_directory(&child).map_err(io_error)?;
    if canonical.parent() != Some(parent) {
        return Err(OperationError::new("invalid_request", "目录超出配置根目录"));
    }
    Ok(canonical)
}

fn config_names(directory: &Path) -> OperationResult<Vec<String>> {
    canonical_directory(directory).map_err(io_error)?;
    let mut names = Vec::new();
    for entry in fs::read_dir(directory).map_err(io_error)? {
        let entry = entry.map_err(io_error)?;
        let Some(name) = entry.file_name().to_str().map(str::to_string) else {
            continue;
        };
        if allowed_config(&name) {
            check_regular(&fs::symlink_metadata(entry.path()).map_err(io_error)?)?;
            names.push(name);
        }
    }
    names.sort();
    Ok(names)
}

fn is_link(metadata: &Metadata) -> bool {
    if metadata.file_type().is_symlink() {
        return true;
    }
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt;
        if metadata.file_attributes() & 0x400 != 0 {
            return true;
        }
    }
    false
}

fn check_regular(metadata: &Metadata) -> OperationResult<()> {
    if is_link(metadata) || !metadata.is_file() {
        return Err(OperationError::new(
            "invalid_request",
            "只允许常规文件，不允许链接或特殊文件",
        ));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        if metadata.nlink() != 1 {
            return Err(OperationError::new("invalid_request", "不允许硬链接文件"));
        }
    }
    Ok(())
}

fn identity(metadata: &Metadata) -> FileIdentity {
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        FileIdentity(format!("{}:{}", metadata.dev(), metadata.ino()))
    }
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt;
        FileIdentity(format!(
            "{}:{}",
            metadata.creation_time(),
            metadata.file_attributes()
        ))
    }
    #[cfg(not(any(unix, windows)))]
    FileIdentity(format!("{:?}", metadata.created()))
}

fn open_regular(directory: &Path, name: &str, write: bool) -> OperationResult<(File, Metadata)> {
    if !safe_name(name) {
        return Err(OperationError::new("invalid_request", "非法文件名"));
    }
    canonical_directory(directory).map_err(io_error)?;
    let path = directory.join(name);
    let before = fs::symlink_metadata(&path).map_err(io_error)?;
    check_regular(&before)?;
    let file = OpenOptions::new()
        .read(true)
        .write(write)
        .open(&path)
        .map_err(io_error)?;
    let metadata = file.metadata().map_err(io_error)?;
    check_regular(&metadata)?;
    let after = fs::symlink_metadata(&path).map_err(io_error)?;
    check_regular(&after)?;
    if identity(&before) != identity(&metadata)
        || identity(&metadata) != identity(&after)
        || fs::canonicalize(&path).map_err(io_error)?.parent() != Some(directory)
    {
        return Err(OperationError::new("conflict", "文件在打开期间发生替换"));
    }
    Ok((file, metadata))
}

fn read_config_file(directory: &Path, name: &str) -> OperationResult<ConfigFile> {
    let (mut file, before) = open_regular(directory, name, false)?;
    if before.len() > MAX_CONFIG_BYTES as u64 {
        return Err(OperationError::new("too_large", "配置文件最大为 2 MiB"));
    }
    let mut bytes = Vec::new();
    (&mut file)
        .take(MAX_CONFIG_BYTES as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(io_error)?;
    if bytes.len() > MAX_CONFIG_BYTES {
        return Err(OperationError::new("too_large", "配置文件最大为 2 MiB"));
    }
    let after = file.metadata().map_err(io_error)?;
    let path_after = fs::symlink_metadata(directory.join(name)).map_err(io_error)?;
    check_regular(&path_after)?;
    if before.len() != after.len()
        || before.modified().ok() != after.modified().ok()
        || identity(&after) != identity(&path_after)
        || bytes.len() as u64 != after.len()
    {
        return Err(OperationError::new("conflict", "文件在读取期间发生变化"));
    }
    Ok(config_from_bytes(bytes))
}

fn config_from_bytes(bytes: Vec<u8>) -> ConfigFile {
    ConfigFile {
        revision: digest(&bytes),
        bytes,
    }
}

fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn unique_id() -> String {
    format!(
        "{:032x}-{:016x}",
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos(),
        FILE_COUNTER.fetch_add(1, Ordering::Relaxed)
    )
}

fn config_write_lock(directory: &Path) -> OperationResult<File> {
    canonical_directory(directory).map_err(io_error)?;
    let file = match OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .open(directory.join(WRITE_LOCK))
    {
        Ok(file) => file,
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
            open_regular(directory, WRITE_LOCK, true)?.0
        }
        Err(error) => return Err(io_error(error)),
    };
    fs2::FileExt::try_lock_exclusive(&file).map_err(|error| {
        if error.kind() == io::ErrorKind::WouldBlock {
            OperationError::new("conflict", "另一个配置保存正在进行")
        } else {
            io_error(error)
        }
    })?;
    Ok(file)
}

struct TemporaryFile(Option<PathBuf>);
impl Drop for TemporaryFile {
    fn drop(&mut self) {
        if let Some(path) = &self.0 {
            let _ = fs::remove_file(path);
        }
    }
}

fn atomic_replace(directory: &Path, name: &str, bytes: &[u8]) -> OperationResult<()> {
    let (_, metadata) = open_regular(directory, name, false)?;
    let path = directory.join(format!(".rcd-operations-{}.tmp", unique_id()));
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&path)
        .map_err(io_error)?;
    let mut temporary = TemporaryFile(Some(path.clone()));
    file.set_permissions(metadata.permissions())
        .map_err(io_error)?;
    file.write_all(bytes).map_err(io_error)?;
    file.sync_all().map_err(io_error)?;
    drop(file);
    let current = fs::symlink_metadata(directory.join(name)).map_err(io_error)?;
    check_regular(&current)?;
    if identity(&current) != identity(&metadata)
        || current.len() != metadata.len()
        || current.modified().ok() != metadata.modified().ok()
    {
        return Err(OperationError::new("conflict", "保存期间目标配置已被替换"));
    }
    // Never remove the destination first: rename is the atomic commit point.
    fs::rename(&path, directory.join(name)).map_err(io_error)?;
    temporary.0 = None;
    sync_directory(directory).map_err(io_error)
}

fn sync_directory(path: &Path) -> io::Result<()> {
    #[cfg(unix)]
    File::open(path)?.sync_all()?;
    #[cfg(not(unix))]
    let _ = path;
    Ok(())
}

fn timestamp(time: SystemTime) -> String {
    DateTime::<Utc>::from(time).to_rfc3339()
}

fn io_error(error: io::Error) -> OperationError {
    let code = match error.kind() {
        io::ErrorKind::NotFound => "not_found",
        io::ErrorKind::InvalidInput | io::ErrorKind::InvalidData => "invalid_request",
        _ => "io_error",
    };
    OperationError::new(code, error.to_string())
}

fn operation_io(error: OperationError) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, error.message)
}

impl Validation {
    fn error(&mut self, message: impl Into<String>) {
        bounded_validation_message(
            &mut self.errors,
            &mut self.error_bytes,
            &mut self.errors_truncated,
            message.into(),
        );
    }

    fn warning(&mut self, message: impl Into<String>) {
        bounded_validation_message(
            &mut self.warnings,
            &mut self.warning_bytes,
            &mut self.warnings_truncated,
            message.into(),
        );
    }
}

fn bounded_validation_message(
    items: &mut Vec<String>,
    total: &mut usize,
    truncated: &mut bool,
    message: String,
) {
    const NOTICE: &str = "其余校验信息已省略，请先修复以上问题后再次校验。";
    const SUFFIX: &str = "…（此条信息已截断）";
    if items.last().is_some_and(|item| item == NOTICE) {
        return;
    }
    let message = if message.len() > 1024 {
        *truncated = true;
        format!("{}{}", utf8_prefix(&message, 1024 - SUFFIX.len()), SUFFIX)
    } else {
        message
    };
    if items.len() >= 100 || *total + message.len() + 1 + NOTICE.len() >= MAX_VALIDATION_TEXT {
        items.push(NOTICE.to_string());
        *total += NOTICE.len() + 1;
        *truncated = true;
        return;
    }
    *total += message.len() + 1;
    items.push(message);
}

fn utf8_prefix(text: &str, limit: usize) -> &str {
    let mut end = text.len().min(limit);
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    &text[..end]
}

fn validate_set(files: &BTreeMap<String, ConfigFile>) -> Validation {
    use crate::calc_engine::config::{normalize_data_type, CalcConfig};
    use crate::calc_engine::{parser, tokenizer};

    let mut result = Validation::default();
    let mut texts = BTreeMap::new();
    for (name, file) in files {
        match std::str::from_utf8(&file.bytes) {
            Ok(content) => {
                texts.insert(name.as_str(), content);
            }
            Err(_) => result.error(format!("{name}: 配置必须为 UTF-8 文本")),
        }
    }
    match texts.get("config.toml") {
        Some(content) => validate_application(content, &mut result),
        None => result.error("缺少 config.toml，无法校验完整配置集"),
    }

    let mut point_owners = BTreeMap::<String, String>::new();
    let mut ports = BTreeMap::<String, (u32, bool, String)>::new();
    let mut devices = HashSet::new();
    for (name, content) in &texts {
        if !name.ends_with(".csv") {
            continue;
        }
        let (config, records) = match crate::csv_parser::validate_csv_content(content) {
            Ok(parsed) => parsed,
            Err(error) => {
                result.error(format!("{name}: {error}"));
                continue;
            }
        };
        if let Some((baud_rate, special, previous)) = ports.get(&config.com) {
            if *baud_rate != config.baud_rate || *special != config.is_special {
                result.error(format!(
                    "{name}: 串口 {} 的 baud_rate/is_special 与 {previous} 冲突",
                    config.com
                ));
            }
        } else {
            ports.insert(
                config.com.clone(),
                (config.baud_rate, config.is_special, name.to_string()),
            );
        }
        if !devices.insert((config.com.clone(), config.device_id)) {
            result.error(format!(
                "{name}: 设备重复 {}:{}",
                config.com, config.device_id
            ));
        }
        if records.is_empty() {
            result.warning(format!("{name}: 此设备没有采集点"));
        }
        for record in records {
            if let Some(previous) = point_owners.insert(record.kks.clone(), name.to_string()) {
                result.error(format!(
                    "{name}: KKS 重复 {}（已存在于 {previous}）",
                    record.kks
                ));
            }
        }
    }

    let mut reserved = BTreeMap::from([
        ("Devices".to_string(), "系统对象".to_string()),
        ("Methods".to_string(), "系统对象".to_string()),
    ]);
    for port in ports.keys() {
        // Keep identical to server::opcua::opc_port_stem without exposing server internals.
        let stem = port
            .rsplit(['/', '\\'])
            .find(|part| !part.is_empty())
            .unwrap_or(port);
        if stem.trim().is_empty() {
            result.error(format!("串口 {port:?} 的 OPC 名称为空"));
        }
        for suffix in [
            "_Send",
            "_SendCommand",
            "_SendCommand_Input",
            "_SendCommand_Output",
        ] {
            let node = format!("{stem}{suffix}");
            if let Some(previous) = reserved.insert(node.clone(), port.clone()) {
                result.error(format!("OPC 节点重名 {node}: {port} 与 {previous}"));
            }
        }
    }
    for (name, owner) in &point_owners {
        if reserved.contains_key(name) {
            result.error(format!("{owner}: KKS {name} 与系统/命令节点冲突"));
        }
    }

    let Some(content) = texts.get("calc.toml") else {
        return result;
    };
    let raw: toml::Value = match toml::from_str(content) {
        Ok(value) => value,
        Err(error) => {
            result.error(format!("calc.toml: {error}"));
            return result;
        }
    };
    if let Some(table) = raw.as_table() {
        for key in table.keys().filter(|key| key.as_str() != "rules") {
            result.error(format!("calc.toml: 未知字段 {key}，公式应使用 [[rules]]"));
        }
    }
    if let Some(rules) = raw.get("rules").and_then(toml::Value::as_array) {
        for (index, rule) in rules.iter().enumerate() {
            if let Some(table) = rule.as_table() {
                for key in table.keys() {
                    if ![
                        "kks_calc",
                        "data_type",
                        "expression",
                        "min_limit",
                        "max_limit",
                        "default_value",
                    ]
                    .contains(&key.as_str())
                    {
                        result.error(format!("calc.toml rules[{}]: 未知字段 {key}", index + 1));
                    }
                }
            }
        }
    }
    let config: CalcConfig = match toml::from_str(content) {
        Ok(config) => config,
        Err(error) => {
            result.error(format!("calc.toml: {error}"));
            return result;
        }
    };
    let mut outputs = HashMap::new();
    for (index, rule) in config.rules.iter().enumerate() {
        if rule.kks_calc.trim().is_empty() || rule.kks_calc.chars().any(char::is_control) {
            result.error(format!(
                "calc.toml rules[{}]: kks_calc 为空或含控制字符",
                index + 1
            ));
        }
        if outputs.insert(rule.kks_calc.clone(), index).is_some() {
            result.error(format!("calc.toml: 计算点重复 {}", rule.kks_calc));
        }
        if point_owners.contains_key(&rule.kks_calc) || reserved.contains_key(&rule.kks_calc) {
            result.error(format!(
                "calc.toml: 计算点 {} 与采集点/系统节点冲突",
                rule.kks_calc
            ));
        }
    }
    let mut edges = vec![Vec::new(); config.rules.len()];
    let mut degrees = vec![0usize; config.rules.len()];
    for (index, rule) in config.rules.iter().enumerate() {
        let name = &rule.kks_calc;
        if normalize_data_type(&rule.data_type).is_none() {
            result.error(format!(
                "calc.toml {name}: 不支持的数据类型 {}",
                rule.data_type
            ));
        }
        if !rule.min_limit.is_finite()
            || !rule.max_limit.is_finite()
            || !rule.default_value.is_finite()
            || rule.min_limit > rule.max_limit
        {
            result.error(format!(
                "calc.toml {name}: 上下限/默认值必须为有限数，且 min_limit <= max_limit"
            ));
        }
        if rule.expression.len() > 16 * 1024 {
            result.error(format!("calc.toml {name}: 单个表达式最大 16 KiB"));
            continue;
        }
        let tokens = match tokenizer::tokenize(&rule.expression) {
            Ok(tokens) => tokens,
            Err(error) => {
                result.error(format!("calc.toml {name}: {error}"));
                continue;
            }
        };
        // The runtime parser and AST traversal are recursive. Bound validation
        // before parsing so an accidentally pasted deeply nested expression cannot
        // overflow the operations worker stack.
        if tokens.len() > 512 {
            result.error(format!("calc.toml {name}: 单个表达式最多 512 个词法单元"));
            continue;
        }
        let ast = match parser::parse(tokens) {
            Ok(ast) => ast,
            Err(error) => {
                result.error(format!("calc.toml {name}: {error}"));
                continue;
            }
        };
        for dependency in parser::extract_variables(&ast) {
            if let Some(&dependency_index) = outputs.get(&dependency) {
                edges[dependency_index].push(index);
                degrees[index] += 1;
            } else if !point_owners.contains_key(&dependency) {
                result.error(format!("calc.toml {name}: 引用不存在的点位 {dependency}"));
            }
        }
    }
    let mut queue: VecDeque<_> = degrees
        .iter()
        .enumerate()
        .filter_map(|(index, degree)| (*degree == 0).then_some(index))
        .collect();
    while let Some(index) = queue.pop_front() {
        for &next in &edges[index] {
            degrees[next] -= 1;
            if degrees[next] == 0 {
                queue.push_back(next);
            }
        }
    }
    for (index, degree) in degrees.iter().enumerate() {
        if *degree != 0 {
            result.error(format!(
                "calc.toml: 计算点 {} 存在循环依赖或依赖循环节点",
                config.rules[index].kks_calc
            ));
        }
    }
    result
}

fn validate_application(content: &str, result: &mut Validation) {
    let config: crate::config::Config = match toml::from_str(content) {
        Ok(config) => config,
        Err(error) => {
            result.error(format!("config.toml: {error}"));
            return;
        }
    };
    if let Err(error) = config.server.opcua_advertised_host() {
        result.error(format!("config.toml server: {error}"));
    }
    if config.serial._baud_rate == 0
        || config.settings.timeout == 0
        || config.settings.poll_interval_ms == 0
        || config.settings.command_retry_count == 0
    {
        result.error(
            "config.toml: baud_rate、timeout、poll_interval_ms、command_retry_count 必须大于 0",
        );
    }
    if config.settings.query_retry_count > 1 {
        result.error("config.toml: query_retry_count 只能是 0 或 1");
    }
    if config.settings.parity_switch_delay_ms > 100
        || config.settings.transmit_settle_guard_ms > 100
    {
        result.error("config.toml: parity_switch_delay_ms、transmit_settle_guard_ms 不能超过 100");
    }
    let logging = &config.logging;
    if let Err(error) = flexi_logger::LogSpecification::parse(&logging.level) {
        result.error(format!("config.toml logging.level: {error}"));
    }
    if !safe_name(&logging.basename) {
        result.error("config.toml logging.basename 必须为单个非空文件名");
    }
    if ![
        "day", "daily", "", "hour", "hourly", "minute", "second", "none", "off",
    ]
    .contains(&logging.rotate_age.trim().to_ascii_lowercase().as_str())
    {
        result.error("config.toml logging.rotate_age 无效");
    }
    if ![
        "direct",
        "buffer_dont_flush",
        "buffer-dont-flush",
        "buffer_and_flush",
        "buffer-and-flush",
        "",
    ]
    .contains(&logging.write_mode.trim().to_ascii_lowercase().as_str())
    {
        result.error("config.toml logging.write_mode 无效");
    }
    if let Some(value) = &logging.duplicate_to_stderr {
        if ![
            "none", "off", "error", "warn", "warning", "info", "debug", "trace", "all",
        ]
        .contains(&value.trim().to_ascii_lowercase().as_str())
        {
            result.error("config.toml logging.duplicate_to_stderr 无效");
        }
    }
    if logging.rotate_size_bytes == Some(0) {
        result.error("config.toml logging.rotate_size_bytes 必须大于 0");
    }
}

impl FileRepository {
    fn allowed_log(&self, name: &str) -> bool {
        safe_name(name) && name.starts_with(&self.log_basename) && name.ends_with(".log")
    }

    fn list_logs(&mut self) -> OperationResult<Value> {
        canonical_directory(&self.log_dir).map_err(io_error)?;
        let mut names = Vec::new();
        for entry in fs::read_dir(&self.log_dir).map_err(io_error)? {
            let entry = entry.map_err(io_error)?;
            let Some(name) = entry.file_name().to_str().map(str::to_string) else {
                continue;
            };
            if self.allowed_log(&name) {
                let metadata = fs::symlink_metadata(entry.path()).map_err(io_error)?;
                // A rotated log may disappear between directory iteration and open.
                if check_regular(&metadata).is_ok() {
                    names.push(name);
                }
            }
        }
        names.sort();
        let mut items = Vec::new();
        for name in names {
            let (mut file, metadata) = match open_regular(&self.log_dir, &name, false) {
                Ok(file) => file,
                Err(error) if error.code == "not_found" => continue,
                Err(error) => return Err(error),
            };
            let generation = self.observe_log(&name, &mut file, &metadata)?;
            items.push(json!({
                "name": name, "size_bytes": metadata.len(),
                "modified_at": timestamp(metadata.modified().unwrap_or(UNIX_EPOCH)),
                "generation": generation,
            }));
        }
        items.sort_by(|a, b| {
            b["modified_at"]
                .as_str()
                .cmp(&a["modified_at"].as_str())
                .then_with(|| a["name"].as_str().cmp(&b["name"].as_str()))
        });
        Ok(json!({ "items": items }))
    }

    fn observe_log(
        &mut self,
        name: &str,
        file: &mut File,
        metadata: &Metadata,
    ) -> OperationResult<String> {
        let current_identity = identity(metadata);
        let prefix = read_at(file, 0, (metadata.len().min(256)) as usize)?;
        let mut changed = false;
        if let Some(previous) = self.log_generations.get(name) {
            changed = previous.identity != current_identity
                || metadata.len() < previous.size
                || !prefix.starts_with(&previous.prefix);
            if !changed {
                for (offset, bytes) in &previous.anchors {
                    if read_at(file, *offset, bytes.len())? != *bytes {
                        changed = true;
                        break;
                    }
                }
            }
        }
        if changed || !self.log_generations.contains_key(name) {
            self.log_generations.insert(
                name.to_string(),
                LogGeneration {
                    token: digest(
                        format!("{:?}:{}:{}", current_identity, name, unique_id()).as_bytes(),
                    ),
                    identity: current_identity,
                    size: metadata.len(),
                    prefix,
                    anchors: VecDeque::new(),
                },
            );
        } else if let Some(previous) = self.log_generations.get_mut(name) {
            previous.size = metadata.len();
            // Grow a short/empty prefix as the log starts, without changing its token.
            previous.prefix = prefix;
        }
        Ok(self.log_generations[name].token.clone())
    }

    fn read_log(&mut self, request: &Value) -> OperationResult<Value> {
        let name = required_string(request, "name")?;
        if !self.allowed_log(name) {
            return Err(OperationError::new(
                "invalid_request",
                "只能读取本程序 basename 开头的 .log 文件",
            ));
        }
        let expected = optional_string(request, "generation")?;
        let requested_offset = optional_u64(request, "offset")?.unwrap_or(0);
        let limit = optional_u64(request, "limit")?.unwrap_or(MAX_LOG_TEXT as u64);
        if !(4..=MAX_LOG_TEXT as u64).contains(&limit) {
            return Err(OperationError::new(
                "invalid_request",
                "limit 必须在 4..=65536 字节之间",
            ));
        }
        let query = optional_string(request, "query")?.unwrap_or("");
        if query.len() > 4096 {
            return Err(OperationError::new("too_large", "日志查询最大 4096 字节"));
        }
        let level = optional_string(request, "level")?
            .unwrap_or("")
            .trim()
            .to_ascii_uppercase();
        if !["", "ALL", "ERROR", "WARN", "INFO", "DEBUG", "TRACE"].contains(&level.as_str()) {
            return Err(OperationError::new("invalid_request", "无效日志 level"));
        }
        let tail = match request.get("tail") {
            None | Some(Value::Null) => false,
            Some(value) => value
                .as_bool()
                .ok_or_else(|| OperationError::new("invalid_request", "tail 必须为布尔值"))?,
        };
        let (mut file, before) = match open_regular(&self.log_dir, name, false) {
            Ok(file) => file,
            Err(error) if expected.is_some() && error.code == "not_found" => {
                return Err(OperationError::new(
                    "conflict",
                    "日志已轮转或删除，请重新列出日志",
                ));
            }
            Err(error) => return Err(error),
        };
        let generation = self.observe_log(name, &mut file, &before)?;
        if expected.is_some_and(|expected| expected != generation) {
            return Err(
                OperationError::new("conflict", "日志代次已变化，请从新文件重新读取")
                    .with_revision(generation),
            );
        }
        let mut offset = if tail {
            before.len().saturating_sub(limit)
        } else {
            requested_offset
        };
        if offset > before.len() {
            return Err(
                OperationError::new("conflict", "日志已截断，offset 超出文件大小")
                    .with_revision(generation),
            );
        }
        let tail_fragment = tail && offset > 0 && read_at(&mut file, offset - 1, 1)? != b"\n";
        let count = (before.len() - offset).min(MAX_LOG_SCAN as u64) as usize;
        let mut bytes = read_at(&mut file, offset, count)?;
        let scanned_end = offset + bytes.len() as u64;
        if offset > 0 {
            let prefix = bytes
                .iter()
                .take(3)
                .take_while(|byte| **byte & 0xc0 == 0x80)
                .count();
            if prefix != 0 {
                bytes.drain(..prefix);
                offset += prefix as u64;
            }
            if tail_fragment {
                // Tail starts at a complete line when one exists in this window.
                if let Some(end) = bytes.iter().position(|byte| *byte == b'\n') {
                    if end + 1 < bytes.len() {
                        bytes.drain(..=end);
                        offset += end as u64 + 1;
                    }
                }
            }
        }
        let more_on_disk = scanned_end < before.len();
        if more_on_disk {
            if let Some(last_newline) = bytes.iter().rposition(|byte| *byte == b'\n') {
                bytes.truncate(last_newline + 1);
            } else {
                bytes.truncate(complete_utf8_end(&bytes));
            }
        }
        let filtering = !query.is_empty() || (!level.is_empty() && level != "ALL");
        let line_prefix = if filtering && !bytes.is_empty() {
            read_log_line_prefix(&mut file, offset)?
        } else {
            Vec::new()
        };
        if more_on_disk && !bytes.contains(&b'\n') && !query.is_empty() {
            // A keyword might occur beyond this unfinished line's bounded scan.
            // Do not discard its prefix now and later report only its tail as a
            // match. The caller can explicitly remove filters for byte paging.
            let context = [line_prefix.as_slice(), bytes.as_slice()].concat();
            let text = String::from_utf8_lossy(&context);
            if log_level_matches(&text, &level) && !text.contains(query) {
                return Err(OperationError::new(
                    "invalid_request",
                    "日志行超过单页筛选扫描上限，无法确定整行是否匹配；请清除筛选后按字节分页读取",
                ));
            }
        }
        let (text, consumed, output_limited) =
            filter_log_page(&bytes, &line_prefix, query, &level, limit as usize);
        let next_offset = offset + consumed as u64;

        let after = file.metadata().map_err(io_error)?;
        let path_after = fs::symlink_metadata(self.log_dir.join(name)).map_err(|error| {
            if error.kind() == io::ErrorKind::NotFound {
                OperationError::new("conflict", "日志在读取期间轮转")
            } else {
                io_error(error)
            }
        })?;
        check_regular(&path_after)?;
        if identity(&path_after) != identity(&before) || after.len() < before.len() {
            return Err(OperationError::new("conflict", "日志在读取期间轮转或截断"));
        }
        if self.observe_log(name, &mut file, &after)? != generation {
            return Err(OperationError::new(
                "conflict",
                "日志在读取期间内容发生替换",
            ));
        }
        if consumed != 0 {
            let anchor_offset = next_offset.saturating_sub(64).max(offset);
            let anchor = bytes[(anchor_offset - offset) as usize..consumed].to_vec();
            if read_at(
                &mut file,
                anchor_offset,
                (next_offset - anchor_offset) as usize,
            )? != anchor
            {
                return Err(OperationError::new(
                    "conflict",
                    "日志在读取期间内容发生变化",
                ));
            }
            if let Some(state) = self.log_generations.get_mut(name) {
                state.anchors.push_back((anchor_offset, anchor));
                while state.anchors.len() > 4 {
                    state.anchors.pop_front();
                }
            }
        }
        Ok(json!({
            "name": name, "generation": generation, "offset": offset, "next_offset": next_offset,
            "size_bytes": after.len(), "eof": next_offset >= after.len(),
            "truncated": output_limited || more_on_disk || (tail && offset != 0), "text": text,
        }))
    }
}

fn optional_u64(request: &Value, key: &str) -> OperationResult<Option<u64>> {
    match request.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(value) => value
            .as_u64()
            .map(Some)
            .ok_or_else(|| OperationError::new("invalid_request", format!("{key} 必须为非负整数"))),
    }
}

fn read_at(file: &mut File, offset: u64, count: usize) -> OperationResult<Vec<u8>> {
    file.seek(SeekFrom::Start(offset)).map_err(io_error)?;
    let mut bytes = Vec::with_capacity(count);
    file.take(count as u64)
        .read_to_end(&mut bytes)
        .map_err(io_error)?;
    Ok(bytes)
}

fn read_log_line_prefix(file: &mut File, offset: u64) -> OperationResult<Vec<u8>> {
    if offset == 0 {
        return Ok(Vec::new());
    }
    // Most pages begin on a newline boundary and need just this one-byte check.
    let last = read_at(file, offset - 1, 1)?;
    if last.len() != 1 {
        return Err(OperationError::new("conflict", "日志在回溯期间被截断"));
    }
    if last == b"\n" {
        return Ok(Vec::new());
    }
    let mut chunks = vec![last];
    let mut cursor = offset - 1;
    let mut scanned = 1;
    while cursor > 0 && scanned < MAX_LOG_LOOKBEHIND {
        let count = (cursor.min(4096)).min((MAX_LOG_LOOKBEHIND - scanned) as u64) as usize;
        cursor -= count as u64;
        let chunk = read_at(file, cursor, count)?;
        if chunk.len() != count {
            return Err(OperationError::new("conflict", "日志在回溯期间被截断"));
        }
        scanned += count;
        if let Some(newline) = chunk.iter().rposition(|byte| *byte == b'\n') {
            let mut prefix = chunk[newline + 1..].to_vec();
            for suffix in chunks.into_iter().rev() {
                prefix.extend_from_slice(&suffix);
            }
            return Ok(prefix);
        }
        chunks.push(chunk);
    }
    if cursor != 0 {
        return Err(OperationError::new(
            "invalid_request",
            "日志行首超过 256 KiB 回溯上限，不能可靠延续级别/关键词筛选；请清除筛选后按字节分页读取",
        ));
    }
    let mut prefix = Vec::with_capacity(scanned);
    for suffix in chunks.into_iter().rev() {
        prefix.extend_from_slice(&suffix);
    }
    Ok(prefix)
}

// Exclude only an incomplete trailing UTF-8 sequence; genuinely invalid bytes
// remain visible as replacement characters instead of causing a stuck cursor.
fn complete_utf8_end(bytes: &[u8]) -> usize {
    let start = bytes.len().saturating_sub(4);
    for index in (start..bytes.len()).rev() {
        let width = match bytes[index] {
            0xc2..=0xdf => 2,
            0xe0..=0xef => 3,
            0xf0..=0xf4 => 4,
            0x80..=0xbf => continue,
            _ => return bytes.len(),
        };
        return if index + width > bytes.len() {
            index
        } else {
            bytes.len()
        };
    }
    bytes.len()
}

fn bounded_text(bytes: &[u8], limit: usize) -> (String, usize) {
    let mut count = bytes.len().min(limit);
    if count < bytes.len() {
        count = complete_utf8_end(&bytes[..count]);
    }
    if let Ok(text) = std::str::from_utf8(&bytes[..count]) {
        return (text.to_string(), count);
    }
    // One invalid byte can expand to the three-byte replacement character.
    // Reserve that space before conversion so malformed logs stay bounded too.
    count = bytes.len().min(limit / 3);
    if count < bytes.len() {
        count = complete_utf8_end(&bytes[..count]);
    }
    if count == 0 && !bytes.is_empty() {
        let width = match bytes[0] {
            0xc2..=0xdf => 2,
            0xe0..=0xef => 3,
            0xf0..=0xf4 => 4,
            _ => 1,
        };
        if width <= bytes.len() && width <= limit {
            if let Ok(text) = std::str::from_utf8(&bytes[..width]) {
                return (text.to_string(), width);
            }
        }
        return ("\u{fffd}".to_string(), 1);
    }
    (String::from_utf8_lossy(&bytes[..count]).into_owned(), count)
}

fn log_level_matches(text: &str, level: &str) -> bool {
    level.is_empty()
        || level == "ALL"
        || text
            .split_once(" [")
            .and_then(|(_, remainder)| remainder.split_once(']'))
            .is_some_and(|(actual, _)| actual.trim() == level)
}

fn filter_log_page(
    bytes: &[u8],
    line_prefix: &[u8],
    query: &str,
    level: &str,
    limit: usize,
) -> (String, usize, bool) {
    let mut output = String::new();
    let mut consumed = 0;
    for (index, line) in bytes.split_inclusive(|byte| *byte == b'\n').enumerate() {
        let text = String::from_utf8_lossy(line);
        let context = if index == 0 && !line_prefix.is_empty() {
            Some([line_prefix, line].concat())
        } else {
            None
        };
        let match_text = String::from_utf8_lossy(context.as_deref().unwrap_or(line));
        if log_level_matches(&match_text, level) && match_text.contains(query) {
            if text.len() <= limit - output.len() {
                output.push_str(&text);
            } else if !output.is_empty() {
                return (output, consumed, true);
            } else {
                let (part, count) = bounded_text(line, limit);
                output.push_str(&part);
                return (output, consumed + count, true);
            }
        }
        consumed += line.len();
    }
    (output, consumed, false)
}

#[cfg(test)]
mod tests {
    use super::*;

    const APPLICATION: &str = r#"
[server]
primary_ip = "127.0.0.1:11001"
secondary_ip = "127.0.0.1:11002"
run_on = "primary"
current_run = "primary"
[serial]
baud_rate = 115200
data_bits = "Eight"
parity = "None"
stop_bits = "One"
flow_control = "none"
[settings]
timeout = 50
poll_interval_ms = 1
command_retry_count = 3
query_retry_count = 0
parity_switch_delay_ms = 1
transmit_settle_guard_ms = 1
"#;

    fn csv(port: &str, address: u8, prefix: &str, point: &str) -> String {
        format!("com,{port},,,,,,,\nbaud_rate,115200,,,,,,,\nis_special,false,,,,,,,\ncom_index,1,,,,,,,\ndevice_id,{address},,,,,,,\ndata_len,8,,,,,,,\nkks_prefix,{prefix},,,,,,,\n{}\n{point},uint,status,1,0,0,255,0,0\n", crate::csv_parser::RECORD_CSV_HEADER)
    }

    struct Fixture {
        _directory: tempfile::TempDir,
        repository: FileRepository,
    }

    impl Fixture {
        fn new() -> Self {
            let directory = tempfile::tempdir().unwrap();
            let config_dir = directory.path().join("config");
            let log_dir = directory.path().join("logs");
            fs::create_dir_all(&config_dir).unwrap();
            fs::create_dir_all(&log_dir).unwrap();
            fs::write(config_dir.join("config.toml"), APPLICATION).unwrap();
            fs::write(config_dir.join("calc.toml"), "rules = []\n").unwrap();
            fs::write(config_dir.join("rcd_a.csv"), csv("COM1", 1, "D1_", "TEMP")).unwrap();
            let repository = FileRepository::new(config_dir, log_dir, "rcd_server".into()).unwrap();
            Self {
                _directory: directory,
                repository,
            }
        }

        fn read(&mut self, name: &str) -> Value {
            self.repository
                .handle(&json!({"action":"read_config", "name":name}))
                .unwrap()
        }

        fn write(&mut self, name: &str, content: &str, revision: &str) -> OperationResult<Value> {
            self.repository
                .handle(&json!({"action":"write_config", "name":name,
                "content":content, "expected_revision":revision}))
        }

        fn log_path(&self) -> PathBuf {
            self.repository.log_dir.join("rcd_server_current.log")
        }

        fn log(&mut self, request: Value) -> OperationResult<Value> {
            let mut request = request;
            request["action"] = json!("read_log");
            request["name"] = json!("rcd_server_current.log");
            self.repository.handle(&request)
        }

        fn log_pages(&mut self, mut request: Value) -> String {
            let limit = request["limit"].as_u64().unwrap_or(MAX_LOG_TEXT as u64);
            let mut reconstructed = String::new();
            for _ in 0..10_000 {
                let page = self.log(request.clone()).unwrap();
                let text = page["text"].as_str().unwrap();
                assert!(text.len() as u64 <= limit);
                assert!(!text.contains('\u{fffd}'));
                reconstructed.push_str(text);
                let offset = page["offset"].as_u64().unwrap();
                let next_offset = page["next_offset"].as_u64().unwrap();
                assert!(next_offset >= offset);
                assert!(next_offset <= page["size_bytes"].as_u64().unwrap());
                if !request["generation"].is_null() {
                    assert_eq!(page["generation"], request["generation"]);
                }
                if page["eof"] == true {
                    return reconstructed;
                }
                assert!(next_offset > offset, "filtered pages must advance");
                request["offset"] = page["next_offset"].clone();
                request["generation"] = page["generation"].clone();
            }
            panic!("filtered log pagination did not finish");
        }
    }

    #[test]
    fn configuration_save_is_atomic_backed_up_and_restore_clears_restart_flag() {
        let mut fixture = Fixture::new();
        let before = fixture.read("rcd_a.csv");
        let original = before["content"].as_str().unwrap();
        let changed = original.replace("115200", "57600");
        let path = fixture.repository.config_dir.join("rcd_a.csv");
        let mut old_handle = File::open(&path).unwrap();
        let after = fixture
            .write("rcd_a.csv", &changed, before["revision"].as_str().unwrap())
            .unwrap();
        assert_eq!(after["restart_required"], true);
        assert_eq!(after["active_revision"], before["revision"]);
        assert_eq!(fs::read_to_string(&path).unwrap(), changed);
        let mut old_content = String::new();
        old_handle.read_to_string(&mut old_content).unwrap();
        assert_eq!(
            old_content, original,
            "a pre-commit reader must retain the complete old file"
        );
        assert_eq!(after["backups"].as_array().unwrap().len(), 1);
        let backup_id = after["backups"][0]["id"].as_str().unwrap();
        let preview = fixture
            .repository
            .handle(&json!({"action":"read_config", "name":"rcd_a.csv", "backup_id":backup_id}))
            .unwrap();
        assert_eq!(preview["content"], original);
        assert_eq!(preview["revision"], after["revision"]);
        assert_eq!(preview["backup_revision"], before["revision"]);
        let restored = fixture
            .write(
                "rcd_a.csv",
                preview["content"].as_str().unwrap(),
                preview["revision"].as_str().unwrap(),
            )
            .unwrap();
        assert_eq!(restored["restart_required"], false);
        assert_eq!(restored["revision"], before["revision"]);
        assert_eq!(restored["backups"].as_array().unwrap().len(), 2);
        assert!(!fs::read_dir(&fixture.repository.config_dir)
            .unwrap()
            .any(|entry| entry
                .unwrap()
                .file_name()
                .to_string_lossy()
                .ends_with(".tmp")));
    }

    #[test]
    fn invalid_csv_and_formula_never_change_disk_or_create_backups() {
        let mut fixture = Fixture::new();
        let csv_before = fixture.read("rcd_a.csv");
        let calc_before = fixture.read("calc.toml");
        let valid_csv = csv_before["content"].as_str().unwrap();
        let invalid_csvs = [
            valid_csv.replace("is_special,false", "is_special,maybe"),
            valid_csv.replace("TEMP,uint,status,1,0", "TEMP,uint,status,0,0"),
            valid_csv.replace("TEMP,uint,status", "TEMP,unknown,status"),
            format!("{valid_csv}TEMP,uint,status,1,0,0,255,0,0\n"),
        ];
        for content in invalid_csvs {
            let error = fixture
                .write(
                    "rcd_a.csv",
                    &content,
                    csv_before["revision"].as_str().unwrap(),
                )
                .unwrap_err();
            assert_eq!(error.code, "invalid_request", "{error}");
            assert_eq!(fixture.read("rcd_a.csv")["content"], csv_before["content"]);
        }
        for expression in ["D1_TEMP +", "MISSING + 1", "OUT + 1"] {
            let content = format!(
                "[[rules]]\nkks_calc = 'OUT'\ndata_type = 'uint'\nexpression = '{expression}'\n"
            );
            let error = fixture
                .write(
                    "calc.toml",
                    &content,
                    calc_before["revision"].as_str().unwrap(),
                )
                .unwrap_err();
            assert_eq!(error.code, "invalid_request", "{error}");
        }
        let cycle = "[[rules]]\nkks_calc='A'\ndata_type='uint'\nexpression='B'\n[[rules]]\nkks_calc='B'\ndata_type='uint'\nexpression='A'\n";
        assert_eq!(
            fixture
                .write(
                    "calc.toml",
                    cycle,
                    calc_before["revision"].as_str().unwrap()
                )
                .unwrap_err()
                .code,
            "invalid_request"
        );
        assert_eq!(fixture.read("calc.toml")["content"], calc_before["content"]);
        assert!(!fixture
            .repository
            .config_dir
            .join(BACKUP_DIRECTORY)
            .exists());
    }

    #[test]
    fn complete_set_validation_rejects_duplicate_points_devices_and_port_settings() {
        for second_csv in [
            csv("COM2", 2, "D1_", "TEMP"),
            csv("COM1", 1, "D2_", "OTHER"),
            csv("COM1", 2, "D2_", "OTHER").replace("115200", "57600"),
            csv("/dev/COM1", 2, "D2_", "OTHER"),
        ] {
            let mut fixture = Fixture::new();
            let before = fixture.read("config.toml");
            fs::write(fixture.repository.config_dir.join("rcd_b.csv"), second_csv).unwrap();
            let changed = format!("{APPLICATION}\n# draft\n");
            assert_eq!(
                fixture
                    .write(
                        "config.toml",
                        &changed,
                        before["revision"].as_str().unwrap()
                    )
                    .unwrap_err()
                    .code,
                "invalid_request"
            );
            assert_eq!(fixture.read("config.toml")["revision"], before["revision"]);
        }
    }

    #[test]
    fn validation_rejects_reserved_nodes_and_formula_typos() {
        let mut fixture = Fixture::new();
        let before = fixture.read("calc.toml");
        for content in [
            "[[rule]]\nkks_calc='OUT'\nexpression='1'\ndata_type='uint'",
            "[[rules]]\nkks_calc='OUT'\nexpression='1'\ndata_type='uint'\nmax_limt=20",
            "[[rules]]\nkks_calc='D1_TEMP'\nexpression='1'\ndata_type='uint'",
            "[[rules]]\nkks_calc='COM1_SendCommand_Input'\nexpression='1'\ndata_type='uint'",
            "[[rules]]\nkks_calc='OUT'\nexpression='1'\ndata_type='invalid'",
            "[[rules]]\nkks_calc='OUT'\nexpression='1'\ndata_type='uint'\nmin_limit=5\nmax_limit=2",
        ] {
            assert_eq!(
                fixture
                    .write("calc.toml", content, before["revision"].as_str().unwrap())
                    .unwrap_err()
                    .code,
                "invalid_request"
            );
        }
    }

    #[test]
    fn revision_and_writer_lock_conflicts_do_not_modify_configuration() {
        let mut fixture = Fixture::new();
        let before = fixture.read("config.toml");
        let error = fixture
            .write("config.toml", APPLICATION, &"0".repeat(64))
            .unwrap_err();
        assert_eq!(error.code, "conflict");
        assert_eq!(
            error.current_revision.as_deref(),
            before["revision"].as_str()
        );
        let lock = config_write_lock(&fixture.repository.config_dir).unwrap();
        assert_eq!(
            fixture
                .write(
                    "config.toml",
                    APPLICATION,
                    before["revision"].as_str().unwrap()
                )
                .unwrap_err()
                .code,
            "conflict"
        );
        drop(lock);
        let same = fixture
            .write(
                "config.toml",
                APPLICATION,
                before["revision"].as_str().unwrap(),
            )
            .unwrap();
        assert_eq!(same["restart_required"], false);
        assert!(same["backups"].as_array().unwrap().is_empty());
    }

    #[test]
    fn file_names_backup_ids_sizes_and_empty_repository_are_bounded() {
        let mut fixture = Fixture::new();
        for name in [
            "../config.toml",
            "rcd_a.csv/../../private.pem",
            "C:\\config.toml",
            "config.toml:secret",
            "private.pem",
            "/etc/passwd",
        ] {
            assert_eq!(
                fixture
                    .repository
                    .handle(&json!({"action":"read_config", "name":name}))
                    .unwrap_err()
                    .code,
                "invalid_request"
            );
        }
        for backup in ["../../config.toml".to_string(), "-".repeat(114)] {
            assert_eq!(
                fixture
                    .repository
                    .handle(
                        &json!({"action":"read_config", "name":"config.toml", "backup_id":backup})
                    )
                    .unwrap_err()
                    .code,
                "invalid_request"
            );
        }
        assert_eq!(
            fixture
                .repository
                .handle(&json!({"action":"read_config", "name":"rcd_missing.csv"}))
                .unwrap_err()
                .code,
            "not_found"
        );
        assert_eq!(fixture.repository.handle(&json!({"action":"validate_config", "name":"calc.toml", "content":"x".repeat(MAX_CONFIG_BYTES + 1)})).unwrap_err().code, "too_large");
        let directory = tempfile::tempdir().unwrap();
        fs::create_dir(directory.path().join("config")).unwrap();
        fs::create_dir(directory.path().join("logs")).unwrap();
        let mut empty = FileRepository::new(
            directory.path().join("config"),
            directory.path().join("logs"),
            "rcd".into(),
        )
        .unwrap();
        assert_eq!(
            empty.handle(&json!({"action":"list_configs"})).unwrap()["items"],
            json!([])
        );
    }

    #[test]
    fn validation_messages_are_utf8_safe_and_below_error_envelope_limit() {
        let mut validation = Validation::default();
        for _ in 0..200 {
            validation.error("超长错误的点位标识".repeat(1000));
        }
        let message = validation.errors.join("\n");
        assert!(message.len() < 16 * 1024);
        assert!(std::str::from_utf8(message.as_bytes()).is_ok());
        assert!(validation.errors_truncated);
        assert!(message.contains("省略"));
        assert!(validation.errors.iter().all(|line| line.len() <= 1024));
        let mut fixture = Fixture::new();
        let before = fixture.read("calc.toml");
        let content = format!(
            "[[rules]]\nkks_calc='{}'\ndata_type='invalid'\nexpression='MISSING'",
            "长名字".repeat(10000)
        );
        let error = fixture
            .write("calc.toml", &content, before["revision"].as_str().unwrap())
            .unwrap_err();
        assert!(error.message.len() < 16 * 1024);
    }

    #[test]
    fn backup_listing_returns_only_newest_200_without_deleting_old_backups() {
        let mut fixture = Fixture::new();
        let directory = fixture
            .repository
            .backup_directory("config.toml", true)
            .unwrap();
        let mut ids = Vec::new();
        for number in 0..MAX_BACKUPS + 3 {
            let id = format!(
                "{number:032x}-0000000000000000-{}",
                digest(APPLICATION.as_bytes())
            );
            fs::write(directory.join(&id), APPLICATION).unwrap();
            ids.push(id);
        }
        let result = fixture.read("config.toml");
        assert_eq!(result["backups"].as_array().unwrap().len(), MAX_BACKUPS);
        assert_eq!(result["backups_truncated"], true);
        assert_eq!(result["backups"][0]["id"], *ids.last().unwrap());
        assert_eq!(fs::read_dir(&directory).unwrap().count(), MAX_BACKUPS + 3);
        // A known older resource remains readable even when omitted from the list.
        let preview = fixture
            .repository
            .handle(&json!({"action":"read_config", "name":"config.toml", "backup_id":ids[0]}))
            .unwrap();
        assert_eq!(preview["content"], APPLICATION);
    }

    #[test]
    fn log_utf8_pages_reassemble_exactly_and_append_keeps_generation() {
        let mut fixture = Fixture::new();
        let content = "2026-08-28 [INFO ] 设备 温度 α😀\n".repeat(200);
        fs::write(fixture.log_path(), &content).unwrap();
        let mut request = json!({"limit":257});
        let mut reconstructed = String::new();
        let last = loop {
            let page = fixture.log(request.clone()).unwrap();
            let text = page["text"].as_str().unwrap();
            assert!(text.len() <= 257);
            assert!(!text.contains('\u{fffd}'));
            reconstructed.push_str(text);
            if page["eof"] == true {
                break page;
            }
            assert!(page["next_offset"].as_u64().unwrap() > page["offset"].as_u64().unwrap());
            request["offset"] = page["next_offset"].clone();
            request["generation"] = page["generation"].clone();
        };
        assert_eq!(reconstructed, content);
        let append = "2026-08-28 [WARN ] appended\n";
        OpenOptions::new()
            .append(true)
            .open(fixture.log_path())
            .unwrap()
            .write_all(append.as_bytes())
            .unwrap();
        let page = fixture
            .log(json!({"offset":last["next_offset"], "generation":last["generation"]}))
            .unwrap();
        assert_eq!(page["text"], append);
        assert_eq!(page["generation"], last["generation"]);
    }

    #[test]
    fn log_list_is_newest_first_with_stable_name_tiebreak() {
        let mut fixture = Fixture::new();
        let older = fixture.repository.log_dir.join("rcd_server_19990101.log");
        fs::write(&older, "old\n").unwrap();
        fs::write(fixture.log_path(), "new\n").unwrap();
        let early = UNIX_EPOCH + std::time::Duration::from_secs(1000);
        let late = UNIX_EPOCH + std::time::Duration::from_secs(2000);
        File::open(&older)
            .unwrap()
            .set_times(fs::FileTimes::new().set_modified(early))
            .unwrap();
        File::open(fixture.log_path())
            .unwrap()
            .set_times(fs::FileTimes::new().set_modified(late))
            .unwrap();
        let listed = fixture
            .repository
            .handle(&json!({"action":"list_logs"}))
            .unwrap();
        assert_eq!(listed["items"][0]["name"], "rcd_server_current.log");
        File::open(&older)
            .unwrap()
            .set_times(fs::FileTimes::new().set_modified(late))
            .unwrap();
        let tied = fixture
            .repository
            .handle(&json!({"action":"list_logs"}))
            .unwrap();
        assert_eq!(tied["items"][0]["name"], "rcd_server_19990101.log");
    }

    #[test]
    fn log_filter_is_local_to_a_bounded_scanned_page() {
        let mut fixture = Fixture::new();
        let info = "2026-08-28 [INFO ] irrelevant\n";
        let content = format!(
            "{}2026-08-28 [WARN ] needle 设备\n",
            info.repeat(MAX_LOG_SCAN / info.len() + 20)
        );
        fs::write(fixture.log_path(), content).unwrap();
        let first = fixture
            .log(json!({"query":"needle", "level":"warn"}))
            .unwrap();
        assert_eq!(first["text"], "");
        assert_eq!(first["eof"], false);
        assert!(first["next_offset"].as_u64().unwrap() <= 256 * 1024);
        let next = fixture.log(json!({"query":"needle", "level":"WARN", "offset":first["next_offset"], "generation":first["generation"]})).unwrap();
        assert!(next["text"].as_str().unwrap().contains("needle 设备"));
        assert!(!next["text"].as_str().unwrap().contains("irrelevant"));
        assert_eq!(next["eof"], true);
        let (text, _, _) = filter_log_page(
            b"2026-08-28 [INFO ] message mentions [WARN ]\n",
            b"",
            "",
            "WARN",
            MAX_LOG_TEXT,
        );
        assert_eq!(
            text, "",
            "a tag in the message must not override the record level"
        );
    }

    #[test]
    fn filtered_utf8_long_lines_reassemble_at_small_page_boundaries() {
        let mut fixture = Fixture::new();
        let long = format!(
            "2026-08-28 [WARN ] needle-prefix {} suffix-needle\n",
            "设备😀".repeat(30)
        );
        let info = "2026-08-28 [INFO ] unrelated information\n";
        let other_warn = "2026-08-28 [WARN ] unrelated warning\n";
        let final_match = "2026-08-28 [WARN ] needle-prefix final suffix-needle\n";
        fs::write(
            fixture.log_path(),
            format!("{long}{info}{other_warn}{final_match}"),
        )
        .unwrap();
        for limit in [4, 31, 32] {
            for (query, level, expected) in [
                ("", "WARN", format!("{long}{other_warn}{final_match}")),
                ("needle-prefix", "", format!("{long}{final_match}")),
                ("needle-prefix", "WARN", format!("{long}{final_match}")),
                ("suffix-needle", "WARN", format!("{long}{final_match}")),
            ] {
                assert_eq!(
                    fixture.log_pages(json!({"limit":limit, "query":query, "level":level})),
                    expected,
                    "limit={limit}, query={query}, level={level}"
                );
            }
        }
    }

    #[test]
    fn filtered_continuations_use_only_the_current_line_prefix() {
        let mut fixture = Fixture::new();
        let previous = "2026-08-28 [INFO ] previous-needle\n";
        let long = format!("2026-08-28 [WARN ] needle {}\n", "设备😀".repeat(900));
        let following = "2026-08-28 [INFO ] later message\n";
        fs::write(fixture.log_path(), format!("{previous}{long}{following}")).unwrap();
        let offset = previous.len() + "2026-08-28 [WARN ] needle ".len() + 500 * "设备😀".len();
        let remaining = &long[offset - previous.len()..];
        let page = fixture
            .log(json!({"offset":offset, "level":"WARN", "query":"needle"}))
            .unwrap();
        assert_eq!(page["text"], remaining);
        assert_eq!(page["eof"], true);
        assert_eq!(
            fixture
                .log(json!({"offset":offset, "level":"WARN", "query":"previous-needle"}))
                .unwrap()["text"],
            "",
            "a match before the current line must not leak through lookbehind"
        );
        assert_eq!(
            fixture.log_pages(json!({"limit":32, "level":"WARN", "query":"needle"})),
            long,
            "continuations crossing multiple backward-read blocks must be complete"
        );
    }

    #[test]
    fn filtered_log_lookbehind_limit_is_explicit_and_does_not_limit_byte_reads() {
        let mut fixture = Fixture::new();
        let content = format!("2026-08-28 [WARN ] needle {}", "x".repeat(300_000));
        fs::write(fixture.log_path(), &content).unwrap();
        let offset = 280_000;
        for filter in [json!({"level":"WARN"}), json!({"query":"needle"})] {
            let mut request = filter;
            request["offset"] = json!(offset);
            request["limit"] = json!(32);
            let error = fixture.log(request).unwrap_err();
            assert_eq!(error.code, "invalid_request");
            assert!(error.message.contains("256 KiB"));
            assert!(error.message.contains("清除筛选"));
        }
        for level in ["", "ALL"] {
            let page = fixture
                .log(json!({"offset":offset, "limit":32, "level":level}))
                .unwrap();
            assert_eq!(page["offset"], offset);
            assert_eq!(page["next_offset"], offset + 32);
            assert_eq!(page["text"], "x".repeat(32));
            assert_eq!(page["eof"], false);
        }
        let boundary = fixture
            .log(json!({"offset":MAX_LOG_LOOKBEHIND, "limit":32, "level":"WARN"}))
            .unwrap();
        assert_eq!(boundary["text"], "x".repeat(32));
        let empty = fixture
            .log(json!({"offset":content.len(), "limit":32, "level":"WARN"}))
            .unwrap();
        assert_eq!(empty["text"], "");
        assert_eq!(empty["eof"], true);
    }

    #[test]
    fn filtered_unfinished_line_with_unknown_keyword_match_requires_byte_reads() {
        let mut fixture = Fixture::new();
        let content = format!(
            "2026-08-28 [WARN ] {} future-needle\n",
            "x".repeat(MAX_LOG_SCAN + 32)
        );
        fs::write(fixture.log_path(), &content).unwrap();
        for level in ["", "WARN"] {
            let error = fixture
                .log(json!({"limit":32, "query":"future-needle", "level":level}))
                .unwrap_err();
            assert_eq!(error.code, "invalid_request");
            assert!(error.message.contains("清除筛选"));
        }
        let unfiltered = fixture.log(json!({"limit":32})).unwrap();
        assert_eq!(unfiltered["text"], &content[..32]);
        assert_eq!(unfiltered["next_offset"], 32);
        assert_eq!(unfiltered["eof"], false);
    }

    #[test]
    fn log_rotation_truncation_and_in_place_rewrite_invalidate_old_generation() {
        for mode in ["rotate", "truncate", "rewrite"] {
            let mut fixture = Fixture::new();
            fs::write(fixture.log_path(), "original log\n".repeat(100)).unwrap();
            let first = fixture.log(json!({"limit":64})).unwrap();
            match mode {
                "rotate" => {
                    fs::rename(
                        fixture.log_path(),
                        fixture.repository.log_dir.join("rcd_server_old.log"),
                    )
                    .unwrap();
                    fs::write(fixture.log_path(), "new file\n").unwrap();
                }
                "truncate" => fs::write(fixture.log_path(), "short\n").unwrap(),
                _ => {
                    let mut file = OpenOptions::new()
                        .write(true)
                        .open(fixture.log_path())
                        .unwrap();
                    file.write_all(b"replaced log\n").unwrap();
                }
            }
            assert_eq!(
                fixture
                    .log(json!({"offset":first["next_offset"], "generation":first["generation"]}))
                    .unwrap_err()
                    .code,
                "conflict",
                "{mode}"
            );
            let fresh = fixture.log(json!({})).unwrap();
            assert_ne!(fresh["generation"], first["generation"]);
        }
    }

    #[test]
    fn log_scan_anchor_detects_rewrite_behind_pagination_cursor() {
        let mut fixture = Fixture::new();
        fs::write(fixture.log_path(), "original log\n".repeat(300)).unwrap();
        let first = fixture.log(json!({"offset":1024, "limit":128})).unwrap();
        let mut file = OpenOptions::new()
            .write(true)
            .open(fixture.log_path())
            .unwrap();
        file.seek(SeekFrom::Start(first["next_offset"].as_u64().unwrap() - 4))
            .unwrap();
        file.write_all(b"xxxx").unwrap();
        assert_eq!(
            fixture
                .log(json!({"offset":first["next_offset"], "generation":first["generation"]}))
                .unwrap_err()
                .code,
            "conflict"
        );
    }

    #[test]
    fn empty_tail_and_malformed_utf8_logs_are_bounded_and_make_progress() {
        let mut fixture = Fixture::new();
        fs::write(fixture.log_path(), b"").unwrap();
        let empty = fixture.log(json!({"tail":true})).unwrap();
        assert_eq!(empty["eof"], true);
        assert_eq!(empty["next_offset"], 0);
        fs::write(fixture.log_path(), "第一行\n第二行😀\n最后一行\n").unwrap();
        let tail = fixture.log(json!({"tail":true,"limit":16})).unwrap();
        assert!(!tail["text"].as_str().unwrap().contains('\u{fffd}'));
        assert!(tail["text"].as_str().unwrap().ends_with("最后一行\n"));
        fs::write(
            fixture.log_path(),
            [b'X', 0xe4, 0xb8, 0xad, 0xff, 0xfe, b'\n'],
        )
        .unwrap();
        let mut offset = 0;
        for _ in 0..10 {
            let page = fixture.log(json!({"offset":offset, "limit":4})).unwrap();
            assert!(page["text"].as_str().unwrap().len() <= 4);
            if page["eof"] == true {
                return;
            }
            let next = page["next_offset"].as_u64().unwrap();
            assert!(next > offset, "malformed text must not trap the cursor");
            offset = next;
        }
        panic!("malformed UTF-8 log did not finish");
    }

    #[cfg(unix)]
    #[test]
    fn configuration_log_and_backup_symlinks_are_never_followed() {
        use std::os::unix::fs::symlink;
        let mut fixture = Fixture::new();
        let outside = fixture._directory.path().join("private.pem");
        fs::write(&outside, "private material").unwrap();
        symlink(&outside, fixture.repository.config_dir.join("rcd_link.csv")).unwrap();
        assert_eq!(
            fixture
                .repository
                .handle(&json!({"action":"read_config", "name":"rcd_link.csv"}))
                .unwrap_err()
                .code,
            "invalid_request"
        );
        symlink(&outside, fixture.log_path()).unwrap();
        assert_eq!(fixture.log(json!({})).unwrap_err().code, "invalid_request");
        let directory = fixture
            .repository
            .backup_directory("config.toml", true)
            .unwrap();
        let id = format!("{}-{}", unique_id(), digest(b"anything"));
        symlink(&outside, directory.join(&id)).unwrap();
        assert_eq!(
            fixture
                .repository
                .handle(&json!({"action":"read_config", "name":"config.toml", "backup_id":id}))
                .unwrap_err()
                .code,
            "invalid_request"
        );
        assert_eq!(fs::read_to_string(&outside).unwrap(), "private material");
    }
}
