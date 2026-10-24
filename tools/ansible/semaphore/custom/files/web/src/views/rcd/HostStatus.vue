<template>
  <section :class="['host-status', { detailed }]">
    <h3 v-if="detailed">主机资源快照</h3>
    <p :class="isHistorical || !status.available ? 'host-warning' : 'host-caption'">
      {{ snapshotLabel }}
    </p>
    <p v-if="detailed" class="host-caption">
      最后成功观测 {{ observedAt }}；每分钟巡检采样，不是实时监控。刷新页面只读取已有快照。
    </p>
    <p v-if="detailed && lastAttemptAt" class="host-caption">最后尝试 {{ lastAttemptAt }}</p>
    <dl class="host-summary">
      <dt>CPU</dt><dd>{{ cpuSummary }}</dd>
      <dt>内存</dt><dd>{{ memorySummary }}</dd>
      <dt>磁盘</dt><dd>{{ diskSummary }}</dd>
      <dt>网络</dt><dd>{{ networkSummary }}</dd>
    </dl>
    <small v-if="!detailed" class="host-caption">{{ observedAt }}</small>
    <template v-if="detailed && status.schema_version === 1">
      <p v-if="status.error" class="host-warning" role="alert">{{ status.error }}</p>
      <dl class="host-details">
        <dt>开机时长</dt><dd>{{ uptime }}</dd>
        <dt>逻辑CPU</dt><dd>{{ count(status.cpu && status.cpu.logical_count) }}</dd>
        <dt>内存总量 / 可用</dt><dd>{{ memoryCapacity }}</dd>
        <dt>采样时间窗</dt><dd>{{ interval }}</dd>
      </dl>
      <p v-if="status.cpu && status.cpu.error" class="host-warning">CPU：{{ status.cpu.error }}</p>
      <p v-if="status.memory && status.memory.error" class="host-warning">
        内存：{{ status.memory.error }}
      </p>
      <h4>磁盘与数据目录</h4>
      <p v-if="!disks.length" class="host-caption">尚未取得磁盘指标。</p>
      <article v-for="disk in disks" :key="disk.path" class="resource-card">
        <strong>{{ disk.path }}</strong>
        <p class="host-caption">
          挂载点 {{ disk.mount_point || '未知' }} · {{ disk.filesystem || '未知文件系统' }}
        </p>
        <p v-if="disk.available">
          已用 {{ percent(disk.used_percent) }} · 总量 {{ bytes(disk.total_bytes) }} ·
          可用 {{ bytes(disk.available_bytes) }}
        </p>
        <p v-else class="host-warning">无法获取容量</p>
        <small v-if="disk.error" class="host-warning">{{ disk.error }}</small>
      </article>
      <h4>网络接口</h4>
      <p class="host-caption">链路up仅代表网卡报告链路已启用，不证明网络、下级或物理站点可达。</p>
      <p v-if="!network.length" class="host-caption">尚未取得非回环网络接口。</p>
      <article v-for="nic in network" :key="nic.name" class="resource-card">
        <strong>{{ nic.name }} · {{ linkLabel(nic.link_up) }}</strong>
        <p>IPv4：{{ addresses(nic) }}</p>
        <p>
          接收 {{ rate(nic.rx_bytes_per_second) }} · 发送 {{ rate(nic.tx_bytes_per_second) }}
        </p>
        <small class="host-caption">
          累计接收 {{ bytes(nic.rx_bytes) }} · 累计发送 {{ bytes(nic.tx_bytes) }}
        </small>
        <p v-if="nic.error || nic.addresses_error" class="host-warning">
          {{ [nic.error, nic.addresses_error].filter(Boolean).join('；') }}
        </p>
      </article>
    </template>
  </section>
</template>

<script>
export default {
  props: { device: { type: Object, required: true }, detailed: Boolean },
  data() { return { now: Date.now(), clockTimer: null }; },
  mounted() { this.updateClock(); },
  beforeDestroy() { clearTimeout(this.clockTimer); },
  computed: {
    status() { return this.device.host_status || {}; },
    age() {
      const observed = Date.parse(this.status.observed_at);
      return Number.isFinite(observed) ? (this.now - observed) / 1000 : null;
    },
    isHistorical() {
      return this.device.state !== 'checked'
        || this.age === null || this.age > 180 || this.age < -30 || this.lastAttemptFailed;
    },
    lastAttemptFailed() {
      const attempt = Date.parse(this.status.last_attempt_at);
      const observed = Date.parse(this.status.observed_at);
      return !!this.status.error && Number.isFinite(attempt)
        && Number.isFinite(observed) && attempt > observed;
    },
    lastAttemptAt() {
      const attempt = Date.parse(this.status.last_attempt_at);
      return Number.isFinite(attempt)
        ? new Date(attempt).toLocaleString('zh-CN', { hour12: false }) : '';
    },
    snapshotLabel() {
      if (this.status.schema_version !== 1) return '尚未获取主机指标';
      if (this.age === null || this.age < -30) return '观测时间无法确认 · 非实时';
      if (this.lastAttemptFailed) return '历史快照 · 本轮指标不可用';
      if (this.age > 180) return '历史快照 · 已过期';
      if (this.device.state !== 'checked') return '历史快照 · 当前检查未成功';
      if (!this.status.available) return '本轮主机指标不可用';
      return this.status.error ? '巡检快照 · 部分指标不可用' : '巡检快照 · 非实时';
    },
    observedAt() {
      return this.age === null ? '观测时间未知'
        : new Date(this.status.observed_at).toLocaleString('zh-CN', { hour12: false });
    },
    cpuSummary() {
      return this.status.cpu?.available ? this.percent(this.status.cpu.utilization_percent) : '未获取';
    },
    memorySummary() {
      return this.status.memory?.available ? this.percent(this.status.memory.used_percent) : '未获取';
    },
    disks() { return Array.isArray(this.status.disks) ? this.status.disks : []; },
    network() { return Array.isArray(this.status.network) ? this.status.network : []; },
    diskSummary() {
      const values = this.disks.filter((d) => d.available && this.validPercent(d.used_percent))
        .map((d) => d.used_percent);
      return values.length ? `最高 ${this.percent(Math.max(...values))}` : '未获取';
    },
    networkSummary() {
      const known = this.network.filter((n) => typeof n.link_up === 'boolean');
      if (!this.network.length || !known.length) return '链路状态未知';
      const up = known.filter((n) => n.link_up).length;
      const unknown = this.network.length - known.length;
      return `${up}/${this.network.length} 接口up${unknown ? `，${unknown}个未知` : ''}`;
    },
    memoryCapacity() {
      const memory = this.status.memory || {};
      return `${this.bytes(memory.total_bytes)} / ${this.bytes(memory.available_bytes)}`;
    },
    uptime() {
      const seconds = this.status.uptime_seconds;
      if (!this.validNumber(seconds)) return '未知';
      const minutes = Math.floor(seconds / 60);
      return `${Math.floor(minutes / 1440)}天 ${Math.floor((minutes % 1440) / 60)}小时 ${minutes % 60}分`;
    },
    interval() {
      const seconds = this.status.sample_interval_seconds;
      return this.validNumber(seconds) ? `${seconds.toFixed(2)}秒` : '未知';
    },
  },
  methods: {
    updateClock() {
      this.now = Date.now();
      this.clockTimer = setTimeout(this.updateClock, 1000);
    },
    validNumber(value) {
      return typeof value === 'number' && Number.isFinite(value) && value >= 0;
    },
    validPercent(value) { return this.validNumber(value) && value <= 100; },
    percent(value) { return this.validPercent(value) ? `${value.toFixed(1)}%` : '未知'; },
    count(value) { return Number.isInteger(value) && value > 0 ? `${value}个` : '未知'; },
    bytes(value) {
      if (!this.validNumber(value)) return '未知';
      if (value < 1024) return `${Math.round(value)} B`;
      const units = ['KiB', 'MiB', 'GiB', 'TiB'];
      const exponent = Math.min(Math.floor(Math.log2(value) / 10), units.length);
      return `${(value / (1024 ** exponent)).toFixed(1)} ${units[exponent - 1]}`;
    },
    rate(value) { return this.validNumber(value) ? `${this.bytes(value)}/s` : '速率未知'; },
    linkLabel(value) {
      if (value === true) return '链路up';
      return value === false ? '链路down' : '链路状态未知';
    },
    addresses(nic) {
      const values = Array.isArray(nic.ipv4_addresses) ? nic.ipv4_addresses : [];
      if (values.length) return values.join('、');
      return nic.addresses_error ? '地址无法获取' : '未配置IPv4';
    },
  },
};
</script>

<style scoped>
.host-status { font-size: 12px; line-height: 1.6; overflow-wrap: anywhere; }
.host-status p { margin: 0 0 6px; }
.host-caption { color: #6b788c; }
.host-warning { color: #986614; }
.host-summary, .host-details {
  display: grid;
  grid-template-columns: 42px minmax(0, 1fr);
  gap: 2px 8px;
}
.host-summary dt, .host-details dt { color: #6b788c; }
.host-summary dd, .host-details dd { margin: 0; }
.detailed { border-top: 1px solid #e0e5ec; margin-top: 24px; padding-top: 20px; font-size: 13px; }
.detailed h3, .detailed h4 { margin-bottom: 10px; }
.detailed h4 { margin-top: 20px; }
.detailed .host-summary { grid-template-columns: 60px minmax(0, 1fr); }
.host-details { grid-template-columns: 112px minmax(0, 1fr); margin: 14px 0; }
.resource-card {
  border: 1px solid #dce2ec;
  border-radius: 4px;
  padding: 12px;
  margin-bottom: 10px;
}
.resource-card strong { display: block; margin-bottom: 5px; }
</style>
