# RCD Server 代码重构修改文档

## 一、项目概述

RCD Server 是一个 **串口采集 → OPC UA 网关** 服务程序，核心功能：
1. 解析 CSV 配置文件获取设备通信参数和数据点定义。
2. 为每个串口启动独立通信线程，使用自定义协议（Mark/Space 奇偶校验地址帧）轮询设备。
3. 将采集数据发布为 OPC UA 变量节点，支持客户端写命令下发。

当前源码共 7 个模块，约 **1500 行** Rust 代码。

---

## 二、现有问题总结

### 2.1 模块职责不清 — `tcp_server.rs` 是"杂物间"

| 当前内容 | 应属模块 |
|----------|----------|
| `DeviceStatus` 结构体（所有模块都使用的核心类型） | `common.rs` |
| TCP 服务器逻辑（`handle_client_1`、`run_tcp_server_1`） | 独立的 `tcp_server.rs` |
| OPC UA 服务器逻辑（`run_opcua_server`、`create_opcua_nodes`、`poll_and_forward_commands`） | 独立的 `opcua_server.rs` |
| OPC UA 辅助类型（`ExpectedType`、`hex_str_to_bytes`） | 独立的 `opcua_server.rs` |

**后果**：`tcp_server.rs` 长达 553 行，职责混杂；`common.rs` 反向依赖 `tcp_server`（`use crate::tcp_server::DeviceStatus`），形成了不必要的**循环引用**倾向。

### 2.2 静态配置与运行时状态混在一起 — `DeviceConfiguration`

`csv_parser.rs` 中的 `DeviceConfiguration` 同时包含：
- **静态配置**：`config`、`records`（从 CSV 解析，运行期不变）
- **运行时状态**：`timeout_count`、`parse_fail_count`、`same_count`、`last_data`、`current_round`、`is_read`、`site_status`

**后果**：
1. 结构体含义模糊——看起来是"配置"，实际是"配置+运行时状态"。
2. 运行时可变字段散布在多个地方被直接修改（`serial_manager.rs` 中大量 `commands[idx].timeout_count += 1`），耦合严重。
3. 添加了 `#[derive(Deserialize)]`，但运行时字段不可能从 CSV 反序列化。

### 2.3 `SerialManager` 内部状态重复

`SerialManager` 有自己的 `time_out_count` 和 `parse_fail_count`，同时 `commands[idx].timeout_count` 和 `commands[idx].parse_fail_count` 也在独立计数。两套计数器含义相似但作用不同（端口级别 vs. 设备级别），极易混淆。

### 2.4 panic 隐患——大量 `expect` / `unwrap`

以下位置在运行时可能 panic，导致整个服务崩溃：

| 文件 | 位置 | 代码 | 风险 |
|------|------|------|------|
| `serial_manager.rs` | `send_command()` | `port.set_parity(Parity::Mark).expect("设置 Mark 校验失败")` | 串口异常时 panic |
| `serial_manager.rs` | `send_command()` | `port.set_parity(Parity::Space).expect("设置 Space 校验失败")` | 同上 |
| `serial_manager.rs` | `parse_status()` | `panic!("无效的位范围: ...")` | 配置错误时 panic |
| `serial_manager.rs` | `start_serial_thread_1()` | `Runtime::new().expect("创建 tokio 运行时失败")` | OOM 时 panic |
| `tcp_server.rs` | `handle_client_1()` | `bincode::deserialize(&msg).unwrap()` | 恶意/异常数据时 panic |
| `tcp_server.rs` | `send_framed()` | `bincode::serialize(msg).expect("序列化失败")` | 理论安全，但可改善 |
| `tcp_server.rs` | `run_opcua_server()` | `.expect("获取节点管理器失败")` | 初始化失败时 panic |
| `file_processor.rs` | `read_and_process_files()` | `Regex::new(pattern).expect("无效的正则表达式")` | 传入错误模式时 panic |

### 2.5 线程模型不合理——每个串口线程内嵌独立 Tokio Runtime

`start_serial_thread_1` 的实现：
```
std::thread::spawn(move || {
    let rt = Runtime::new().expect("...");
    rt.block_on(async { ... });
});
```

**问题**：
1. `main()` 已经有一个 `#[tokio::main]` 运行时，每个串口又创建独立运行时，资源浪费。
2. 函数签名是 `async fn`，在 async 上下文中 `await`，然后内部又 `thread::spawn` 再建运行时，语义混乱。
3. 串口 I/O 本身是阻塞的（`serialport` 是同步库），应使用 `tokio::task::spawn_blocking` 或 `std::thread` + 通道，而不是套嵌 async 运行时。

### 2.6 全局发送器模式过于复杂

```rust
Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>
```

四层嵌套的类型签名频繁出现在函数参数中，难以阅读。应提取为 **类型别名** 或封装为专用结构体。

### 2.7 魔数遍布协议代码

`serial_manager.rs` 中：
- `0x08`、`0x02`、`0x30`、`0x10` — 协议命令码，无任何常量定义或注释。
- `timeout_count > 3`、`current_round > 10`、`parse_fail_count > 100`、`time_out_count > 20`、`same_count >= 10` — 阈值硬编码分散在各处。
- 帧结构偏移 `data[0]`、`data[1]`、`data[2]`、`data[3..data.len()-1]` 等无命名常量。

### 2.8 错误处理风格不统一

- `main.rs`：使用 `expect` / `eprintln!`。
- `serial_manager.rs`：混用 `eprintln!` 和 `log::warn` 的 `warn!`（实际未 use `warn!`，只在 `tcp_server.rs` 中导入了 `log::warn`）。
- `tcp_server.rs`：使用 `log::warn`。
- 没有统一的日志框架策略（`println!`、`eprintln!`、`log::warn` 混用）。

### 2.9 dead_code 警告——未使用的字段和方法

- `config.rs`：`primary_ip`、`secondary_ip`、`baud_rate`、`flow_control`、`timeout`、`settings` 等字段从配置中加载但从未使用。
- `csv_parser.rs`：`f_type`、`def`、`max`、`min`、`site_status` 字段从未使用。
- `common.rs`：`add_device_status` 方法从未调用。
- `tcp_server.rs`：`run_tcp_server_1` 被标记 `#[allow(unused)]`，整个 TCP 服务器逻辑未使用。

### 2.10 命名不规范

| 当前命名 | 问题 | 建议命名 |
|----------|------|----------|
| `start_serial_thread_1` | 后缀 `_1` 无意义 | `spawn_serial_worker` |
| `handle_client_1` | 后缀 `_1` 无意义 | `handle_tcp_client` |
| `run_tcp_server_1` | 后缀 `_1` 无意义 | `run_tcp_server` |
| `commands` (在 `SerialPortConfig` 中) | 实际含义是"设备列表" | `devices` |
| `serial_config_data` (在 `SerialManager` 中) | 与 `serial_config` 容易混淆 | `port_config` |
| `lh` (在 `Record` 中) | 含义不明 | `byte_order` 或 `is_big_endian` |
| `pre` (在 `Config` 中) | 含义不明 | `kks_prefix` |
| `f_type` (在 `Record` 中) | 含义不明 | `function_type` |
| `check_data` (在 `DeviceConfiguration` 中) | 功能是判断是否应推送 | `should_broadcast` |

### 2.11 缺少单元测试

整个项目没有任何测试代码。协议解析（`parse_data_packet`、`parse_status`、`verify_checksum`）、CSV 解析、`check_data` 去重逻辑等都是纯函数，非常适合编写单元测试。

---

## 三、重构方案

### 3.1 模块拆分与职责重新划分

**目标结构**：

```
src/
├── main.rs                  # 入口：加载配置、启动各子系统
├── config.rs                # TOML 配置加载（不变）
├── csv_parser.rs            # CSV 解析（仅静态配置和记录）
├── file_scanner.rs          # 文件扫描（原 file_processor.rs，改名更直观）
├── model/
│   ├── mod.rs               # 导出所有公共类型
│   ├── device.rs            # DeviceStatus、DeviceConfiguration（静态）、DeviceRuntimeState（运行时）
│   ├── command.rs           # Command、CommandType、SendData、PortStatus、DeviceSetting
│   ├── message.rs           # MessageType 枚举
│   └── state.rs             # SystemState、PortRuntimeState、Value
├── protocol.rs              # 协议常量、帧解析、校验和、parse_status 等纯函数
├── serial/
│   ├── mod.rs               # 导出
│   ├── manager.rs           # SerialManager（核心状态机）
│   ├── port_config.rs       # SerialPortConfig（原 serial_port_config.rs）
│   └── worker.rs            # 线程入口、轮询调度
├── server/
│   ├── mod.rs               # 导出
│   ├── opcua.rs             # OPC UA 服务器（原 tcp_server.rs 的 OPC UA 部分）
│   └── tcp.rs               # TCP 服务器（原 tcp_server.rs 的 TCP 部分）
└── broadcast.rs             # Broadcaster 封装全局发送器
```

**关键变化**：
1. 将 `DeviceStatus` 移出 `tcp_server.rs`，放入 `model/device.rs`，消除循环依赖。
2. 将 `common.rs` 拆分为 `model/` 子目录，按关注点分文件。
3. 将协议解析纯函数提取到 `protocol.rs`，方便独立测试。
4. 将 OPC UA 和 TCP 服务器拆分为独立模块。

### 3.2 分离静态配置与运行时状态

**当前**：
```rust
pub struct DeviceConfiguration {
    pub config: Config,         // 静态
    pub records: Vec<Record>,   // 静态
    pub timeout_count: u32,     // 运行时
    pub last_data: Vec<u8>,     // 运行时
    // ...
}
```

**改为**：
```rust
// csv_parser.rs 或 model/device.rs
pub struct DeviceConfig {
    pub config: Config,
    pub records: Vec<Record>,
}

// serial/manager.rs 内部
struct DeviceRuntimeState {
    timeout_count: u32,
    current_round: u32,
    parse_fail_count: u32,
    last_data: Vec<u8>,
    same_count: u32,
    is_enabled: bool,
}
```

**好处**：
- `DeviceConfig` 不可变，可安全共享（`Arc<DeviceConfig>`）。
- `DeviceRuntimeState` 仅在 `SerialManager` 内部使用，生命周期清晰。
- 去掉 `DeviceConfiguration` 上无意义的 `#[derive(Deserialize)]`。

### 3.3 封装全局广播器

**当前**：
```rust
Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>
```

**改为**：
```rust
/// 多订阅者广播器：串口线程发送数据，多个消费者（OPC UA / TCP）接收。
#[derive(Clone)]
pub struct Broadcaster {
    senders: Arc<TokioMutex<Vec<Arc<tokio_mpsc::Sender<DeviceStatus>>>>>,
}

impl Broadcaster {
    pub fn new() -> Self { ... }
    pub async fn subscribe(&self) -> tokio_mpsc::Receiver<DeviceStatus> { ... }
    pub async fn broadcast(&self, data: DeviceStatus) { ... }
}
```

**好处**：所有函数签名中的四层嵌套类型变成一个简洁的 `Broadcaster`，语义也更清晰。

### 3.4 消除 panic——统一错误处理

| 位置 | 当前 | 改为 |
|------|------|------|
| `set_parity().expect(...)` | panic | `set_parity(...)?` + `reconnect()` |
| `panic!("无效的位范围")` | panic | 返回 `Result`，上层跳过该数据点并 `warn!` |
| `bincode::deserialize().unwrap()` | panic | `match` 处理，错误时断开连接 |
| `Regex::new().expect(...)` | panic | 返回 `Result` 到调用方 |
| `Runtime::new().expect(...)` | panic | 返回 `Result`，线程启动失败时上报 |

### 3.5 协议常量化

```rust
// protocol.rs
pub mod cmd {
    pub const READ_STATUS: u8 = 0x08;
}
pub mod param {
    pub const READ_SUBCMD: u8 = 0x02;
    pub const STATUS_PAGE: u8 = 0x30;
    pub const STATUS_OFFSET: u8 = 0x10;
}
pub mod threshold {
    pub const DEVICE_TIMEOUT_SKIP: u32 = 3;
    pub const DEVICE_SKIP_ROUNDS: u32 = 10;
    pub const PORT_FAILOVER_TIMEOUT: u32 = 20;
    pub const PARSE_FAIL_FAILOVER: u32 = 100;
    pub const SAME_DATA_INTERVAL: u32 = 10;
}
```

### 3.6 改进线程模型

**方案**：
1. 移除串口线程内的独立 `Runtime::new()`。
2. 使用 `tokio::task::spawn_blocking` 在主运行时的阻塞线程池中执行串口 I/O。
3. 串口线程与主异步代码之间使用 `tokio::sync::mpsc` 通信（替换 `std::sync::mpsc`）。

```rust
// 改前
pub async fn start_serial_thread_1(...) -> thread::JoinHandle<()> {
    thread::spawn(move || {
        let rt = Runtime::new().expect("...");
        rt.block_on(async { loop { ... } });
    })
}

// 改后
pub fn spawn_serial_worker(
    ...,
    mut cmd_rx: tokio::sync::mpsc::Receiver<Command>,
) -> tokio::task::JoinHandle<()> {
    tokio::task::spawn_blocking(move || {
        // 串口同步 I/O 在阻塞线程池中运行
        // 通过 cmd_rx.try_recv() 非阻塞接收命令
    })
}
```

### 3.7 统一日志

- 全部使用 `log` crate（`info!`、`warn!`、`error!`），移除 `println!` / `eprintln!`。
- 在 `main.rs` 中初始化 `env_logger`（当前仅在 `run_opcua_server` 中调用 `env_logger::try_init()`）。
- 日志中保持统一前缀格式：`[串口号] 消息内容`。

### 3.8 命名规范化

按照 §2.10 表格中的建议统一重命名，保持 Rust 命名惯例（snake_case 函数名、无意义后缀移除、字段名语义化）。

### 3.9 清理未使用代码

1. **确认功能需求**：`run_tcp_server_1` 及相关 TCP 服务器代码是否还需要？如不需要，直接删除而非 `#[allow(unused)]`。
2. **确认字段需求**：`primary_ip`、`secondary_ip`、`f_type`、`def`、`max`、`min` 等字段是预留还是遗留？如是遗留则移除。
3. 移除 `add_device_status` 方法或改为在合适时机调用。

### 3.10 添加单元测试

优先为以下纯函数添加测试：

| 函数 | 测试要点 |
|------|----------|
| `verify_checksum` | 正确校验 / 校验失败 / 空数组 |
| `parse_data_packet` | 正常帧 / 过短帧 / 校验错误 / 长度不匹配 |
| `parse_status` | 单位解析 / 位范围解析 / 大端小端 / 越界处理 |
| `get_sum` | 正常累加 / 溢出回绕 |
| `check_data` (改名 `should_broadcast`) | 数据变化推送 / 连续相同阈值推送 |
| `hex_str_to_bytes` | 正常解析 / 奇数长度 / 非法字符 |
| `parse_csv` | 正常文件 / 空文件 / 配置行不足 |
| `find_double_zero` | 找到 / 未找到 / 边界位置 |

---

## 四、实施优先级

| 优先级 | 修改项 | 影响范围 | 预估工作量 |
|--------|--------|----------|------------|
| **P0** | 消除 panic（§3.4） | 运行稳定性 | 小 | ✅ 已完成 |
| **P0** | 移动 `DeviceStatus` 到 `common.rs`（§3.1 部分） | 消除循环依赖 | 小 | ✅ 已完成 |
| **P1** | 分离静态配置与运行时状态（§3.2） | `csv_parser`、`serial_manager`、`serial_port_config` | 中 | ✅ 已完成 |
| **P1** | 封装 `Broadcaster`（§3.3） | 全模块函数签名 | 中 | ✅ 已完成 |
| **P1** | 协议常量化（§3.5） | `serial_manager.rs` | 小 | ✅ 已完成 |
| **P1** | 统一日志（§3.7） | 全模块 | 小 | ✅ 已完成 |
| **P2** | 命名规范化（§3.8） | 全模块 | 小 | ✅ 已完成 |
| **P2** | 模块拆分（§3.1 完整版） | 全模块结构 | 大 | ✅ 已完成 |
| **P2** | 改进线程模型（§3.6） | `serial_manager`、`main` | 大 | ✅ 已完成 |
| **P3** | 清理未使用代码（§3.9） | 需确认功能需求 | 小 | ✅ 已完成 |
| **P3** | 添加单元测试（§3.10） | 新增文件 | 中 | ✅ 已完成 |

---

## 五、预期收益

1. **可读性**：模块职责单一，类型命名语义化，魔数消除。
2. **健壮性**：消除所有运行时 panic 路径，统一错误处理。
3. **可维护性**：静态配置与运行时状态分离，修改不易引入副作用。
4. **可扩展性**：新增协议/服务器类型时只需增加模块，不影响已有代码。
5. **可测试性**：协议解析等核心逻辑可独立测试。

---

> **下一步**：请确认以上修改方案是否符合预期，以及 §3.9 中需要确认的功能问题（TCP 服务器是否保留、预留字段是否需要）。确认后即可按优先级分批实施。
