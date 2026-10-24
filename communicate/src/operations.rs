//! Configuration-only Operations v1. No acquisition, restart or maintenance
//! capability is claimed. Writes are CAS, validated across both bound files,
//! backed up and atomic. They never change the running serial/OPC state.
use crate::config::{self, LoadedConfig};
mod config_set;
use serde::Serialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    fs::{self, File, OpenOptions},
    io::{self, Read, Write},
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

const MAX_BYTES: usize = 2 * 1024 * 1024;
pub const URI: &str = "urn:SerialServer:operations";

#[derive(Debug, Serialize, thiserror::Error)]
#[error("{message}")]
pub struct Error {
    pub code: &'static str,
    pub message: String,
}
impl Error {
    fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }
}
impl From<io::Error> for Error {
    fn from(error: io::Error) -> Self {
        Self::new(
            if error.kind() == io::ErrorKind::NotFound {
                "not_found"
            } else {
                "io_error"
            },
            error.to_string(),
        )
    }
}
type Result<T> = std::result::Result<T, Error>;

#[derive(Clone)]
pub struct Operations(Arc<Mutex<Repository>>);
struct Repository {
    files: BTreeMap<&'static str, PathBuf>,
    active: BTreeMap<&'static str, String>,
    backups: PathBuf,
    instance_id: String,
    points_file: PathBuf,
}
fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}
fn text<'a>(request: &'a Value, key: &str) -> Result<&'a str> {
    request
        .get(key)
        .and_then(Value::as_str)
        .ok_or_else(|| Error::new("invalid_request", format!("缺少 {key}")))
}
fn regular(path: &Path) -> Result<()> {
    if !fs::symlink_metadata(path)?.file_type().is_file() {
        return Err(Error::new(
            "invalid_request",
            "配置必须为普通文件，不能为链接",
        ));
    }
    Ok(())
}
fn read(path: &Path) -> Result<String> {
    regular(path)?;
    if path.canonicalize()?.parent() != path.parent() {
        return Err(Error::new("invalid_request", "配置目录发生替换"));
    }
    let before = fs::symlink_metadata(path)?;
    let file = File::open(path)?;
    let opened = file.metadata()?;
    let mut value = String::new();
    file.take(MAX_BYTES as u64 + 1).read_to_string(&mut value)?;
    regular(path)?;
    let after = fs::symlink_metadata(path)?;
    if value.len() > MAX_BYTES {
        return Err(Error::new("too_large", "配置文件最大 2 MiB"));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        if (before.dev(), before.ino()) != (opened.dev(), opened.ino())
            || (opened.dev(), opened.ino()) != (after.dev(), after.ino())
        {
            return Err(Error::new("conflict", "配置文件在打开期间被替换"));
        }
    }
    if !opened.is_file()
        || path.canonicalize()?.parent() != path.parent()
        || before.len() != after.len()
        || before.modified()? != after.modified()?
        || after.len() != value.len() as u64
    {
        return Err(Error::new("conflict", "读取期间配置已变化"));
    }
    Ok(value)
}
fn directory(path: &Path) -> Result<PathBuf> {
    fs::create_dir_all(path)?;
    if !fs::symlink_metadata(path)?.file_type().is_dir() {
        return Err(Error::new("invalid_request", "配置备份目录不能为链接"));
    }
    Ok(path.canonicalize()?)
}
fn sync_dir(path: &Path) -> Result<()> {
    #[cfg(unix)]
    File::open(path)?.sync_all()?;
    #[cfg(not(unix))]
    let _ = path;
    Ok(())
}

fn write_lock(backups: &Path) -> Result<File> {
    let path = backups.join("write.lock");
    if path.exists() {
        regular(&path)?;
    }
    let file = OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(path)?;
    fs2::FileExt::try_lock_exclusive(&file)
        .map_err(|_| Error::new("conflict", "另一个配置保存正在进行"))?;
    Ok(file)
}

/// Recover a previously accepted configuration set before loading either file.
/// The caller must hold the instance runtime lock.
pub fn recover_configuration(path: &Path, state: &Path) -> Result<()> {
    config_set::recover(path, state)
}

pub fn ensure_configuration_complete(path: &Path) -> Result<()> {
    if config_set::pending(path) {
        return Err(Error::new(
            "conflict",
            "成套配置保存尚未完成，需启动程序恢复；禁止读取混合配置",
        ));
    }
    Ok(())
}

impl Operations {
    pub fn open(path: &Path, state: &Path, loaded: &LoadedConfig) -> Result<Self> {
        regular(path)?;
        let path = path.canonicalize()?;
        let point_path = if loaded.config.points_file.is_absolute() {
            loaded.config.points_file.clone()
        } else {
            path.parent()
                .ok_or_else(|| Error::new("invalid_request", "配置目录无效"))?
                .join(&loaded.config.points_file)
        };
        regular(&point_path)?;
        let point_path = point_path.canonicalize()?;
        if path == point_path {
            return Err(Error::new("invalid_request", "主配置和点表必须不同"));
        }
        let files = BTreeMap::from([("config.toml", path), ("points.csv", point_path)]);
        let mut active = BTreeMap::from([
            ("config.toml", digest(loaded.config_text.as_bytes())),
            ("points.csv", digest(loaded.points_text.as_bytes())),
        ]);
        let set = config_set::Contents {
            config_toml: loaded.config_text.clone(),
            points_csv: loaded.points_text.clone(),
        }
        .render()?;
        active.insert(config_set::NAME, digest(set.as_bytes()));
        let backups = directory(&state.join("config-backups"))?;
        Ok(Self(Arc::new(Mutex::new(Repository {
            files,
            active,
            backups,
            instance_id: loaded.config.instance_id.clone(),
            points_file: loaded.config.points_file.clone(),
        }))))
    }
    pub fn capabilities() -> Value {
        json!({"schema_version":1,"features":["configuration","configuration_forms","configuration_set"],"max_config_bytes":MAX_BYTES,"max_log_bytes":0,"configuration_apply":"restart"})
    }
    pub fn execute_json(&self, input: &str) -> String {
        let result = if input.len() > 13 * 1024 * 1024 {
            Err(Error::new("too_large", "请求过大"))
        } else {
            serde_json::from_str(input)
                .map_err(|_| Error::new("invalid_request", "无效 JSON"))
                .and_then(|value| self.execute(&value))
        };
        match result {
            Ok(value) => json!({"ok":true,"result":value}),
            Err(error) => json!({"ok":false,"error":error}),
        }
        .to_string()
    }
    pub fn execute(&self, request: &Value) -> Result<Value> {
        self.0
            .lock()
            .map_err(|_| Error::new("unavailable", "配置锁不可用"))?
            .execute(request)
    }
}

impl Repository {
    fn contents(&self) -> Result<config_set::Contents> {
        Ok(config_set::Contents {
            config_toml: read(&self.files["config.toml"])?,
            points_csv: read(&self.files["points.csv"])?,
        })
    }
    fn current(&self, name: &str) -> Result<String> {
        if name == config_set::NAME {
            self.contents()?.render()
        } else {
            read(&self.files[name])
        }
    }
    fn metadata(&self, name: &str, content: &str) -> Value {
        let revision = digest(content.as_bytes());
        let active = &self.active[name];
        json!({"name":name,"kind":if name==config_set::NAME {"成套配置（主配置＋点表）"} else if name=="config.toml" {"application"} else {"point_csv"},"size_bytes":content.len(),"revision":revision,"active_revision":active,"restart_required":revision!=*active})
    }
    fn backup_dir(&self, name: &str) -> Result<PathBuf> {
        directory(&self.backups.join(name))
    }
    fn document(&self, name: &str, backup: Option<&str>) -> Result<Value> {
        let current = self.current(name)?;
        let mut result = self.metadata(name, &current);
        let dir = self.backup_dir(name)?;
        let content = if let Some(id) = backup {
            if id.len() != 64 || !id.bytes().all(|b| b.is_ascii_hexdigit()) {
                return Err(Error::new("invalid_request", "无效备份编号"));
            }
            let bytes = read(&dir.join(id))?;
            if digest(bytes.as_bytes()) != id {
                return Err(Error::new("invalid_request", "备份摘要不符"));
            }
            result["viewing_backup_id"] = json!(id);
            result["backup_revision"] = json!(id);
            bytes
        } else {
            current
        };
        let mut backups = Vec::new();
        for entry in fs::read_dir(dir)? {
            let entry = entry?;
            let id = entry.file_name().to_string_lossy().into_owned();
            if id.len() != 64
                || !id.bytes().all(|b| b.is_ascii_hexdigit())
                || !entry.file_type()?.is_file()
            {
                continue;
            }
            let meta = entry.metadata()?;
            let date: chrono::DateTime<chrono::Utc> = meta.modified()?.into();
            backups.push(json!({"id":id,"created_at":date.to_rfc3339(),"size_bytes":meta.len()}));
        }
        backups.sort_by(|a, b| b["created_at"].as_str().cmp(&a["created_at"].as_str()));
        result["backups_truncated"] = json!(backups.len() > 200);
        backups.truncate(200);
        result["backups"] = json!(backups);
        result["content"] = json!(content);
        Ok(result)
    }
    fn validate(&self, name: &str, content: &str) -> Result<Value> {
        if content.len() > MAX_BYTES || content.contains('\0') {
            return Err(Error::new("too_large", "配置超限或包含 NUL"));
        }
        if name == config_set::NAME {
            let contents = config_set::Contents::parse(content)?;
            return self.validate_pair(&contents.config_toml, &contents.points_csv);
        }
        let other_name = if name == "config.toml" {
            "points.csv"
        } else {
            "config.toml"
        };
        let other = read(&self.files[other_name])?;
        let (config_text, points_text) = if name == "config.toml" {
            (content, other.as_str())
        } else {
            (other.as_str(), content)
        };
        self.validate_pair(config_text, points_text)
    }
    fn validate_pair(&self, config_text: &str, points_text: &str) -> Result<Value> {
        let result = config::parse(config_text, points_text).and_then(|loaded| {
            if loaded.config.instance_id != self.instance_id
                || loaded.config.points_file != self.points_file
            {
                return Err(config::ConfigError::Invalid(
                    "远程修改不得改变 instance_id 或 points_file 文件绑定".into(),
                ));
            }
            config::validate_adapters(&loaded.config)
        });
        let errors = result
            .err()
            .map(|error| error.to_string().chars().take(1024).collect::<String>())
            .into_iter()
            .collect::<Vec<_>>();
        Ok(
            json!({"valid":errors.is_empty(),"errors":errors,"warnings":["保存只修改文件；需显式重启后生效。"],"restart_required":true}),
        )
    }
    fn execute(&mut self, request: &Value) -> Result<Value> {
        ensure_configuration_complete(&self.files["config.toml"])?;
        let action = text(request, "action")?;
        if action == "list_configs" {
            let mut items = Vec::new();
            for (name, path) in &self.files {
                items.push(self.metadata(name, &read(path)?));
            }
            let set = self.current(config_set::NAME)?;
            if set.len() <= MAX_BYTES {
                items.push(self.metadata(config_set::NAME, &set));
            }
            return Ok(json!({"items":items}));
        }
        let name = text(request, "name")?;
        if name == config_set::NAME {
            return match action {
                "read_config" => {
                    self.document(name, request.get("backup_id").and_then(Value::as_str))
                }
                "validate_config" => self.validate(name, text(request, "content")?),
                "write_config" => self.write_set(
                    text(request, "content")?,
                    text(request, "expected_revision")?,
                ),
                _ => Err(Error::new("unsupported", "成套配置请编辑原文、校验后保存")),
            };
        }
        let path = self
            .files
            .get(name)
            .ok_or_else(|| Error::new("not_found", "只能访问公布的配置文件"))?
            .clone();
        match action {
            "read_config" => self.document(name, request.get("backup_id").and_then(Value::as_str)),
            "validate_config" => self.validate(name, text(request, "content")?),
            "prepare_config" => {
                if name != "config.toml" {
                    return Err(Error::new("unsupported", "点表请使用 CSV 原文编辑"));
                }
                let edits: Vec<rcd_config_form::Edit> =
                    serde_json::from_value(request.get("edits").cloned().unwrap_or(json!([])))
                        .map_err(|_| Error::new("invalid_request", "参数格式无效"))?;
                let form = rcd_config_form::prepare(
                    text(request, "content")?,
                    &edits,
                    rcd_config_form::Profile::Communicate,
                )
                .map_err(|e| Error::new("invalid_request", e))?;
                serde_json::to_value(form)
                    .map_err(|_| Error::new("operation_failed", "草稿编码失败"))
            }
            "write_config" => {
                let content = text(request, "content")?;
                let expected = text(request, "expected_revision")?;
                let parent = path
                    .parent()
                    .ok_or_else(|| Error::new("invalid_request", "配置路径无效"))?;
                let _lock = write_lock(&self.backups)?;
                let old = read(&path)?;
                if digest(old.as_bytes()) != expected {
                    return Err(Error::new("conflict", "配置版本已变化，请核对最新文件"));
                }
                let sibling = if name == "config.toml" {
                    "points.csv"
                } else {
                    "config.toml"
                };
                let sibling_before = read(&self.files[sibling])?;
                if self.validate(name, content)?["valid"] != true {
                    return Err(Error::new(
                        "invalid_request",
                        "配置校验失败，请先校验草稿查看详情",
                    ));
                }
                if content == old {
                    return self.document(name, None);
                }
                let backup_path = self.backup_dir(name)?.join(expected);
                if backup_path.exists() {
                    if read(&backup_path)? != old {
                        return Err(Error::new("conflict", "已有备份内容不符"));
                    }
                } else {
                    let mut backup =
                        tempfile::NamedTempFile::new_in(backup_path.parent().unwrap())?;
                    backup.write_all(old.as_bytes())?;
                    backup.as_file().sync_all()?;
                    backup
                        .persist_noclobber(&backup_path)
                        .map_err(|e| Error::from(e.error))?;
                    sync_dir(backup_path.parent().unwrap())?;
                }
                let mut temporary = tempfile::NamedTempFile::new_in(parent)?;
                temporary
                    .as_file()
                    .set_permissions(fs::metadata(&path)?.permissions())?;
                temporary.write_all(content.as_bytes())?;
                temporary.as_file().sync_all()?;
                if read(&path)? != old || read(&self.files[sibling])? != sibling_before {
                    return Err(Error::new("conflict", "保存期间配置集被外部修改"));
                }
                temporary.persist(&path).map_err(|e| Error::from(e.error))?;
                sync_dir(parent)?;
                self.document(name, None)
            }
            _ => Err(Error::new("unsupported", "未提供此运维功能")),
        }
    }
    fn write_set(&self, content: &str, expected: &str) -> Result<Value> {
        let _lock = write_lock(&self.backups)?;
        ensure_configuration_complete(&self.files["config.toml"])?;
        let before = self.contents()?;
        let old = before.render()?;
        if digest(old.as_bytes()) != expected {
            return Err(Error::new(
                "conflict",
                "主配置或点表已变化，请重新读取成套配置",
            ));
        }
        let after = config_set::Contents::parse(content)?;
        if self.validate_pair(&after.config_toml, &after.points_csv)?["valid"] != true {
            return Err(Error::new(
                "invalid_request",
                "成套配置校验失败，请先校验草稿查看详情",
            ));
        }
        if after.render()?.len() > MAX_BYTES {
            return Err(Error::new("too_large", "成套配置最大 2 MiB"));
        }
        if before == after {
            return self.set_receipt(content);
        }
        let backup = self.backup_dir(config_set::NAME)?.join(expected);
        if backup.exists() {
            if read(&backup)? != old {
                return Err(Error::new("conflict", "已有成套备份内容不符"));
            }
        } else {
            let mut tmp = tempfile::NamedTempFile::new_in(backup.parent().unwrap())?;
            tmp.write_all(old.as_bytes())?;
            tmp.as_file().sync_all()?;
            tmp.persist_noclobber(&backup)
                .map_err(|e| Error::from(e.error))?;
            sync_dir(backup.parent().unwrap())?;
        }
        if self.contents()? != before {
            return Err(Error::new("conflict", "保存期间配置集被外部修改"));
        }
        config_set::commit(
            &self.files["config.toml"],
            &self.files["points.csv"],
            before,
            after,
        )?;
        self.set_receipt(content)
    }
    fn set_receipt(&self, submitted: &str) -> Result<Value> {
        let mut result = self.document(config_set::NAME, None)?;
        // Operations v1 requires exact submitted text in a write receipt.
        // The opaque revision identifies the pair, not wrapper formatting.
        result["content"] = json!(submitted);
        result["size_bytes"] = json!(submitted.len());
        Ok(result)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn configuration_set_changes_role_without_invalid_intermediate_configuration() {
        let dir = tempfile::tempdir().unwrap();
        let config = include_str!("../examples/unit1-receiver.toml")
            .replace("unit1-receiver.csv", "points.csv");
        fs::write(dir.path().join("config.toml"), &config).unwrap();
        fs::write(
            dir.path().join("points.csv"),
            include_str!("../examples/unit1-receiver.csv"),
        )
        .unwrap();
        let loaded = config::load(&dir.path().join("config.toml")).unwrap();
        let ops = Operations::open(
            &dir.path().join("config.toml"),
            &dir.path().join("state"),
            &loaded,
        )
        .unwrap();
        let initial = ops
            .execute(&json!({"action":"read_config", "name":config_set::NAME}))
            .unwrap();
        let next = config_set::Contents {
            config_toml: include_str!("../examples/unit9-receiver.toml")
                .replace("unit9-receiver.csv", "points.csv")
                .replace(
                    "instance_id = \"unit9-receiver\"",
                    "instance_id = \"unit1-receiver\"",
                ),
            points_csv: include_str!("../examples/unit9-receiver.csv").into(),
        };
        assert_eq!(
            ops.execute(
                &json!({"action":"validate_config","name":"config.toml","content":next.config_toml})
            )
            .unwrap()["valid"],
            false
        );
        assert_eq!(
            ops.execute(
                &json!({"action":"validate_config","name":"points.csv","content":next.points_csv})
            )
            .unwrap()["valid"],
            false
        );
        let content = next.render().unwrap();
        assert_eq!(
            ops.execute(
                &json!({"action":"validate_config","name":config_set::NAME,"content":content})
            )
            .unwrap()["valid"],
            true
        );
        let request = json!({"action":"write_config","name":config_set::NAME,"content":content,"expected_revision":initial["revision"]});
        let saved = ops.execute(&request).unwrap();
        assert_eq!(saved["restart_required"], true);
        assert_eq!(saved["active_revision"], initial["active_revision"]);
        assert_eq!(
            fs::read_to_string(dir.path().join("config.toml")).unwrap(),
            next.config_toml
        );
        assert_eq!(
            fs::read_to_string(dir.path().join("points.csv")).unwrap(),
            next.points_csv
        );
        config::load(&dir.path().join("config.toml")).unwrap();
        assert_eq!(ops.execute(&request).unwrap_err().code, "conflict");
        let backup = ops.execute(&json!({"action":"read_config","name":config_set::NAME,"backup_id":initial["revision"]})).unwrap();
        assert_eq!(backup["content"], initial["content"]);
        let restored = ops.execute(&json!({"action":"write_config","name":config_set::NAME,"content":backup["content"],"expected_revision":saved["revision"]})).unwrap();
        assert_eq!(restored["restart_required"], false);
    }

    #[test]
    fn configuration_set_checks_both_revisions_and_rejects_invalid_pair() {
        let (_dir, ops) = fixture();
        let initial = ops
            .execute(&json!({"action":"read_config","name":config_set::NAME}))
            .unwrap();
        let wrapper_only = format!(
            "# caller formatting\n{}",
            initial["content"].as_str().unwrap()
        );
        let echoed = ops.execute(&json!({"action":"write_config","name":config_set::NAME,"content":wrapper_only,"expected_revision":initial["revision"]})).unwrap();
        assert_eq!(echoed["content"], wrapper_only);
        assert_eq!(echoed["size_bytes"], wrapper_only.len());
        assert_eq!(echoed["revision"], initial["revision"]);
        let mut set = config_set::Contents::parse(initial["content"].as_str().unwrap()).unwrap();
        set.points_csv = "bad".into();
        assert!(ops.execute(&json!({"action":"write_config","name":config_set::NAME,"content":set.render().unwrap(),"expected_revision":initial["revision"]})).is_err());
        assert_eq!(
            ops.execute(&json!({"action":"read_config","name":config_set::NAME}))
                .unwrap()["revision"],
            initial["revision"]
        );
        let single = ops
            .execute(&json!({"action":"read_config","name":"points.csv"}))
            .unwrap();
        ops.execute(&json!({"action":"write_config","name":"points.csv","content":format!("{}\n",single["content"].as_str().unwrap()),"expected_revision":single["revision"]})).unwrap();
        assert_eq!(ops.execute(&json!({"action":"write_config","name":config_set::NAME,"content":initial["content"],"expected_revision":initial["revision"]})).unwrap_err().code,"conflict");
    }
    pub(super) fn fixture() -> (tempfile::TempDir, Operations) {
        let dir = tempfile::tempdir().unwrap();
        fs::write(
            dir.path().join("config.toml"),
            include_str!("../examples/config.toml"),
        )
        .unwrap();
        fs::write(
            dir.path().join("points.csv"),
            include_str!("../examples/points.csv"),
        )
        .unwrap();
        let loaded = config::load(&dir.path().join("config.toml")).unwrap();
        let ops = Operations::open(
            &dir.path().join("config.toml"),
            &dir.path().join("state"),
            &loaded,
        )
        .unwrap();
        (dir, ops)
    }
    #[test]
    fn save_is_cas_backed_up_and_restart_only() {
        let (_dir, ops) = fixture();
        let initial = ops
            .execute(&json!({"action":"read_config","name":"config.toml"}))
            .unwrap();
        let draft = format!("{}\n# changed\n", initial["content"].as_str().unwrap());
        let req = json!({"action":"write_config","name":"config.toml","content":draft,"expected_revision":initial["revision"]});
        let result = ops.execute(&req).unwrap();
        assert_eq!(result["restart_required"], true);
        assert_eq!(result["active_revision"], initial["revision"]);
        assert_eq!(result["backups"].as_array().unwrap().len(), 1);
        assert_eq!(ops.execute(&req).unwrap_err().code, "conflict");
        let backup=ops.execute(&json!({"action":"read_config","name":"config.toml","backup_id":initial["revision"]})).unwrap();
        assert_eq!(backup["content"], initial["content"]);
        assert_eq!(backup["revision"], result["revision"]);
    }
    #[test]
    fn rejects_paths_identity_changes_and_unimplemented_protocol() {
        let (_dir, ops) = fixture();
        assert!(ops
            .execute(&json!({"action":"read_config","name":"../config.toml"}))
            .is_err());
        let initial = ops
            .execute(&json!({"action":"read_config","name":"config.toml"}))
            .unwrap();
        for content in [
            initial["content"]
                .as_str()
                .unwrap()
                .replace("communicate-example", "other"),
            initial["content"]
                .as_str()
                .unwrap()
                .replace("enabled = false", "enabled = true"),
        ] {
            let result = ops
                .execute(
                    &json!({"action":"validate_config","name":"config.toml","content":content}),
                )
                .unwrap();
            assert_eq!(result["valid"], false);
        }
    }
    #[test]
    fn rejects_bad_points_without_replacing_file() {
        let (_dir, ops) = fixture();
        let initial = ops
            .execute(&json!({"action":"read_config","name":"points.csv"}))
            .unwrap();
        let error=ops.execute(&json!({"action":"write_config","name":"points.csv","content":"bad","expected_revision":initial["revision"]})).unwrap_err();
        assert_eq!(error.code, "invalid_request");
        assert_eq!(
            ops.execute(&json!({"action":"read_config","name":"points.csv"}))
                .unwrap()["content"],
            initial["content"]
        );
    }

    #[test]
    fn refuses_toml_exceeding_startup_limit_before_save() {
        let (_dir, ops) = fixture();
        let initial = ops
            .execute(&json!({"action":"read_config","name":"config.toml"}))
            .unwrap();
        let content = format!(
            "{}\n#{}\n",
            initial["content"].as_str().unwrap(),
            "x".repeat(1024 * 1024)
        );
        let validation = ops
            .execute(&json!({"action":"validate_config","name":"config.toml","content":content}))
            .unwrap();
        assert_eq!(validation["valid"], false);
        let error = ops.execute(&json!({"action":"write_config","name":"config.toml","content":content,"expected_revision":initial["revision"]})).unwrap_err();
        assert_eq!(error.code, "invalid_request");
        assert_eq!(
            ops.execute(&json!({"action":"read_config","name":"config.toml"}))
                .unwrap()["revision"],
            initial["revision"]
        );
    }
    #[test]
    #[cfg(unix)]
    fn rejects_replaced_config_symlinks() {
        let (dir, ops) = fixture();
        let path = dir.path().join("config.toml");
        let moved = dir.path().join("other.toml");
        fs::rename(&path, &moved).unwrap();
        std::os::unix::fs::symlink(&moved, &path).unwrap();
        assert!(ops
            .execute(&json!({"action":"read_config","name":"config.toml"}))
            .is_err());
    }
}
