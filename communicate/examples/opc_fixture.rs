//! Software-only interoperability fixture. Never opens a serial device. It
//! exits after 45 seconds and must never be installed as the production binary.
use chrono::Utc;
use rcd_communicate::{
    config, opc,
    points::{PointStore, PointUpdate, Quality, Value},
};
use std::{path::PathBuf, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = PathBuf::from(
        std::env::args()
            .nth(1)
            .ok_or("fixture directory required")?,
    );
    let loaded = config::load(&root.join("config.toml"))?;
    if !loaded.config.opc.bind_ip.is_loopback()
        || loaded.config.opc.port < 10000
        || loaded.config.instance_id != "communicate-example"
    {
        return Err("fixture requires example identity, loopback and isolated port".into());
    }
    config::validate_adapters(&loaded.config)?;
    let store = Arc::new(PointStore::new(&loaded.config, &loaded.points));
    let cancel = CancellationToken::new();
    let updater = {
        let store = store.clone();
        let cancel = cancel.clone();
        tokio::spawn(async move {
            for round in 0..45 {
                match round % 2 {
                    1 => store
                        .fail(
                            "example-task",
                            Quality::NoCommunication,
                            "software fixture timeout",
                        )
                        .unwrap(),
                    _ => store
                        .publish(
                            "example-task",
                            &[
                                PointUpdate {
                                    name: "COMM_EXAMPLE_VALUE".into(),
                                    value: Value::Double(0.0),
                                    quality: Quality::Good,
                                    error: String::new(),
                                },
                                PointUpdate {
                                    name: "COMM_EXAMPLE_STATE".into(),
                                    value: Value::Boolean(false),
                                    quality: Quality::Good,
                                    error: String::new(),
                                },
                            ],
                            Utc::now(),
                        )
                        .unwrap(),
                }
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            cancel.cancel();
        })
    };
    let result = opc::serve(
        &loaded.config,
        store,
        &root.join("fixture-pki"),
        cancel.clone(),
        None,
        None,
    )
    .await;
    cancel.cancel();
    updater.abort();
    let _ = updater.await;
    result?;
    Ok(())
}
