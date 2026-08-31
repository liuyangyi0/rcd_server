# RCD Server 使用说明

**版本：** 0.1.0
**说明：** 串口采集 OPC UA 网关——从串口设备轮询数据，通过 OPC UA 协议发布。

---

## 目录

1. [概述](#1-概述)
2. [部署目录结构](#2-部署目录结构)
3. [全局配置文件](#3-全局配置文件-configconfigtoml)
4. [设备配置文件](#4-设备配置文件-configrdccsv)
5. [启动与运行](#5-启动与运行)
6. [OPC UA 客户端接入](#6-opc-ua-客户端接入)
7. [命令下发](#7-命令下发)
8. [主备切换机制](#8-主备切换机制)
9. [串口通信细节](#9-串口通信细节)
10. [日志与故障排查](#10-日志与故障排查)
11. [PKI 证书管理](#11-pki-证书管理)
12. [附录：协议帧格式](#12-附录协议帧格式)

---

## 1. 概述

RCD Server 是一个**串口采集 OPC UA 网关**，主要功能：

- 通过串口轮询下位机设备，采集实时状态数据。
- 将采集数据解析后发布为 OPC UA 变量节点，供上位系统读取。
- 支持 OPC UA 客户端向设备下发命令。
- 支持**主备双机热切换**，提高系统可用性。

### 工作流程

```
启动 → 加载配置 → 扫描设备CSV → 打开串口 → 轮询采集 → OPC UA发布
                                            ↑
                                  OPC UA客户端写命令 → 转发到串口
```

---

## 2. 部署目录结构

编译后请按以下结构部署：

```
<部署目录>/
├── rcd_server.exe              # 可执行文件（Linux 下为 rcd_server）
├── config/
│   ├── config.toml             # 全局配置文件（必需）
│   ├── rcd_device1.csv         # 设备配置文件（至少一个）
│   ├── rcd_device2.csv         # 可添加多个设备文件
│   └── ...
└── pki/                        # OPC UA 证书目录（首次运行自动生成）
    ├── own/                    # 服务器自身证书
    ├── private/                # 服务器私钥
    ├── rejected/               # 被拒绝的客户端证书
    └── trusted/                # 受信任的客户端证书
```

> **重要：** `config/` 目录必须位于**可执行文件同级目录**下，不是工作目录。

---

## 3. 全局配置文件 `config/config.toml`

```toml
[server]
primary_ip   = "192.168.1.100"    # 主机 IP 地址（预留，主备切换）
secondary_ip = "192.168.1.101"    # 备机 IP 地址（预留，主备切换）
run_on       = "primary"          # 本机角色："primary"（主机）或 "secondary"（备机）
current_run  = "primary"          # 启动时生效角色（运行时可自动切换）

[serial]
baud_rate     = 9600              # 全局默认波特率（各 CSV 文件可覆盖）
data_bits     = "Eight"           # 数据位：Five / Six / Seven / Eight
parity        = "None"            # 校验位：None / Odd / Even / Mark / Space
stop_bits     = "One"             # 停止位：One / Two
flow_control  = "None"            # 流控方式（预留）

[settings]
timeout = 300                     # 串口读取超时，单位：毫秒
poll_interval_ms = 20             # 串口轮询间隔，单位：毫秒
command_retry_count = 3           # 命令未收到 ACK 时的最大连续下发次数

[logging]
level = "info"                    # 文件日志级别，也可用 RUST_LOG 临时覆盖
directory = "logs"                # 相对路径按可执行文件同级目录解析
basename = "rcd_server"           # 日志文件基础名
duplicate_to_stderr = "warn"      # none/error/warn/info/debug/trace/all
rotate_age = "day"                # day/hour/minute/second/none
rotate_size_bytes = 104857600     # 单个日志文件超过 100 MiB 时滚动
keep_files = 30                   # 保留滚动日志文件数；0 表示不清理
write_mode = "buffer_and_flush"   # direct/buffer_and_flush/buffer_dont_flush
serial_frame_logs = false         # 是否记录串口收发原始帧
opcua_transport_errors = false    # 是否保留 OPC UA 客户端断开类传输噪声
```

### 配置项说明

| 配置项 | 必填 | 说明 |
|--------|------|------|
| `server.run_on` | ✅ | 决定本机是主机还是备机 |
| `server.current_run` | ✅ | 启动时实际生效的角色，通常与 `run_on` 一致 |
| `serial.data_bits` | ✅ | 所有串口共用的数据位设置 |
| `serial.parity` | ✅ | 所有串口共用的默认校验位设置 |
| `serial.stop_bits` | ✅ | 所有串口共用的停止位设置 |
| `serial.baud_rate` | ✅ | 全局默认波特率（每个 CSV 文件中的 `baud_rate` 会覆盖此值） |
| `logging.level` | 否 | 默认 `info`，可配置为 `warn`、`debug` 或模块级 log spec |
| `logging.keep_files` | 否 | 默认保留 30 个滚动日志文件，`0` 表示不清理 |
| `logging.serial_frame_logs` | 否 | 默认关闭，高频原始帧日志只在定位串口问题时打开 |

---

## 4. 设备配置文件 `config/rcd*.csv`

### 命名规则

文件必须放在 `config/` 目录下，文件名匹配正则表达式 `^rcd.*\.csv$`。

**合法文件名示例：**
- `rcd_pump.csv`
- `rcd01.csv`
- `rcd_valve_group.csv`

**不合法文件名示例：**
- `device.csv`（不以 `rcd` 开头）
- `rcd_pump.txt`（不以 `.csv` 结尾）

### 文件格式

每个 CSV 文件描述**一个设备**的通信参数和数据点定义。同一串口上的多个设备使用多个 CSV 文件，软件会自动按串口号聚合。

#### 前 7 行：设备配置头

| 行号 | 第 1 列（标签，任意） | 第 2 列（值） | 说明 |
|------|----------------------|--------------|------|
| 1 | COM | `COM3` | 串口号 |
| 2 | BaudRate | `9600` | 波特率（覆盖全局设置） |
| 3 | IsSpecial | `true` 或 `false` | `true` 为整帧直写；`false` 为 Mark/Space 分段发送 |
| 4 | ComIndex | `1` | 串口索引编号（OPC UA 节点 ID 使用） |
| 5 | DeviceId | `1` | 设备站地址（0~255） |
| 6 | DataLen | `32` | 期望的数据字节数 |
| 7 | KKSPrefix | `10HNA01_` | KKS 前缀（自动拼接到每个数据点名称前） |

#### 第 8 行：分隔行（自动跳过）

#### 第 9 行起：数据记录表

| 列名 | 类型 | 说明 |
|------|------|------|
| `kks` | 字符串 | KKS 标识符（自动拼接第 7 行的前缀） |
| `type_` | 字符串 | 值类型：`uint` / `bool` / `float` |
| `f_type` | 字符串 | 功能类型（预留，填任意值） |
| `byte_index` | 整数 | 数据在响应帧中的字节偏移（**从 1 开始**） |
| `bit_index` | 字符串 | 位偏移：单位 `"3"` 或范围 `"0.15"`（表示第 0~15 位） |
| `def` | 整数 | 默认值（预留，填 `0`） |
| `max` | 整数 | 最大值（预留，填 `0`） |
| `min` | 整数 | 最小值（预留，填 `0`） |
| `lh` | 整数 | 字节序：`1` = 大端，`0` = 小端 |

### CSV 文件示例

```csv
COM,COM3
BaudRate,9600
IsSpecial,true
ComIndex,1
DeviceId,1
DataLen,32
KKSPrefix,10HNA01_
separator_line,,,,,,,,,
kks,type_,f_type,byte_index,bit_index,def,max,min,lh
RunStatus,bool,DI,1,0,0,1,0,0
FaultAlarm,bool,DI,1,1,0,1,0,0
Temperature,float,AI,2,0.15,0,10000,0,0
Pressure,uint,AI,4,0.15,0,65535,0,1
```

### 值类型解析规则

| `type_` 值 | OPC UA 类型 | 解析方式 |
|------------|-------------|---------|
| `uint` | UInt32 | 原始无符号整数 |
| `bool` | Boolean | 非零为 `true`，零为 `false` |
| `float` | Float | 原始值 ÷ 100.0（如 500 → 5.0） |

### 位偏移 `bit_index` 说明

- **单位**：填写一个数字，如 `3`，表示取 `byte_index` 对应字节的第 3 位。
- **范围**：填写两个数字用 `.` 分隔，如 `0.15`，表示从 `byte_index` 起的第 0 位到第 15 位（共 16 位 = 2 字节）。

---

## 5. 启动与运行

### 启动命令

```bash
# Windows
rcd_server.exe

# Linux
./rcd_server
```

程序会自动：
1. 读取 `config/config.toml` 全局配置。
2. 扫描 `config/` 下所有 `rcd*.csv` 文件。
3. 按串口号聚合设备，为每个串口启动独立通信线程。
4. 启动下级 OPC UA 服务器，默认监听 `0.0.0.0:4841`（`4840` 保留给 RCD Manager）。
5. 等待客户端连接。

### 日志级别控制

程序使用 `flexi_logger`。默认读取 `config/config.toml` 中的 `[logging]`，文件日志写入可执行文件同级的 `logs/` 目录，按配置滚动和清理。也可以通过环境变量 `RUST_LOG` 临时覆盖 `logging.level`：

```bash
# 显示所有日志（调试时推荐）
set RUST_LOG=debug        # Windows CMD
$env:RUST_LOG="debug"     # Windows PowerShell
export RUST_LOG=debug     # Linux

# 只显示警告和错误
set RUST_LOG=warn

# 只显示 rcd_server 的 info 级别日志
set RUST_LOG=rcd_server=info

# 启动
rcd_server.exe
```

高频或噪声日志建议只在排查时打开：

```bash
# 记录串口收发原始帧
set RCD_LOG_SERIAL_FRAMES=1
$env:RCD_LOG_SERIAL_FRAMES="1"
export RCD_LOG_SERIAL_FRAMES=1

# 保留 OPC UA 客户端断开连接类传输错误
set RCD_LOG_OPCUA_TRANSPORT_ERRORS=1
$env:RCD_LOG_OPCUA_TRANSPORT_ERRORS="1"
export RCD_LOG_OPCUA_TRANSPORT_ERRORS=1
```

| 日志级别 | 用途 |
|---------|------|
| `error` | 严重错误（串口打开失败、OPC UA 服务器异常） |
| `warn` | 警告信息（CSV 解析失败、角色切换、订阅者断开） |
| `info` | 运行状态（串口重连成功、收到客户端命令、服务器启动） |
| `debug` | 调试信息 |

### 退出

按 `Ctrl+C` 可安全退出程序，OPC UA 服务器会优雅关闭。

---

## 6. OPC UA 客户端接入

### 连接信息

| 项目 | 值 |
|------|-----|
| 服务器地址 | `opc.tcp://<服务器IP>:4841/` |
| 应用名称 | `Serial OPC UA Server` |
| 应用 URI | `urn:SerialServer:rcd_server` |
| 命名空间 URI | `urn:SerialServer` |

### 安全端点

| 端点名 | 安全策略 | 认证方式 |
|--------|---------|---------|
| `none` | 无加密无签名 | 仅匿名 |
| `basic256sha256_signenc` | Basic256Sha256 签名+加密（**默认**） | 匿名 / 用户名密码 |

**内置用户账号：**
- 用户名：`user1`
- 密码：`pwd`

### 节点结构

```
Objects/
├── Devices/                              # 数据变量节点文件夹
│   ├── 10HNA01_RunStatus                 # 设备数据节点：纯 KKS（kks_prefix + 名称）
│   ├── 10HNA01_FaultAlarm
│   ├── 10HNA01_Temperature
│   ├── 10HNA01_Pressure
│   ├── 91UGM91110_Fire                   # 计算衍生节点：纯 kks_calc
│   ├── COM3_Send                         # 写命令节点：{串口号}_Send（仅此节点保留串口号）
│   ├── 10HNB01_Speed
│   ├── COM4_Send
│   └── ...
└── Methods/                              # Method 调用节点文件夹
    ├── COM3_SendCommand                  # {串口号}_SendCommand
    ├── COM4_SendCommand
    └── ...
```

### 节点命名规则

| 节点类型 | 命名格式 | 示例 | 是否可写 |
|---------|---------|------|------|
| 设备数据节点 | 纯 `{KKS}` | `10HNA01_Temperature` | 否 |
| 计算衍生节点 | 纯 `{kks_calc}` | `91UGM91110_Fire` | 否 |
| 写命令节点 | `{串口号}_Send` | `COM3_Send` | 是 |
| Method 节点 | `{串口号}_SendCommand` | `COM3_SendCommand` | — |

Linux 串口配置使用完整设备路径时，OPC UA 节点只使用路径末段。例如
`/dev/ttyAP0` 对外发布为 `ttyAP0_Send` 和 `ttyAP0_SendCommand`，串口通信内部仍使用
`/dev/ttyAP0`。

**约束**：所有 KKS（来自 CSV 的 `kks_prefix` + 数据点名）必须在所有串口/设备间**全局唯一**，否则启动时会输出 `[OPC UA] 检测到 ... KKS 重复` 错误日志，OPC 节点会冲突。请在 CSV 的 `kks_prefix` 中保证唯一。

**计算引擎**：[calc.toml](#) 表达式中可以直接引用 OPC 节点暴露的同名变量（设备数据节点和计算衍生节点）。基础变量未在 CSV 数据点中定义时，公式将回退到 `default_value`，启动期会输出警告列表。

---

## 7. 命令下发

向设备发送命令有两种方式：

### 方式一：写入 Send 节点

1. 在 OPC UA 客户端中找到 `{串口号}_Send` 节点（如 `COM3_Send`）。
2. 向该节点**写入十六进制字符串**（不含空格，不含前缀 `0x`）。
3. 软件会自动追加校验和并发送到串口。
4. 发送完成后节点自动清零。

**示例：** 向设备地址 `0x01` 发送命令 `0x06 0x02`：
- 写入字符串：`010602`
- 实际发送帧：`01 06 02 09`（最后一字节 `09` 为自动追加的校验和）

> 写命令节点每 200ms 轮询一次。

### 方式二：调用 Method 节点

1. 在 OPC UA 客户端中找到 `{串口号}_SendCommand` 节点（如 `COM3_SendCommand`）。
2. 调用该 Method，传入参数 `HexData`（十六进制字符串）。
3. 返回执行结果描述字符串。

**优点：** 实时响应，无需等待轮询周期。

### 命令格式要求

- 十六进制字符串，**偶数长度**，大小写均可。
- **最少 2 字节**（第 1 字节为设备地址，第 2 字节起为命令数据）。
- **无需手动计算校验和**，程序自动追加。

| 输入 | 说明 |
|------|------|
| `010602` | ✅ 合法（3 字节：地址 + 命令） |
| `0a1bff03` | ✅ 合法（4 字节） |
| `01` | ❌ 过短（至少 2 字节） |
| `0G12` | ❌ 非法十六进制字符 |
| `012` | ❌ 奇数长度 |

---

## 8. 主备切换机制

### 角色说明

| 角色 | 行为 |
|------|------|
| **主机 (Primary)** | 主动轮询设备、广播采集数据、转发数据给备机 |
| **备机 (Secondary)** | 被动接收主机转发的数据帧，不主动轮询设备 |

### 自动切换规则

| 触发条件 | 切换动作 |
|---------|---------|
| 备机连续 **20 次**超时（主机可能宕机） | 备机 → 临时升级为主机 |
| 临时主机连续 **100 次**解析失败 | 临时主机 → 切回备机 |
| 临时主机收到原主机的**转发帧** | 立即切回备机（主机已恢复） |

### 切换行为

- 切换时**重置所有故障计数器**，避免频繁来回切换。
- 切换事件会记录到系统日志并输出 `WARN` 级别日志。
- 两台机器的 `config.toml` 中 `run_on` 应分别设为 `primary` 和 `secondary`。

### 典型部署

```
┌─────────────────────┐    ┌─────────────────────┐
│   主机 (Primary)     │    │   备机 (Secondary)   │
│  run_on = primary   │    │  run_on = secondary  │
│  current_run = pri  │    │  current_run = sec   │
│                     │───→│   接收转发数据        │
│  主动轮询设备        │    │   监控主机状态        │
└─────────────────────┘    └─────────────────────┘
         │                          │
         │    串口总线               │
    ═════╧══════════════════════════╧═════
              下位机设备
```

---

## 9. 串口通信细节

### 基本参数

| 参数 | 值 | 说明 |
|------|-----|------|
| 读取超时 | 由 `settings.timeout` 配置（示例 300ms） | 超过此时间无有效数据视为超时 |
| 打开延迟 | 2000ms | 打开串口前等待设备就绪 |
| 读缓冲区 | 1024 字节 | 单帧最大接收长度 |
| 轮询间隔 | 由 `settings.poll_interval_ms` 配置（默认 20ms） | 每次轮询循环后的等待时间 |

### 整帧直写模式（`is_special = true`）

串口驱动或硬件自行处理地址位时，程序直接写入完整命令帧，不切换校验位。

### Mark/Space 模式（`is_special = false`）

程序通过切换校验位模拟 9 位数据通信：

1. 设置 **Mark** 校验位 → 发送**地址字节**（第 9 位 = 1）。
2. 等待 **100ms**，确保地址字节已发送。
3. 设置 **Space** 校验位 → 发送**数据部分**（第 9 位 = 0）。
4. 当前实现保持 Space 校验位用于后续接收。

### 设备超时跳过策略

当单个设备连续超时超过 **3 次**后：
- 每 **10 轮**才重试该设备一次。
- 其余轮次跳过，避免拖慢其他设备的轮询速度。
- 设备恢复响应后自动取消跳过。

### 重连机制

- 串口发生 I/O 错误时自动尝试重连。
- 重连失败时向上层发送全零数据状态。
- 下次轮询周期会再次尝试重连。

### 数据推送策略

| 情况 | 行为 |
|------|------|
| 新数据与上次**不同** | 立即推送到 OPC UA |
| 新数据与上次**相同** | 连续 10 次后强制推送一次 |

---

## 10. 日志与故障排查

### 常见日志信息

| 日志 | 级别 | 含义 |
|------|------|------|
| `配置文件路径: "..."` | INFO | 正常启动，显示加载的配置文件路径 |
| `已加载 N 个串口, 共 M 个设备` | INFO | 正常启动，显示扫描到的设备数量 |
| `串口 COMX 打开失败` | WARN | 串口不存在或被占用 |
| `串口 COMX 重新连接成功` | INFO | 重连串口成功 |
| `重连串口 COMX 失败` | ERROR | 重连失败，将在下次轮询时重试 |
| `角色切换 主机 → 备机：...` | WARN | 发生主备切换 |
| `角色切换 备机 → 主机：...` | WARN | 发生主备切换 |
| `订阅者已断开，移除` | WARN | OPC UA 客户端断开连接 |
| `未找到任何有效的设备配置文件` | WARN | `config/` 下没有匹配的 CSV 文件 |
| `OPC UA 服务器已正常退出` | INFO | Ctrl+C 退出 |
| `OPC UA 服务器运行失败` | ERROR | 服务器异常退出 |

### 故障排查清单

| 问题 | 排查步骤 |
|------|---------|
| 程序闪退 / 报"加载配置文件失败" | 检查 `config/config.toml` 是否存在且格式正确 |
| 无数据节点出现 | 检查 `config/` 目录下是否有 `rcd*.csv` 文件 |
| OPC UA 客户端连接不上 | 检查防火墙是否放行下级默认的 **4841** 端口 |
| OPC UA 显示安全错误 | 尝试使用 `none` 端点连接，或检查 `pki/` 目录证书 |
| 所有数据都是 0 | 检查串口号是否正确，设备是否通电，波特率是否匹配 |
| 部分设备数据为 0 | 检查 CSV 中的 `device_id` 与设备实际站地址是否一致 |
| 数据值异常 | 检查 CSV 中的 `byte_index`、`bit_index`、`lh`（字节序）是否正确 |
| 命令下发无响应 | 确认写入的十六进制字符串长度为偶数且 ≥ 4 字符（2 字节） |

---

## 11. PKI 证书管理

### 首次运行

程序首次启动时会在 `pki/` 目录下**自动生成**服务器证书和密钥对，无需手动配置。

### 客户端证书信任

程序默认配置为 `trust_client_certs(true)`，即**自动信任所有客户端证书**。

如需手动管理信任关系：
1. 客户端首次连接后，其证书会出现在 `pki/rejected/` 目录。
2. 将该证书文件移动到 `pki/trusted/` 目录即可信任。

### 证书更新

删除 `pki/own/` 和 `pki/private/` 中的文件后重启程序，会重新生成证书。

---

## 12. 附录：协议帧格式

### 查询命令帧（主机 → 设备）

```
字节位置:  [0]        [1]    [2]    [3]    [4]    [5]       [6]
含义:      设备地址   CMD    子CMD   页码   偏移   数据长度   校验和
固定值:              0x08   0x02   0x30   0x10   (来自CSV)
```

校验和 = 前面所有字节的**累加和**（溢出截断，即 wrapping add）。

### 响应数据帧（设备 → 主机）

```
字节位置:  [0]        [1]    [2]     [3 .. Len+3]    [Len+4]
含义:      设备地址   CMD    Len     状态数据         校验和
```

- **总帧长** = `Len + 5` 字节
- **状态数据长度** = `Len + 1` 字节
- 校验和 = 前面所有字节的累加和

### 转发帧格式（主机 → 备机，总线上）

```
[转发头(7字节)] [0x00] [0x00] [响应数据帧]
```

- 转发头包含目标设备地址，用于备机识别当前数据对应哪个设备。
- 双零 `00 00` 作为分隔符。
- 备机仅在转发头长度、校验和、设备地址和 `00 00` 分隔符均有效时才分离数据帧。

---

## 快速开始

1. **准备配置文件**
   ```
   config/config.toml     ← 按第 3 节填写
   config/rcd_xxx.csv     ← 按第 4 节填写（每个设备一个文件）
   ```

2. **启动程序**
   ```bash
   set RUST_LOG=info
   rcd_server.exe
   ```

3. **使用 OPC UA 客户端连接**
   - 地址：`opc.tcp://服务器IP:4841/`
   - 选择 `none` 端点（无加密）或 `basic256sha256_signenc` 端点
   - 在 `Devices` 文件夹下浏览数据节点

4. **下发命令**
   - 找到对应串口的 `xxx_Send` 节点，写入十六进制字符串
   - 或调用 `xxx_SendCommand` Method 节点
