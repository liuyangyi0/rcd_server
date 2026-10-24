<template>
  <section class="machine-editor">
    <v-btn v-if="allowed" small outlined @click="open">管理机器绑定</v-btn>
    <v-dialog v-model="opened" max-width="680" :persistent="busy">
      <v-card>
        <v-card-title>统一机器绑定 · {{ device.name }}</v-card-title>
        <v-card-text>
          <p>固定 IP：{{ device.address }}。不修改串口、站点、计算或选源配置。三台管理端约 60 秒同步一次；占用或身份无法确认时保留原绑定。</p>
          <v-alert v-if="error" type="error" text role="alert">{{ error }}</v-alert>
          <v-alert v-if="device.active_operation_id" type="warning" text>
            本机有进行中或待确认的操作，不能改变绑定。
          </v-alert>
          <p v-if="blockedReason && !busy" class="binding-warning" role="status">
            {{ blockedReason }}
          </p>
          <v-text-field v-model="form.name" label="机器显示名称"
            outlined dense :disabled="busy" maxlength="128" />
          <v-text-field v-model="form.opc_endpoint" label="下级 OPC 地址"
            outlined dense :disabled="busy" />
          <v-text-field v-model="form.agent_url" label="Agent 内网运维地址"
            outlined dense :disabled="busy" />
          <v-text-field v-model="form.agent_instance_id" label="Agent 稳定 ID（只读检查确认）"
            outlined dense readonly />
          <v-text-field v-model="form.gateway_instance_id" label="下级稳定 ID（保存时再次核对）"
            outlined dense readonly />
          <v-text-field v-model="form.manager_instance_id" label="管理端稳定 ID（只读检查确认）"
            outlined dense readonly />
          <p>地址只能属于此固定 IP；既有 ID 不随名称或地址变化。缺少身份时先执行“检查状态”，不要手工编造 ID。</p>
        </v-card-text>
        <v-card-actions><v-btn text :disabled="busy" @click="opened = false">取消</v-btn><v-spacer />
          <v-btn color="primary" depressed :loading="busy" :disabled="blocked" @click="save">
            核对身份并保存
          </v-btn>
        </v-card-actions>
      </v-card>
    </v-dialog>
  </section>
</template>
<script>
import axios from 'axios';

export default {
  props: { projectId: Number, device: Object, allowed: Boolean },
  data() {
    return {
      opened: false,
      busy: false,
      error: '',
      revision: 0,
      form: {},
      generation: 0,
      now: Date.now(),
      clockTimer: null,
    };
  },
  computed: {
    blocked() {
      return !!this.blockedReason;
    },
    blockedReason() {
      if (this.busy) return '正在核对身份并保存，请等待原请求结果。';
      if (!this.allowed) return '当前账号没有管理机器绑定权限。';
      if (this.device.active_operation_id) return '本机有进行中或待确认的操作，须先核对原任务。';
      const age = (this.now - Date.parse(this.device.last_success_at)) / 1000;
      if (this.device.state !== 'checked' || !Number.isFinite(age) || age < -30 || age > 180) {
        return '需要180秒内的新鲜检查；请关闭此窗口，先检查设备状态。';
      }
      if (!this.form.agent_instance_id || !this.form.gateway_instance_id
        || !this.form.manager_instance_id) {
        return '尚未取得完整运行身份。请先检查状态，不要手工编造稳定ID。';
      }
      if (!this.form.name?.trim()) return '请填写机器显示名称。';
      if (!this.form.opc_endpoint?.trim() || !this.form.agent_url?.trim()) return '请填写下级OPC和Agent地址。';
      return '';
    },
  },
  watch: {
    projectId() { this.invalidate(); },
    'device.id': function deviceChanged() { this.invalidate(); },
    opened(open) {
      clearTimeout(this.clockTimer);
      if (open) this.updateClock();
    },
  },
  beforeDestroy() { this.generation += 1; clearTimeout(this.clockTimer); },
  methods: {
    invalidate() {
      this.generation += 1; this.opened = false; this.busy = false; this.error = '';
      clearTimeout(this.clockTimer);
    },
    updateClock() {
      this.now = Date.now();
      if (this.opened) this.clockTimer = setTimeout(this.updateClock, 1000);
    },
    open() {
      if (!this.allowed || this.busy) return;
      this.generation += 1; this.now = Date.now();
      const info = (c) => (this.device.components || [])
        .find((v) => v.component === c)?.compatibility || {};
      this.form = {
        name: this.device.name,
        opc_endpoint: this.device.opc_endpoint || `opc.tcp://${this.device.address}:4841`,
        agent_url: this.device.agent_url || `http://${this.device.address}:8090`,
        agent_instance_id: this.device.agent_instance_id || info('agent').instance_id || '',
        gateway_instance_id: this.device.gateway_instance_id || info('agent').gateway_instance_id || '',
        manager_instance_id: this.device.manager_instance_id || info('manager').instance_id || '',
      };
      this.revision = this.device.revision; this.error = ''; this.opened = true;
    },
    async save() {
      if (this.blocked) return;
      const project = this.projectId; const id = this.device.id; const generation = this.generation;
      const current = () => generation === this.generation && project === this.projectId
        && id === this.device.id && this.opened;
      this.busy = true; this.error = '';
      try {
        const { data } = await axios.put(
          `/api/project/${project}/rcd/devices/${id}/machine-binding`,
          { ...this.form, revision: this.revision },
          { headers: { 'If-Match': `"${this.revision}"` } },
        );
        if (!current()) return;
        if (data.id !== id) throw new Error('machine binding identity mismatch');
        this.opened = false; this.$emit('changed', data);
      } catch (e) {
        if (current()) {
          this.error = e.response?.status === 401 ? '登录已失效，请重新登录。'
            : e.response?.data?.error || '保存未确认，请刷新台账核对原绑定。';
        }
      } finally { if (generation === this.generation) this.busy = false; }
    },
  },
};
</script>
<style scoped>
.machine-editor { margin: 16px 0; }
.binding-warning { color: #986614; }
</style>
