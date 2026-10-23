//! Virtual configuration file over Operations v1. A durable intent precedes
//! either physical replacement; startup completes interrupted saves before OPC.
use super::*;
use serde::Deserialize;

pub(super) const NAME: &str = "configuration-set.toml";
const JOURNAL: &str = ".rcd-config-set.pending.json";

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(super) struct Contents {
    pub config_toml: String,
    pub points_csv: String,
}
impl Contents {
    pub fn render(&self) -> Result<String> {
        let body =
            toml::to_string(self).map_err(|e| Error::new("invalid_request", e.to_string()))?;
        Ok(format!("# 成套配置：同时编辑 config_toml 主配置与 points_csv 点表。\n# 两份共同校验、共同版本检查；保存后需显式重启，不自动启用串口。\n# 不得改变 instance_id 或 points_file。不要删除多行字符串的引号。\n{body}"))
    }
    pub fn parse(content: &str) -> Result<Self> {
        if content.len() > MAX_BYTES || content.contains('\0') {
            return Err(Error::new("too_large", "成套配置最大 2 MiB，不能包含 NUL"));
        }
        toml::from_str(content).map_err(|e| Error::new("invalid_request", e.to_string()))
    }
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Intent {
    version: u32,
    before: Contents,
    after: Contents,
}

fn journal(config: &Path) -> PathBuf {
    config.with_file_name(JOURNAL)
}
pub(super) fn pending(config: &Path) -> bool {
    fs::symlink_metadata(journal(config)).is_ok()
}

fn replace(path: &Path, content: &str) -> Result<()> {
    regular(path)?;
    let parent = path
        .parent()
        .ok_or_else(|| Error::new("invalid_request", "缺少配置目录"))?;
    let mut tmp = tempfile::NamedTempFile::new_in(parent)?;
    tmp.as_file()
        .set_permissions(fs::metadata(path)?.permissions())?;
    tmp.write_all(content.as_bytes())?;
    tmp.as_file().sync_all()?;
    tmp.persist(path).map_err(|e| Error::from(e.error))?;
    sync_dir(parent)
}

fn finish(config: &Path, points: &Path, intent: &Intent) -> Result<()> {
    // Check both halves before writing either; never replace an external edit.
    let config_now = read(config)?;
    let points_now = read(points)?;
    if (config_now != intent.before.config_toml && config_now != intent.after.config_toml)
        || (points_now != intent.before.points_csv && points_now != intent.after.points_csv)
    {
        return Err(Error::new(
            "conflict",
            "成套保存未完成且文件被外部修改；保留事务，请人工核对",
        ));
    }
    if points_now != intent.after.points_csv {
        replace(points, &intent.after.points_csv)?;
    }
    if config_now != intent.after.config_toml {
        replace(config, &intent.after.config_toml)?;
    }
    fs::remove_file(journal(config))?;
    sync_dir(config.parent().unwrap())
}

pub(super) fn commit(
    config: &Path,
    points: &Path,
    before: Contents,
    after: Contents,
) -> Result<()> {
    if pending(config) {
        return Err(Error::new(
            "conflict",
            "存在未完成的成套保存，需重启恢复后再编辑",
        ));
    }
    let intent = Intent {
        version: 1,
        before,
        after,
    };
    let parent = config.parent().unwrap();
    let mut tmp = tempfile::NamedTempFile::new_in(parent)?;
    serde_json::to_writer(tmp.as_file_mut(), &intent)
        .map_err(|e| Error::new("io_error", e.to_string()))?;
    tmp.as_file().sync_all()?;
    tmp.persist_noclobber(journal(config))
        .map_err(|e| Error::from(e.error))?;
    sync_dir(parent)?;
    finish(config, points, &intent)
}

/// Caller holds the runtime lock. No serial or network access.
pub(super) fn recover(path: &Path, state: &Path) -> Result<()> {
    regular(path)?;
    let path = path.canonicalize()?;
    if !pending(&path) {
        return Ok(());
    }
    let backups = directory(&state.join("config-backups"))?;
    let _lock = write_lock(&backups)?;
    let pending_path = journal(&path);
    regular(&pending_path)?;
    let mut bytes = Vec::new();
    File::open(&pending_path)?
        .take(13 * 1024 * 1024 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > 13 * 1024 * 1024 {
        return Err(Error::new("too_large", "配置事务文件超限"));
    }
    let intent: Intent = serde_json::from_slice(&bytes)
        .map_err(|e| Error::new("invalid_request", format!("配置事务损坏，禁止启动：{e}")))?;
    if intent.version != 1 {
        return Err(Error::new("invalid_request", "未知配置事务版本"));
    }
    let before = config::parse(&intent.before.config_toml, &intent.before.points_csv)
        .map_err(|e| Error::new("invalid_request", e.to_string()))?;
    let after = config::parse(&intent.after.config_toml, &intent.after.points_csv)
        .map_err(|e| Error::new("invalid_request", e.to_string()))?;
    if before.config.instance_id != after.config.instance_id
        || before.config.points_file != after.config.points_file
    {
        return Err(Error::new(
            "invalid_request",
            "配置事务改变了文件绑定或实例身份",
        ));
    }
    config::validate_adapters(&after.config)
        .map_err(|e| Error::new("invalid_request", e.to_string()))?;
    let points = if before.config.points_file.is_absolute() {
        before.config.points_file
    } else {
        path.parent().unwrap().join(before.config.points_file)
    };
    regular(&points)?;
    let points = points.canonicalize()?;
    if points == path {
        return Err(Error::new("invalid_request", "主配置和点表必须不同"));
    }
    finish(&path, &points, &intent)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn interrupted_commit_recovers_both_halves_and_refuses_external_edits() {
        for phase in 0..4 {
            let (dir, ops) = super::super::tests::fixture();
            let repo = ops.0.lock().unwrap();
            let before = repo.contents().unwrap();
            let mut after = before.clone();
            after.config_toml.push_str("\n# committed\n");
            after.points_csv.push('\n');
            let intent = Intent {
                version: 1,
                before,
                after: after.clone(),
            };
            let path = &repo.files["config.toml"];
            fs::write(journal(path), serde_json::to_vec(&intent).unwrap()).unwrap();
            if phase >= 1 {
                replace(&repo.files["points.csv"], &after.points_csv).unwrap();
            }
            if phase >= 2 {
                replace(path, &after.config_toml).unwrap();
            }
            if phase == 3 {
                fs::write(path, format!("{}\n# user edit", after.config_toml)).unwrap();
            }
            let result = recover(path, &dir.path().join("state"));
            if phase == 3 {
                assert_eq!(result.unwrap_err().code, "conflict");
                assert!(pending(path));
                assert!(read(path).unwrap().ends_with("# user edit"));
            } else {
                result.unwrap();
                assert_eq!(repo.contents().unwrap(), after);
                assert!(!pending(path));
                config::load(path).unwrap();
            }
        }
    }
}
