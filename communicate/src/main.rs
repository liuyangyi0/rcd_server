use std::error::Error;
use std::fs::{self, OpenOptions};
use std::path::PathBuf;
use std::sync::Arc;

use flexi_logger::{Cleanup, Criterion, Duplicate, FileSpec, Logger, Naming, WriteMode};
use fs2::FileExt;
use rcd_communicate::{config, points::PointStore, protocol::production_registry, runtime};
use tokio_util::sync::CancellationToken;

fn usage() {
    println!("rcd_communicate {}\nCommands: check-config | run | version\nOptions: --config FILE --state-dir DIR --pki-dir DIR --log-dir DIR\nUnit synchronization receives continuously; pending protocols cannot be enabled.", env!("CARGO_PKG_VERSION"));
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let mut args = std::env::args().skip(1);
    let Some(command) = args.next() else {
        usage();
        return Ok(());
    };
    if ["--help", "-h"].contains(&command.as_str()) {
        usage();
        return Ok(());
    }
    if command == "version" || command == "--version" {
        let option = args.next();
        if args.next().is_some() || !matches!(option.as_deref(), None | Some("--json")) {
            return Err("version accepts only --json".into());
        }
        let info = rcd_communicate::build_info::current();
        if option.is_some() {
            println!("{}", serde_json::to_string(&info)?);
        } else {
            println!("rcd_communicate {}", info.version);
        }
        return Ok(());
    }
    if command != "run" && command != "check-config" {
        return Err("unknown command; use --help".into());
    }
    let mut config_file = PathBuf::from("config.toml");
    let mut state_dir = PathBuf::from("state");
    let mut pki_dir = PathBuf::from("pki");
    let mut log_dir = PathBuf::from("logs");
    let mut seen = std::collections::HashSet::new();
    while let Some(option) = args.next() {
        if !seen.insert(option.clone()) {
            return Err(format!("duplicate argument {option}").into());
        }
        let value = PathBuf::from(args.next().ok_or("option needs a value")?);
        if value.as_os_str().is_empty() {
            return Err("option value cannot be empty".into());
        }
        match option.as_str() {
            "--config" => config_file = value,
            "--state-dir" => state_dir = value,
            "--pki-dir" => pki_dir = value,
            "--log-dir" => log_dir = value,
            _ => return Err(format!("unknown option {option}").into()),
        }
    }
    if command == "check-config" {
        rcd_communicate::operations::ensure_configuration_complete(&config_file)?;
        let loaded = config::load(&config_file)?;
        config::validate_adapters(&loaded.config)?;
        println!(
            "configuration valid; {} points, {} ports; no serial I/O",
            loaded.points.len(),
            loaded.config.ports.len()
        );
        return Ok(());
    }
    // Invalid ordinary configuration must not create runtime directories. A
    // pending pair can be between renames; only recover under the runtime lock.
    if rcd_communicate::operations::ensure_configuration_complete(&config_file).is_ok() {
        let preflight = config::load(&config_file)?;
        config::validate_adapters(&preflight.config)?;
    }
    fs::create_dir_all(&state_dir)?;
    let lock = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(state_dir.join("runtime.lock"))?;
    lock.try_lock_exclusive()
        .map_err(|e| format!("instance state directory already in use: {e}"))?;
    rcd_communicate::operations::recover_configuration(&config_file, &state_dir)?;
    let loaded = config::load(&config_file)?;
    config::validate_adapters(&loaded.config)?;
    let _logger = Logger::try_with_env_or_str("warn,rcd_communicate=info")?
        .log_to_file(
            FileSpec::default()
                .directory(log_dir)
                .basename("rcd_communicate"),
        )
        .duplicate_to_stderr(Duplicate::Info)
        .rotate(
            Criterion::Size(10 * 1024 * 1024),
            Naming::Numbers,
            Cleanup::KeepLogFiles(10),
        )
        .write_mode(WriteMode::BufferAndFlush)
        .start()?;
    let store = Arc::new(PointStore::new(&loaded.config, &loaded.points));
    let operations =
        rcd_communicate::operations::Operations::open(&config_file, &state_dir, &loaded)?;
    let cancel = CancellationToken::new();
    let signal = {
        let cancel = cancel.clone();
        tokio::spawn(async move {
            if let Err(error) = shutdown_signal().await {
                log::error!("signal handler: {error}");
            }
            cancel.cancel();
        })
    };
    let result = runtime::run(
        loaded.config,
        store,
        production_registry(),
        pki_dir,
        cancel.clone(),
        operations,
    )
    .await;
    cancel.cancel();
    signal.abort();
    let _ = signal.await;
    result?;
    log::info!("gateway stopped");
    Ok(())
}

async fn shutdown_signal() -> std::io::Result<()> {
    #[cfg(unix)]
    {
        let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
        tokio::select! { result = tokio::signal::ctrl_c() => result, _ = term.recv() => Ok(()) }
    }
    #[cfg(not(unix))]
    tokio::signal::ctrl_c().await
}
