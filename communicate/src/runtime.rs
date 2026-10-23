//! Own the OPC task and serial workers together. Worker failure shuts down the
//! gateway instead of leaving a healthy-looking OPC server with dead acquisition.
use crate::{
    config::Config,
    opc,
    points::PointStore,
    poller,
    protocol::Registry,
    serial::{NativeSerial, Transport},
};
use std::{io, path::PathBuf, sync::Arc, time::Duration};
use tokio::{sync::oneshot, task::JoinSet};
use tokio_util::sync::CancellationToken;

pub async fn run(
    config: Config,
    store: Arc<PointStore>,
    registry: Registry,
    pki: PathBuf,
    cancel: CancellationToken,
    operations: crate::operations::Operations,
) -> io::Result<()> {
    let prepared = registry.prepare(&config).map_err(io::Error::other)?;
    let (ready_tx, ready_rx) = oneshot::channel();
    let mut server = {
        let config = config.clone();
        let store = store.clone();
        let cancel = cancel.clone();
        tokio::spawn(async move {
            opc::serve(
                &config,
                store,
                &pki,
                cancel,
                Some(ready_tx),
                Some(operations),
            )
            .await
        })
    };
    // Bind OPC before taking ownership of physical ports.
    if ready_rx.await.is_err() {
        cancel.cancel();
        return server.await.map_err(io::Error::other)?;
    }
    let mut workers = JoinSet::new();
    for (port, tasks) in config.ports.into_iter().zip(prepared) {
        if tasks.is_empty() {
            continue;
        }
        let store = store.clone();
        let cancel = cancel.clone();
        workers.spawn_blocking(move || {
            poller::run_worker(port, tasks, store, cancel, |config| {
                NativeSerial::open(config).map(|p| Box::new(p) as Box<dyn Transport>)
            })
        });
    }
    let mut server_done = false;
    let mut result = tokio::select! {
        outcome = &mut server => {
            server_done = true;
            match outcome { Ok(value) => value, Err(e) => Err(io::Error::other(e)) }
        }
        outcome = workers.join_next(), if !workers.is_empty() => {
            match outcome {
                Some(Ok(Ok(()))) if cancel.is_cancelled() => Ok(()),
                value => Err(io::Error::other(format!("serial worker ended unexpectedly: {value:?}"))),
            }
        }
        _ = cancel.cancelled() => Ok(()),
    };
    cancel.cancel();
    while let Some(worker) = workers.join_next().await {
        if let Err(e) = worker
            .map_err(io::Error::other)
            .and_then(|v| v.map_err(io::Error::other))
        {
            result = Err(e);
        }
    }
    if !server_done {
        match tokio::time::timeout(Duration::from_secs(6), &mut server).await {
            Ok(Ok(Ok(()))) => {}
            Ok(Ok(Err(e))) => result = Err(e),
            Ok(Err(e)) => result = Err(io::Error::other(e)),
            Err(_) => {
                server.abort();
                let _ = server.await;
                result = Err(io::Error::new(
                    io::ErrorKind::TimedOut,
                    "OPC task did not stop",
                ));
            }
        }
    }
    result
}
