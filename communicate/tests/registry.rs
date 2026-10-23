use rcd_communicate::{
    config::{self, TaskConfig},
    protocol::{DeviceProtocol, ProtocolError, Registry, Response},
};
use std::sync::{
    atomic::{AtomicUsize, Ordering},
    Arc,
};

struct Stub;
impl DeviceProtocol for Stub {
    fn request(&mut self, _: &TaskConfig) -> Result<Vec<u8>, ProtocolError> {
        Ok(Vec::new())
    }
    fn response(&mut self, _: &TaskConfig, _: &[u8]) -> Result<Response, ProtocolError> {
        Ok(Response::Ignore)
    }
}

fn config() -> config::Config {
    config::load(&std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("examples/config.toml"))
        .unwrap()
        .config
}

#[test]
fn registry_rejects_duplicates_reserved_and_missing_protocols() {
    let mut registry = Registry::default();
    assert!(registry
        .register("pending", |_| Ok(Box::new(Stub)))
        .is_err());
    registry
        .register("test-only", |_| Ok(Box::new(Stub)))
        .unwrap();
    assert!(registry
        .register("test-only", |_| Ok(Box::new(Stub)))
        .is_err());
    let mut config = config();
    config.ports[0].tasks[0].protocol = "typo".into();
    assert!(registry.prepare(&config).is_err());
}

#[test]
fn disabled_tasks_never_instantiate_and_enabled_tasks_prepare_once() {
    let count = Arc::new(AtomicUsize::new(0));
    let created = count.clone();
    let mut registry = Registry::default();
    registry
        .register("test-only", move |_| {
            created.fetch_add(1, Ordering::SeqCst);
            Ok(Box::new(Stub))
        })
        .unwrap();
    let mut config = config();
    config.ports[0].tasks[0].protocol = "test-only".into();
    assert!(registry.prepare(&config).unwrap()[0].is_empty());
    assert_eq!(count.load(Ordering::SeqCst), 0);
    config.ports[0].enabled = true;
    config.ports[0].tasks[0].enabled = true;
    assert_eq!(registry.prepare(&config).unwrap()[0].len(), 1);
    assert_eq!(count.load(Ordering::SeqCst), 1);
}

#[test]
fn factory_errors_abort_preparation() {
    let mut registry = Registry::default();
    registry
        .register("invalid", |_| {
            Err(ProtocolError("missing device address".into()))
        })
        .unwrap();
    let mut config = config();
    config.ports[0].enabled = true;
    config.ports[0].tasks[0].enabled = true;
    config.ports[0].tasks[0].protocol = "invalid".into();
    assert!(registry.prepare(&config).is_err());
}
