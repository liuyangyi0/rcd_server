//! 日志初始化与串口高频帧日志开关。

use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};

use flexi_logger::{
    filter::{LogLineFilter, LogLineWriter},
    Age, Cleanup, Criterion, DeferredNow, Duplicate, FileSpec, Logger, LoggerHandle, Naming,
    Record, WriteMode,
};
use serde::Deserialize;

const DEFAULT_LOG_SPEC: &str = "info";
const DEFAULT_LOG_BASENAME: &str = "rcd_server";
const SERIAL_FRAME_LOG_ENV: &str = "RCD_LOG_SERIAL_FRAMES";
const OPCUA_TRANSPORT_ERRORS_ENV: &str = "RCD_LOG_OPCUA_TRANSPORT_ERRORS";
const LOG_CONFIG_PATH_ENV: &str = "RCD_CONFIG";

static SERIAL_FRAME_LOGS_ENABLED: AtomicBool = AtomicBool::new(false);
static OPCUA_TRANSPORT_ERRORS_ENABLED: AtomicBool = AtomicBool::new(false);

/// 日志配置，对应 `config.toml` 的 `[logging]` 段。
#[derive(Deserialize, Debug, Clone)]
#[serde(default)]
pub struct LoggingConfig {
    /// 文件日志级别或 log spec，例如 `info`、`debug`、`rcd_server=debug,async_opcua=warn`。
    pub level: String,
    /// 日志目录。相对路径按可执行文件同级目录解析。
    pub directory: Option<String>,
    /// 日志文件基础名。
    pub basename: String,
    /// 控制台复制级别：none/error/warn/info/debug/trace/all。
    pub duplicate_to_stderr: Option<String>,
    /// 滚动周期：day/hour/minute/second/none。
    pub rotate_age: String,
    /// 滚动大小阈值，单位字节；未配置则只按时间滚动。
    pub rotate_size_bytes: Option<u64>,
    /// 保留滚动日志文件数；0 表示不清理。
    pub keep_files: usize,
    /// 写入模式：direct/buffer_and_flush/buffer_dont_flush。
    pub write_mode: String,
    /// 是否记录串口收发原始帧。
    pub serial_frame_logs: bool,
    /// 是否保留 OPC UA 客户端断开连接类传输错误。
    pub opcua_transport_errors: bool,
}

impl Default for LoggingConfig {
    fn default() -> Self {
        Self {
            level: DEFAULT_LOG_SPEC.to_string(),
            directory: None,
            basename: DEFAULT_LOG_BASENAME.to_string(),
            duplicate_to_stderr: None,
            rotate_age: "day".to_string(),
            rotate_size_bytes: None,
            keep_files: 30,
            write_mode: "buffer_and_flush".to_string(),
            serial_frame_logs: false,
            opcua_transport_errors: false,
        }
    }
}

#[derive(Deserialize, Debug, Default)]
#[serde(default)]
struct LoggingFileConfig {
    logging: LoggingConfig,
}

/// 初始化全局 logger。
///
/// 默认读取可执行文件同级 `config/config.toml` 的 `[logging]` 段。
/// `RUST_LOG` 会覆盖 `logging.level`，便于现场临时调整模块级别。
pub fn init() -> Option<LoggerHandle> {
    init_with_config(load_config_from_file().unwrap_or_else(|e| {
        eprintln!("读取日志配置失败，使用默认日志配置: {e}");
        LoggingConfig::default()
    }))
}

pub fn init_with_config(config: LoggingConfig) -> Option<LoggerHandle> {
    SERIAL_FRAME_LOGS_ENABLED.store(config.serial_frame_logs, Ordering::Relaxed);
    OPCUA_TRANSPORT_ERRORS_ENABLED.store(config.opcua_transport_errors, Ordering::Relaxed);

    let log_dir = match log_dir(config.directory.as_deref()) {
        Ok(dir) => dir,
        Err(e) => {
            eprintln!("获取日志目录失败，将只输出到控制台: {e}");
            return init_console_logger();
        }
    };

    let duplicate = duplicate_to_stderr(&config);
    let write_mode = write_mode_from(&config.write_mode);

    match Logger::try_with_env_or_str(config.level.as_str())
        .map(|logger| {
            let file_spec = FileSpec::default()
                .directory(log_dir)
                .basename(config.basename.as_str());
            let logger = apply_rotation(
                logger
                    .log_to_file(file_spec)
                    .write_mode(write_mode)
                    .format(log_format)
                    .duplicate_to_stderr(duplicate),
                &config,
            );
            with_opcua_transport_filter(logger)
        })
        .and_then(|logger| logger.start())
    {
        Ok(handle) => Some(handle),
        Err(e) => {
            eprintln!("初始化文件日志失败，将只输出到控制台: {e}");
            init_console_logger()
        }
    }
}

fn init_console_logger() -> Option<LoggerHandle> {
    // 不再读取配置或 RUST_LOG，确保正是这些值格式错误时仍能保留诊断日志。
    match Logger::try_with_str("warn")
        .map(|logger| with_opcua_transport_filter(logger.log_to_stderr().format(log_format)))
        .and_then(|logger| logger.start())
    {
        Ok(handle) => Some(handle),
        Err(e) => {
            eprintln!("初始化控制台日志失败: {e}");
            None
        }
    }
}

fn load_config_from_file() -> Result<LoggingConfig, Box<dyn std::error::Error>> {
    let path = configured_config_path()?;
    if !path.exists() {
        return Ok(LoggingConfig::default());
    }
    let config_str = std::fs::read_to_string(path)?;
    let config: LoggingFileConfig = toml::from_str(&config_str)?;
    Ok(config.logging)
}

fn configured_config_path() -> Result<PathBuf, std::io::Error> {
    if let Some(path) = std::env::var_os(LOG_CONFIG_PATH_ENV) {
        return Ok(PathBuf::from(path));
    }
    let exe = std::env::current_exe()?;
    Ok(config_path_for_exe(&exe))
}

pub(crate) fn config_path_for_exe(exe: &Path) -> PathBuf {
    exe.parent()
        .map(|p| p.join("config").join("config.toml"))
        .unwrap_or_else(|| PathBuf::from("config").join("config.toml"))
}

fn log_dir(configured_dir: Option<&str>) -> Result<PathBuf, std::io::Error> {
    let exe = std::env::current_exe()?;
    Ok(log_dir_for_exe(&exe, configured_dir))
}

pub(crate) fn log_dir_for_exe(exe: &Path, configured_dir: Option<&str>) -> PathBuf {
    match configured_dir.map(str::trim).filter(|dir| !dir.is_empty()) {
        Some(dir) => {
            let dir = PathBuf::from(dir);
            if dir.is_absolute() {
                dir
            } else {
                exe.parent()
                    .map(|p| p.join(&dir))
                    .unwrap_or_else(|| dir.clone())
            }
        }
        None => exe
            .parent()
            .map(|p| p.join("logs"))
            .unwrap_or_else(|| PathBuf::from("logs")),
    }
}

pub(crate) fn serial_frame_logs_enabled() -> bool {
    env_flag_value(std::env::var_os(SERIAL_FRAME_LOG_ENV).as_deref())
        .unwrap_or_else(|| SERIAL_FRAME_LOGS_ENABLED.load(Ordering::Relaxed))
}

#[cfg(test)]
pub(crate) fn serial_frame_logs_enabled_from(value: Option<&std::ffi::OsStr>) -> Option<bool> {
    env_flag_value(value)
}

fn duplicate_to_stderr(config: &LoggingConfig) -> Duplicate {
    match config.duplicate_to_stderr.as_deref() {
        Some(value) => parse_duplicate(value).unwrap_or_else(|| {
            eprintln!("无效的 logging.duplicate_to_stderr={value:?}，回退到 warn");
            Duplicate::Warn
        }),
        None if std::env::var_os("RUST_LOG").is_some() => Duplicate::All,
        None => Duplicate::Warn,
    }
}

fn parse_duplicate(value: &str) -> Option<Duplicate> {
    match value.trim().to_ascii_lowercase().as_str() {
        "none" | "off" => Some(Duplicate::None),
        "error" => Some(Duplicate::Error),
        "warn" | "warning" => Some(Duplicate::Warn),
        "info" => Some(Duplicate::Info),
        "debug" => Some(Duplicate::Debug),
        "trace" => Some(Duplicate::Trace),
        "all" => Some(Duplicate::All),
        _ => None,
    }
}

fn write_mode_from(value: &str) -> WriteMode {
    match value.trim().to_ascii_lowercase().as_str() {
        "direct" => WriteMode::Direct,
        "buffer_dont_flush" | "buffer-dont-flush" => WriteMode::BufferDontFlush,
        "buffer_and_flush" | "buffer-and-flush" | "" => WriteMode::BufferAndFlush,
        other => {
            eprintln!("无效的 logging.write_mode={other:?}，回退到 buffer_and_flush");
            WriteMode::BufferAndFlush
        }
    }
}

fn apply_rotation(logger: Logger, config: &LoggingConfig) -> Logger {
    match rotation_criterion(config) {
        Some(criterion) => logger.rotate(
            criterion,
            Naming::Timestamps,
            cleanup_from(config.keep_files),
        ),
        None => logger,
    }
}

fn rotation_criterion(config: &LoggingConfig) -> Option<Criterion> {
    let age = age_from(&config.rotate_age);
    match (age, config.rotate_size_bytes) {
        (Some(age), Some(size)) => Some(Criterion::AgeOrSize(age, size)),
        (Some(age), None) => Some(Criterion::Age(age)),
        (None, Some(size)) => Some(Criterion::Size(size)),
        (None, None) => None,
    }
}

fn age_from(value: &str) -> Option<Age> {
    match value.trim().to_ascii_lowercase().as_str() {
        "day" | "daily" | "" => Some(Age::Day),
        "hour" | "hourly" => Some(Age::Hour),
        "minute" => Some(Age::Minute),
        "second" => Some(Age::Second),
        "none" | "off" => None,
        other => {
            eprintln!("无效的 logging.rotate_age={other:?}，回退到 day");
            Some(Age::Day)
        }
    }
}

fn cleanup_from(keep_files: usize) -> Cleanup {
    if keep_files == 0 {
        Cleanup::Never
    } else {
        Cleanup::KeepLogFiles(keep_files)
    }
}

fn env_flag_value(value: Option<&std::ffi::OsStr>) -> Option<bool> {
    value.and_then(|v| v.to_str()).and_then(parse_bool_flag)
}

fn parse_bool_flag(value: &str) -> Option<bool> {
    match value.trim().to_ascii_lowercase().as_str() {
        "1" | "true" | "yes" | "on" => Some(true),
        "0" | "false" | "no" | "off" => Some(false),
        _ => None,
    }
}

fn with_opcua_transport_filter(logger: Logger) -> Logger {
    if opcua_transport_errors_enabled() {
        logger
    } else {
        logger.filter(Box::new(OpcuaTransportDisconnectFilter))
    }
}

fn opcua_transport_errors_enabled() -> bool {
    env_flag_value(std::env::var_os(OPCUA_TRANSPORT_ERRORS_ENV).as_deref())
        .unwrap_or_else(|| OPCUA_TRANSPORT_ERRORS_ENABLED.load(Ordering::Relaxed))
}

struct OpcuaTransportDisconnectFilter;

impl LogLineFilter for OpcuaTransportDisconnectFilter {
    fn write(
        &self,
        now: &mut DeferredNow,
        record: &Record,
        log_line_writer: &dyn LogLineWriter,
    ) -> std::io::Result<()> {
        if !is_async_opcua_transport_target(record.target()) {
            return log_line_writer.write(now, record);
        }
        let message = record.args().to_string();
        if should_suppress_opcua_transport_log(record.target(), &message, false) {
            Ok(())
        } else {
            log_line_writer.write(now, record)
        }
    }
}

fn should_suppress_opcua_transport_log(
    target: &str,
    message: &str,
    opcua_transport_errors_enabled: bool,
) -> bool {
    if opcua_transport_errors_enabled || !is_async_opcua_transport_target(target) {
        return false;
    }

    message.contains("Connection reset by peer")
        || message.contains("Broken pipe")
        || message.contains("BadConnectionClosed")
}

fn is_async_opcua_transport_target(target: &str) -> bool {
    target.contains("opcua")
        && (target.contains("transport::tcp") || target.contains("session::controller"))
}

fn log_format(w: &mut dyn Write, now: &mut DeferredNow, record: &Record) -> std::io::Result<()> {
    writeln!(
        w,
        "{} [{:<5}] {} {}",
        now.now().format("%Y-%m-%d %H:%M:%S%.3f"),
        record.level(),
        record.target(),
        record.args()
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ffi::OsStr;
    use std::path::PathBuf;

    #[test]
    fn log_dir_is_next_to_exe() {
        let exe = PathBuf::from(r"C:\deploy\rcd_server.exe");
        assert_eq!(
            log_dir_for_exe(&exe, None),
            PathBuf::from(r"C:\deploy\logs")
        );
    }

    #[test]
    fn relative_log_dir_is_next_to_exe() {
        let exe = PathBuf::from(r"C:\deploy\rcd_server.exe");
        assert_eq!(
            log_dir_for_exe(&exe, Some("runtime_logs")),
            PathBuf::from(r"C:\deploy\runtime_logs")
        );
    }

    #[test]
    fn config_path_is_next_to_exe() {
        let exe = PathBuf::from(r"C:\deploy\rcd_server.exe");
        assert_eq!(
            config_path_for_exe(&exe),
            PathBuf::from(r"C:\deploy\config\config.toml")
        );
    }

    #[test]
    fn serial_frame_logs_disabled_when_env_missing() {
        assert_eq!(serial_frame_logs_enabled_from(None), None);
    }

    #[test]
    fn serial_frame_logs_parse_common_bool_values() {
        assert_eq!(
            serial_frame_logs_enabled_from(Some(OsStr::new("1"))),
            Some(true)
        );
        assert_eq!(
            serial_frame_logs_enabled_from(Some(OsStr::new("true"))),
            Some(true)
        );
        assert_eq!(
            serial_frame_logs_enabled_from(Some(OsStr::new("on"))),
            Some(true)
        );
        assert_eq!(
            serial_frame_logs_enabled_from(Some(OsStr::new("0"))),
            Some(false)
        );
        assert_eq!(
            serial_frame_logs_enabled_from(Some(OsStr::new("false"))),
            Some(false)
        );
    }

    #[test]
    fn duplicate_level_parses_config_values() {
        assert!(matches!(parse_duplicate("none"), Some(Duplicate::None)));
        assert!(matches!(parse_duplicate("warn"), Some(Duplicate::Warn)));
        assert!(matches!(parse_duplicate("all"), Some(Duplicate::All)));
        assert!(parse_duplicate("verbose").is_none());
    }

    #[test]
    fn rotation_supports_age_size_and_disabled() {
        let mut config = LoggingConfig::default();
        assert!(matches!(
            rotation_criterion(&config),
            Some(Criterion::Age(Age::Day))
        ));

        config.rotate_size_bytes = Some(10 * 1024 * 1024);
        assert!(matches!(
            rotation_criterion(&config),
            Some(Criterion::AgeOrSize(Age::Day, 10_485_760))
        ));

        config.rotate_age = "none".to_string();
        assert!(matches!(
            rotation_criterion(&config),
            Some(Criterion::Size(10_485_760))
        ));

        config.rotate_size_bytes = None;
        assert!(rotation_criterion(&config).is_none());
    }

    #[test]
    fn opcua_transport_disconnect_logs_are_suppressed_by_default() {
        assert!(should_suppress_opcua_transport_log(
            "opcua_server::transport::tcp",
            "Error reading from stream Os { message: \"Connection reset by peer\" }",
            false,
        ));
        assert!(should_suppress_opcua_transport_log(
            "opcua_server::transport::tcp",
            "write bytes task failed: Broken pipe (os error 32)",
            false,
        ));
        assert!(should_suppress_opcua_transport_log(
            "opcua_server::session::controller",
            "Fatal transport error: BadConnectionClosed",
            false,
        ));
    }

    #[test]
    fn opcua_transport_disconnect_logs_can_be_enabled() {
        assert!(!should_suppress_opcua_transport_log(
            "opcua_server::transport::tcp",
            "write bytes task failed: Broken pipe (os error 32)",
            true,
        ));
    }

    #[test]
    fn opcua_transport_filter_keeps_other_errors() {
        assert!(!should_suppress_opcua_transport_log(
            "opcua_server::transport::tcp",
            "TLS certificate validation failed",
            false,
        ));
        assert!(!should_suppress_opcua_transport_log(
            "rcd_server::server::opcua",
            "BadConnectionClosed",
            false,
        ));
    }
}
