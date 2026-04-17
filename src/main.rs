//! RCD Server  串口采集 OPC UA 网关。
//!
//! 主要流程：
//! 1. 加载 `config/config.toml` 全局配置。
//! 2. 扫描 `config/rcd*.csv` 文件，解析设备通信参数与数据点定义。
//! 3. 为每个串口启动独立的通信线程（轮询采集 + 命令下发）。
//! 4. 启动 OPC UA 服务器，将采集数据发布为变量节点。

mod broadcast;
mod calc_engine;
mod config;
mod csv_parser;
mod file_scanner;
mod model;
mod protocol;
mod serial;
mod server;

use std::collections::HashMap;
use std::io;
use std::sync::{Arc, mpsc};

use bounded_vec_deque::BoundedVecDeque;
use tokio::sync::Mutex as TokioMutex;

use crate::broadcast::Broadcaster;
use crate::model::{Command, SystemState};
use crate::csv_parser::DeviceConfig;
use crate::serial::manager::SerialConfig;
use crate::serial::worker::spawn_serial_worker;
use crate::serial::port_config::SerialPortConfig;
use crate::server::opcua::run_opcua_server;
use crate::calc_engine::config::load_calc_config;
use crate::calc_engine::engine::CalcEngine;
use log::{info, warn, error};

// ============================================================
//  程序入口
// ============================================================

#[tokio::main]
async fn main() -> tokio::io::Result<()> {
    env_logger::init();

    // 全局广播器：串口数据会广播到所有订阅者（OPC UA / TCP 客户端）
    let broadcaster = Broadcaster::new();

    // 加载配置
    let software_config = config::init_config().expect("加载配置文件失败");

    // 系统状态 & 操作记录
    let system_state = SystemState::new();
    let system_record = Arc::new(TokioMutex::new(BoundedVecDeque::<String>::new(20000)));

    // 加载计算引擎：配置异常时降级为空引擎，不终止服务
    let calc_engine = {
        let config_dir = config::config_dir().expect("获取配置目录失败");
        let calc_path = config_dir.join("calc.toml");
        let calc_config = load_calc_config(&calc_path)
            .unwrap_or_else(|e| {
                error!("[CalcEngine] 加载计算规则失败: {}，以空规则启动", e);
                crate::calc_engine::config::CalcConfig { rules: Vec::new() }
            });

        if calc_config.rules.is_empty() {
            info!("[CalcEngine] 无计算规则，引擎未启用");
            Arc::new(CalcEngine::new(vec![]).unwrap())
        } else {
            match CalcEngine::new(calc_config.rules) {
                Ok(engine) => {
                    info!(
                        "[CalcEngine] 初始化成功，共 {} 条规则",
                        engine.output_names().len()
                    );
                    Arc::new(engine)
                }
                Err(e) => {
                    error!("[CalcEngine] 构建引擎失败: {}，以空引擎启动", e);
                    Arc::new(CalcEngine::new(vec![]).unwrap())
                }
            }
        }
    };

    // 初始化串口并启动通信线程
    match init_serial_ports(
        broadcaster.clone(),
        &software_config,
        system_state.clone(),
        system_record.clone(),
    ).await {
        Ok((configs, txs)) => {
            // 校验计算引擎的基础变量是否在 CSV 数据点中定义（需求 4.1 依赖存在性分析）
            if !calc_engine.is_empty() {
                let all_kks: std::collections::HashSet<String> = configs.iter()
                    .flat_map(|port| port.devices.iter())
                    .flat_map(|dev| dev.records.iter())
                    .map(|rec| rec.kks.clone())
                    .collect();

                let required = calc_engine.required_base_variables();
                let missing: Vec<&String> = required.iter()
                    .filter(|v| !all_kks.contains(v.as_str()))
                    .collect();

                if !missing.is_empty() {
                    warn!(
                        "[CalcEngine] 以下公式变量未在 CSV 数据点中定义: {:?}，运行时将使用默认值",
                        missing
                    );
                }
            }

            // 启动 OPC UA 服务器（阻塞直到退出）
            match run_opcua_server(configs, broadcaster, system_state, system_record, txs, calc_engine).await {
                Ok(_) => info!("OPC UA 服务器已正常退出"),
                Err(e) => error!("OPC UA 服务器运行失败: {}", e),
            }
        }
        Err(e) => {
            error!("初始化失败: {}", e);
        }
    }

    Ok(())
}

// ============================================================
//  初始化
// ============================================================

/// 扫描 CSV 配置文件、聚合串口设备、启动通信线程。
///
/// 返回 `(串口配置列表, 命令发送器列表)` 供服务器使用。
async fn init_serial_ports(
    broadcaster: Broadcaster,
    software_config: &config::Config,
    system_state: SystemState,
    system_record: Arc<TokioMutex<BoundedVecDeque<String>>>,
) -> io::Result<(Vec<SerialPortConfig>, Vec<mpsc::Sender<Command>>)> {

    // 1. 扫描 CSV 并按串口号聚合
    let serial_port_configs = load_and_group_csv_configs()?;

    if serial_port_configs.is_empty() {
        warn!("未找到任何有效的设备配置文件");
    } else {
        info!(
            "已加载 {} 个串口, 共 {} 个设备",
            serial_port_configs.len(),
            serial_port_configs.iter().map(|c| c.devices.len()).sum::<usize>(),
        );
    }

    // 2. 为每个串口启动通信线程
    let mut txs = Vec::with_capacity(serial_port_configs.len());

    for port_config in &serial_port_configs {
        let (tx, rx) = mpsc::channel();
        txs.push(tx);

        system_state.add_port(port_config.to_runtime_state()).await;

        spawn_serial_worker(
            broadcaster.clone(),
            SerialConfig::new(
                port_config.port_number.clone(),
                port_config.baud_rate,
                software_config.serial.data_bits,
                software_config.serial.stop_bits,
                software_config.serial.parity,
            ),
            port_config.clone(),
            rx,
            software_config.clone(),
            system_state.clone(),
            system_record.clone(),
        );
    }

    Ok((serial_port_configs, txs))
}

/// 从 config 目录加载所有 `rcd*.csv` 文件并按串口号聚合。
fn load_and_group_csv_configs() -> io::Result<Vec<SerialPortConfig>> {
    let config_dir = config::config_dir()
        .map_err(|e| io::Error::new(io::ErrorKind::NotFound, e.to_string()))?;

    let csv_files = file_scanner::read_and_process_files(&config_dir, r"^rcd.*\.csv$")
        .unwrap_or_else(|e| {
            warn!("读取配置文件目录失败: {}", e);
            Vec::new()
        });

    let mut configs: HashMap<String, SerialPortConfig> = HashMap::new();

    for file in &csv_files {
        match csv_parser::parse_csv(file) {
            Ok((conf, recs)) => {
                let device = DeviceConfig::new(conf.clone(), recs);
                configs
                    .entry(conf.com.clone())
                    .or_insert_with(|| SerialPortConfig::from_csv_config(&conf))
                    .devices
                    .push(device);
            }
            Err(e) => warn!("解析 CSV {:?} 失败: {}", file, e),
        }
    }

    Ok(configs.into_values().collect())
}
