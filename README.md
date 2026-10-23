# RCD 采集与 OPC 管理系统

本仓库包含 Rust 下级串口采集程序和 Go 管理程序。当前开发分支为 `codex/opc-device-groups`；不要把其它 Git worktree 的旧版本目录当作当前发布源码。

## 从这里开始

- 全部软件怎么配合、怎么操作：[整体软件使用说明](docs/整体软件使用说明.md)。
- 使用管理程序：[使用说明](manager/docs/guides/使用说明.md)、[功能说明](manager/docs/guides/功能说明.md)、[网页登录与分页](manager/docs/guides/web-login-and-usability.md)。
- 配置计算：[站点状态计算输入](manager/docs/guides/calculation-state-inputs.md)、[带注释的计算配置示例](manager/docs/examples/calc-station-state-commented.toml)。
- 安装与维护：[Linux 固定部署与自启动](manager/docs/deployment/linux-autostart.md)、[Windows 部署](manager/docs/deployment/windows-deployment.md)。
- 开发管理端：[管理模块说明](manager/README.md)、[完整文档索引](manager/docs/README.md)。
- 新通信程序：[Communicate](communicate/README.md)、[OPC客户端变量与读写说明](communicate/docs/opc-client-guide.md)。已实现1↔9数据布局，2026-10-20在46以串口停用模式部署原名接口；真实收发未验收，9→2布局未支持。
- 查找旧文件：[整理记录](docs/maintenance/layout-2026-09-19.md)、[历史产物说明](artifacts/README.md)。

## 目录约定

```text
src/                     现有 RCD Rust 下级程序源码
crates/                  下级共用的纯逻辑模块（配置参数草稿，无文件写入）
tests/                   Rust 集成测试
test_data/               测试输入、期望结果
Cargo.toml / Cargo.lock  Rust 依赖与版本锁
calc.toml                已有计算配置文件（保留原路径，不擅自替换）
serialport-rs/           Rust 串口本地依赖
vendor/                  Rust OPC 库补丁依赖
communicate/             机组间串口同步；独立依赖锁、1↔9编解码与原名 OPC 收发接口
manager/
  cmd/ internal/         Go 入口、业务逻辑与测试
  web/                   React 页面、前端测试和内嵌资源入口
  third_party/           Go OPC 本地补丁依赖，不能当缓存删除
  deploy/                服务模板及环境配置示例
  docs/                  分类文档、配置示例与验收证据
  tools/                 管理端测试工具；archive/ 是历史专用脚本
tools/                   工具导航见 tools/README.md
  serial/                串口探针、实验及日志分析
  opc/                   OPC 观察、客户端及端到端测试
  fuzz-device/           设备模拟与配套打包入口
  maintenance/           目录整理清单和检查工具
docs/                    全项目说明、下级参考、历史设计与整理记录
artifacts/               本地历史包、测试日志、传输补丁及迁移清单（忽略入 Git）
```

`src/` 与 `manager/` 的构建根目录未变；没有把源码再复制出一套“新版”。`manager/web/dist/` 是前端内嵌资源路径，不是历史发布目录；其中的占位文件也不能删除。

## 工作边界

- 开发与操作授权统一见 [AGENTS.md](AGENTS.md)，按改动选择检查见 [验证参考](docs/development/agent-validation.md)。
- `target/`、`manager/web/node_modules/` 等是可重建缓存，但不应在无关任务中顺手清空。
- 历史产物先归档而非删除。归档文件不是“最新安装包”，发布版本以验收记录与二进制 `version` 输出为准。
- 根目录的旧构建入口仍保留，避免破坏已有调用；历史 Windows/musl 构建脚本不因这次整理而被宣称已重新验收。

第二轮整理的位置与检查结果见 [2026-09-20 整理记录](docs/maintenance/layout-2026-09-20.md)。
