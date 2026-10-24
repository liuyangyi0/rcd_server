# Ansible 发布编排验收（2026-09-28）

## 结论与范围

已补齐可执行的 `stage.yml`、`deploy.yml`、`verify.yml`、`rollback.yml`、`recover.yml`，保留既有 `inspect.yml`。不是仅新增文档或按钮；Ansible 实际调用原 Python 发布/恢复实现，没有重写启停逻辑。

本批源码在 `codex/opc-device-groups` 工作区，保留原有未提交修改；未提交、未推送。仅在 192.168.10.46 隔离环境执行，不部署/重启 30、46、58 的现场服务，未发送设备控制报文。

## 改动

- 显式主机别名列表、单组件、串行执行，失败立即停止后续主机；无自动重试/自动回滚。修改动作要求当次精确目标、暂停确认和操作者。
- 46 构建产物可在兼容架构目标复用；暂存校验原构建清单 SHA-256、二进制摘要、ELF 架构及版本 CLI，不要求目标源码/编译器。原构建清单不重写，目标清单另绑 host/profile。
- 只传二进制和构建清单，不同步现场配置、PKI、数据库。上传前解析并拒绝暂存路径与运行目录、源码目录或固定 EXE 重叠。目标 profile 是部署工具配置，不限制使用人修改业务配置。
- 工具按内容摘要分目录；适配器持久保存操作意图、目标、profile/工具/产物摘要和结果。成功重复操作返回原记录，未知结果不重放；新编号不能绕过未完成状态。
- 恢复明确引用原维护作业；人工回滚明确引用备份，默认保留当前数据，覆盖历史配置/数据库需额外授权。保留旧 release 的 schema/agent 兼容性护栏。
- `--check` 不上传、不执行二进制、不修改服务，仅校验输入并显示计划。

## 验证环境与命令

主机：`192.168.10.46`。隔离目录：

`/home/lenovo/code/rcd_ansible_release_20260928_iyiS5D`

Ansible：`/home/lenovo/code/rcd_ansible_tools_20260928/venv/bin/ansible-playbook`，ansible-core 2.19.13，控制端 Python 3.13.15。目标解释器 `/usr/bin/python3`；此次隔离 inventory 使用该主机上的独立临时路径和 `ansible_connection=local`，不是重新验收跨机器 SSH。

1. `python3 -m unittest discover -s tools/release -p 'test_*.py'`：**96 项通过**。新增覆盖暂存/原清单保留/架构/加载失败/篡改/不可变目录、运行路径保护、操作重放/冲突/未知恢复/持久意图、无源码目标预检。
2. 设置 `RCD_ANSIBLE_PLAYBOOK`、`RCD_ANSIBLE_STAGE_BINARY` 后运行 `python3 tools/ansible/test_playbooks.py`：**8 项通过**。实际执行 6 个入口的语法检查、预览无落盘、限制目标、防无授权修改、两虚拟目标首台失败停批、暂存重复 `changed=0`、路径误配置拒绝、未知发布不重试。
3. 在 `manager/` 设置上述 Ansible 路径及 `RCD_MAINTENANCE_INTEROP_BINARY` 后运行：

```sh
go test -race ./internal/processagent \
  -run '^TestMaintenanceRealRustInterop$/^DurableReleaseRunner$/^AnsibleRelease$' \
  -count=1 -v
```

**通过，60.918 秒**。真实链路为 ansible-playbook → Python release → Go agent HTTP → Rust OPC。使用临时配置、状态库、PKI、端口，无串口点表：

- 一次完整发布，重复同一编号不再次 start/stop/complete；只读 verify 成功。
- 第二次发布在 agent 已完成后丢失 complete 回复，Ansible 报失败；重复编号拒绝执行。
- 只读查看原记录，换恢复操作编号、保留原部署编号，显式 finish 完成；总计仅两次 start、两次 stop、两次 complete，无重复过程控制。
- 使用同一 Rust 测试映像验证发布与恢复协议，不声称完成两种正式版本之间的升级兼容验收。

本机仅编辑、传输、查看差异；Go 格式化在远端进行后取回。`git diff --check` 通过。

## 证据和未覆盖项

远端 `evidence/` 包含最终 Python/Ansible/race 互操作日志、工具版本和逐文件摘要；本地归档在 `artifacts/runs/2026-09-28-ansible-release/`。

本批没有执行现场全套升级、管理端/agent 自身新旧版本真实回滚、硬件命令、性能/一小时/过夜测试；Semaphore 尚未安装。Ansible 的 `verify` 只验运行摘要、健康与端口，不把真实点表、订阅延迟或设备动作验收混为已完成。

schema v2 agent 自身升级及新版启动后的通用回退仍有原工具的明确限制，不能用 Ansible 绕过。操作方式见 [Ansible 使用说明](../../../tools/ansible/README.md)。
