# REST 与 SSE 接口

## 基本约定

默认基地址为 `http://<manager-host>:8080`。接口无登录、无 token，也没有用户权限模型。所有能够访问监听端口的非浏览器客户端都可以调用读写接口，因此必须依赖隔离内网、防火墙/VLAN 和主机访问控制。

JSON 写请求必须满足：

- `Content-Type: application/json`；
- 请求体不超过 1 MiB；
- 未定义字段会被拒绝；
- 浏览器写请求的 `Origin` 必须与请求 scheme/host 完全一致；
- `Sec-Fetch-Site` 若存在，只接受 `same-origin` 或 `none`。

服务不开放 CORS。没有 `Origin` 的脚本/设备请求仍然允许，所以同源检查不是身份认证。

## Endpoint 一览

| Method | Path | 说明 |
|---|---|---|
| GET | `/api/v1/overview` | 总览、计数、版本和时间 |
| GET/POST | `/api/v1/groups` | 组列表/创建组 |
| GET/PUT/DELETE | `/api/v1/groups/{id}` | 读取/更新/删除组 |
| PUT | `/api/v1/groups/{id}/default-source` | 修改默认源 |
| GET/POST | `/api/v1/sources` | 来源列表/创建来源 |
| GET/PUT/DELETE | `/api/v1/sources/{id}` | 读取/更新/删除来源 |
| POST | `/api/v1/sources/{id}/probe` | 独立执行 Endpoint、登录、Namespace 和目录浏览测试，不修改在线连接 |
| POST | `/api/v1/sources/{id}/reconnect` | 立即重启该来源连接监督器；重连过程也会重新浏览目录 |
| POST | `/api/v1/sources/{id}/rediscover` | 立即重连并重新发现目录；保留用于脚本表达操作意图 |
| GET | `/api/v1/sessions` | 当前在线 OPC Session 列表及各组路由 |
| PUT | `/api/v1/sessions/{session}/groups/{group}/selection` | 修改目标 Session 的组选源 |
| GET | `/api/v1/points` | 分页点位，可查看默认源、指定 Session 或指定来源样本 |
| GET | `/api/v1/points/export` | 按相同筛选条件导出点位 CSV |
| GET | `/api/v1/commands` | 分页命令审计 |
| GET | `/api/v1/commands/export` | 按相同筛选条件导出命令 CSV |
| GET | `/api/v1/alarms` | 分页持久告警 |
| PUT | `/api/v1/alarms/{id}/acknowledgement` | 确认告警 |
| PUT | `/api/v1/alarms/{id}/silence` | 设置或取消告警静默截止时间 |
| GET | `/api/v1/source-events` | 下级连接、Session、订阅、目录和错误状态历史 |
| GET | `/api/v1/management-audit` | revision 化管理操作审计 |
| GET | `/api/v1/system` | 运行参数、证书有效期、存储和进程队列信息 |
| GET | `/api/v1/configuration` | 导出不含密码的配置 JSON |
| POST | `/api/v1/configuration/import` | 配置预检或导入；实际导入只允许空管理器 |
| GET | `/api/v1/diagnostics` | 下载不含数据库和密码的诊断 ZIP |
| GET | `/api/v1/events` | SSE 事件流 |
| GET | `/metrics` | Prometheus 文本指标 |
| GET | `/healthz` | 存活/数据库和目录持久化健康状态 |
| GET | `/readyz` | 是否已经进入可服务状态 |

路径中的组、来源和 Session ID 必须做 URL 编码，尤其是 OPC Session NodeId 文本可能包含 `;`、`=` 等字符。

## Revision 与 `If-Match`

更新、删除、默认源切换和 Session 选源都使用乐观并发控制。先从单项 GET 的 JSON `revision` 或响应 `ETag` 读取当前版本，再在写请求中发送：

```http
If-Match: "7"
```

支持带引号的数字和弱 ETag 形式。缺失 header 返回 HTTP 428；格式非法返回 400；版本已变化返回 HTTP 409：

```json
{
  "error": {
    "code": "revision_conflict",
    "message": "revision conflict: expected 7, actual 8",
    "details": {"expected": 7, "actual": 8}
  }
}
```

组的配置 revision、来源配置 revision，以及每个 Session/组的 selection revision 相互独立。修改默认源不会改变在线 Session 的 selection revision。

## 配置示例

以下 PowerShell 示例创建一个组和一个无安全下级来源：

```powershell
$base = 'http://127.0.0.1:8080'

$group = Invoke-RestMethod -Method Post `
  -Uri "$base/api/v1/groups" `
  -ContentType 'application/json' `
  -Body (@{ id = 'line-a'; name = '一号线' } | ConvertTo-Json)

$source = Invoke-RestMethod -Method Post `
  -Uri "$base/api/v1/sources" `
  -ContentType 'application/json' `
  -Body (@{
    id = 'line-a-primary'
    group_id = 'line-a'
    name = '一号线主机'
    endpoint = 'opc.tcp://10.10.1.21:4840'
    security_policy = 'None'
    security_mode = 'None'
    enabled = $true
  } | ConvertTo-Json)
```

合法的下级安全组合只有：

- `None` / `None`；
- `Basic256Sha256` / `SignAndEncrypt`。

用户名为空时使用 Anonymous；用户名非空时使用 Username。创建时 `password` 可省略。更新来源时省略 `password` 会保留现有密码，传空字符串会清除密码；响应只包含 `has_password`。

来源完成首次发现、`catalog_state` 变为 `Ready` 后设置默认源：

```powershell
$current = Invoke-RestMethod "$base/api/v1/groups/line-a"
Invoke-RestMethod -Method Put `
  -Uri "$base/api/v1/groups/line-a/default-source" `
  -Headers @{ 'If-Match' = '"' + $current.revision + '"' } `
  -ContentType 'application/json' `
  -Body (@{ source_id = 'line-a-primary' } | ConvertTo-Json)
```

清除默认源时传空字符串。清除后仅影响以后激活的新 Session，已有 Session 的选源不变：

```powershell
$current = Invoke-RestMethod "$base/api/v1/groups/line-a"
Invoke-RestMethod -Method Put `
  -Uri "$base/api/v1/groups/line-a/default-source" `
  -Headers @{ 'If-Match' = '"' + $current.revision + '"' } `
  -ContentType 'application/json' `
  -Body (@{ source_id = '' } | ConvertTo-Json)
```

新来源处于 `Discovering`、来源被禁用或目录为 `CatalogConflict` 时，设为默认源/选中源会返回 HTTP 409 `source_not_selectable`。

## 组与来源请求体

创建/更新组：

```json
{
  "id": "line-a",
  "name": "一号线"
}
```

更新请求中的 `id` 可以省略，但如果给出必须和路径一致。`id`/`group_id` 只允许 1–64 个字母、数字、点、下划线和连字符。

创建/更新来源：

```json
{
  "id": "line-a-secure",
  "group_id": "line-a",
  "name": "一号线安全端点",
  "endpoint": "opc.tcp://10.10.1.22:4840",
  "security_policy": "Basic256Sha256",
  "security_mode": "SignAndEncrypt",
  "username": "user1",
  "password": "replace-me",
  "enabled": true
}
```

修改 endpoint、group、安全策略或安全模式会清除该来源的持久目录并重新发现；当前进程内旧点保留为 retired，直到重启清理。修改用户名、密码或启用状态会重启连接但保留缓存目录；仅修改显示名称不会中断现有连接或重建 5900 点订阅。

删除来源前必须保证它不是任何组的默认源、没有被在线 Session 选择、也没有 pending 命令。删除整个组要求没有在线 Session、没有 pending 命令；提交后会在同一事务内删除组内来源和目录缓存。

## Session 选源示例

先读取在线 Session：

```powershell
$sessions = Invoke-RestMethod "$base/api/v1/sessions"
$session = $sessions.items[0]
$route = $session.routes.'line-a'
$sessionPath = [uri]::EscapeDataString($session.id)

Invoke-RestMethod -Method Put `
  -Uri "$base/api/v1/sessions/$sessionPath/groups/line-a/selection" `
  -Headers @{ 'If-Match' = '"' + $route.revision + '"' } `
  -ContentType 'application/json' `
  -Body (@{ source_id = 'line-a-backup' } | ConvertTo-Json)
```

Session 已结束时返回 404。成功后仅刷新该 Session 对应组的已监控点，不改变其他 Session 或全局默认源。同值切源也会递增 revision。

## 分页与筛选

所有列表接口使用 `limit`/`offset`，默认 `limit=100`，最大 1000。响应结构为：

```json
{
  "items": [],
  "total": 0,
  "limit": 100,
  "offset": 0
}
```

支持的 query 参数：

- `group_id`：组筛选；
- `source_id`：来源筛选；
- `route_source_id`：只对点位接口有效；用该来源生成样本视图，不修改任何路由；
- `session_id`：只对点位接口有效；按该在线 OPC Session 的真实路由生成样本视图，Session 结束返回 404；
- `status`：点位可用 `good`、`bad` 或十六进制 StatusCode；命令按状态文本筛选；
- `kind`：来源状态历史和管理审计的事件类型筛选；
- `q`：点位 identifier/BrowseName 或命令文本搜索；
- `limit`、`offset`：分页。

点位路由优先级为 `session_id`、`route_source_id`、`source_id`、组默认源。`source_id` 同时把目录限制为该来源实际提供的成员；需要只改变样本路由而仍查看组并集时，应使用 `route_source_id`。诊断视图只读，不会修改默认源或 Session 路由。

浏览器可在请求头发送最多 64 字符的 `X-RCD-Operator`。服务把它与远端 IP 一起写入后续管理审计；该字段只用于内网追溯，不是身份认证，客户端不得把它当作权限凭证。

配置导出明确省略下级密码。`POST /configuration/import` 请求体为 `{"document": <导出文档>, "dry_run": true|false}`；`dry_run=true` 始终只校验。已有任何组或来源时，真实导入返回 409，避免覆盖在线路由。Username 来源导入后需要重新填写密码。

## SSE

连接：

```powershell
curl.exe -N http://127.0.0.1:8080/api/v1/events
```

服务每 15 秒发送注释 heartbeat。当前命名事件包括：

- `source.status`、`source.sample`；
- `catalog.changed`、`group.changed`；
- `selection.changed`、`session.changed`；
- `command.changed`；
- `alarm.changed`、`configuration.changed`。

SSE 是“相关资源已变化”的通知，不是全量实时点流。`source.sample` 只携带来源和 identifier 等轻量信息；客户端应使相关分页查询失效后按需重取，不能把约 10 万点持续塞入全局前端状态。

## HTTP 错误

错误统一为：

```json
{
  "error": {
    "code": "pending_command",
    "message": "group has a pending command",
    "details": null
  }
}
```

主要状态：

| HTTP | 常见 code | 含义 |
|---:|---|---|
| 400 | `invalid_json`、`invalid_revision`、`invalid_page` | 请求格式错误 |
| 403 | `cross_origin_write` | 浏览器写请求不同源 |
| 404 | `not_found` | 资源不存在或 Session 已结束 |
| 409 | `revision_conflict`、`pending_command`、`resource_in_use`、`source_not_selectable` | 并发冲突或状态不允许 |
| 415 | `json_required` | 写请求不是 JSON |
| 422 | `invalid_request` | 字段或关联关系非法 |
| 428 | `revision_required` | 缺少 `If-Match` |
| 500 | `internal_error` | 内部错误，明细只写日志 |
