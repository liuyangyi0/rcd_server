//! Isolated software fixture: no serial handle is ever opened.
use rcd_communicate::{
    config, opc,
    points::{PointStore, Quality},
    unit_report::Layout,
};
use std::{path::PathBuf, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = PathBuf::from(
        std::env::args()
            .nth(1)
            .ok_or("isolated directory required")?,
    );
    let mut loaded = config::load(&root.join("config.toml"))?;
    if !loaded.config.opc.bind_ip.is_loopback()
        || loaded.config.opc.port < 10000
        || loaded.config.instance_id != "unit1-receiver"
    {
        return Err("unit fixture requires isolated loopback config".into());
    }
    // Enable only the memory mailbox. This fixture never calls runtime or opens
    // serial devices, even though the mailbox requires an enabled source.
    loaded.config.ports[0].enabled = true;
    loaded.config.ports[0].tasks[0].enabled = true;
    loaded.config.ports[0]
        .synchronization
        .as_mut()
        .unwrap()
        .transmit_enabled = true;
    let store = Arc::new(PointStore::new(&loaded.config, &loaded.points));
    let lifetime: usize = std::env::var("RCD_SYNC_FIXTURE_LIFETIME_SECONDS")
        .ok()
        .map(|s| s.parse())
        .transpose()?
        .unwrap_or(45);
    if !(45..=120).contains(&lifetime) {
        return Err("invalid bounded fixture lifetime".into());
    }
    let cancel = CancellationToken::new();
    let (ready_tx, ready_rx) = tokio::sync::oneshot::channel();
    let updater = {
        let store = store.clone();
        let cancel = cancel.clone();
        tokio::spawn(async move {
            if ready_rx.await.is_err() {
                cancel.cancel();
                return;
            }
            let mut raw = vec![0; 105];
            raw[0] = 9;
            raw[50..55].copy_from_slice(&[1, 0xc8, 0, 7, 0]);
            let values = Layout::Unit9To1.decode("rx9", &raw).unwrap();
            for step in 0..lifetime {
                if step.is_multiple_of(2) {
                    store.publish("rx9", &values, chrono::Utc::now()).unwrap();
                } else {
                    store
                        .fail("rx9", Quality::NoCommunication, "fixture peer silent")
                        .unwrap();
                }
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            cancel.cancel();
        })
    };
    let result = opc::serve(
        &loaded.config,
        store,
        &root.join("pki"),
        cancel.clone(),
        Some(ready_tx),
        None,
    )
    .await;
    cancel.cancel();
    updater.abort();
    let _ = updater.await;
    result?;
    Ok(())
}
