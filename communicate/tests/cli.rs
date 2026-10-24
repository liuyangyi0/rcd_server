use std::process::Command;

#[test]
fn version_identity_is_read_only_and_identifies_the_communicate_component() {
    let directory = tempfile::tempdir().unwrap();
    for command in ["version", "--version"] {
        let output = Command::new(env!("CARGO_BIN_EXE_rcd_communicate"))
            .current_dir(directory.path())
            .args([command, "--json"])
            .output()
            .unwrap();
        assert!(output.status.success());
        let info: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
        assert_eq!(info["schema_version"], 1);
        assert_eq!(info["component"], "communicate");
        assert!(info["version"].as_str().is_some_and(|s| !s.is_empty()));
        assert!(info["dirty"].is_boolean());
    }
    let bad = Command::new(env!("CARGO_BIN_EXE_rcd_communicate"))
        .current_dir(directory.path())
        .args(["version", "--json", "--config", "anything"])
        .output()
        .unwrap();
    assert!(!bad.status.success());
    assert_eq!(std::fs::read_dir(directory.path()).unwrap().count(), 0);
}

struct ChildGuard(std::process::Child);
impl Drop for ChildGuard {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

#[test]
fn startup_recovers_configuration_pair_only_after_exclusive_lock() {
    use fs2::FileExt;
    use std::{
        fs,
        io::Write,
        time::{Duration, Instant},
    };
    let dir = tempfile::tempdir().unwrap();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    let original = include_str!("../examples/unit1-receiver.toml")
        .replace("unit1-receiver.csv", "points.csv")
        .replace("port = 14842", &format!("port = {port}"));
    let points = include_str!("../examples/unit1-receiver.csv");
    let next = format!("{original}\n# paired save\n");
    let next_points = format!("{points}\n");
    fs::write(dir.path().join("config.toml"), &original).unwrap();
    fs::write(dir.path().join("points.csv"), &next_points).unwrap();
    let pending = dir.path().join(".rcd-config-set.pending.json");
    let journal = serde_json::json!({"version":1,"before":{"config_toml":original,"points_csv":points},"after":{"config_toml":next,"points_csv":next_points}}).to_string();
    fs::write(&pending, &journal).unwrap();
    let checked = Command::new(env!("CARGO_BIN_EXE_rcd_communicate"))
        .current_dir(dir.path())
        .arg("check-config")
        .output()
        .unwrap();
    assert!(!checked.status.success());
    assert_eq!(fs::read_to_string(&pending).unwrap(), journal);
    assert!(!dir.path().join("state").exists());
    fs::create_dir(dir.path().join("state")).unwrap();
    let mut lock = fs::File::create(dir.path().join("state/runtime.lock")).unwrap();
    lock.write_all(b"test lock").unwrap();
    lock.try_lock_exclusive().unwrap();
    let locked = Command::new(env!("CARGO_BIN_EXE_rcd_communicate"))
        .current_dir(dir.path())
        .arg("run")
        .output()
        .unwrap();
    assert!(!locked.status.success());
    assert!(String::from_utf8_lossy(&locked.stderr).contains("already in use"));
    assert_eq!(fs::read_to_string(&pending).unwrap(), journal);
    drop(lock);
    drop(listener);
    let log_path = dir.path().join("startup.log");
    let log = fs::File::create(&log_path).unwrap();
    let mut child = ChildGuard(
        Command::new(env!("CARGO_BIN_EXE_rcd_communicate"))
            .current_dir(dir.path())
            .arg("run")
            .stdout(log.try_clone().unwrap())
            .stderr(log)
            .spawn()
            .unwrap(),
    );
    // This integration test also creates a fresh OPC certificate/address space.
    // Its startup budget is separate from serial/configuration deadlines.
    let deadline = Instant::now() + Duration::from_secs(45);
    loop {
        assert!(
            child.0.try_wait().unwrap().is_none(),
            "gateway exited before OPC ready"
        );
        if std::net::TcpStream::connect(("127.0.0.1", port)).is_ok() {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "OPC did not start after recovery; pending={}; log={}",
            pending.exists(),
            fs::read_to_string(&log_path).unwrap()
        );
        std::thread::sleep(Duration::from_millis(20));
    }
    assert!(!pending.exists());
    assert_eq!(
        fs::read_to_string(dir.path().join("config.toml")).unwrap(),
        next
    );
    assert_eq!(
        fs::read_to_string(dir.path().join("points.csv")).unwrap(),
        next_points
    );
}

#[test]
fn validation_is_read_only_and_active_pending_is_rejected_before_startup() {
    let dir = tempfile::tempdir().unwrap();
    let config = dir.path().join("config.toml");
    std::fs::write(&config, include_str!("../examples/config.toml")).unwrap();
    std::fs::write(
        dir.path().join("points.csv"),
        include_str!("../examples/points.csv"),
    )
    .unwrap();
    let output = Command::new(env!("CARGO_BIN_EXE_rcd_communicate"))
        .current_dir(dir.path())
        .args(["check-config", "--config", "config.toml"])
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    for name in ["state", "pki", "logs"] {
        assert!(!dir.path().join(name).exists());
    }
    std::fs::write(
        &config,
        include_str!("../examples/config.toml").replace("enabled = false", "enabled = true"),
    )
    .unwrap();
    let output = Command::new(env!("CARGO_BIN_EXE_rcd_communicate"))
        .current_dir(dir.path())
        .args(["run", "--config", "config.toml"])
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("pending"));
    for name in ["state", "pki", "logs"] {
        assert!(!dir.path().join(name).exists());
    }
}
