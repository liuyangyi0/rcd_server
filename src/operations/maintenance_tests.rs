use super::*;

fn gate(root: &tempfile::TempDir) -> Gate {
    Gate::open(root.path().join("maintenance.sqlite"), "gateway-a")
}

#[test]
fn maintenance_persists_across_restart_and_rejects_stale_owners() {
    let root = tempfile::tempdir().unwrap();
    let first = gate(&root);
    first.enter("job-a", 1).unwrap();
    first.enter("job-a", 1).unwrap();
    assert!(first.admit().is_err());
    assert_eq!(first.enter("job-b", 2).unwrap_err().code, "conflict");
    drop(first);
    let restarted = gate(&root);
    assert_eq!(restarted.snapshot(|| 0)["drained"], true);
    assert_eq!(
        restarted.leave("job-b", 1, || 0).unwrap_err().code,
        "conflict"
    );
    restarted.leave("job-a", 1, || 0).unwrap();
    restarted.leave("job-a", 1, || 0).unwrap(); // lost response is safe
    assert!(restarted.admit().is_ok());
    assert_eq!(restarted.enter("job-a", 1).unwrap_err().code, "conflict");
    restarted.enter("job-b", 2).unwrap();
    assert_eq!(
        restarted.leave("job-a", 1, || 0).unwrap_err().code,
        "conflict"
    );
    assert!(restarted.admit().is_err());
}

#[test]
fn maintenance_waits_for_reservations_and_asynchronous_controls() {
    let root = tempfile::tempdir().unwrap();
    let fence = gate(&root);
    let admission = fence.admit().unwrap();
    fence.enter("job", 1).unwrap();
    assert_eq!(fence.snapshot(|| 0)["drained"], false);
    assert_eq!(fence.leave("job", 1, || 0).unwrap_err().code, "busy");
    drop(admission);
    assert_eq!(fence.snapshot(|| 1)["drained"], false);
    assert_eq!(fence.leave("job", 1, || 1).unwrap_err().code, "busy");
    assert_eq!(fence.snapshot(|| 0)["drained"], true);
    fence.leave("job", 1, || 0).unwrap();
}

#[test]
fn maintenance_storage_failure_blocks_admission_without_claiming_durability() {
    let root = tempfile::tempdir().unwrap();
    let fence = gate(&root);
    let db = Connection::open(root.path().join("maintenance.sqlite")).unwrap();
    db.execute_batch("BEGIN IMMEDIATE").unwrap(); // isolated slow/locked storage
    assert_eq!(
        fence.enter("job", 1).unwrap_err().code,
        "persistence_failed"
    );
    assert!(fence.admit().is_err());
    assert_eq!(fence.snapshot(|| 0)["drained"], false);
    assert_eq!(fence.snapshot(|| 0)["durable"], false);
    db.execute_batch("ROLLBACK").unwrap();
    fence.enter("job", 1).unwrap(); // exact owner retries persistence only
    db.execute_batch("BEGIN IMMEDIATE").unwrap();
    assert_eq!(
        fence.leave("job", 1, || 0).unwrap_err().code,
        "persistence_failed"
    );
    assert!(fence.admit().is_err());
    db.execute_batch("ROLLBACK").unwrap();
    fence.enter("job", 1).unwrap();
    fence.leave("job", 1, || 0).unwrap();
}

#[test]
fn maintenance_missing_record_corruption_and_identity_mismatch_fail_closed() {
    let root = tempfile::tempdir().unwrap();
    let fence = gate(&root);
    fence.enter("job", 1).unwrap();
    let other = Gate::open(root.path().join("maintenance.sqlite"), "different-gateway");
    assert!(other.admit().is_err());
    assert_eq!(other.snapshot(|| 0)["durable"], false);
    let db = Connection::open(root.path().join("maintenance.sqlite")).unwrap();
    db.execute("DELETE FROM maintenance_state", []).unwrap();
    drop(db);
    assert!(gate(&root).admit().is_err()); // must not reinitialize an existing DB
    let corrupt = root.path().join("corrupt.sqlite");
    std::fs::write(&corrupt, b"not a database").unwrap();
    assert!(Gate::open(corrupt, "gateway-a").admit().is_err());
}

#[test]
fn maintenance_entry_and_admission_are_atomic_under_contention() {
    for _ in 0..30 {
        let root = tempfile::tempdir().unwrap();
        let fence = gate(&root);
        let start = Arc::new(std::sync::Barrier::new(2));
        let caller = fence.clone();
        let waiter = start.clone();
        let thread = std::thread::spawn(move || {
            waiter.wait();
            caller.admit()
        });
        start.wait();
        fence.enter("job", 1).unwrap();
        let admission = thread.join().unwrap();
        assert_eq!(
            fence.snapshot(|| 0)["pending_admissions"],
            if admission.is_ok() { 1 } else { 0 }
        );
        assert!(fence.admit().is_err());
        drop(admission);
        assert_eq!(fence.snapshot(|| 0)["drained"], true);
    }
}
