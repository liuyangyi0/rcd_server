import { processMaintenanceActive, type ProcessSnapshot } from '../../api/process';
import { InlineNotice } from '../../components/InlineNotice';
import './ProcessMaintenanceNotice.css';

const phases: Record<string, string> = {
  preparing: '正在登记维护', entering: '正在确认维护保护', draining: '等待已接收命令完成',
  ready: '已排空，等待发布', stopping: '正在停止下级', stopped: '下级已停止，维护尚未结束',
  starting: '正在启动下级', verifying: '等待发布验收', releasing: '正在确认解除维护',
  unknown: '维护结果未知，需核对',
};

export function ProcessMaintenanceNotice({ snapshot, stale }: { snapshot: ProcessSnapshot; stale: boolean }) {
  const active = processMaintenanceActive(snapshot);
	const lifecycle = snapshot.lifecycle;
	if (snapshot.lifecycle_supported) return <section aria-label="安全运维状态" className="process-maintenance-notice">
		<InlineNotice tone={active || stale ? 'warning' : 'info'}>
			<strong>{stale ? '运维旧状态，仅供核对' : active ? '目标机器正在维护或结果待确认' : snapshot.manual_stopped ? '下级已人为停止' : '安全启停已就绪'}</strong>
			{active && lifecycle && !['succeeded', 'failed'].includes(lifecycle.state) ? <>
				<p>发起管理端：{lifecycle.manager_instance_id}；操作者：{lifecycle.actor}；操作编号：{lifecycle.request_id}</p>
				<p>阶段：{({ accepted: '已受理', entering: '进入维护', draining: '等待命令完成', stopping: '停止中', starting: '启动中', verifying: '验证中', releasing: '解除维护', unknown: '结果待确认' } as Record<string, string>)[lifecycle.phase] ?? lifecycle.phase}。{lifecycle.error}</p>
			</> : active ? <p>发布编号：{snapshot.maintenance_deployment_id}；阶段：{phases[snapshot.maintenance_phase ?? ''] ?? snapshot.maintenance_phase}。请核对原发布任务，不要重复执行。</p> : <p>启动、停止、重启由目标 Agent 完成维护、排空和验证。发布或其他管理端占用时会拒绝冲突操作；软件升级请使用发布平台。</p>}
			{snapshot.manual_stopped ? <p>停止状态会保留；升级和 Agent 重启不会自动启动下级。明确点击“启动下级”后恢复。</p> : null}
			{stale ? <p>未取得新状态，控制保持禁用。</p> : null}
		</InlineNotice>
	</section>;
  return <section aria-label="发布维护状态" className="process-maintenance-notice">
    <InlineNotice tone={active || stale ? 'warning' : 'info'}>
      <strong>{stale ? '维护旧状态，仅供核对' : active ? '发布维护未结束' : snapshot.maintenance_enabled === true ? '已启用发布维护保护' : snapshot.maintenance_enabled === false ? '未启用发布维护保护' : '代理未报告维护能力'}</strong>
      {active ? <>
        <dl className="operations-file-meta"><div><dt>发布编号</dt><dd><code>{snapshot.maintenance_deployment_id}</code></dd></div><div><dt>维护阶段</dt><dd>{phases[snapshot.maintenance_phase ?? ''] ?? '暂不识别的维护阶段，需核对'}（{snapshot.maintenance_phase}）</dd></div></dl>
        <p>普通启动、停止、重启和绑定修改已禁用。进程运行不代表维护结束；下级已停止也不能绕过维护直接启动。</p>
        <p>请由发布负责人使用原发布编号核对并恢复流程。本页不续发发布动作、不解除维护，也不会自动重试发布。日志和历史仍可查看。</p>
      </> : snapshot.maintenance_enabled === true ? <p>当前 Agent 尚不支持完整安全启停，请先通过发布平台升级 Agent。本页不退回无保护的启停接口。</p>
        : <p>代理未提供安全生命周期能力，不能执行无保护启停。请先通过发布平台升级并配置 Agent；日志与状态仍可查看。</p>}
      {stale ? <p>本次未取得最新代理状态，普通控制保持禁用。请刷新核对，不以旧状态判断维护已结束。</p> : null}
    </InlineNotice>
  </section>;
}
