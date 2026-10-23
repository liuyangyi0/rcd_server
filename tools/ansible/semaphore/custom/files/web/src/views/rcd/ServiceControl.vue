<template>
  <section class="service-control">
    <template v-if="component === 'manager'">
      <p>只控制本机 OPC 管理端，不停止 Agent、下级或其他管理端。</p>
      <v-btn v-for="action in ['start', 'stop', 'restart']" :key="action" small outlined
        :disabled="!!reason(action)" :title="reason(action) || descriptions[action]"
        @click="open(action)">
        {{ labels[action] }}
      </v-btn>
    </template>
    <template v-else-if="component === 'agent'">
      <p>仅恢复已确认停止的 Agent。按其持久状态恢复下级；人为停止的下级不会自动启动。</p>
      <v-btn small outlined :disabled="!!reason('recover')"
        :title="reason('recover') || descriptions.recover"
        @click="open('recover')">恢复 Agent 服务</v-btn>
    </template>
    <template v-else-if="isApplication">
      <p>只控制此注册软件的固定服务。首次安装保持停止，请先完成配置；正常SIGTERM退出不等于设备事务已排空。</p>
      <v-btn v-for="a in ['start', 'stop', 'restart']" :key="a" small outlined
        :disabled="!!reason(a)" :title="reason(a) || genericDescriptions[a]" @click="open(a)">
        {{ labels[a] }}
      </v-btn>
    </template>
    <p v-if="commonReason" class="muted">{{ commonReason }}</p>
    <v-dialog v-model="opened" max-width="600" :persistent="busy || !!pending">
      <v-card>
        <v-card-title>
          {{ labels[action] }}{{ componentTitle }}
        </v-card-title>
        <v-card-text>
          <p>{{ device.name }} · {{ device.address }}</p>
          <p>{{ isApplication ? genericDescriptions[action] : descriptions[action] }}</p>
          <p v-if="!isApplication">排空超时或状态不明会保留原作业，不强杀、不自动重复执行。本操作不更换程序版本。</p>
          <p v-else>退出超时或状态不明会保留原作业，不自动强杀或重复执行。本操作不升级软件，不校验内部设备事务。</p>
          <v-alert v-if="error" type="error" text role="alert">{{ error }}</v-alert>
          <v-alert v-if="pending && !busy" type="warning" text>
            尚未收到确认，目标已锁定。请确认原请求结果，勿新建重复操作。
          </v-alert>
          <p v-if="loginRequired">
            <a href="/auth/login" target="_blank" rel="noopener noreferrer">重新登录</a>
            后返回此处确认原请求。
          </p>
          <p v-if="knownOperationId" role="status">
            服务端已登记原操作。
            <a :href="`/project/${projectId}/rcd/operations?operation=${knownOperationId}`"
              target="_blank" rel="noopener noreferrer">在新标签页查看原操作任务</a>；
            不要另建启停请求。
          </p>
          <v-checkbox v-model="paused" :disabled="busy || !!pending"
            label="已确认本次暂停相关控制命令，允许执行所列操作" />
          <v-checkbox v-if="isApplication" v-model="applicationConfirmed"
            :disabled="busy || !!pending"
            label="已完成必要配置，并确认本次启停安全；平台不核实设备事务排空" />
        </v-card-text>
        <v-card-actions>
          <v-btn text :disabled="busy || !!pending" @click="opened = false">取消</v-btn><v-spacer />
          <v-btn color="primary" depressed :loading="busy"
            :disabled="busy || (!pending && (!paused || (isApplication && !applicationConfirmed)))"
            @click="submit">
            {{ pending ? '确认原请求结果' : `确认${labels[action]}` }}
          </v-btn>
        </v-card-actions>
      </v-card>
    </v-dialog>
  </section>
</template>
<script>
import axios from 'axios';

export default {
  props: {
    projectId: Number, device: Object, component: String, allowed: Boolean, application: Object,
  },
  data() {
    return {
      opened: false,
      action: '',
      paused: false,
      applicationConfirmed: false,
      busy: false,
      pending: null,
      knownOperationId: '',
      key: '',
      error: '',
      loginRequired: false,
      now: Date.now(),
      clockTimer: null,
      generation: 0,
      labels: {
        start: '启动', stop: '停止', restart: '重启', recover: '恢复',
      },
      descriptions: {
        start: '核对原停止记录后启动并验证管理端，恢复命令准入。',
        stop: '关闭新命令准入，等待在途事务完成后停止。此人为停止状态跨主机重启保留。',
        restart: '安全排空、停止并重新启动管理端；其 OPC 客户端需重新连接。',
        recover: '经 Ansible/systemd 启动已停止的 Agent，可能按原策略恢复下级采集。',
      },
      genericDescriptions: {
        start: '启动该软件的固定systemd服务并恢复开机自启，启动前请确认配置及运行环境已完成。',
        stop: '正常发送SIGTERM并等待退出，不强杀；人为停止跨主机重启保留，平台不核实设备事务排空。',
        restart: '正常停止后启动该软件的固定服务，仅影响本组件，不自动更新配置或软件版本。',
      },
    };
  },
  mounted() { this.updateClock(); },
  beforeDestroy() { this.generation += 1; clearTimeout(this.clockTimer); },
  computed: {
    isApplication() {
      return !['lower', 'manager', 'agent', 'communicate'].includes(this.component)
        && this.application?.component === this.component;
    },
    componentTitle() {
      if (this.isApplication) return ` ${this.application.name}`;
      return this.component === 'agent' ? ' Agent 服务' : ' OPC 管理端';
    },
    observation() {
      return (this.device.components || []).find((c) => c.component === this.component) || {};
    },
    commonReason() {
      if (!['manager', 'agent'].includes(this.component) && !this.isApplication) return '该组件不提供此处的直接启停操作。';
      if (!this.allowed) return '当前账号没有执行任务权限。';
      if (!this.device.control_template_id) return '尚未接入安全服务控制，请由维护人员完成接入。';
      if (this.device.active_operation_id) return '本机有进行中或待确认的作业，请查看原操作记录。';
      if (this.device.state !== 'checked') return '需要先获得近期成功的软件状态检查。';
      const age = (this.now - Date.parse(this.device.last_success_at)) / 1000;
      if (!Number.isFinite(age) || age < -30 || age > 180) return '软件状态检查已过期或时间不明，请先检查主机。';
      if (!['running', 'stopped'].includes(this.observation.state)) return '程序状态无法确认，不能执行启停。';
      if (this.observation.disk_differs) return '磁盘与运行映像不一致，请通过软件发布流程处理。';
      return '';
    },
  },
  methods: {
    updateClock() {
      this.now = Date.now(); this.clockTimer = setTimeout(this.updateClock, 1000);
    },
    reason(action) {
      if (this.commonReason) return this.commonReason;
      const unsupported = this.component === 'agent'
        ? action !== 'recover' : !['start', 'stop', 'restart'].includes(action);
      if (unsupported) {
        return '此组件不支持该服务操作。';
      }
      if (['start', 'recover'].includes(action) && this.observation.state !== 'stopped') return '只适用于已确认停止的程序。';
      if (['stop', 'restart'].includes(action) && this.observation.state !== 'running') return '只适用于已确认运行中的程序。';
      return '';
    },
    open(action) {
      if (this.reason(action) || this.pending) return;
      this.action = action; this.error = ''; this.paused = false; this.applicationConfirmed = false;
      this.knownOperationId = '';
      this.key = ''; this.loginRequired = false; this.opened = true;
    },
    async submit() {
      if (this.busy || (!this.pending && (!this.paused || this.reason(this.action)
        || (this.isApplication && !this.applicationConfirmed)))) return;
      const retry = !!this.pending;
      if (!this.key) this.key = Array.from(crypto.getRandomValues(new Uint8Array(20)), (v) => v.toString(16).padStart(2, '0')).join('');
      if (!this.pending) {
        this.pending = {
          kind: 'service_control',
          component: this.component,
          action: this.action,
          commands_paused: true,
          targets: [{ device_id: this.device.id, revision: this.device.revision }],
        };
      }
      const body = this.pending; const project = this.projectId;
      const generation = this.generation;
      this.busy = true; this.error = '';
      try {
        const { data } = await axios.post(
          `/api/project/${project}/rcd/operations`,
          body,
          { headers: { 'If-Match': `"${body.targets[0].revision}"`, 'Idempotency-Key': this.key }, timeout: 30000 },
        );
        if (generation !== this.generation || project !== this.projectId) return;
        this.pending = null; this.opened = false;
        if (project === this.projectId) this.$router.push({ path: `/project/${project}/rcd/operations`, query: { operation: data.id } });
      } catch (e) {
        if (generation !== this.generation || project !== this.projectId) return;
        const operationId = e.response?.data?.operation_id;
        if (/^[a-f0-9]{64}$/.test(operationId || '')) this.knownOperationId = operationId;
        if (!retry && !this.knownOperationId
          && [400, 401, 403, 409, 422, 428].includes(e.response?.status)) { this.pending = null; this.key = ''; }
        this.loginRequired = e.response?.status === 401;
        this.error = e.response?.data?.error || '请求未确认；请确认原请求结果，不要重复启停。';
      } finally {
        if (generation === this.generation && project === this.projectId) this.busy = false;
      }
    },
  },
};
</script>
<style scoped>
.service-control { margin: 18px 0; font-size: 14px; }
.service-control .v-btn { margin: 0 8px 8px 0; }
.muted { color: #6b788c; margin: 8px 0; }
</style>
