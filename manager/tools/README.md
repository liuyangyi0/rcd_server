# 管理端辅助工具

- `device-groups-fixture.py`：设备分组测试数据/仿真夹具，不能替代真实串口验收。
- `configure-device-groups.py`：独立测试实例配置辅助工具。
- `lower-operations-qa.py`：下级运维回归，包含显式写入确认与目标边界检查。
- `archive/soak-review5/`：历史环境专用的压测监控和切源脚本。

工具应在远端测试环境运行。先阅读参数和目标保护逻辑；登录接口新增后，旧脚本是否适配当前鉴权须单独确认，不能直接针对现网重放。
