import type { ConfigField } from '../../api/operations';

const descriptions: Record<string, [string, string]> = {
  'logging.http_push.queue_capacity': ['日志内存队列容量（条）', '等待上传的日志条数上限。'],
  'logging.http_push.spool_directory': ['日志补传缓存目录', '下级本机的缓存目录，不是管理端或浏览器的路径。'],
  'logging.http_push.spool_max_bytes': ['日志补传缓存上限（字节）', '下级磁盘缓存容量，例如 16777216 为 16 MiB。'],
  'logging.http_push.request_timeout_ms': ['日志上传请求超时（ms）', '一次 HTTP 日志上传允许等待的时间。'],
  'logging.http_push.batch_size': ['每批日志条数', '每次上传最多包含的日志条数。'],
  'logging.http_push.flush_interval_ms': ['日志上传间隔（ms）', '正常情况下合并上传日志的间隔。'],
  'logging.http_push.enabled': ['启用日志推送', '将下级日志发送到已配置的管理端。'],
  'logging.http_push.url': ['日志接收地址', '管理端日志接收接口地址。'],
  'logging.http_push.source_id': ['日志来源标识', '应与管理端已登记的物理来源一致。'],
};
const common = new Set(['command_retry_count', 'command_interval_ms', 'query_retry_count', 'timeout',
  'poll_interval_ms', 'parity_switch_delay_ms', 'transmit_settle_guard_ms', 'send_interval_ms',
  'input_refresh_interval_ms', 'receive_timeout_ms', 'snapshot_max_age_ms']);

export function parameterPresentation(field: ConfigField) {
  const known = descriptions[field.key];
  const leaf = String(field.path.at(-1) ?? field.key);
  const group = field.key.startsWith('logging.') ? '日志' : field.key.startsWith('calculation.') ? '计算'
    : /command_(?:retry_count|interval_ms)$/.test(leaf) ? '控制命令'
    : field.key.startsWith('settings.') ? '采集轮询'
    : field.key.startsWith('serial.') || field.key.startsWith('ports.') ? '串口与通信' : '其它设置';
  return {
    label: field.label === field.key && known ? known[0] : field.label,
    hint: field.label === field.key && known ? known[1] : field.hint,
    group, common: common.has(leaf),
  };
}

export function parameterOption(field: ConfigField, value: string | number | boolean) {
  if (typeof value === 'boolean') return value ? '启用' : '停用';
  if (field.key === 'calculation.mode') return value === 'local' ? '下级计算（local）' : value === 'manager' ? '管理端计算（manager）' : String(value);
  return String(value);
}
