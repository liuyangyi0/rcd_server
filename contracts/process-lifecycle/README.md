# 安全生命周期 v1

管理关系不等于运行依赖：Semaphore 停止不影响 OPC；管理端停止不停止下级；Agent 是下级唯一的进程守护者。

## Agent 接口

- `GET /api/v1/process` 返回 `lifecycle_supported`、`gateway_instance_id`、`manual_stopped` 和 `lifecycle`。不支持时管理端拒绝新的日常启停，不回退到无保护旧入口。
- `POST /api/v1/lifecycle/actions`：下述完整请求，`If-Match: "expected_revision"`，`X-RCD-Instance-ID` 固定 Agent ID。
- `GET /api/v1/lifecycle/operations/{request_id}` 查询原作业；`POST …/{request_id}/resolve` 携带原完整请求与原 If-Match。对账不再次发送 enter/stop/start/leave。

```json
{
  "request_id": "operation-unique-id",
  "instance_id": "real-lower",
  "gateway_instance_id": "gateway-stable-id",
  "manager_instance_id": "manager-stable-id",
  "actor": "server-authenticated-user",
  "action": "restart",
  "expected_revision": "agent-boot.counter",
  "lifecycle": true
}
```

动作仅 `start/stop/restart`；浏览器不能提供进程名、路径、命令或维护代次。管理端从登录上下文填入操作者、从自己的持久身份填入 manager ID，固定来源，不使用 Session 选源。

同编号同内容返回原记录；内容不同 409。未找到并不等于未执行，显式对账持久封存缺失编号，拒绝迟到提交。未知结果保留原作业、宿主机占用；只能在读取到原作业的明确证据时完成对账，不重投。

阶段：accepted、entering、draining、stopping、starting、verifying、releasing、completed、manual_stopped、unknown。已接受与执行成功不同。Agent 内部过程持久格式升级到 4，HTTP 仍为 v1；不支持格式 4 的旧 Agent 不可直接替换回退。

人工停止保留原下级栅栏，独立存为 `deferred_stop_fence`。发布可添加自己的停止保留，不覆盖这个栅栏。后续明确启动后才用原身份完成收尾。

## 宿主机互斥

Agent 日常控制与发布器共用固定二进制目录的 `.rcd-host-release.lock`。发布器和 Agent 分别持久保存发布意图与 `.rcd-host-control/lifecycle.json`；OS 锁释放不代表事务已完成。Agent 持锁调用其内部维护步骤，不重复申请同一锁。远程只允许三个显式 manager IP 使用安全生命周期写入口，底层发布维护写入口只接受 loopback；浏览器不直连 Agent。

## Semaphore

`POST /api/project/{project}/rcd/operations` 添加 `kind: service_control`、`component: manager|agent`、`action: start|stop|restart|recover`。manager 仅前三者，agent 仅 recover。单个固定设备、revision、Idempotency-Key、本次控制暂停确认必需；不接受 package。服务端选择已绑定模板与程序摘要，沿原生队列执行 `control-platform.yml`。

启动、停止和重启不更换软件。Agent 恢复仅在进程已停止且无残留下级时启动已有守护服务；若下级原作业未决则原样保留，不冒称采集已恢复。人为停止的管理端由 systemd `ConditionPathExists` 与独立停止记录保持；停止不强杀。

## 统一台账

`GET /api/project/{project}/rcd/fleet` 是原生项目只读授权接口。管理员通过设备 `machine-binding` 与 `control-binding` 分别绑定物理身份和原生控制模板；不能用设备创建接口跳过校验。

管理端 `--fleet-config /absolute/fleet-sync.json`，配置 schema 1、platform_url、project_id、token_file、source_devices。token 为原生 Semaphore 只读账号凭据，单独私有文件，不放在 URL 或接口返回。60 秒顺序同步，离线保留 SQLite 缓存；同一 revision 不接受不同绑定，身份变化拒绝自动替换。OPC 和 Agent 双重验证后才应用地址，连接重建仍核对 gateway 身份。

仅同步机器名字、地址、软件观察与进程绑定；不修改稳定 source ID、组、采集、站点、计算、账号或凭据。OPC 下级停止时不以 TCP 断开判定物理身份变化。
