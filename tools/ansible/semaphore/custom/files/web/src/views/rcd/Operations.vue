<template>
  <main class="rcd-operations">
    <header>
      <div>
        <v-btn class="mobile-menu" small outlined @click="openNavigation">菜单</v-btn>
        <h1>操作任务</h1>
        <p>升级、恢复和服务控制共用原生队列；结果待确认时不会自动重投。</p>
      </div>
      <v-btn outlined :loading="loading" @click="load">刷新列表</v-btn>
    </header>
    <v-alert v-if="error" type="error" text role="alert">{{ error }}</v-alert>
    <p v-if="error && items.length" class="rcd-warning" role="status">
      本次筛选或第 {{ pendingPage }} 页尚未确认，下方保留上次成功读取的第 {{ page }} 页。
    </p>
    <form @submit.prevent="searchNow">
      <v-text-field
        v-model="search"
        label="搜索主机、IP、版本或编号"
        outlined
        dense
        clearable
        hint="也支持发布编号和操作者；不会搜索业务日志"
        persistent-hint
        @click:clear="searchNow"
      /><v-select
        v-model="state"
        :items="states"
        label="任务状态"
        outlined
        dense
        hide-details
        @change="searchNow"
      /><v-select v-model="kind" :items="operationKinds"
        label="操作类型" outlined dense hide-details @change="searchNow" />
      <v-btn outlined type="submit">查询</v-btn>
    </form>
    <p class="muted refresh-status" role="status">
      {{ listRefreshText }}
      · 自动刷新不会重新执行任务
    </p>
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th>操作 / 组件</th>
            <th>目标版本</th>
            <th>目标机器</th>
            <th>结果</th>
            <th>创建时间 / 操作者</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="o in items" :key="o.id">
            <td data-label="操作">
              {{ actionName(o.request.action) }}
              <span>{{ componentName(o.request.component || o.package.component) }}</span>
            </td>
            <td data-label="版本">
              <strong>
                {{ o.request.kind === 'service_control' ? '不更换版本' : o.package.version }}
              </strong>
            </td>
            <td data-label="机器">{{ o.targets.map((t) => t.name).join(' → ') }}</td>
            <td data-label="结果">
              <span :class="statusClass(o.state)">{{ status(o.state, o.request.action) }}</span>
              <small>{{ duration(o.targets.flatMap((target) => target.stages || [])) }}</small>
            </td>
            <td data-label="创建时间">
              {{ date(o.created_at) }}<span class="muted">{{ o.actor }}</span>
            </td>
            <td><v-btn small outlined @click="show(o)">查看过程</v-btn></td>
          </tr>
          <tr v-if="!loading && !items.length">
            <td colspan="6" class="empty">
              {{
                error
                  ? '任务列表暂时无法读取，请重试；不代表任务已停止。'
                  : '还没有符合条件的操作任务。上传软件包不会自动部署。'
              }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <footer>
      <span>共 {{ total }} 条 · 第 {{ totalPages ? page : 0 }} / {{ totalPages }} 页</span>
      <div>
        <v-select
          v-model="pageSize"
          :items="[20, 50, 100]"
          label="每页条数"
          outlined
          dense
          hide-details
          @change="searchNow"
        /><v-btn outlined :disabled="loading || page <= 1" @click="changePage(-1)">上一页</v-btn
        ><v-btn outlined :disabled="loading || page >= totalPages" @click="changePage(1)"
          >下一页</v-btn
        >
      </div>
    </footer>
    <v-navigation-drawer
      :value="!!selected"
      right
      temporary
      fixed
      :stateless="!!cancelTarget || cancelling"
      :width="$vuetify.breakpoint.xsOnly ? '100%' : 680"
      @input="closeDetails"
    >
      <section v-if="selected" class="details">
        <div class="detail-heading">
          <h2>执行过程</h2>
          <v-btn outlined :loading="detailLoading" title="只读取原任务，不重新安装或重启"
            @click="refreshDetail">刷新任务详情</v-btn>
          <v-btn text @click="closeDetails(false)">关闭详情</v-btn>
        </div>
        <p>
          {{ actionName(selected.request.action) }} ·
          {{ componentName(selected.request.component || selected.package.component) }} ·
          {{ selected.package.version || '不更换版本' }}
        </p>
        <p :class="statusClass(selected.state)">
          {{ status(selected.state, selected.request.action) }}
        </p>
        <v-alert v-if="selected.error" type="warning" text>{{ selected.error }}</v-alert>
        <v-alert v-if="detailError" type="error" text role="alert">{{ detailError }}</v-alert>
        <p v-if="detailError" class="rcd-warning">下方保留上次结果，不能据此确认任务当前状态。</p>
        <p class="muted" role="status">{{ detailRefreshText }}</p>
        <p class="muted">操作编号：{{ selected.id }}</p>
        <v-btn v-if="selected.task_id" outlined @click="openLog(selected.task_id)"
          >查看原生任务日志 #{{ selected.task_id }}</v-btn
        >
        <v-btn
          v-if="selected.state === 'queued' && canRun"
          outlined
          :loading="cancelling"
          @click="openCancel"
          >取消尚未执行的任务</v-btn
        >
        <p class="muted">进入执行后不能通过取消强制中断操作。SSH 断开不表示目标机没有执行。</p>
        <v-btn
          v-if="
            canRun && ['unknown', 'needs_maintenance', 'recovery_failed'].includes(selected.state)
          "
          outlined
          :loading="reconciling"
          @click="readback"
          >补查原事务</v-btn
        >
        <p v-if="readbackMessage" role="status">{{ readbackMessage }}</p>
        <section v-for="t in selected.targets" :key="t.device_id" class="target-detail">
          <h3>{{ t.name }} · {{ t.address }}</h3>
          <p :class="statusClass(t.state)">{{ status(t.state, selected.request.action) }}</p>
          <p class="muted">{{ duration(t.stages || []) }}</p>
          <p v-if="t.error" class="rcd-warning">{{ t.error }}</p>
          <ol>
            <li v-for="(s, i) in t.stages" :key="i">
              <strong>{{ stageName(s.stage) }}</strong> · {{ stageState(s.state)
              }}<small
                >{{ date(s.started_at)
                }}<template v-if="s.ended_at"> — {{ date(s.ended_at) }}</template></small
              ><small v-if="s.error">{{ s.error.message }}</small
              ><small v-else-if="s.error_type">{{ s.error_type }}</small>
            </li>
          </ol>
          <p v-if="!(t.stages || []).length" class="muted">
            {{
              t.state === 'not_started'
                ? '尚未执行；取消或前序目标失败后不会继续此目标。'
                : '等待目标机的结构化结果。详细实时输出见原生任务日志。'
            }}
          </p>
          <p v-if="t.acceptance && t.acceptance.state === 'installed_stopped'">
            已安装并保持停止，未将磁盘版本冒充运行版本。
          </p>
        </section>
      </section>
    <v-dialog :value="!!cancelTarget" max-width="560" :persistent="cancelling"
      @input="closeCancel">
      <v-card v-if="cancelTarget" class="cancel-dialog">
        <v-card-title>取消尚未执行的任务？</v-card-title>
        <v-card-text>
          <p>
            {{ actionName(cancelTarget.request.action) }} ·
            {{ componentName(cancelTarget.request.component || cancelTarget.package.component) }}
          </p>
          <p>目标：{{
            cancelTarget.targets.map((target) => `${target.name}（${target.address}）`).join('、')
          }}</p>
          <p>
            只取消当前操作编号 {{ cancelTarget.id }} 的排队请求，不停止已经执行的程序。
            如果任务已开始或状态已改变，服务端会拒绝取消；关闭此对话框不会取消任务。
          </p>
          <v-alert v-if="cancelError" type="warning" text role="alert">{{ cancelError }}</v-alert>
        </v-card-text>
        <v-card-actions><v-spacer />
          <v-btn text :disabled="cancelling" @click="closeCancel(false)">保留任务</v-btn>
          <v-btn color="primary" depressed :loading="cancelling" :disabled="!!cancelError"
            @click="cancelOperation">确认取消此任务</v-btn>
        </v-card-actions>
      </v-card>
    </v-dialog>
    </v-navigation-drawer>
  </main>
</template>
<script>
import axios from 'axios';
import EventBus from '@/event-bus';
import PermissionsCheck from '@/components/PermissionsCheck';
import { USER_PERMISSIONS } from '@/lib/constants';

const labels = {
  created: '等待入队',
  queued: '等待执行',
  running: '执行中',
  succeeded: '成功',
  recovered: '已恢复原版本',
  partial_failed: '部分完成（有失败）',
  failed_before_install: '安装前失败',
  failed_before_execution: '执行前失败',
  unknown: '结果待确认',
  needs_maintenance: '需要维护处理',
  recovery_failed: '恢复失败',
  cancelled: '已取消',
  waiting: '等待设备',
  not_started: '未开始',
};
export default {
  mixins: [PermissionsCheck],
  props: { projectId: Number },
  data() {
    return {
      items: [],
      total: 0,
      totalPages: 0,
      page: 1,
      pendingPage: 1,
      pageSize: 20,
      search: '',
      kind: '',
      operationKinds: [
        { text: '全部操作', value: '' },
        { text: '软件升级 / 恢复', value: 'release' },
        { text: '服务启停 / 恢复', value: 'service_control' },
      ],
      state: '',
      error: '',
      loading: false,
      selected: null,
      timer: null,
      sequence: 0,
      detailSequence: 0,
      detailError: '',
      detailLoading: false,
      detailUpdatedAt: '',
      lastUpdatedAt: '',
      pollAllowed: true,
      cancelling: false,
      cancelTarget: null,
      cancelError: '',
      cancelSequence: 0,
      reconciling: false,
      readbackMessage: '',
      previousTitle: '',
      applications: [],
      applicationSequence: 0,
      states: [
        { text: '全部状态', value: '' },
        ...Object.entries(labels)
          .filter(([value]) => !['waiting', 'not_started'].includes(value))
          .map(([value, text]) => ({ value, text })),
      ],
    };
  },
  computed: {
    listRefreshText() {
      if (this.loading && !this.items.length) return '正在读取任务…';
      return this.lastUpdatedAt ? `列表更新于 ${this.date(this.lastUpdatedAt)}` : '尚未取得任务列表';
    },
    detailRefreshText() {
      if (this.detailLoading) return '正在核对原任务…';
      return this.detailUpdatedAt ? `任务确认于 ${this.date(this.detailUpdatedAt)}` : '正在获取任务详情';
    },
    base() {
      return `/api/project/${this.projectId}/rcd/operations`;
    },
    canRun() {
      return this.can(USER_PERMISSIONS.runProjectTasks);
    },
  },
  created() {
    this.previousTitle = document.title;
    document.title = '操作任务 - Semaphore UI';
    this.load();
    this.loadApplications();
  },
  beforeDestroy() {
    clearTimeout(this.timer);
    this.sequence += 1;
    this.detailSequence += 1;
    this.applicationSequence += 1;
    this.cancelSequence += 1;
    document.title = this.previousTitle;
  },
  watch: {
    projectId() {
      this.selected = null;
      this.cancelTarget = null;
      this.cancelError = '';
      this.cancelSequence += 1;
      this.cancelling = false;
      this.items = [];
      this.detailSequence += 1;
      this.detailUpdatedAt = '';
      this.lastUpdatedAt = '';
      this.applications = [];
      this.loadApplications();
      this.searchNow();
    },
    '$route.query.operation': function operationChanged() {
      this.detailSequence += 1;
      this.detailUpdatedAt = '';
      this.readbackMessage = '';
      this.detailError = '';
      if (this.$route.query.operation) this.load();
      else this.selected = null;
    },
  },
  methods: {
    async loadApplications() {
      this.applicationSequence += 1;
      const sequence = this.applicationSequence;
      const project = this.projectId;
      try {
        const { data } = await axios.get(`/api/project/${project}/rcd/applications`, { timeout: 15000 });
        if (sequence === this.applicationSequence && project === this.projectId) {
          this.applications = data.items;
        }
      } catch (e) {
        // An optional display-name lookup must not block original task reconciliation.
      }
    },
    async readback() {
      if (!this.selected || this.reconciling) return;
      const selected = this.selected;
      const base = this.base;
      this.reconciling = true;
      this.detailError = '';
      try {
        const { data } = await axios.post(
          `${base}/${selected.id}/reconcile`,
          {},
          { headers: { 'If-Match': `"${selected.revision}"` }, timeout: 20000 },
        );
        if (this.selected?.id !== selected.id || base !== this.base) return;
        this.readbackMessage = data.failed_device_ids.length
          ? '部分补查未入队，请查看设备状态后重试；不会重复安装。'
          : '已提交只读补查，结果会自动更新；不会重新安装或重启程序。';
        await this.load();
      } catch (e) {
        if (this.selected?.id === selected.id && base === this.base) this.detailError = this.errorText(e, '补查结果未确认，请刷新后重试；不会重新安装。');
      } finally {
        this.reconciling = false;
      }
    },
    openNavigation() {
      EventBus.$emit('i-show-drawer');
    },
    openLog(id) {
      EventBus.$emit('i-show-task', { taskId: id });
    },
    componentName(c) {
      return {
        lower: '下级 OPC', manager: 'OPC 管理端', agent: 'RCD Agent', communicate: '串口通信程序',
      }[c] || this.applications.find((app) => app.component === c)?.name || c;
    },
    actionName(action) {
      return {
        upgrade: '软件升级', restore: '版本恢复', start: '启动', stop: '停止', restart: '重启', recover: '恢复 Agent 服务',
      }[action] || '未知操作';
    },
    date(v) {
      return v ? new Date(v).toLocaleString('zh-CN', { hour12: false }) : '—';
    },
    status(s, action) {
      if (s === 'succeeded') return `${this.actionName(action)}成功`;
      return labels[s] || '无法确认';
    },
    duration(stages) {
      const starts = stages.map((s) => Date.parse(s.started_at)).filter(Number.isFinite);
      const ends = stages.map((s) => Date.parse(s.ended_at)).filter(Number.isFinite);
      if (!starts.length || !ends.length) return '耗时：等待执行记录';
      const seconds = Math.max(0, Math.round((Math.max(...ends) - Math.min(...starts)) / 1000));
      // Only confirmed stage timestamps count; an unknown transaction is not
      // presented as still running or completed based on the browser clock.
      return `已记录耗时：${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`;
    },
    statusClass(s) {
      if (s === 'succeeded') return 'valid';
      if (['partial_failed', 'failed_before_install', 'failed_before_execution', 'recovery_failed'].includes(s)) return 'rcd-failure';
      if (['unknown', 'needs_maintenance', 'recovery_failed', 'recovered'].includes(s)) {
        return 'rcd-warning';
      }
      return 'muted';
    },
    stageState(s) {
      return (
        {
          intent: '等待确认',
          confirmed: '已确认',
          unknown: '结果未知',
          failed: '失败',
        }[s] || s
      );
    },
    stageName(s) {
      return (
        {
          executor_start: '启动执行器',
          maintenance: '关闭准入并排空',
          stopped: '停止目标程序',
          backed_up: '备份当前程序与数据',
          installed: '安装程序',
          software_acceptance: '确认实际运行映像',
          maintenance_completed: '结束维护',
          maintenance_enter: '关闭命令准入并排空',
          service_stop: '安全停止并保留人为停止状态',
          service_start: '启动、验证并解除维护',
          agent_recovery: '恢复 Agent 服务并核对下级状态',
          automatic_binary_recovery: '恢复原程序',
        }[s] || s
      );
    },
    searchNow() {
      this.$nextTick(() => this.load(false, 1));
    },
    async changePage(d) {
      if (this.loading) return;
      await this.load(false, this.page + d);
    },
    async show(o) {
      this.cancelTarget = null;
      this.cancelError = '';
      this.detailSequence += 1;
      this.readbackMessage = '';
      this.detailError = '';
      this.detailUpdatedAt = '';
      this.selected = o;
      if (this.$route.query.operation !== o.id) {
        await this.$router.replace({ query: { ...this.$route.query, operation: o.id } });
      }
    },
    closeDetails(v) {
      if (!v) {
        // A dependent dialog must not dismiss its owning task drawer.
        if (this.cancelling || this.cancelTarget) return;
        this.cancelTarget = null;
        this.cancelError = '';
        this.detailSequence += 1;
        this.selected = null;
        this.readbackMessage = '';
        this.detailError = '';
        this.detailUpdatedAt = '';
        this.detailLoading = false;
        const q = { ...this.$route.query };
        delete q.operation;
        this.$router.replace({ query: q }).catch(() => {});
      }
    },
    openCancel() {
      if (!this.selected || this.selected.state !== 'queued' || !this.canRun || this.cancelling) return;
      this.cancelTarget = this.selected;
      this.cancelError = '';
    },
    closeCancel(value) {
      if (!value && !this.cancelling) { this.cancelTarget = null; this.cancelError = ''; }
    },
    async cancelOperation() {
      if (!this.selected || this.cancelling) return;
      const selected = this.cancelTarget || this.selected;
      if (this.cancelTarget && (this.selected.id !== selected.id
        || this.selected.revision !== selected.revision || this.selected.state !== 'queued')) {
        this.cancelError = '任务状态已变化，请关闭对话框、刷新原任务后重新确认；未发送取消请求。';
        return;
      }
      const base = this.base;
      this.cancelSequence += 1;
      const sequence = this.cancelSequence;
      this.cancelling = true;
      this.detailError = '';
      try {
        const { data } = await axios.post(
          `${base}/${selected.id}/cancel`,
          {},
          { headers: { 'If-Match': `"${selected.revision}"` }, timeout: 20000 },
        );
        if (sequence !== this.cancelSequence
          || this.selected?.id !== selected.id || base !== this.base) return;
        this.selected = data;
        this.cancelTarget = null;
        this.cancelError = '';
        await this.load();
      } catch (e) {
        if (sequence === this.cancelSequence
          && this.selected?.id === selected.id && base === this.base) {
          this.detailError = this.errorText(e, '取消结果未确认；请刷新原任务，不代表任务已停止。');
          this.cancelError = this.detailError;
        }
      } finally {
        if (sequence === this.cancelSequence && base === this.base) this.cancelling = false;
      }
    },
    errorText(e, fallback) {
      return e.response?.status === 401
        ? '登录已失效，请重新登录。'
        : e.response?.data?.error || fallback;
    },
    async refreshDetail() {
      const id = this.selected?.id || this.$route.query.operation;
      if (!id) return;
      const base = this.base;
      this.detailSequence += 1;
      const sequence = this.detailSequence;
      this.detailLoading = true;
      try {
        const { data } = await axios.get(`${base}/${id}`, { timeout: 15000 });
        if (sequence !== this.detailSequence || base !== this.base) return;
        if (data.id !== id) throw new Error('task identity mismatch');
        this.selected = data;
        this.detailError = '';
        this.detailUpdatedAt = new Date().toISOString();
      } catch (e) {
        if (sequence === this.detailSequence && base === this.base) {
          this.detailError = this.errorText(e, '任务详情暂时无法确认，保留上次结果；不会重新执行。');
          if (e.response?.status === 401 || e.response?.status === 403) this.pollAllowed = false;
        }
      } finally {
        if (sequence === this.detailSequence && base === this.base) this.detailLoading = false;
      }
    },
    async load(background = false, requestedPage = this.pendingPage) {
      const page = Number.isInteger(requestedPage) && requestedPage > 0
        ? requestedPage : this.pendingPage;
      this.pendingPage = page;
      clearTimeout(this.timer);
      this.sequence += 1;
      const seq = this.sequence;
      this.pollAllowed = true;
      if (background !== true) this.loading = true;
      // Details must continue to reconcile even when filtering/list reads fail.
      const detail = this.refreshDetail();
      try {
        const { data } = await axios.get(this.base, {
          params: {
            page,
            page_size: this.pageSize,
            q: this.search || '',
            state: this.state,
            kind: this.kind,
          },
          timeout: 15000,
        });
        if (seq !== this.sequence) return;
        this.items = data.items;
        this.total = data.total;
        this.totalPages = data.total_pages;
        if (page > Math.max(1, this.totalPages)) {
          this.load(false, Math.max(1, this.totalPages));
          return;
        }
        this.page = page;
        this.error = '';
        this.lastUpdatedAt = new Date().toISOString();
      } catch (e) {
        if (seq === this.sequence) {
          this.error = e.response?.status === 401
            ? '登录已失效，请重新登录。'
            : e.response?.data?.error || '任务读取失败，不代表执行已停止。';
          if (e.response?.status === 401 || e.response?.status === 403) this.pollAllowed = false;
        }
      } finally {
        await detail;
        if (seq === this.sequence) {
          this.loading = false;
          if (this.pollAllowed) this.timer = setTimeout(() => this.load(true), 4000);
        }
      }
    },
  },
};
</script>
<style scoped>
.rcd-operations {
  padding: 30px 32px;
  background: #fff;
  min-height: 100%;
  color: #202a3d;
}
header,
footer,
footer > div,
.detail-heading {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
}
header {
  align-items: flex-start;
  margin-bottom: 26px;
}
h1 {
  font-size: 28px;
  line-height: 1.3;
  margin: 0 0 12px;
  font-weight: 650;
}
header p,
.muted {
  color: #6b788c;
}
.valid {
  color: #24654c;
}
.rcd-warning {
  color: #995b16;
}
.rcd-failure {
  color: #b42318;
}
form {
  display: flex;
  gap: 12px;
  align-items: center;
  margin-bottom: 24px;
}
form > .v-input {
  max-width: 320px;
}
.table-wrap {
  border: 1px solid #dce2ec;
  border-radius: 4px;
}
table {
  border-collapse: collapse;
  width: 100%;
  table-layout: fixed;
  font-size: 13px;
}
th {
  text-align: left;
  background: #f4f6f9;
}
th,
td {
  padding: 16px 12px;
  border-bottom: 1px solid #e8ebf0;
  overflow-wrap: anywhere;
}
td span {
  display: block;
}
td small {
  display: block;
  margin-top: 4px;
  color: #6b788c;
}
.rcd-operations .v-btn--outlined {
  color: #344e79;
  border-color: #cbd5e3;
}
th:last-child {
  width: 110px;
}
.empty {
  text-align: center;
  padding: 48px 18px;
  color: #6b788c;
}
footer {
  margin-top: 20px;
  font-size: 13px;
}
footer .v-select {
  max-width: 125px;
}
.details {
  padding: 28px;
  overflow-wrap: anywhere;
}
.details h2 {
  font-size: 23px;
}
.details .v-btn {
  margin-bottom: 12px;
}
.target-detail {
  padding: 20px 0;
  border-top: 1px solid #dce2ec;
}
.target-detail li {
  margin: 12px 0;
}
.target-detail small {
  display: block;
  color: #6b788c;
}
.refresh-status {
  min-height: 22px;
}
.cancel-dialog { padding: 8px; color: #202a3d; overflow-wrap: anywhere; }
.v-btn {
  text-transform: none;
  letter-spacing: 0;
}
.mobile-menu {
  display: none;
}
@media (max-width: 960px) {
  .mobile-menu {
    display: inline-flex;
    margin-bottom: 14px;
  }
}
@media (max-width: 650px) {
  .rcd-operations {
    padding: 20px 16px;
  }
  header,
  form,
  footer,
  footer > div {
    flex-wrap: wrap;
  }
  thead {
    display: none;
  }
  tr,
  td {
    display: block;
  }
  td {
    border: 0;
    padding: 5px 12px;
  }
  tr {
    padding: 12px 0;
    border-bottom: 1px solid #dce2ec;
  }
  td[data-label]::before {
    content: attr(data-label) '：';
    color: #6b788c;
  }
  td span {
    display: inline-block;
    margin-left: 6px;
  }
  .details {
    padding: 20px;
  }
}
</style>
