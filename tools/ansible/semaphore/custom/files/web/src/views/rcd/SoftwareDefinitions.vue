<template>
  <v-dialog :value="value" max-width="760" :persistent="busy || !!pending"
    @input="$emit('input', $event)">
    <v-card class="software-definitions">
      <v-card-title>添加软件种类</v-card-title>
      <v-card-text>
        <p>登记新的受管软件种类后，再上传该种类的版本包。此处不会安装或启动任何主机。</p>
        <p>首版只支持Linux单二进制标准包，不接收自定义安装脚本、任意安装路径或shell命令。</p>
        <v-alert v-if="error" type="error" text role="alert">{{ error }}</v-alert>
        <v-alert v-if="pending && !busy" type="warning" text>
          登记结果未确认，内容已锁定。请确认原登记结果，不能另换内容重复提交。
        </v-alert>
        <template v-if="!registered">
          <v-text-field v-model="component" label="软件种类ID" placeholder="例如 report-worker"
            outlined dense maxlength="32"
            hint="小写英文字母开头，允许小写字母、数字和连字符；创建后不可更改"
            persistent-hint :disabled="busy || !!pending" />
          <v-text-field v-model="name" label="显示名称" placeholder="例如 数据报表服务"
            outlined dense maxlength="80"
            :disabled="busy || !!pending" />
          <v-textarea v-model="argumentsText" label="启动参数（可选，每行一个参数）"
            outlined rows="3" :disabled="busy || !!pending"
            hint="原样作为参数传给单个程序，不经shell；不填安装命令。登记后不可修改。"
            persistent-hint />
          <v-checkbox v-model="gracefulShutdown" :disabled="busy || !!pending"
            label="已确认软件支持SIGTERM正常退出；平台不核实其设备事务排空" />
          <p v-if="validation" class="warning-text" role="status">{{ validation }}</p>
          <p class="muted">标识符不能与四个内置组件或系统保留名称重复，服务名和目录由平台生成。</p>
          <details v-if="validCustomId" class="definition-preview">
            <summary>查看自动生成的位置与服务名（不能自行修改）</summary>
            <dl><dt>程序位置</dt><dd>/opt/rcd/apps/{{ component }}/bin/{{ component }}</dd>
              <dt>工作目录</dt><dd>/var/lib/rcd/apps/{{ component }}</dd>
              <dt>用户服务</dt><dd>rcd-app-{{ component }}.service</dd></dl>
            <p>这些是安装时的受控位置，登记种类本身不会创建目标目录或启动服务。</p>
          </details>
        </template>
        <template v-else>
          <v-alert type="success" text role="status">{{ resultMessage }}</v-alert>
          <dl>
            <dt>软件种类</dt><dd>{{ registered.name }}（{{ registered.component }}）</dd>
            <dt>安装位置</dt><dd>{{ registered.install_path || '由平台固定配置' }}</dd>
            <dt>工作目录</dt><dd>{{ registered.work_dir || '由平台固定配置' }}</dd>
            <dt>服务</dt><dd>{{ registered.service_unit || '由平台固定配置' }}</dd>
          </dl>
          <p>下一步上传manifest中component为{{ registered.component }}的标准版本包。</p>
        </template>
      </v-card-text>
      <v-card-actions>
        <v-btn text :disabled="busy || !!pending" @click="$emit('input', false)">关闭</v-btn>
        <v-spacer />
        <v-btn v-if="!registered" color="primary" depressed :loading="busy"
          :disabled="!allowed || busy || (!pending && !!validation)" @click="submit">
          {{ pending ? '确认原登记结果' : '登记软件种类' }}
        </v-btn>
        <v-btn v-else color="primary" depressed @click="uploadVersion">上传该软件的版本包</v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>
</template>
<script>
import axios from 'axios';

export default {
  props: { value: Boolean, projectId: Number, allowed: Boolean },
  data() {
    return {
      component: '',
      name: '',
      argumentsText: '',
      gracefulShutdown: false,
      busy: false,
      error: '',
      pending: null,
      registered: null,
      resultMessage: '',
      generation: 0,
    };
  },
  computed: {
    validCustomId() {
      return /^[a-z][a-z0-9-]{0,31}$/.test(this.component)
        && !['lower', 'manager', 'agent', 'communicate'].includes(this.component);
    },
    arguments() { return this.argumentsText.split(/\r?\n/).filter((s) => s.length); },
    validation() {
      if (!this.allowed) return '当前账号没有项目资源管理权限，不能登记软件。';
      if (!/^[a-z][a-z0-9-]{0,31}$/.test(this.component)) return '请填写有效的软件种类ID。';
      if (['lower', 'manager', 'agent', 'communicate'].includes(this.component)) return '不能覆盖内置组件。';
      if (!this.name.trim() || this.name.length > 80) return '请填写1–80字符的显示名称。';
      if (this.arguments.length > 16 || this.argumentsText.length > 2048
        || this.arguments.some((arg) => arg.length > 256 || !/^[A-Za-z0-9 ./:=,@+_-]+$/.test(arg)
          || arg.includes('..'))) {
        return '启动参数最多16项，每项256字符、合计2048字符，仅接受普通字母数字与路径标点。';
      }
      if (!this.gracefulShutdown) return '请先确认该软件支持正常退出，平台不能替它排空业务事务。';
      return '';
    },
  },
  watch: {
    value(open) {
      this.generation += 1;
      if (open) this.reset();
    },
    projectId() { this.generation += 1; this.reset(); this.$emit('input', false); },
  },
  beforeDestroy() { this.generation += 1; },
  methods: {
    reset() {
      this.component = ''; this.name = ''; this.argumentsText = ''; this.gracefulShutdown = false;
      this.pending = null; this.registered = null; this.error = ''; this.resultMessage = '';
      this.busy = false;
    },
    async submit() {
      if (this.busy || !this.allowed || (!this.pending && this.validation)) return;
      const generation = this.generation;
      const retry = !!this.pending;
      const body = this.pending || {
        component: this.component,
        name: this.name.trim(),
        arguments: this.arguments,
        graceful_shutdown_confirmed: this.gracefulShutdown,
      };
      this.pending = body; this.busy = true; this.error = '';
      try {
        const { data, status } = await axios.post(
          `/api/project/${this.projectId}/rcd/applications`,
          body,
          { timeout: 20000 },
        );
        if (generation !== this.generation) return;
        if (data.component !== body.component) throw new Error('registered software identity mismatch');
        this.registered = data; this.pending = null;
        this.resultMessage = status === 201 ? '已登记软件种类，尚未上传或安装。'
          : '该定义已存在，已返回原记录，未重复登记或安装。';
        this.$emit('registered', data);
      } catch (e) {
        if (generation !== this.generation) return;
        if (!retry && [400, 401, 403, 409, 422, 428].includes(e.response?.status)) {
          this.pending = null;
        }
        this.error = e.response?.status === 401 ? '登录失效，请重新登录后确认原登记。'
          : e.response?.data?.error || '未收到登记确认，请使用原内容确认结果；不要另建种类绕过。';
      } finally { if (generation === this.generation) this.busy = false; }
    },
    uploadVersion() {
      if (!this.registered) return;
      this.$emit('upload', this.registered); this.$emit('input', false);
    },
  },
};
</script>
<style scoped>
.software-definitions { padding: 8px; color: #202a3d; }
.software-definitions p { line-height: 1.65; }
.software-definitions dl { display: grid; grid-template-columns: 100px minmax(0, 1fr); gap: 10px; }
.software-definitions dd { margin: 0; overflow-wrap: anywhere; }
.muted { color: #6b788c; }
.warning-text { color: #986614; }
.definition-preview { border-top: 1px solid #dce2ec; padding-top: 14px; }
.definition-preview summary { color: #344e79; cursor: pointer; }
</style>
