<template>
  <v-dialog
    :value="value"
    max-width="880"
    :persistent="busy || !!pendingRequest"
    @input="$emit('input', $event)"
  >
    <v-card class="rcd-release-dialog">
      <v-card-title>{{ action === 'restore' ? '恢复历史版本' : '升级软件' }}</v-card-title>
      <v-card-text>
        <nav class="release-steps" aria-label="发布步骤">
          <span
            v-for="(title, index) in ['选择软件', '选择机器', '确认执行']"
            :key="title"
            :class="{ current: step === index }"
            >{{ index + 1 }}. {{ title }}</span
          >
        </nav>
        <v-alert v-if="error" type="error" text role="alert">{{ error }}</v-alert>
        <p v-if="loginRequired">
          <a href="/auth/login" target="_blank" rel="noopener noreferrer">在新标签页重新登录</a>
          ，完成后返回此处，保留原请求继续确认。
        </p>
        <p v-if="knownOperationId" role="status">
          服务端已登记原操作。
          <a :href="`/project/${projectId}/rcd/operations?operation=${knownOperationId}`"
            target="_blank" rel="noopener noreferrer">在新标签页查看原操作任务</a>，
          不会重新执行；请保留当前请求直到确认结果。
        </p>
        <v-alert v-if="pendingRequest && !busy" type="warning" text role="status">
          请求结果尚未确认，软件包与目标已锁定。请点击“确认原请求结果”，使用原编号查询，不创建另一条发布。
        </v-alert>
        <p v-if="loading" role="status">正在读取软件包和设备…</p>
        <p v-if="applicationError" class="rcd-warning" role="status">{{ applicationError }}</p>
        <p v-if="!loading && devicesError" class="rcd-warning" role="alert">{{ devicesError }}</p>
        <section v-if="step === 0">
          <v-select
            v-model="component"
            :items="componentChoices"
            label="组件"
            outlined
            dense
            :disabled="loading || packagesLoading || action === 'restore'"
            @change="loadPackages"
          />
          <v-text-field
            v-model="search"
            label="搜索版本或发布编号"
            outlined
            dense
            :disabled="loading || action === 'restore'"
            @keyup.enter="loadPackages"
            append-icon="mdi-magnify"
            @click:append="loadPackages"
          />
          <v-select
            v-model="packageId"
            :items="packages"
            item-value="id"
            :item-text="packageLabel"
            label="软件版本"
            outlined
            :disabled="loading || packagesLoading || action === 'restore'"
            :menu-props="{ maxHeight: 320 }"
          />
          <p v-if="packagesLoading" role="status">正在读取可用版本…</p>
          <p v-else-if="!packages.length" class="muted">
            没有符合条件的可用版本，请调整搜索或先上传软件包。
          </p>
          <p v-if="packageTotal > packages.length" class="muted">
            已显示 {{ packages.length }} / {{ packageTotal }} 个版本。
            <v-btn small text :loading="packagesLoading" @click="loadPackages(true)"
              >加载更多版本</v-btn
            >
          </p>
          <p v-if="chosen">{{ chosen.manifest.release_notes || '此版本没有填写发布说明。' }}</p>
          <p class="muted">
            每次只更新一个组件，保留当前配置、数据库和 PKI。找不到版本？请先到“软件包”上传
            .rcd.tar.gz 文件。
          </p>
        </section>
        <section v-if="step === 1">
          <v-btn outlined small :loading="devicesLoading" :disabled="busy || !!pendingRequest"
            title="重新读取已保存的设备状态和兼容检查，不重启或安装程序"
            @click="refreshTargets">刷新设备与兼容性</v-btn>
          <p v-if="inspectionMessage" role="status">{{ inspectionMessage }}</p>
          <p v-if="compatibilityLoading" role="status">正在检查软件与设备的兼容性…</p>
          <v-btn v-if="compatibilityFailed" small outlined @click="loadCompatibility"
            >重新检查兼容性</v-btn
          >
          <p>已选择 {{ selectedDevices.length }} 台。多台按 30 → 58 → 46 串行执行，失败或结果不明时停止后续目标。</p>
          <div v-for="d in devices" :key="d.id" class="release-device">
            <v-checkbox
              v-model="selectedIds"
              :value="d.id"
              :label="`${d.name} · ${d.address}`"
              :disabled="
                !!disabledReason(d) || (action === 'restore' && !initialDeviceIds.includes(d.id))
              "
              hide-details
            />
            <p>
              {{ componentName(component) }}：{{ currentVersion(d) }} →
              {{ chosen ? chosen.manifest.version : '未选择' }}
            </p>
            <small :class="disabledReason(d) ? 'rcd-warning' : 'muted'">{{
              disabledReason(d) || '安装前将再次检查实际状态和数据兼容性'
            }}</small>
            <v-btn v-if="!d.active_operation_id" small text :loading="checking === d.id"
              :disabled="!!checking || !!d.pending_task_id || devicesLoading"
              title="提交原生只读检查任务，不重启程序，也不下发设备命令"
              @click="inspect(d)">{{ d.pending_task_id ? '正在检查状态' : '检查此设备状态' }}</v-btn>
          </div>
          <p v-if="!devices.length">没有已绑定设备，请由维护人员完成接入。</p>
        </section>
        <section v-if="step === 2">
          <h3>{{ componentName(component) }} · {{ chosen ? chosen.manifest.version : '' }}</h3>
          <v-alert v-if="!pendingRequest && selectedDevices.length !== selectedIds.length"
            type="warning" text role="status">
            部分目标状态已过期或不再满足发布条件。请返回“选择机器”刷新状态并重新确认，不能继续使用旧确认。
          </v-alert>
          <p>{{ impact }}</p>
          <p>执行顺序：</p>
          <ol>
            <li v-for="d in selectedDevices" :key="d.id">
              {{ d.name }}（{{ d.address }}）：{{ currentVersion(d) }}
            </li>
          </ol>
          <p>
            原本停止的程序保持停止。失败后只在能确认安全时恢复原程序；状态不明将保留现场，不重复重启。
          </p>
          <v-checkbox
            v-model="paused"
            label="已确认本次暂停所有相关管理端及直连客户端的控制命令"
            :disabled="busy || !!pendingRequest"
          />
          <p v-if="action === 'restore'" class="rcd-warning">
            这是一次新的恢复任务，保留当前配置与数据，不覆盖旧升级记录。
            数据库版本一致不代表配置语法兼容；包含新语法的活动配置，须先确认旧程序能够读取。
          </p>
        </section>
      </v-card-text>
      <v-card-actions>
        <v-btn text :disabled="busy || !!pendingRequest" @click="$emit('input', false)">取消</v-btn
        ><v-spacer />
        <v-btn v-if="step > 0" outlined :disabled="busy || !!pendingRequest" @click="step -= 1"
          >上一步</v-btn
        >
        <v-btn
          v-if="step < 2"
          color="primary"
          depressed
          :disabled="
            loading || packagesLoading || devicesLoading || !chosen
              || (step === 1 && !selectedDevices.length)
          "
          @click="step += 1"
          >下一步</v-btn
        >
        <v-btn
          v-else
          color="primary"
          depressed
          :loading="busy"
          :disabled="busy || (!pendingRequest && (!paused || !chosen || !selectedDevices.length
            || selectedDevices.length !== selectedIds.length))"
          @click="submit"
          >{{
            pendingRequest ? '确认原请求结果' : action === 'restore' ? '确认恢复' : '确认升级'
          }}</v-btn
        >
      </v-card-actions>
    </v-card>
  </v-dialog>
</template>
<script>
import axios from 'axios';

export default {
  props: {
    value: Boolean,
    projectId: Number,
    initialDeviceIds: { type: Array, default: () => [] },
    initialPackage: { type: Object, default: null },
    initialComponent: { type: String, default: 'lower' },
    action: { type: String, default: 'upgrade' },
  },
  data() {
    return {
      step: 0,
      component: 'lower',
      packageId: '',
      packages: [],
      devices: [],
      applications: [],
      applicationError: '',
      devicesError: '',
      devicesLoading: false,
      deviceSequence: 0,
      checking: '',
      inspectionMessage: '',
      inspectionTimer: null,
      compatibility: {},
      selectedIds: [],
      search: '',
      paused: false,
      loading: false,
      packagesLoading: false,
      packagePage: 1,
      packageTotal: 0,
      compatibilityLoading: false,
      compatibilityFailed: false,
      generation: 0,
      packageSequence: 0,
      compatibilitySequence: 0,
      busy: false,
      error: '',
      loginRequired: false,
      requestKey: '',
      pendingRequest: null,
      knownOperationId: '',
      now: Date.now(),
      clockTimer: null,
      components: [
        { text: '下级 OPC', value: 'lower' },
        { text: 'OPC 管理端', value: 'manager' },
        { text: 'RCD Agent', value: 'agent' },
        { text: '串口通信程序', value: 'communicate' },
      ],
    };
  },
  computed: {
    componentChoices() {
      const known = new Set(this.components.map((item) => item.value));
      return [...this.components, ...this.applications.filter((app) => !known.has(app.component))
        .map((app) => ({ text: app.name, value: app.component }))];
    },
    base() {
      return `/api/project/${this.projectId}/rcd`;
    },
    chosen() {
      return this.packages.find((p) => p.id === this.packageId);
    },
    selectedDevices() {
      const rank = (address) => {
        const i = ['192.168.10.30', '192.168.10.58', '192.168.10.46'].indexOf(address);
        return i < 0 ? 3 : i;
      };
      return this.devices.filter((d) => this.selectedIds.includes(d.id) && !this.disabledReason(d))
        .sort((a, b) => rank(a.address) - rank(b.address) || a.id.localeCompare(b.id));
    },
    impact() {
      return {
        lower: '停止并重新启动所选机器的下级采集程序，期间该来源的数据会短暂中断。',
        manager: '只重新启动所选 OPC 管理端；其 OPC 客户端需要重连，不停止远端下级。',
        agent: '先排空并停止其下级，再更新 Agent，按升级前状态恢复下级。',
        communicate: '仅更新所选机器的独立串口通信程序，不停止原采集程序。'
          + '当前必须先确认串口、同步任务和发送全部停用；安装器会再次核对并拒绝活动通信。',
      }[this.component] || '仅更新所选主机中已登记的单个软件服务，保留当前配置与数据。'
        + '经systemd正常停止，但平台不能核实该软件的内部设备事务排空；执行前须自行确认。'
        + '原本停止的保持停止，不运行自定义安装脚本。';
    },
  },
  watch: {
    packageId() {
      this.paused = false;
      this.loadCompatibility();
    },
    selectedIds() {
      this.paused = false;
    },
    value(open) {
      if (open) this.open();
      else {
        this.generation += 1;
        clearTimeout(this.inspectionTimer);
        clearTimeout(this.clockTimer);
      }
    },
    projectId() {
      this.generation += 1;
      clearTimeout(this.inspectionTimer);
      clearTimeout(this.clockTimer);
      this.$emit('input', false);
    },
  },
  beforeDestroy() {
    this.generation += 1;
    clearTimeout(this.inspectionTimer);
    clearTimeout(this.clockTimer);
  },
  methods: {
    updateClock() {
      clearTimeout(this.clockTimer);
      this.now = Date.now();
      if (this.value) this.clockTimer = setTimeout(this.updateClock, 1000);
    },
    async loadApplications() {
      const generation = this.generation;
      try {
        const { data } = await axios.get(`${this.base}/applications`, { timeout: 15000 });
        if (generation === this.generation) {
          this.applications = data.items;
          this.applicationError = '';
        }
      } catch (e) {
        if (generation === this.generation) this.applicationError = '软件种类未刷新，按组件ID显示。';
      }
    },
    async refreshTargets() {
      if (this.pendingRequest || this.busy) return;
      const generation = this.generation;
      this.deviceSequence += 1;
      const sequence = this.deviceSequence;
      clearTimeout(this.inspectionTimer);
      this.devicesLoading = true;
      this.devicesError = '';
      this.compatibility = {};
      this.paused = false;
      try {
        const { data } = await axios.get(`${this.base}/devices`, {
          params: { page_size: 100 }, timeout: 15000,
        });
        if (generation !== this.generation || sequence !== this.deviceSequence) return;
        this.devices = data.items;
        await this.loadCompatibility();
        if (generation !== this.generation || sequence !== this.deviceSequence) return;
        if (this.devices.some((d) => d.pending_task_id)) {
          this.inspectionTimer = setTimeout(this.refreshTargets, 3000);
        }
      } catch (e) {
        if (generation === this.generation && sequence === this.deviceSequence) {
          this.devicesError = `${this.errorText(e)} 当前不能确认设备状态，请刷新后再选择。`;
        }
      } finally {
        if (generation === this.generation && sequence === this.deviceSequence) {
          this.devicesLoading = false;
        }
      }
    },
    async inspect(d) {
      if (this.checking || this.pendingRequest || this.busy
        || d.pending_task_id || d.active_operation_id) return;
      const generation = this.generation;
      this.checking = d.id;
      this.inspectionMessage = '';
      try {
        const { data } = await axios.post(
          `${this.base}/devices/${d.id}/inspections`,
          { revision: d.revision },
          { headers: { 'If-Match': `"${d.revision}"` }, timeout: 15000 },
        );
        if (generation !== this.generation) return;
        this.inspectionMessage = `只读检查任务 #${data.task_id} 已接收；不会重启程序，检查完成后自动更新。`;
        await this.refreshTargets();
      } catch (e) {
        if (generation === this.generation) {
          this.inspectionMessage = `${this.errorText(e)} 请刷新查看原检查结果，不要反复点击。`;
          await this.refreshTargets();
        }
      } finally {
        if (generation === this.generation) this.checking = '';
      }
    },
    async loadCompatibility() {
      const generation = this.generation;
      this.compatibilitySequence += 1;
      const sequence = this.compatibilitySequence;
      this.compatibility = {};
      this.compatibilityFailed = false;
      this.compatibilityLoading = false;
      const id = this.packageId;
      if (!id) return;
      this.compatibilityLoading = true;
      try {
        const { data } = await axios.get(`${this.base}/packages/${id}/compatibility`, { timeout: 15000 });
        if (generation !== this.generation || sequence !== this.compatibilitySequence) return;
        if (data.package_revision !== this.chosen?.revision) {
          this.error = '软件包状态已变化，请返回重新选择版本。';
          this.compatibilityFailed = true;
          return;
        }
        this.compatibility = data.reasons;
        this.error = '';
      } catch (e) {
        if (generation !== this.generation || sequence !== this.compatibilitySequence) return;
        this.compatibilityFailed = true;
        this.error = this.errorText(e);
      } finally {
        if (generation === this.generation && sequence === this.compatibilitySequence) {
          this.compatibilityLoading = false;
        }
      }
    },
    async open() {
      this.generation += 1;
      this.updateClock();
      const generation = this.generation;
      this.step = 0;
      this.busy = false;
      this.error = '';
      this.paused = false;
      this.loginRequired = false;
      this.search = '';
      this.requestKey = '';
      this.pendingRequest = null;
      this.knownOperationId = '';
      this.devices = [];
      this.packages = [];
      this.packageId = '';
      this.compatibility = {};
      this.selectedIds = [...this.initialDeviceIds];
      this.component = this.initialPackage?.manifest.component || this.initialComponent;
      this.loading = true;
      this.checking = '';
      this.inspectionMessage = '';
      this.devicesError = '';
      clearTimeout(this.inspectionTimer);
      try {
        await Promise.all([
          this.refreshTargets(),
          this.loadPackages(false),
          this.loadApplications(),
        ]);
        if (generation !== this.generation) return;
        if (this.initialPackage) {
          if (!this.packages.some((p) => p.id === this.initialPackage.id)) {
            this.packages.push(this.initialPackage);
          }
          this.packageId = this.initialPackage.id;
        }
      } catch (e) {
        if (generation === this.generation) this.error = this.errorText(e);
      } finally {
        if (generation === this.generation) this.loading = false;
      }
    },
    async loadPackages(append = false) {
      // Vuetify change events pass the selected value, not a pagination flag.
      const more = append === true;
      const generation = this.generation;
      this.packageSequence += 1;
      const sequence = this.packageSequence;
      const page = more ? this.packagePage + 1 : 1;
      this.packagesLoading = true;
      this.error = '';
      if (!more) {
        this.packages = [];
        this.packageId = '';
        this.packageTotal = 0;
      }
      try {
        const { data } = await axios.get(`${this.base}/packages`, {
          params: {
            component: this.component,
            enabled: true,
            q: this.search,
            page_size: 100,
            page,
          },
          timeout: 15000,
        });
        if (generation !== this.generation || sequence !== this.packageSequence) return;
        this.packages = more ? [...this.packages, ...data.items] : data.items;
        this.packagePage = page;
        this.packageTotal = data.total;
      } catch (e) {
        if (generation === this.generation && sequence === this.packageSequence) {
          this.error = this.errorText(e);
        }
      } finally {
        if (generation === this.generation && sequence === this.packageSequence) {
          this.packagesLoading = false;
        }
      }
    },
    packageLabel(p) {
      return `${p.manifest.version} · ${p.manifest.release_id}`;
    },
    componentName(c) {
      return this.componentChoices.find((item) => item.value === c)?.text || c;
    },
    currentVersion(d) {
      const c = d.components.find((v) => v.component === this.component) || {};
      if (c.state === 'not_installed') return '未安装';
      return c.state === 'stopped'
        ? `已停止（磁盘 ${c.disk_build?.version || '未知'}）`
        : c.running_build?.version || '无法确认';
    },
    disabledReason(d) {
      if (this.devicesLoading || this.devicesError) return '设备状态正在刷新或读取失败，请先确认状态';
      if (d.pending_task_id) return '只读检查进行中，请等待检查结果';
      if (d.state !== 'checked') return '需要先完成一次成功的近期状态检查';
      const age = (this.now - Date.parse(d.last_success_at)) / 1000;
      if (!Number.isFinite(age) || age < -30 || age > 180) return '状态检查已过期或时间不明，请先检查此设备状态';
      if (d.active_operation_id) return '该机器存在未结束的发布操作，请先查看原任务';
      if (!d.release_template_id) return '尚未绑定发布模板';
      const c = d.components.find((v) => v.component === this.component);
      if (c?.state === 'not_installed') return '此机器未安装该组件，需维护人员完成首次接入，不能直接升级';
      if (!c || !['running', 'stopped'].includes(c.state)) return '程序状态或实际版本无法确认';
      return this.compatibility[d.id] ?? '正在检查兼容性；没有检查结果时不能发布';
    },
    errorText(e) {
      return e.response?.status === 401
        ? '登录已失效，请重新登录。'
        : e.response?.data?.error || '读取失败，请检查连接后重试。';
    },
    async submit() {
      if (this.busy) return;
      if (!this.pendingRequest && (!this.paused || !this.chosen || !this.selectedDevices.length
        || this.selectedDevices.length !== this.selectedIds.length)) {
        return;
      }
      const retry = !!this.pendingRequest;
      let refreshRejected = false;
      this.busy = true;
      this.error = '';
      if (!this.requestKey) {
        this.requestKey = Array.from(
          crypto.getRandomValues(new Uint8Array(20)),
          (v) => v.toString(16).padStart(2, '0'),
        ).join('');
      }
      const body = this.pendingRequest || {
        package_id: this.chosen.id,
        package_revision: this.chosen.revision,
        action: this.action,
        commands_paused: this.paused,
        targets: this.selectedDevices.map((d) => ({ device_id: d.id, revision: d.revision })),
      };
      this.pendingRequest = body;
      const project = this.projectId;
      const generation = this.generation;
      const headers = {
        'If-Match': `"${body.package_revision}"`,
        'Idempotency-Key': this.requestKey,
      };
      try {
        clearTimeout(this.inspectionTimer);
        const { data } = await axios.post(`${this.base}/operations`, body, { headers, timeout: 30000 });
        if (generation !== this.generation || project !== this.projectId) return;
        this.pendingRequest = null;
        this.$emit('created', data);
        this.$emit('input', false);
        this.$router.push({
          path: `/project/${this.projectId}/rcd/operations`,
          query: { operation: data.id },
        });
      } catch (e) {
        if (generation !== this.generation || project !== this.projectId) return;
        const operationId = e.response?.data?.operation_id;
        if (project === this.projectId && /^[a-f0-9]{64}$/.test(operationId || '')) {
          this.knownOperationId = operationId;
        }
        // Only a definitive rejection of the first submission permits editing.
        // A later authentication/revision error cannot disprove an earlier accepted request.
        if (!retry && !this.knownOperationId
          && [400, 401, 403, 409, 422, 428].includes(e.response?.status)) {
          this.pendingRequest = null;
          this.requestKey = '';
          if (e.response?.status === 409) {
            this.step = 1;
            this.paused = false;
            refreshRejected = true;
          }
        }
        if (project === this.projectId) {
          this.loginRequired = e.response?.status === 401;
          this.error = this.loginRequired ? '登录已失效，请重新登录后确认原请求。'
            : e.response?.data?.error || '未收到确认；不能据此判断任务未创建，请确认原请求结果。';
        }
      } finally {
        if (generation === this.generation && project === this.projectId) this.busy = false;
        if (refreshRejected && generation === this.generation && project === this.projectId) {
          const rejection = this.error;
          await this.refreshTargets();
          if (generation === this.generation && project === this.projectId) {
            this.error = `${rejection} 已重新读取设备状态，请重新确认目标和本次控制暂停。`;
          }
        }
      }
    },
  },
};
</script>
<style scoped>
.rcd-release-dialog {
  color: #202a3d;
}
.rcd-release-dialog .v-card__title {
  font-size: 24px;
  padding: 26px 28px 12px;
}
.rcd-release-dialog .v-card__text {
  padding: 12px 28px;
  font-size: 14px;
  line-height: 1.65;
}
.rcd-release-dialog .v-card__actions {
  padding: 18px 28px;
  border-top: 1px solid #dce2ec;
}
.release-steps {
  display: flex;
  gap: 24px;
  border-bottom: 1px solid #dce2ec;
  margin-bottom: 24px;
}
.release-steps span {
  padding: 8px 0 12px;
  color: #6b788c;
}
.release-steps .current {
  color: #234eaa;
  border-bottom: 2px solid #234eaa;
}
.muted {
  color: #6b788c;
}
.rcd-warning {
  color: #995b16;
}
.release-device {
  padding: 4px 0 18px;
  border-bottom: 1px solid #e8ebf0;
}
.release-device p {
  margin: 4px 0 0 34px;
}
.release-device small {
  margin-left: 34px;
  display: block;
}
.rcd-release-dialog li {
  margin: 6px 0;
}
.rcd-release-dialog .v-btn {
  text-transform: none;
  letter-spacing: 0;
}
@media (max-width: 600px) {
  .release-steps {
    gap: 12px;
    font-size: 12px;
  }
  .rcd-release-dialog .v-card__text {
    padding: 12px 18px;
  }
  .rcd-release-dialog .v-card__actions {
    padding: 16px;
  }
}
</style>
