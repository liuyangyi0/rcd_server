use super::*;

#[test]
fn historical_identity_never_replays_when_cold_lookup_fails_after_restart() {
    let root = tempfile::tempdir().unwrap();
    let path = root.path().join("journal.sqlite");
    let tracker = Tracker::open(path.clone());
    tracker.begin("manager", "old", "TEST", 1, "1108").unwrap();
    tracker.update("TEST", Some(1), |v| {
        v.phase = "AckReceived".into();
        v.accepted = true;
    });
    assert!(tracker.flush(Duration::from_secs(2)));
    drop(tracker);
    std::thread::sleep(Duration::from_millis(50));
    let restarted = Tracker::open(path.clone());
    let db = Connection::open(path).unwrap();
    db.pragma_update(None, "user_version", 2).unwrap();
    assert!(restarted.get("manager", "old").is_err());
    let error = restarted
        .begin("manager", "old", "TEST", 2, "1108")
        .unwrap_err();
    assert!(error.contains("never replayed"), "{error}");
    // Availability policy still applies to identities proved new at startup.
    let (new, admitted) = restarted
        .begin("manager", "new", "TEST", 3, "1108")
        .unwrap();
    assert!(admitted && new.audit_incomplete);
    assert!(
        !restarted
            .begin("manager", "new", "TEST", 4, "1108")
            .unwrap()
            .1
    );
}

#[test]
fn unreadable_historical_inventory_is_not_assumed_empty() {
    let root = tempfile::tempdir().unwrap();
    let path = root.path().join("journal.sqlite");
    std::fs::write(&path, b"corrupted existing journal").unwrap();
    let tracker = Tracker::open(path);
    assert!(tracker
        .begin("manager", "unknown", "TEST", 1, "1108")
        .unwrap_err()
        .contains("history unavailable"));
}

fn fixture_snapshot() -> Snapshot {
    serde_json::from_str(include_str!(
        "../contracts/command-tracking/v1/ack-received.json"
    ))
    .unwrap()
}

#[test]
fn rejected_full_cache_does_not_burn_command_identity() {
    let dir = tempfile::tempdir().unwrap();
    let tracker = Tracker::open(dir.path().join("missing/journal.sqlite"));
    {
        let mut state = tracker.0.state.lock().unwrap();
        let mut value = fixture_snapshot();
        value.phase = "Executing".into();
        for i in 0..CACHE_LIMIT {
            value.command_id = format!("active-{i}");
            state.entries.insert(value.key(), value.clone());
        }
    }
    assert_eq!(
        tracker
            .begin("a", "retry-after-capacity", "COM1", 1, "1108")
            .unwrap_err(),
        "too many active tracked commands"
    );
    {
        let mut state = tracker.0.state.lock().unwrap();
        let first = state.entries.keys().next().unwrap().clone();
        state.entries.remove(&first);
    }
    assert!(
        tracker
            .begin("a", "retry-after-capacity", "COM1", 2, "1108")
            .unwrap()
            .1
    );
    assert_eq!(tracker.0.state.lock().unwrap().entries.len(), CACHE_LIMIT);
}

#[test]
fn degraded_cache_eviction_marks_gap_and_never_replays_evicted_identity() {
    let dir = tempfile::tempdir().unwrap();
    let tracker = Tracker::open(dir.path().join("missing/journal.sqlite"));
    tracker.begin("a", "oldest", "COM1", 1, "1108").unwrap();
    tracker.update("COM1", Some(1), |v| v.phase = "AckReceived".into());
    {
        let mut state = tracker.0.state.lock().unwrap();
        let mut value = fixture_snapshot();
        value.phase = "Executing".into();
        for i in 1..CACHE_LIMIT {
            value.command_id = format!("active-{i}");
            state.entries.insert(value.key(), value.clone());
        }
    }
    assert!(tracker.begin("a", "new", "COM1", 2, "1108").unwrap().1);
    assert_eq!(tracker.identity()["history_gap"], true);
    assert_eq!(tracker.0.state.lock().unwrap().entries.len(), CACHE_LIMIT);
    let error = tracker.begin("a", "oldest", "COM1", 3, "1108").unwrap_err();
    assert!(error.contains("never replayed"), "{error}");
}

#[test]
fn retention_keeps_active_commands_and_durable_cursor_head() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("journal.sqlite");
    let (db, _, _) = open_database(&path, "test-boot", None).unwrap();
    let mut value = fixture_snapshot();
    for i in 1..=5 {
        value.command_id = format!("command-{i}");
        value.revision = i;
        value.phase = if i == 1 { "Executing" } else { "AckReceived" }.into();
        value.updated_at = if i <= 2 {
            "2020-01-01T00:00:00Z"
        } else {
            "2026-09-22T00:00:00Z"
        }
        .into();
        save(&db, &value).unwrap();
    }
    prune_journal(&db, "2026-09-15T00:00:00Z", 2).unwrap();
    let count = |table: &str| {
        db.query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |r| {
            r.get::<_, u64>(0)
        })
        .unwrap()
    };
    assert_eq!(count("commands"), 3); // active + newest two completed
    assert_eq!(count("events"), 3);
    assert_eq!(
        db.query_row("SELECT id FROM commands WHERE finished=0", [], |r| r
            .get::<_, String>(0))
            .unwrap(),
        "command-1"
    );
    prune_journal(&db, "2099-01-01T00:00:00Z", 0).unwrap();
    assert_eq!(count("commands"), 1);
    assert_eq!(count("events"), 1);
    assert_eq!(
        db.query_row("SELECT head FROM journal_meta WHERE id=1", [], |r| r
            .get::<_, u64>(0))
            .unwrap(),
        5
    );
}

#[test]
fn command_snapshot_matches_shared_contract() {
    let raw = include_str!("../contracts/command-tracking/v1/ack-received.json");
    let snapshot: Snapshot = serde_json::from_str(raw).unwrap();
    assert_eq!(snapshot.ack_id, u64::MAX.to_string());
    assert!(snapshot.terminal());
    assert_eq!(
        serde_json::to_value(snapshot).unwrap(),
        serde_json::from_str::<Value>(raw).unwrap()
    );
}

#[test]
fn journal_deduplicates_and_recovers_without_replay() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("commands.sqlite");
    let tracker = Tracker::open(path.clone());
    let identity = tracker.identity();
    let (first, new) = tracker
        .begin("manager-a", "command-a", "COM1", 1, "11 08")
        .unwrap();
    assert!(new);
    tracker.update("COM1", Some(1), |v| {
        v.accepted = true;
        v.phase = "Executing".into();
        v.attempt_count = 1;
    });
    assert!(tracker.flush(Duration::from_secs(2)));
    let (_, new) = tracker
        .begin("manager-a", "command-a", "COM1", 2, "1108")
        .unwrap();
    assert!(!new);
    assert!(tracker
        .begin("manager-a", "command-a", "COM1", 3, "1208")
        .is_err());
    assert_eq!(
        tracker
            .get("manager-a", "command-a")
            .unwrap()
            .unwrap()
            .ack_id,
        first.ack_id
    );
    drop(tracker);
    std::thread::sleep(Duration::from_millis(50));
    let restarted = Tracker::open(path);
    assert_eq!(
        identity["gateway_instance_id"],
        restarted.identity()["gateway_instance_id"]
    );
    assert_ne!(identity["boot_id"], restarted.identity()["boot_id"]);
    let recovered = restarted.get("manager-a", "command-a").unwrap().unwrap();
    assert_eq!(recovered.phase, "Unknown");
    assert!(recovered.audit_incomplete);
    assert!(restarted
        .validate_identity(
            identity["gateway_instance_id"].as_str().unwrap(),
            identity["boot_id"].as_str().unwrap()
        )
        .is_err());
}

#[test]
fn journal_keeps_attempts_and_manager_isolation() {
    let dir = tempfile::tempdir().unwrap();
    let tracker = Tracker::open(dir.path().join("journal.sqlite"));
    tracker.begin("a", "same", "COM1", 1, "1108").unwrap();
    tracker.begin("b", "same", "COM2", 2, "1108").unwrap();
    tracker.update("COM1", Some(1), |v| {
        v.accepted = true;
        v.attempt_count = 2;
        v.sent_count = 2;
        v.phase = "AckReceived".into();
        v.attempts = vec![
            Attempt {
                attempt: 1,
                sent: true,
                result: "ack_timeout".into(),
                at: now(),
            },
            Attempt {
                attempt: 2,
                sent: true,
                result: "AckReceived".into(),
                at: now(),
            },
        ];
    });
    assert!(tracker.flush(Duration::from_secs(2)));
    let a = tracker.get("a", "same").unwrap().unwrap();
    assert_eq!(a.sent_count, 2);
    assert_eq!(a.attempts[0].result, "ack_timeout");
    let events = tracker.events("a", 0, 200).unwrap();
    assert!(!events["items"].as_array().unwrap().is_empty());
    for e in events["items"].as_array().unwrap() {
        assert_eq!(e["manager_instance_id"], "a")
    }
}

#[test]
fn unavailable_audit_does_not_prevent_control_admission() {
    let dir = tempfile::tempdir().unwrap();
    let tracker = Tracker::open(dir.path().join("missing/journal.sqlite"));
    let start = Instant::now();
    let (v, new) = tracker.begin("a", "c", "COM1", 1, "1108").unwrap();
    assert!(new);
    assert!(v.audit_incomplete);
    assert!(start.elapsed() < Duration::from_secs(1));
    tracker.update("COM1", Some(1), |v| {
        v.accepted = true;
        v.phase = "AckReceived".into();
        v.sent_count = 1;
        v.attempt_count = 1;
    });
    assert_eq!(tracker.get("a", "c").unwrap().unwrap().phase, "AckReceived");
    assert_eq!(tracker.identity()["audit_degraded"], true);
}

#[test]
fn journal_recovers_buffered_evidence_and_rejects_duplicate() {
    let dir = tempfile::tempdir().unwrap();
    let parent = dir.path().join("late");
    let tracker = Tracker::open(parent.join("journal.sqlite"));
    let (_, new) = tracker.begin("a", "recover", "COM1", 8, "1108").unwrap();
    assert!(new);
    tracker.update("COM1", Some(8), |v| {
        v.phase = "AckReceived".into();
        v.accepted = true;
        v.attempt_count = 1;
        v.sent_count = 1;
    });
    std::fs::create_dir(&parent).unwrap();
    assert!(tracker.flush(Duration::from_secs(3)));
    let value = tracker.get("a", "recover").unwrap().unwrap();
    assert!(value.audit_incomplete);
    assert_eq!(value.phase, "AckReceived");
    let (_, new) = tracker.begin("a", "recover", "COM1", 9, "1108").unwrap();
    assert!(!new);
    let events = tracker.events("a", 0, 200).unwrap();
    assert!(events["items"]
        .as_array()
        .unwrap()
        .iter()
        .any(|v| v["phase"] == "AckReceived"));
    let identity = tracker.identity();
    drop(tracker);
    std::thread::sleep(Duration::from_millis(50));
    let restarted = Tracker::open(parent.join("journal.sqlite"));
    assert_eq!(
        identity["gateway_instance_id"],
        restarted.identity()["gateway_instance_id"]
    );
    let recovered = restarted.get("a", "recover").unwrap().unwrap();
    assert_eq!(
        recovered.gateway_instance_id,
        identity["gateway_instance_id"].as_str().unwrap()
    );
    assert_eq!(recovered.phase, "AckReceived");
    assert!(
        !restarted
            .begin("a", "recover", "COM1", 10, "1108")
            .unwrap()
            .1
    );
}

#[test]
fn recovery_refuses_to_merge_another_instance_journal() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("journal.sqlite");
    let (db, gateway, _) = open_database(&path, "first-boot", None).unwrap();
    drop(db);
    assert!(open_database(&path, "other-boot", Some("another-instance")).is_err());
    assert_eq!(read_gateway_identity(&path).unwrap(), gateway);
}

#[test]
fn newer_journal_schema_is_preserved_and_does_not_block_control() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("journal.sqlite");
    let (db, gateway, _) = open_database(&path, "first-boot", None).unwrap();
    db.pragma_update(None, "user_version", 2).unwrap();
    drop(db);
    let tracker = Tracker::open(path.clone());
    assert_eq!(tracker.identity()["gateway_instance_id"], gateway);
    assert_eq!(tracker.identity()["audit_degraded"], true);
    let (value, new) = tracker.begin("a", "newer", "COM1", 1, "1108").unwrap();
    assert!(new && value.audit_incomplete);
    let db = Connection::open(path).unwrap();
    assert_eq!(
        db.pragma_query_value(None, "user_version", |r| r.get::<_, u32>(0))
            .unwrap(),
        2
    );
}

#[test]
fn historical_lookup_and_dedup_do_not_wait_for_saturated_writer() {
    let dir = tempfile::tempdir().unwrap();
    let tracker = Tracker::open(dir.path().join("journal.sqlite"));
    tracker.begin("a", "historical", "COM1", 1, "1108").unwrap();
    tracker.update("COM1", Some(1), |v| {
        v.phase = "AckReceived".into();
        v.accepted = true;
        v.attempt_count = 1;
        v.sent_count = 1;
    });
    assert!(tracker.flush(Duration::from_secs(2)));
    // Force the cold history path (as after restart/cache eviction), then pause
    // the writer deterministically: no assumptions about fsync/disk speed.
    tracker
        .0
        .state
        .lock()
        .unwrap()
        .entries
        .remove(&key("a", "historical"));
    let (ready_tx, ready_rx) = mpsc::channel();
    let (resume_tx, resume_rx) = mpsc::channel();
    tracker
        .0
        .requests
        .send(Request::Pause(ready_tx, resume_rx))
        .unwrap();
    ready_rx.recv_timeout(Duration::from_secs(2)).unwrap();
    let mut queued = 0;
    while tracker.0.requests.try_send(Request::Wake).is_ok() {
        queued += 1;
    }
    assert!(queued > 0);

    let historical = tracker.get("a", "historical").unwrap().unwrap();
    assert_eq!(historical.phase, "AckReceived");
    assert_eq!(historical.sent_count, 1);
    assert!(tracker.get("b", "historical").unwrap().is_none());
    assert!(
        !tracker
            .begin("a", "historical", "COM1", 2, "1108")
            .unwrap()
            .1
    );
    assert_eq!(
        tracker
            .begin("a", "historical", "COM1", 3, "1208")
            .unwrap_err(),
        "command ID reused with different content"
    );
    assert_eq!(tracker.identity()["audit_degraded"], false);

    // Receiving a genuinely new command still has the original 100 ms
    // durability budget and degrades instead of blocking control indefinitely.
    let started = Instant::now();
    let (new, admitted) = tracker.begin("a", "new", "COM1", 4, "1108").unwrap();
    assert!(admitted && new.audit_incomplete);
    assert!(started.elapsed() < Duration::from_secs(1));
    assert_eq!(tracker.identity()["audit_degraded"], true);
    resume_tx.send(()).unwrap();
    assert!(tracker.flush(Duration::from_secs(2)));
}

#[test]
fn cold_lookup_is_read_only_and_checks_schema_and_gateway() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("journal.sqlite");
    assert!(lookup_journal(&path, "gateway", "a", "unknown").is_err());
    assert!(
        !path.exists(),
        "a read must not create a replacement journal"
    );
    let (db, gateway, _) = open_database(&path, "boot", None).unwrap();
    let mut value = fixture_snapshot();
    value.gateway_instance_id = gateway.clone();
    save(&db, &value).unwrap();
    assert!(lookup_journal(&path, "another-instance", "a", "unknown")
        .unwrap_err()
        .contains("instance mismatch"));
    // A WAL reader sees committed evidence even during an uncommitted write.
    db.execute_batch("BEGIN IMMEDIATE; UPDATE commands SET data='uncommitted'")
        .unwrap();
    assert_eq!(
        lookup_journal(
            &path,
            &gateway,
            &value.manager_instance_id,
            &value.command_id
        )
        .unwrap()
        .unwrap()
        .revision,
        value.revision
    );
    db.execute_batch("ROLLBACK").unwrap();
    db.pragma_update(None, "user_version", 2).unwrap();
    assert!(lookup_journal(
        &path,
        &gateway,
        &value.manager_instance_id,
        &value.command_id
    )
    .unwrap_err()
    .contains("unsupported command journal schema 2"));
    assert_eq!(
        db.pragma_query_value(None, "user_version", |r| r.get::<_, u32>(0))
            .unwrap(),
        2
    );
}
