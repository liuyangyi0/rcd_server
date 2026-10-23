# 项目文档导航

管理端当前文档集中在 [manager/docs](../manager/docs/README.md)，包括用户操作、计算、OPC 接入、部署及验收记录。

- [全部软件关系与使用说明](整体软件使用说明.md)：运维平台、管理端、采集、Agent、新通信程序和外部OPC客户端的分工、入口及日常操作。

- [Rust 单机使用参考](lower/standalone-guide.md)：原根目录 `USER_MANUAL.md`，部分内容针对早期单机版本，联机管理以管理端文档为准。
- [目录整理记录](maintenance/layout-2026-09-19.md)：旧路径、新路径、保留边界和恢复依据。
- [第二轮整理](maintenance/layout-2026-09-20.md)：诊断工具分类、传输产物归位及验证。
- [历史设计与源码](archive/README.md)：仅供追溯，不参与构建。
- [开发验证参考](development/agent-validation.md)：按改动选择远端检查及部署验收边界。
- [架构、发布与运维标准化计划](development/architecture-operations-standardization-plan.md)：保留 Rust/Go 业务核心，分阶段统一版本、目录、发布与运维协议；待实施，不代表已部署。
- [指令审计记录](maintenance/instruction-audit-2026-09-20.md)：本次 AGENTS 与关联文档调整及验证结果。

现场配置留在各自部署数据目录；不要从历史文档复制配置覆盖正在运行的设备。
