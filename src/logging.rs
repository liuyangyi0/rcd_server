//! 日志初始化与串口高频帧日志开关。

use std::io::Write;
use std::path::{Path, PathBuf};

use flexi_logger::{
    filter::{LogLineFilter, LogLineWriter},
    Age, Cleanup, Criterion, DeferredNow, Duplicate, FileSpec, Logger, LoggerHandle, Naming,
    Record, WriteMode,
};

const DEFAULT_LOG_SPEC: &str = "info";
const SERIAL_FRAME_LOG_ENV: &str = "RCD_LOG_SERIAL_FRAMES";
const OPCUA_TRANSPORT_ERRORS_ENV: &str = "RCD_LOG_OPCUA_TRANSPORT_ERRORS";

/// 初始化全局 logger。
///
/// 文件日志写入可执行文件同级 `logs/`，默认 `info`，按天滚动。
/// 控制台默认只复制 warn/error；设置 `RUST_LOG` 时控制台复制所有满足
/// log spec 的日志，便于现场临时打开 info 诊断。
pub fn init() -> Option<LoggerHandle> {
    let log_dir = match log_dir() {
        Ok(dir) => dir,
        Err(e) => {
            eprintln!("获取日志目录失败，将只输出到控制台: {e}");
            return init_console_logger();
        }
    };

    let duplicate = if std::env::var_os("RUST_LOG").is_some() {
        Duplicate::All
    } else {
        Duplicate::Warn
    };

    match Logger::try_with_env_or_str(DEFAULT_LOG_SPEC)
        .map(|logger| {
            let logger = logger
                .log_to_file(
                    FileSpec::default()
                        .directory(log_dir)
                        .basename("rcd_server"),
                )
                .rotate(Criterion::Age(Age::Day), Naming::Timestamps, Cleanup::Never)
                .write_mode(WriteMode::BufferAndFlush)
                .format(log_format)
                .duplicate_to_stderr(duplicate);
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
    match Logger::try_with_env_or_str("warn")
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

fn log_dir() -> Result<PathBuf, std::io::Error> {
    let exe = std::env::current_exe()?;
    Ok(log_dir_for_exe(&exe))
}

pub(crate) fn log_dir_for_exe(exe: &Path) -> PathBuf {
    exe.parent()
        .map(|p| p.join("logs"))
        .unwrap_or_else(|| PathBuf::from("logs"))
}

pub(crate) fn serial_frame_logs_enabled() -> bool {
    serial_frame_logs_enabled_from(std::env::var_os(SERIAL_FRAME_LOG_ENV).as_deref())
}

pub(crate) fn serial_frame_logs_enabled_from(value: Option<&std::ffi::OsStr>) -> bool {
    value
        .and_then(|v| v.to_str())
        .map(|v| v.trim() == "1")
        .unwrap_or(false)
}

fn with_opcua_transport_filter(logger: Logger) -> Logger {
    if opcua_transport_errors_enabled() {
        logger
    } else {
        logger.filter(Box::new(OpcuaTransportDisconnectFilter))
    }
}

fn opcua_transport_errors_enabled() -> bool {
    env_flag_enabled(std::env::var_os(OPCUA_TRANSPORT_ERRORS_ENV).as_deref())
}

fn env_flag_enabled(value: Option<&std::ffi::OsStr>) -> bool {
    value
        .and_then(|v| v.to_str())
        .map(|v| v.trim() == "1")
        .unwrap_or(false)
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
        assert_eq!(log_dir_for_exe(&exe), PathBuf::from(r"C:\deploy\logs"));
    }

    #[test]
    fn serial_frame_logs_disabled_when_env_missing() {
        assert!(!serial_frame_logs_enabled_from(None));
    }

    #[test]
    fn serial_frame_logs_enabled_only_for_one() {
        assert!(serial_frame_logs_enabled_from(Some(OsStr::new("1"))));
        assert!(!serial_frame_logs_enabled_from(Some(OsStr::new("true"))));
        assert!(!serial_frame_logs_enabled_from(Some(OsStr::new("0"))));
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
