# 下级集中日志

新版 Rust 下级可把运行日志通过 HTTP 批量发送到管理程序。该通道只用于运维日志，不替代 OPC 实时数据、命令代理或下级本地滚动日志。

## 工作方式

1. 日志仍先写下级本机滚动文件。
2. logger 使用有界 `try_send` 投递到独立发送线程；串口采集不做网络和磁盘补发 I/O，也不会等待管理端。
3. 常规日志按 500 ms/100 条成批发送，`WARN`、`ERROR` 立即唤醒发送；一次最多 500 条。
4. 失败事件写入下级有界 spool，并按 1–30 秒指数退避补发。超过上限优先淘汰低等级日志；本地滚动文件不受影响。
5. 每次启动生成 `boot_id`，事件带单调 `sequence`。管理端在 SQLite 只保存 ACK 游标，日志正文按来源保存为 8 MiB 滚动 JSONL 文件；重试不会在页面重复显示。

管理端接收接口为 `POST /api/v1/lower-ingest/logs`，查询接口为 `GET /api/v1/lower-logs?source_id=...`。收到新批次后只通过 SSE 发送 `lower_logs_changed` 失效通知，不把日志正文推入浏览器全局状态。

## 配置与使用

在每个下级 `config/config.toml` 的 `[logging]` 后添加：

```toml
[logging.http_push]
enabled = true
url = "http://192.168.10.46:8087/api/v1/lower-ingest/logs"
source_id = "lower-a" # 必须与管理端来源 ID 完全一致
batch_interval_ms = 500
batch_size = 100
queue_capacity = 10000
spool_directory = "push-spool"
spool_max_bytes = 16777216
request_timeout_ms = 3000
```

重启下级后，在管理 Web 左侧进入“下级日志”，选择来源，可按等级、串口、站点和关键字过滤，并导出当前页 CSV。“刚收到推送”只表示最近有日志到达；安静运行时显示“已有历史日志”不等于连接故障。

排障时以两份数据互补：集中日志适合统一搜索；下级“运维 → 日志”直接读取本机原始文件，可核对管理端断网期间或 spool 淘汰的低等级日志。
