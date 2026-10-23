# 统一软件与下级管理接入

这是 2026-10-09 统一管理版接入说明。三台现场接入已完成，行为与稳定性验收进度见文末实施记录。平台、管理端、Agent 三者可以独立停止；业务运行不依赖 Semaphore 常驻在线。

## 人员怎么操作

- 管理端服务：Semaphore → 设备与版本 → 查看详情 → OPC 管理端 → 启动 / 停止 / 重启。不需要软件包，确认框列出影响并要求本次暂停控制。
- 下级服务：任一 OPC 管理网页 → 下级 OPC → 固定来源 → 运维 → 进程。Agent 登记、排空、执行及验证；不随选源改变目标。
- Agent 已停止：Semaphore → 设备详情 → RCD Agent → 恢复 Agent 服务。只启动已确认停止的服务，不强杀未知进程；原下级状态和未完作业保留。
- 软件升级与历史恢复仍在 Semaphore；管理端来源页提供对应机器链接。版本恢复不回写旧业务配置，也不清空维护状态。
- 看到“结果待确认”先查询/补查原编号；不要反复新建作业。未知执行记录会继续阻止同机其他启停和发布。

“人为停止”会保存。下级由 Agent 记录停止意愿及原维护身份；管理端通过停止记录与 systemd 条件保留。明确启动后才解除。进程停止、OPC 断开、Agent 不可达、站点通信故障是不同状态。

## 维护人员首次接入

全部操作应在当次控制暂停窗口，按 30 → 58 → 46 单机进行。不要并发运行旧工具或删除已有锁文件。

1. 备份对应程序、数据库（SQLite 一致性备份）、维护记录及服务定义。核对现有固定路径、原本运行/停止状态及三个稳定身份，不从 IP 推造 ID。
2. 升级受控发布工具、管理端及 Agent 到支持该能力的版本，保留原状态；需要停止的程序先进入已有维护保护并排空。Agent 仅管理下级，manager 的 systemd 单元不增加 Agent 依赖。
3. 为三个管理端各准备原生 Semaphore 只读用户令牌文件，归各管理端服务用户所有，权限 `0600`。令牌不放 Git、命令行或配置正文；这里不新设登录系统。
4. 为每台准备固定来源 ID 到已有 Semaphore 设备 ID 的映射（下面示例的 ID 必须替换为现有值，不重新生成）。
5. 使用 `tools/release/unified_install.py` 默认只读检查。`--profiles` 是包含 `lower/manager/agent` 三个管理员发布 profile 绝对路径的 JSON；`--fleet` 是下述非秘密配置。安装器本身不启停服务，`--apply` 要求程序已在原维护身份下确认停止。
6. 获得当次控制暂停确认后，用 `unified_bootstrap.py` 协调首次接入：持有共享主机锁，备份、排空管理端与下级、安全停止、调用安装器，然后按原维护身份恢复并验收；需明确 `--operation-id`、两份配置、`--expected-sha256`、`--initiator`、`--confirm-paused`。失败保留原作业，不重复执行。它不是日常启停入口，也不替换二进制。原本停止的下级仍保持停止。Agent 支持的远程 IP 必须显式为 `192.168.10.30,192.168.10.58,192.168.10.46`，原发布器继续走本机接口。
7. Semaphore 的已验证不可变工具快照更新后，用 `custom/onboard.py --enable-service-control` 绑定专用控制模板。脚本只登记资源，不执行任务；发现既有绑定不同会停止供核对。
8. 在 Semaphore 先“检查状态”，再于每台设备详情“管理机器绑定”核对地址及稳定 ID。三管理端约 60 秒同步，首次需下级正在运行以核对 OPC 身份；之后平台离线时不影响缓存绑定与本地下级操作。

配置示例（不包含令牌）：

```json
{
  "schema_version": 1,
  "platform_url": "http://192.168.10.46:3000",
  "project_id": 1,
  "token_file": "/var/lib/rcd/manager/semaphore-readonly.token",
  "source_devices": {
    "rcd-30": "这里填30现有Semaphore设备ID",
    "rcd-46": "这里填46现有Semaphore设备ID",
    "rcd-58": "这里填58现有Semaphore设备ID"
  }
}
```

安装器的三个 profile 必须位于管理员控制的固定位置。示例命令只用于说明；执行前核对参数与本次授权：

```sh
python3 tools/release/unified_install.py --host 192.168.10.30 \
  --profiles /absolute/three-profiles.json --fleet /absolute/fleet-sync.json \
  --manager-ips 192.168.10.30,192.168.10.58,192.168.10.46
```

Agent 采用 `--host-release-dir <固定二进制目录>`、`--remote-managers <三个IP>`。底层发布维护写接口仍限 loopback，远端管理端仅能提交安全生命周期作业。浏览器不直连 Agent。管理端使用 `--fleet-config` 或 `RCD_MANAGER_FLEET_CONFIG`，不替换原数据、日志、PKI 路径或业务端口。

## 兼容与恢复边界

- 2026-10-23 命名统一后，采集组为 `serial-acquisition`（串口采集组），采集来源为 `rcd-30`、`rcd-46`、`rcd-58`。不保留旧来源/组ID别名；计算导入文件须使用新ID。`ns=2`业务点不改名，`ns=3`来源和分组路径需客户端重新Browse。
- Agent实例身份、gateway/manager身份、systemd服务名和安装目录不是来源ID，不随本次来源重命名改变。历史命令/运维审计及旧计算版本保留原文，不当作当前配置；恢复旧计算版本前须手动修正旧ID。

- Agent 持久状态新增格式 4；不可回退到只识别 1–3 的旧 Agent。管理端 SQLite 增加台账缓存结构；旧二进制不能直接打开新版数据库。普通软件恢复仅支持包声明可读取当前数据格式的版本。
- 不通过恢复旧数据库、删除状态、删锁文件或改操作编号解决未知执行结果。涉及旧数据库的灾难恢复必须另行决策，不能算普通一键恢复。
- `manual-stop.json` 是停止意愿，不是故障日志；不要清理。共享 `.rcd-host-release.lock` 也不删除，锁 inode 必须固定。
- systemd 的 `ConditionPathExists=!/absolute/manual-stop.json` 不是 shell 参数，不能把整个条件加引号。本版对含空白/特殊字符的路径拒绝安装，不静默生成失效保护。
- 接入 drop-in 使用 `zz-rcd-unified.conf`，保证位于原有 `95-rcd-maintenance.conf` 之后。旧管理端单元若声明对 Agent 的依赖，会先备份原文件，再只移除该指定依赖；空 drop-in 不能用于清除 systemd 依赖。
- 麒麟环境还需核对共享锁、发布事务标记和控制目录的 KYSEC KID，Unix 权限正确不代表 Agent 可访问。标签修正必须取得定点授权并备份；不能关闭全局策略或删除锁绕过。发布器保留已配置事务文件的 KID，首次创建继承控制目录的 KID，不复制可执行信任标记。`repair_unified_kysec.py` 是本次 58 固定路径、固定原标签的一次性修复工具，不能当成通用安装步骤重跑。
- 只读取机器绑定与软件观察，不同步组、默认来源、Session 选择、计算、站点、串口或采集设置。已在执行的操作始终使用登记时的固定地址和身份。

实际测试证据与未完成项见 [实施记录](../../../docs/development/unified-management-implementation.md)。
