import type { ProcessAgentIdentity as Identity } from '../../api/process';
import { InlineNotice } from '../../components/InlineNotice';
import { formatDateTime } from '../../lib/format';
import './ProcessAgentIdentity.css';

const reported = (text: string) => text === 'unknown' ? '未知（未注入）' : text;

export function ProcessAgentIdentity({ value, stale }: { value?: Identity; stale: boolean }) {
  return <section className="operations-process-card process-agent-identity" aria-label="守护代理运行版本">
    <h3>守护代理运行版本</h3>
    {stale ? <InlineNotice tone="warning">代理版本为旧记录，仅供核对；本次未取得最新状态。</InlineNotice> : null}
    {!value ? <p className="operations-muted">此代理未报告运行版本，不能根据下级版本或磁盘文件推断。是否可安全启停，以代理报告的生命周期能力为准；旧代理仍可读取状态与日志。</p> : <>
      <dl className="operations-file-meta">
        <div><dt>agent 软件版本</dt><dd>{value.build.version}{value.build.version === 'dev' ? '（开发构建）' : ''}</dd></div>
        <div><dt>发布编号</dt><dd>{reported(value.build.release_id)}</dd></div>
      </dl>
      <details><summary>构建与启动详情</summary><dl className="operations-file-meta">
        <div><dt>构建提交</dt><dd><code>{reported(value.build.commit)}</code></dd></div>
        <div><dt>构建来源</dt><dd>{value.build.dirty ? '包含未提交修改（构建自报）' : '未标记工作区修改（构建自报）'}</dd></div>
        <div><dt>源码 SHA-256</dt><dd><code>{reported(value.build.source_sha256)}</code></dd></div>
        <div><dt>构建时间（UTC）</dt><dd>{reported(value.build.built_at)}</dd></div>
        <div><dt>目标平台</dt><dd>{value.build.target}</dd></div>
        <div><dt>守护实例启动</dt><dd>{formatDateTime(value.started_at)}</dd></div>
        <div><dt>守护启动标识</dt><dd><code>{value.boot_id}</code></dd></div>
        <div><dt>代理报告时间</dt><dd>{formatDateTime(value.reported_at)}</dd></div>
      </dl></details>
      <p className="operations-muted">以上由运行中的 agent 自报，不是 Rust 采集程序或管理端的版本，也不证明磁盘入口与运行映像一致。部署核验仍需核对实际映像摘要；“刷新进程与作业”会重新读取，不后台轮询。</p>
    </>}
  </section>;
}
