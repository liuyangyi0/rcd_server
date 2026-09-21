# 项目指令审计记录 · 2026-09-20

## 范围与依据

审计对象为 `rcd_server_device_groups`，分支 `codex/opc-device-groups`，基线 HEAD `6f0dd6a`，含本轮开始前已有的未提交功能和目录整理变更。另一个 `rcd_server` / `dev_opc` worktree 未修改。

对当前 worktree 使用包含隐藏及忽略文件的文件名扫描，排除 `.git`、构建缓存、node_modules 和 artifacts：发现项目指令仅根 `AGENTS.md`，未发现 `AGENTS.override.md` 或 `SKILL.md`，因而也没有项目技能专属 references。未把供应商文档、历史包里的文件当成当前指令，未创建空壳技能，未改用户全局/插件技能。

采用 [GPT-6 Astra 官方模型指南](https://developers.openai.com/api/docs/guides/latest-model?model=gpt-6-astra) 的指令清晰性、主动执行与权限边界、冲突透明说明、按风险验证原则。官方示例需要结合运行环境裁剪，不整段复制；本次没有引入主动多 agent、API 模型参数或模型版本绑定。

审计使用 OpenAI Docs 技能核对官方资料，并用 Skill Creator 的渐进披露原则组织项目说明：常驻文件保留必要约束，命令与领域细节按需读取。技能没有导致任务暂停，也未授予额外部署权限。

## 发现与处理

| 问题 | 修改 |
| --- | --- |
| 原 AGENTS 只描述 Rust，遗漏 Go、Web 及补丁依赖 | 补齐入口，完整目录不再重复，链接 README |
| 明确写有本机 `cargo run`，与远端测试要求冲突 | 移除本机执行指引；集中远端限制和隔离要求 |
| 构建与测试章节重复列命令，容易被理解成全量必跑 | 命令集中到按需读取的验证参考，按风险选择检查 |
| 缺少查日志、修复、部署和物理控制的授权区分 | 明确任务边界；常规已授权步骤不中断，新增影响才询问 |
| 缺少选源、中央计算及命令 ACK 的长期约束 | 精炼成业务不变量，链接现有详细协议 |
| 设备分组指南和管理 README 笼统称管理端不计算 | 限定为未激活中央计算的兼容转发模式，补充中央模式 |
| 历史本机/PTY 验收与现行要求混淆 | 历史证据保留并标注；PTY 仅为隔离回归，不能替代实机 |
| 根 README 重复操作边界，含“本轮”临时表述 | 改为引用 AGENTS，去掉跨任务易过期的措辞 |
| 原链接检查未覆盖 AGENTS | 将 AGENTS 纳入现有只读检查，无新增测试框架 |

精简的是重复内容和指令歧义，并非宣称文件字数一定更少：原文件遗漏了重要业务与授权约束，本次补齐后常驻内容有所增加。没有照搬模型的人格、表达风格或完整工具操作手册。

## 边界人工复核

以下是逐条阅读后的场景核对，不是独立模型行为评测：

| 场景 | 修订后应采取的行为 |
| --- | --- |
| “看一下日志” | 只读检查；不清日志、不重启 |
| “修复并远端测试” | 编辑代码并在隔离目录验证；不自动替换运行服务 |
| “修改并部署到指定机器” | 已获授权的检查、备份、部署直接推进，不逐步重复询问 |
| 远端无法连接 | 明确未验证，不转为本机执行 |
| 纯文档变更 | 链接、差异及冲突审查，不启动全量软件压测 |
| 通信中断/Session 切源 | 采集清零后继续计算并传播质量；中央计算不随 Session 切源 |
| 模拟测试通过/长测刚开始 | 分别标注证据边界，不宣称实机或长测完成 |
| 技能与任务冲突 | 指明文件、条款及解释，不静默扩权或停止 |

## 验证结果

- 本地 `git diff --check -- AGENTS.md README.md docs manager/README.md manager/docs tools/maintenance/check_layout.py` 通过；仅有 Git 的 LF/CRLF 提示，无空白错误。
- 将本轮文档及检查器同步到远端 `192.168.10.46` 的隔离副本 `/home/lenovo/code/rcd_layout_20260919`，执行 `python3 tools/maintenance/check_layout.py`，退出码 0：`PASS: 16 required inputs, 35 documents, 95 local links, Rust/Go path dependencies.`
- 原始检查日志：远端 `/home/lenovo/code/rcd_layout_20260919/artifacts/runs/instruction-audit-20260920/structure.log`；本地副本 `artifacts/maintenance/instruction-audit-20260920-structure.log`。
- 已人工复核上述 8 个边界场景；没有运行多模型 A/B 或独立 agent 行为评测，不能据此声称模型执行效率已量化提升。

未修改业务源码、运行配置、认证策略或二进制；未执行部署、服务重启、真实串口命令或长测。本记录不会把此前测试结果算作本次回归。旧 worktree 与已有用户修改均保留，未提交或推送 Git。
