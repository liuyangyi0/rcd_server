# 管理程序文档索引

## 使用与操作

- [使用说明](guides/使用说明.md) · [功能说明](guides/功能说明.md)
- [网页登录、密码恢复与分页](guides/web-login-and-usability.md)
- [设备拓扑、分组、选源及通信状态](guides/device-groups.md)
- [自动切源与来源数据差异告警](guides/redundancy.md)
- [下级配置、串口和站点操作](guides/lower-operations.md)
- [下级进程启停与重启](guides/lower-process-management.md)
- [集中日志](guides/central-lower-logs.md)
- [设备版本管理](guides/设备版本管理说明.md)
- [计算中心状态输入](guides/calculation-state-inputs.md) · [详细配置示例](examples/calc-station-state-commented.toml)

## 部署与接口

- [Linux 固定路径、自启动](deployment/linux-autostart.md)
- [Windows 部署](deployment/windows-deployment.md)
- [架构与 OPC 行为](reference/architecture.md)
- [REST / SSE](reference/rest-api.md) · [安全边界](reference/security.md)

## 测试与历史验收

- [验证方法与发布检查](validation/validation.md)
- [设备分组](validation/device-groups-validation.md)
- [进程管理历史验收](validation/process-control-validation.md)
- [站点通信状态](validation/station-communication-validation.md)
- [计算状态输入](validation/calculation-state-inputs-validation.md)
- [Web 登录与易用性](validation/web-login-validation.md)
- [自动切源与差异告警](validation/redundancy-validation.md)
- [第二管理端 .58 部署](validation/manager58-deployment.md)

验收记录是特定日期、版本的快照，不表示所有历史包仍在运行，也不代表每次后续修改都重新执行了所有长期压测。最新部署状态以相应验收记录为准。
