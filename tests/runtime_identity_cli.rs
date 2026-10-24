//! CLI checks run only in an isolated directory, before any acquisition starts.
use std::fs;
use std::process::Command;

#[test]
fn version_never_loads_configuration_or_creates_runtime_files() {
    let root = tempfile::tempdir().unwrap();
    fs::write(root.path().join("config.toml"), "not valid TOML [").unwrap();
    let before = fs::read_dir(root.path()).unwrap().count();
    let output = Command::new(env!("CARGO_BIN_EXE_rcd_server"))
        .args(["version", "--json"])
        .current_dir(root.path())
        .env("RCD_CONFIG_DIR", root.path().join("missing-config"))
        .env("RCD_STATE_DIR", root.path().join("missing-state"))
        .env("RCD_PKI_DIR", root.path().join("missing-pki"))
        .env("RCD_LOG_DIR", root.path().join("missing-log"))
        .env("RCD_CONFIG", root.path().join("config.toml"))
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let value: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(value["schema_version"], 1);
    assert_eq!(value["component"], "lower");
    assert!(value["source_sha256"].is_string());
    assert_eq!(before, fs::read_dir(root.path()).unwrap().count());
}

#[test]
fn missing_explicit_config_fails_without_fallback_or_initialization() {
    let root = tempfile::tempdir().unwrap();
    let output = Command::new(env!("CARGO_BIN_EXE_rcd_server"))
        .arg("--config-dir")
        .arg(root.path().join("missing-config"))
        .current_dir(root.path())
        .env_remove("RCD_STATE_DIR")
        .env_remove("RCD_PKI_DIR")
        .env_remove("RCD_LOG_DIR")
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("explicit runtime directory"));
    assert_eq!(fs::read_dir(root.path()).unwrap().count(), 0);
}

#[test]
fn malformed_version_options_fail_before_startup() {
    let root = tempfile::tempdir().unwrap();
    let output = Command::new(env!("CARGO_BIN_EXE_rcd_server"))
        .args(["version", "--json", "--config-dir=x"])
        .current_dir(root.path())
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("usage:"));
    assert_eq!(fs::read_dir(root.path()).unwrap().count(), 0);
}
