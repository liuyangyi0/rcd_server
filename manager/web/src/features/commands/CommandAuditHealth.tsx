import { useQuery } from '@tanstack/react-query';

export default function CommandAuditHealth() {
  const health = useQuery({ queryKey: ['commands', 'health'], refetchInterval: 5000,
    queryFn: async ({ signal }) => {
      const response = await fetch('/healthz', { signal, credentials: 'same-origin' });
      const value = await response.json() as { checks?: Record<string, string> };
      if (!value.checks) throw new Error('健康详情不可用');
      return Object.entries(value.checks).filter(([key, message]) => key.startsWith('command') && message !== 'ok');
    } });
  if (health.isError) return <p role="status" className="text-warning">暂时无法读取命令审计健康状态。</p>;
  if (!health.data?.length) return null;
  return <section role="alert" className="content-panel"><strong className="text-warning">命令审计降级，控制仍可执行</strong><p>以下来源的记录可能不完整。恢复后仅补查、补存记录，不重新发送设备命令。</p><ul>{health.data.map(([key, message]) => <li key={key}>{key}：{message}</li>)}</ul></section>;
}
