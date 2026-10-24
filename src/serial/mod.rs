//! 串口通信模块。
//!
//! 包含串口管理器、端口配置和线程入口。

mod command_audit;
mod command_pacing;
mod command_transaction;
pub mod manager;
pub mod port_config;
mod transport;
mod version_transaction;
pub mod worker;
