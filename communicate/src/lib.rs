//! Serial acquisition infrastructure. Device requests and business decoding are
//! supplied by a protocol adapter, never inferred from sample bytes.
pub mod build_info;
pub mod config;
pub mod frame;
pub mod opc;
pub mod operations;
pub mod points;
pub mod poller;
pub mod protocol;
pub mod runtime;
pub mod serial;
pub mod synchronization;
pub mod unit_report;
