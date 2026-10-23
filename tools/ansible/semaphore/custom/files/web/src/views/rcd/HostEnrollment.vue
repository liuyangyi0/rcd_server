<template>
  <v-dialog :value="value" max-width="840" :persistent="busy" @input="$emit('input', $event)">
    <v-card class="host-enrollment">
      <v-card-title>{{ isExistingTarget ? '首次安装软件' : '添加主机' }}</v-card-title>
      <v-card-text>
        <ol class="enrollment-progress" data-testid="host-enrollment-progress" aria-label="接入进度">
          <li v-for="(step, index) in enrollmentSteps" :key="step.key"
            :class="{ current: step.key === currentStep, completed: index < currentStepIndex }"
            :aria-current="step.key === currentStep ? 'step' : null">
            <span>{{ index + 1 }}</span>{{ step.label }}
          </li>
        </ol>
        <p class="next-step" role="status" data-testid="host-enrollment-next-step">
          {{ nextStepHint }}
        </p>
        <p v-if="!isExistingTarget">通过现有加密SSH凭据接入Linux主机，再选择单组件版本包首次安装。只登记或检查不会安装软件。</p>
        <p v-else>为已登记主机补充一个尚未安装的软件组件，不重新添加主机，不替换其他软件或现有业务配置。</p>
        <p class="muted">目标机须已有非root服务账户、systemd及所需运行环境。初装后保持停止、等待配置，不自动开启串口。</p>
        <p class="muted">首次安装写入系统目录，须root登录或选择sudo凭据。非root且无sudo凭据仍可登记/只读检查，但不能安装。</p>
        <v-alert v-if="error" type="error" text role="alert">{{ error }}</v-alert>
        <p v-if="loading" role="status">正在读取接入选项或原作业…</p>
        <p v-if="storageError" class="warning-text" role="status">
          浏览器无法保存作业引用，请保留此页和下面的原作业编号；刷新后不要重新登记同一主机。
        </p>
        <template v-if="!setup && !resumeId && !initialDevice">
          <v-alert v-if="pendingCreate" type="warning" text role="status">
            登记结果待确认，目标已锁定。请确认原请求，不能换编号重复创建作业。
          </v-alert>
          <div class="input-grid">
            <v-text-field v-model="request.name" label="主机名称" outlined dense maxlength="128"
              :disabled="locked" />
            <v-text-field v-model="request.address" label="固定IP地址" outlined dense
              :disabled="locked" />
            <v-text-field v-model="request.ssh_port" label="SSH端口" type="number" min="1" max="65535"
              outlined dense :disabled="locked" />
            <v-text-field v-model="request.ssh_user" label="SSH登录账号" outlined dense
              :disabled="locked" />
            <v-select v-model="request.ssh_key_id" :items="sshChoices" label="已加密SSH凭据"
              outlined dense :disabled="locked" />
            <v-select v-model="request.become_key_id" :items="becomeChoices" label="sudo凭据"
              outlined dense :disabled="locked" />
          </div>
          <v-text-field v-model="request.service_account" label="目标机已有的非root服务账户" outlined dense
            :disabled="locked" hint="软件文件及服务归该账户使用；不会创建账户或自动配置业务。" persistent-hint />
          <p v-if="!options.available && !loading" class="warning-text" role="status">
            {{ options.error || '接入能力暂不可用，请维护人员核对原生Ansible环境。' }}
            <v-btn small text @click="loadOptions">重新读取接入选项</v-btn>
          </p>
          <p v-if="options.available && !sshChoices.length" class="warning-text" role="status">
            尚无可用SSH凭据。请先在高级管理的密钥存储中配置加密凭据，此页不接收明文密码。
          </p>
          <v-checkbox v-model="accessConfirmed" :disabled="locked"
            label="已获准访问该主机，确认IP及账号正确；后续仍需核对SSH主机指纹" />
          <p v-if="validation && !pendingCreate" class="warning-text" role="status">
            {{ validation }}
          </p>
          <p v-if="pendingCreate" class="muted">原请求编号：{{ pendingCreate.key }}</p>
          <v-btn color="primary" depressed :loading="busy"
            :disabled="!canSubmitCreate" @click="create">
            {{ pendingCreate ? '确认原登记结果' : '登记并获取主机指纹' }}
          </v-btn>
        </template>
        <div v-else-if="!setup && !resumeId && initialDevice" class="existing-target">
          <strong>{{ initialDevice.name }} · {{ initialDevice.address }}</strong>
          <p>首次安装依据最近一次可信检查，只接受确认未安装的组件。目标和设备revision已固定。</p>
          <p v-if="firstInstallReason" class="warning-text" role="status">
            {{ firstInstallReason }}
          </p>
        </div>
        <p v-else-if="!setup" class="warning-text" role="status">
          正在确认原作业 {{ resumeId }}；没有因此发起新接入。读取失败时请刷新原作业，勿重新登记。
        </p>
        <template v-else>
          <dl class="setup-summary">
            <dt>主机</dt><dd>
              {{ setup.request.name }} · {{ setup.request.address }}:{{ setup.request.ssh_port }}
            </dd>
            <dt>作业编号</dt><dd>{{ setup.id }}</dd>
            <dt>当前状态</dt><dd role="status">{{ stateLabel(setup.state) }}</dd>
            <dt>执行阶段</dt><dd>{{ phaseLabel(setup.phase) }}</dd>
            <dt>最近更新</dt><dd>{{ date(setup.updated_at) }}</dd>
          </dl>
          <v-alert v-if="setup.error" type="warning" text>{{ setup.error }}</v-alert>
          <v-alert v-if="setup.warnings && setup.warnings.length" type="info" text>
            <ul><li v-for="warning in setup.warnings" :key="warning">{{ warning }}</li></ul>
          </v-alert>
          <p v-if="setup.state === 'unknown'" class="warning-text">
            执行结果未知，只能刷新原作业并核对日志。不要重新登记或重复安装。
          </p>
          <section v-if="canReconcile">
            <p>可经原生只读任务核对原首装记录；不会重新安装、启停或更改原失败任务结论。</p>
            <v-checkbox v-model="reconcileConfirmed" :disabled="busy || !!pendingReconcile"
              label="确认仅核对原首装记录，不重新安装、不启停" />
            <v-btn outlined :disabled="!canSubmitReconcile"
              @click="reconcile">{{ pendingReconcile ? '刷新原补查作业' : '补查原首装结果' }}</v-btn>
          </section>
          <p v-if="setup.original_install_task_id" class="muted">
            原安装任务：#{{ setup.original_install_task_id }}；补查不会改写它的原执行结果。
          </p>
          <v-btn v-if="setup.original_install_task_id" small text
            @click="showTask(setup.original_install_task_id)">
            查看原安装任务 #{{ setup.original_install_task_id }} 日志
          </v-btn>
          <v-btn v-if="setup.phase === 'reconcile' && setup.task_id" small text
            @click="showTask(setup.task_id)">查看只读补查任务 #{{ setup.task_id }} 日志</v-btn>
          <p v-if="setup.state === 'host_key_conflict'" class="warning-text">
            SSH主机指纹与已保存的信任记录冲突。已停止接入，不会覆盖旧指纹，请维护人员核实目标身份。
          </p>
          <section v-if="setup.state === 'awaiting_host_key'">
            <h3>核对全部SSH主机指纹</h3>
            <p>请通过目标机控制台或其他受信任渠道逐项核对以下全部指纹。仅点击确认不能证明目标安全。</p>
            <ul class="host-keys">
              <li v-for="(key, index) in setup.host_keys" :key="`${key.algorithm}:${index}`">
                <strong>{{ key.algorithm }}</strong><code>{{ key.fingerprint }}</code>
              </li>
            </ul>
            <v-checkbox v-model="keysConfirmed" :disabled="busy || !!pendingKeys || !!pendingCancel"
              label="已通过受信任渠道核对以上全部指纹，均与目标机一致" />
            <p v-if="pendingKeys" class="warning-text">原指纹确认结果待核对，请先刷新原作业。</p>
            <v-btn outlined :disabled="!canConfirmKeys" :loading="busy" @click="confirmKeys">
              {{ pendingKeys ? '确认原指纹请求' : '确认指纹并检查主机' }}
            </v-btn>
          </section>
          <v-alert v-if="setup.state === 'installed_stopped'" type="success" text role="status">
            已安装，保持停止、等待配置。软件安装成功不表示已开始采集或串口设备正常；不会自动开启通信。
          </v-alert>
          <v-alert v-if="setup.state === 'cancelled'" type="info" text role="status">
            已取消此次未开始安装的接入，不会安装软件。原作业历史与已登记主机保留。
          </v-alert>
          <section v-if="canCancel">
            <v-checkbox v-model="cancelConfirmed" :disabled="busy || !!pendingCancel"
              label="确认取消此尚未安装的接入，保留历史，不删除已登记主机" />
            <v-btn outlined :disabled="!canSubmitCancel" @click="cancel">
              {{ pendingCancel ? '确认原取消结果' : '取消此次接入' }}
            </v-btn>
            <p v-if="pendingCancel" class="warning-text">取消结果待确认，不能继续安装。请刷新原作业核对。</p>
          </section>
          <section v-if="setup.steps && setup.steps.length" class="steps">
            <h3>执行记录</h3>
            <div v-for="(step, index) in setup.steps" :key="index">
              <span>{{ phaseLabel(step.phase) }} · {{ stateLabel(step.state) }}</span>
              <v-btn v-if="step.task_id" small text @click="showTask(step.task_id)">
                查看任务 #{{ step.task_id }} 日志
              </v-btn>
              <p v-if="step.error" class="warning-text">{{ step.error }}</p>
            </div>
          </section>
          <p class="muted">关闭页面不会取消已受理任务；再次打开会查询原作业，不会重复提交。</p>
        </template>
        <section v-if="canChooseInstall">
          <h3>{{ initialDevice ? '选择首次安装的软件' : '主机检查通过 · 选择首次安装的软件' }}</h3>
          <p>平台与软件兼容性仍会在安装前校验。此次只安装一个组件，不自动启动；缺少业务配置时标为待配置。</p>
          <p v-if="setup && setup.platform" class="muted">检查到的平台：{{ setup.platform }}</p>
          <p v-if="firstInstallReason && !initialDevice" class="warning-text" role="status">
            {{ firstInstallReason }}
          </p>
          <div class="input-grid">
            <v-select v-model="component" :items="installableComponents" label="软件种类" outlined dense
              :disabled="busy || !!pendingInstall" @change="packagePage = 1; loadPackages()" />
            <v-select v-model="packageId" :items="packageChoices" label="软件版本包" outlined dense
              :loading="packagesLoading" :disabled="busy || !!pendingInstall || packagesLoading" />
          </div>
          <p v-if="packagesError" class="warning-text" role="alert">{{ packagesError }}</p>
          <p v-if="!packagesLoading && !packages.length && !packagesError" class="muted">
            此软件还没有已启用的版本包。请先在软件包页面上传，再刷新此处；接入作业会保留。
          </p>
          <div class="package-controls">
            <span class="muted" data-testid="host-package-pages">
              已加载 {{ packages.length }} 个版本 · 第 {{ packagePages ? packagePage : 0 }}
              / {{ packagePages }} 页
            </span>
            <v-btn small outlined :disabled="busy || !!pendingInstall || packagesLoading"
              @click="loadPackages()">
              刷新软件包
            </v-btn>
            <v-btn v-if="packagePage < packagePages" small outlined
              :disabled="busy || !!pendingInstall || packagesLoading"
              @click="loadPackages(true)">加载更多版本</v-btn>
          </div>
          <v-checkbox v-model="installConfirmed" :disabled="busy || !!pendingInstall"
            label="确认首次安装此组件；安装后保持停止，配置和串口须由使用人确认后再启用" />
          <p v-if="pendingInstall" class="warning-text" role="status">
            原安装请求待确认，软件与版本已锁定；不会另建安装任务。
            原软件包：{{ pendingInstall.packageLabel || pendingInstall.body.package_id }}。
          </p>
          <v-btn color="primary" depressed :loading="busy"
            :disabled="!canSubmitInstall"
            @click="install">{{ pendingInstall ? '确认原安装结果' : '首次安装并保持停止' }}</v-btn>
        </section>
      </v-card-text>
      <v-card-actions class="enrollment-actions">
        <v-btn text :disabled="busy" @click="$emit('input', false)">关闭</v-btn>
        <v-spacer />
        <v-btn v-if="setup || resumeId" outlined :disabled="busy || loading" @click="refresh">
          刷新原作业
        </v-btn>
        <v-btn v-if="settledSetup && !isExistingTarget && !initialSetupId" outlined :disabled="busy"
          @click="newHost">接入另一台主机</v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>
</template>

<script>
import axios from 'axios';
import EventBus from '@/event-bus';

export default {
  props: {
    value: Boolean,
    projectId: Number,
    allowed: Boolean,
    components: Array,
    initialDevice: Object,
    initialComponent: String,
    initialSetupId: String,
  },
  data() {
    return {
      request: {
        name: '',
        address: '',
        ssh_port: 22,
        ssh_user: '',
        ssh_key_id: null,
        become_key_id: 0,
        service_account: '',
      },
      options: {
        available: false, ssh_keys: [], become_keys: [], error: '',
      },
      accessConfirmed: false,
      keysConfirmed: false,
      installConfirmed: false,
      cancelConfirmed: false,
      setup: null,
      resumeId: '',
      pendingCreate: null,
      pendingKeys: null,
      pendingInstall: null,
      pendingCancel: null,
      pendingReconcile: null,
      reconcileConfirmed: false,
      busy: false,
      loading: false,
      error: '',
      storageError: false,
      generation: 0,
      readSequence: 0,
      timer: null,
      now: Date.now(),
      clockTimer: null,
      component: 'lower',
      packageId: '',
      packages: [],
      packagePage: 1,
      packagePages: 0,
      packagesLoading: false,
      packagesError: '',
      packageSequence: 0,
    };
  },
  computed: {
    endpoint() { return `/api/project/${this.projectId}/rcd/host-setups`; },
    isExistingTarget() { return !!(this.initialDevice || this.setup?.existing_device); },
    enrollmentSteps() {
      if (this.initialDevice || this.setup?.existing_device) {
        return [
          { key: 'select', label: '选择软件' }, { key: 'result', label: '安装结果' },
        ];
      }
      return [
        { key: 'register', label: '登记主机' }, { key: 'keys', label: '核对指纹' },
        { key: 'probe', label: '检查主机' }, { key: 'select', label: '选择软件' },
        { key: 'result', label: '安装结果' },
      ];
    },
    currentStep() {
      if (!this.setup) return this.initialDevice ? 'select' : 'register';
      if (['keyscan_queued', 'awaiting_host_key'].includes(this.setup.state)) return 'keys';
      if (this.setup.state === 'ssh_probe_queued') return 'probe';
      if (this.setup.state === 'ready_for_install') return 'select';
      if (this.setup.state === 'running' && this.setup.phase === 'probe') return 'probe';
      if (this.setup.state === 'running' && this.setup.phase === 'keyscan') return 'keys';
      if (['failed', 'host_key_conflict', 'unknown'].includes(this.setup.state)
        && !this.setup.install) return this.setup.phase === 'probe' ? 'probe' : 'keys';
      return 'result';
    },
    currentStepIndex() {
      return this.enrollmentSteps.findIndex((step) => step.key === this.currentStep);
    },
    nextStepHint() {
      if (!this.setup) {
        if (this.resumeId) return '正在读取已受理的原作业；此处不会重新登记或安装。';
        if (this.pendingCreate) return '请先确认原登记请求结果，不要重复添加同一台主机。';
        return this.initialDevice ? '确认目标和软件版本，首次安装后仍保持停止。'
          : '先填写主机与加密凭据，再核对SSH指纹；登记不会安装软件。';
      }
      return {
        keyscan_queued: '正在获取主机指纹；完成后需人工核对，不会自动信任目标。',
        awaiting_host_key: '下一步：从目标机或可信管理员核对全部指纹，再确认检查主机。',
        ssh_probe_queued: '正在只读检查系统和账户；通过后才能选择首次安装的软件。',
        ready_for_install: '主机已登记，尚未安装。可选择版本包，也可关闭后从记录中继续。',
        installation_queued: '正在执行原首次安装任务，请等待结果；关闭窗口不会取消安装。',
        reconcile_queued: '正在只读核对原安装证据；不会重新安装或启停程序。',
        running: '原任务正在执行，请等待下方执行记录更新。',
        installed_stopped: '安装已确认，程序保持停止。完成业务配置后，再明确执行受控启动。',
        cancelled: '此接入已取消，历史记录保留；没有因此删除已登记主机。',
        failed: '请先查看失败原因和原任务日志；不要直接换编号重试安装。',
        host_key_conflict: '目标指纹存在冲突，需要维护人员核实；不会覆盖原信任。',
        unknown: '结果尚未确认。先刷新原作业、查看日志；允许时可只读补查原首装结果。',
      }[this.setup.state] || '请核对当前状态及原任务日志，不要重复提交操作。';
    },
    locked() { return this.busy || this.loading || !!this.pendingCreate || !!this.resumeId; },
    canChooseInstall() {
      if (this.pendingCancel) return false;
      if (this.setup) return this.setup.state === 'ready_for_install';
      return !!this.initialDevice && !this.resumeId;
    },
    canCancel() {
      return !this.initialDevice && !!this.setup && !this.setup.install
        && !this.pendingInstall && !this.pendingKeys
        && ['awaiting_host_key', 'ready_for_install'].includes(this.setup.state);
    },
    canReconcile() {
      return this.setup?.state === 'unknown' && !!this.setup.install;
    },
    canSubmitReconcile() {
      return this.allowed && !this.busy && (!!this.pendingReconcile || this.reconcileConfirmed);
    },
    settledSetup() {
      return this.setup && ['installed_stopped', 'cancelled'].includes(this.setup.state);
    },
    canSubmitCreate() {
      return this.allowed && !this.busy && !this.loading
        && (!!this.pendingCreate || !this.validation);
    },
    canConfirmKeys() {
      return this.allowed && !this.busy && !this.pendingCancel && this.setup?.host_keys.length
        && (this.pendingKeys || this.keysConfirmed);
    },
    canSubmitCancel() {
      return this.allowed && !this.busy && (this.pendingCancel || this.cancelConfirmed);
    },
    canSubmitInstall() {
      return this.allowed && !this.busy && (this.pendingInstall
        || (!this.firstInstallReason && this.selectedPackage && this.installConfirmed));
    },
    installableComponents() {
      if (!this.initialDevice) return this.components;
      const observations = this.initialDevice.components || [];
      return (this.components || []).filter((item) => observations
        .some((c) => c.component === item.value && c.state === 'not_installed'));
    },
    firstInstallReason() {
      if (this.installPrivilegeReason) return this.installPrivilegeReason;
      if (!this.initialDevice) return '';
      const d = this.initialDevice;
      if (d.active_operation_id || d.pending_task_id) return '此主机已有操作或检查进行中，请先核对原任务。';
      const age = (this.now - Date.parse(d.last_success_at)) / 1000;
      if (d.state !== 'checked' || !Number.isFinite(age) || age < -30 || age > 180) {
        return '需要180秒内的新鲜可信检查，先关闭此页并检查主机。';
      }
      if (!(d.components || []).some((c) => c.component === this.component
        && c.state === 'not_installed')) {
        return '尚未可信确认该软件未安装，不能通过首次安装覆盖已有或未知状态的程序。';
      }
      return '';
    },
    installPrivilegeReason() {
      if (this.initialDevice) return '';
      const account = this.setup?.request || this.request;
      return account.ssh_user && account.ssh_user !== 'root' && !account.become_key_id
        ? '首次安装需要root登录或已配置的sudo凭据；当前只能只读登记/检查。'
        : '';
    },
    sshChoices() {
      return (this.options.ssh_keys || []).map((key) => ({ value: key.id, text: `${key.name}（${key.type}）` }));
    },
    becomeChoices() {
      return [{ value: 0, text: '不使用sudo凭据（首次安装须root登录）' },
        ...(this.options.become_keys || []).map((key) => ({ value: key.id, text: `${key.name}（${key.type}）` }))];
    },
    validation() {
      if (!this.allowed) return '需要项目资源管理与执行任务权限才能接入主机。';
      if (!this.options.available) return this.options.error || '接入能力暂不可用。';
      if (!this.request.name.trim() || this.request.name.length > 128) return '请填写1–128字符的主机名称。';
      // The server is authoritative for IPv4/IPv6 parsing and unicast checks.
      if (!/^[0-9a-fA-F:.]+$/.test(this.request.address)) return '请填写固定IP，不接受域名或URL。';
      const port = Number(this.request.ssh_port);
      if (!Number.isInteger(port) || port < 1 || port > 65535) return 'SSH端口必须为1–65535。';
      const account = /^[A-Za-z_][A-Za-z0-9_-]{0,31}$/;
      if (!account.test(this.request.ssh_user)) return '请填写有效的SSH账号。';
      if (!account.test(this.request.service_account) || this.request.service_account === 'root') {
        return '请填写目标机已有的非root服务账户。';
      }
      if (!this.sshChoices.some((key) => key.value === this.request.ssh_key_id)) return '请选择已有的加密SSH凭据。';
      if (!this.becomeChoices.some((key) => key.value === this.request.become_key_id)) return '请选择有效的sudo凭据。';
      if (!this.accessConfirmed) return '请确认本次已获准访问目标主机。';
      return '';
    },
    selectedPackage() { return this.packages.find((pkg) => pkg.id === this.packageId) || null; },
    packageChoices() {
      return this.packages.map((pkg) => ({
        value: pkg.id,
        text: `${pkg.manifest.version} · ${pkg.manifest.target} · ${pkg.manifest.release_id}`,
      }));
    },
  },
  watch: {
    value: {
      immediate: true,
      handler(open) {
        this.generation += 1; clearTimeout(this.timer);
        clearTimeout(this.clockTimer);
        if (open) { this.updateClock(); this.open(); }
      },
    },
    projectId() {
      this.generation += 1; clearTimeout(this.timer); this.packageSequence += 1;
      clearTimeout(this.clockTimer);
      this.reset(); this.$emit('input', false);
    },
  },
  beforeDestroy() {
    this.generation += 1; this.packageSequence += 1; clearTimeout(this.timer);
    clearTimeout(this.clockTimer);
  },
  methods: {
    updateClock() {
      this.now = Date.now();
      if (this.value) this.clockTimer = setTimeout(this.updateClock, 1000);
    },
    newRequestKey() {
      return Array.from(crypto.getRandomValues(new Uint8Array(20)), (v) => v.toString(16).padStart(2, '0')).join('');
    },
    storageKey() {
      return `rcd-host-setup:${this.projectId}:${this.initialSetupId
        ? `setup:${this.initialSetupId}` : this.initialDevice?.id || 'new'}`;
    },
    persist() {
      try {
        sessionStorage.setItem(this.storageKey(), JSON.stringify({
          setup_id: this.setup?.id || this.resumeId,
          pending_create: this.pendingCreate,
          pending_keys: this.pendingKeys,
          pending_install: this.pendingInstall,
          pending_cancel: this.pendingCancel,
          pending_reconcile: this.pendingReconcile,
        }));
      } catch (e) { this.storageError = true; }
    },
    restore() {
      try {
        const saved = JSON.parse(sessionStorage.getItem(this.storageKey()) || '{}');
        this.resumeId = saved.setup_id || ''; this.pendingCreate = saved.pending_create || null;
        this.pendingKeys = saved.pending_keys || null;
        this.pendingInstall = saved.pending_install || null;
        this.pendingCancel = saved.pending_cancel || null;
        this.pendingReconcile = saved.pending_reconcile || null;
        if (this.pendingCreate) this.request = { ...this.pendingCreate.body };
        if (this.pendingInstall?.component) this.component = this.pendingInstall.component;
      } catch (e) { this.storageError = true; }
    },
    reset() {
      this.setup = null; this.resumeId = ''; this.pendingCreate = null; this.pendingKeys = null;
      this.pendingInstall = null; this.keysConfirmed = false; this.installConfirmed = false;
      this.pendingCancel = null; this.cancelConfirmed = false;
      this.pendingReconcile = null; this.reconcileConfirmed = false;
      this.accessConfirmed = false; this.busy = false; this.loading = false; this.error = '';
      this.packageId = ''; this.packages = [];
      this.request = {
        name: '',
        address: '',
        ssh_port: 22,
        ssh_user: '',
        ssh_key_id: null,
        become_key_id: 0,
        service_account: '',
      };
    },
    async open() {
      this.busy = false; this.error = ''; this.restore();
      if (this.initialSetupId) this.resumeId = this.initialSetupId;
      if (!this.pendingInstall) this.component = this.initialComponent || 'lower';
      let original = Promise.resolve();
      if (this.setup || this.resumeId) original = this.refresh();
      else if (this.initialDevice) original = this.loadPackages();
      await Promise.all([this.initialDevice ? Promise.resolve() : this.loadOptions(), original]);
    },
    async loadOptions() {
      const generation = this.generation;
      this.loading = true;
      try {
        const { data } = await axios.get(`${this.endpoint}/options`, { timeout: 15000 });
        if (generation === this.generation) this.options = data;
      } catch (e) {
        if (generation === this.generation) {
          this.options = {
            available: false,
            ssh_keys: [],
            become_keys: [],
            error: this.errorText(e),
          };
        }
      } finally { if (generation === this.generation) this.loading = false; }
    },
    accept(data) {
      const previous = this.setup;
      if (!data.id || (this.resumeId && data.id !== this.resumeId)) throw new Error('host setup identity mismatch');
      if (this.initialDevice && data.device_id !== this.initialDevice.id) throw new Error('first-install device identity mismatch');
      if (previous && data.revision < previous.revision) return;
      this.setup = data; this.resumeId = data.id; this.pendingCreate = null;
      if (data.state !== 'awaiting_host_key') this.pendingKeys = null;
      if (data.state !== 'ready_for_install' && data.state !== 'unknown') this.pendingInstall = null;
      if (!['awaiting_host_key', 'ready_for_install', 'unknown'].includes(data.state)) {
        this.pendingCancel = null;
      }
      if (data.state !== 'unknown' || (this.pendingReconcile
        && data.phase === 'reconcile' && data.revision > this.pendingReconcile.revision)) {
        this.pendingReconcile = null; this.reconcileConfirmed = false;
      }
      if (!previous || previous.revision !== data.revision) this.keysConfirmed = false;
      this.persist(); this.$emit('changed', data);
      if (data.state === 'ready_for_install' && !this.packages.length && !this.pendingInstall) {
        this.loadPackages();
      }
    },
    schedule() {
      clearTimeout(this.timer);
      const activeStates = ['keyscan_queued', 'ssh_probe_queued', 'installation_queued',
        'reconcile_queued', 'running', 'unknown'];
      if (this.value && this.setup && activeStates.includes(this.setup.state)) {
        this.timer = setTimeout(this.refresh, 3000);
      }
    },
    async refresh() {
      const id = this.setup?.id || this.resumeId;
      if (!id || this.busy) return;
      const generation = this.generation;
      this.readSequence += 1;
      const sequence = this.readSequence;
      const current = () => generation === this.generation && sequence === this.readSequence;
      clearTimeout(this.timer); this.loading = true;
      try {
        const { data } = await axios.get(`${this.endpoint}/${encodeURIComponent(id)}`, { timeout: 15000 });
        if (!current()) return;
        this.accept(data); this.error = '';
      } catch (e) {
        if (current()) this.error = `${this.errorText(e)} 原作业引用已保留，不会重新提交。`;
      } finally {
        if (current()) { this.loading = false; this.schedule(); }
      }
    },
    async create() {
      if (this.busy || !this.allowed || this.resumeId || this.initialDevice
        || (!this.pendingCreate && this.validation)) return;
      const retry = !!this.pendingCreate;
      const pending = this.pendingCreate || {
        key: this.newRequestKey(),
        body: {
          ...this.request,
          name: this.request.name.trim(),
          ssh_port: Number(this.request.ssh_port),
        },
      };
      this.pendingCreate = pending; this.persist();
      await this.mutate(() => axios.post(
        this.endpoint,
        pending.body,
        { headers: { 'Idempotency-Key': pending.key }, timeout: 20000 },
      ), (e) => {
        if (!retry && this.definitive(e)) { this.pendingCreate = null; this.persist(); }
      });
    },
    async confirmKeys() {
      if (this.busy || !this.allowed || !this.setup || this.setup.state !== 'awaiting_host_key'
        || this.pendingCancel
        || (!this.pendingKeys && (!this.keysConfirmed || !this.setup.host_keys.length))) return;
      const retry = !!this.pendingKeys;
      const body = this.pendingKeys || {
        revision: this.setup.revision,
        fingerprints: this.setup.host_keys.map((key) => key.fingerprint),
        confirmed: true,
      };
      this.pendingKeys = body; this.persist();
      await this.mutate(() => axios.post(
        `${this.endpoint}/${this.setup.id}/host-key`,
        body,
        { headers: { 'If-Match': `"${body.revision}"` }, timeout: 20000 },
      ), (e) => {
        if (!retry && [400, 401, 403, 422, 428].includes(e.response?.status)) {
          this.pendingKeys = null; this.persist();
        }
      });
    },
    async loadPackages(append = false) {
      if (this.pendingInstall) return;
      this.packageSequence += 1;
      const sequence = this.packageSequence; const generation = this.generation;
      const component = this.component;
      const page = append ? this.packagePage + 1 : 1;
      if (!append) { this.packageId = ''; this.packages = []; this.packagePage = 1; }
      this.packagesLoading = true; this.packagesError = '';
      try {
        const { data } = await axios.get(`/api/project/${this.projectId}/rcd/packages`, {
          params: {
            component, enabled: 'true', page, page_size: 100,
          },
          timeout: 15000,
        });
        if (sequence !== this.packageSequence || generation !== this.generation) return;
        const items = (data.items || []).filter((pkg) => pkg.enabled
          && pkg.manifest.component === component);
        this.packages = append ? [...this.packages, ...items] : items;
        this.packagePage = page;
        this.packagePages = data.total_pages;
      } catch (e) {
        if (sequence === this.packageSequence && generation === this.generation) {
          this.packagesError = this.errorText(e);
        }
      } finally {
        if (sequence === this.packageSequence && generation === this.generation) {
          this.packagesLoading = false;
        }
      }
    },
    async install() {
      if (!this.canSubmitInstall || !this.canChooseInstall) return;
      const retry = !!this.pendingInstall;
      const pending = this.pendingInstall || {
        key: this.newRequestKey(),
        component: this.component,
        packageLabel: `${this.component} / ${this.selectedPackage.manifest.version}`,
        deviceId: this.initialDevice?.id || '',
        body: {
          revision: this.setup?.revision || this.initialDevice.revision,
          package_id: this.selectedPackage.id,
          package_revision: this.selectedPackage.revision,
          confirmed: true,
        },
      };
      this.pendingInstall = pending; this.persist();
      const endpoint = pending.deviceId && !this.setup
        ? `/api/project/${this.projectId}/rcd/devices/${encodeURIComponent(pending.deviceId)}/first-installs`
        : `${this.endpoint}/${this.setup.id}/install`;
      await this.mutate(() => axios.post(endpoint, pending.body, {
        headers: { 'If-Match': `"${pending.body.revision}"`, 'Idempotency-Key': pending.key },
        timeout: 20000,
      }), (e) => {
        if (!retry && [400, 401, 403, 413, 422, 428].includes(e.response?.status)) {
          this.pendingInstall = null; this.persist();
        }
      });
    },
    async mutate(send, onError) {
      const generation = this.generation;
      // A read started before a mutation cannot replace its newer result.
      this.readSequence += 1; this.loading = false;
      this.busy = true; this.error = ''; clearTimeout(this.timer);
      try {
        const { data } = await send();
        if (generation === this.generation) this.accept(data);
      } catch (e) {
        if (generation !== this.generation) return;
        if (onError) onError(e);
        this.error = `${this.errorText(e)} 请核对原请求或刷新原作业，不要重新登记/安装。`;
      } finally {
        if (generation === this.generation) { this.busy = false; this.schedule(); }
      }
    },
    definitive(e) { return [400, 401, 403, 409, 413, 422, 428].includes(e.response?.status); },
    async cancel() {
      if (!this.canCancel || !this.canSubmitCancel) return;
      const body = this.pendingCancel || { revision: this.setup.revision };
      this.pendingCancel = body; this.persist();
      await this.mutate(() => axios.post(
        `${this.endpoint}/${this.setup.id}/cancel`,
        body,
        { headers: { 'If-Match': `"${body.revision}"` }, timeout: 20000 },
      ));
    },
    async reconcile() {
      if (!this.canSubmitReconcile || !this.canReconcile) return;
      if (this.pendingReconcile) { await this.refresh(); return; }
      const body = this.pendingReconcile || { revision: this.setup.revision, confirmed: true };
      this.pendingReconcile = body; this.persist();
      await this.mutate(() => axios.post(
        `${this.endpoint}/${this.setup.id}/reconcile`,
        body,
        { headers: { 'If-Match': `"${body.revision}"` }, timeout: 20000 },
      ), (e) => {
        if ([400, 401, 403, 409, 422, 428].includes(e.response?.status)) {
          this.pendingReconcile = null; this.persist();
        }
      });
    },
    errorText(e) {
      if (e.response?.status === 401) return '登录已失效，请在新标签页重新登录后返回。';
      return e.response?.data?.error || '暂未取得服务端确认。';
    },
    newHost() {
      if (this.initialDevice || this.initialSetupId
        || !['installed_stopped', 'cancelled'].includes(this.setup?.state) || this.busy) return;
      this.reset(); this.persist();
    },
    showTask(id) { if (Number.isInteger(id) && id > 0) EventBus.$emit('i-show-task', { taskId: id }); },
    date(value) { return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '尚未确认'; },
    phaseLabel(phase) {
      return {
        keyscan: '获取主机指纹', probe: '检查主机', install: '首次安装', reconcile: '核对原首装结果',
      }[phase] || phase;
    },
    stateLabel(state) {
      return {
        keyscan_queued: '已登记 · 等待获取指纹',
        awaiting_host_key: '待人工核对主机指纹',
        ssh_probe_queued: '已确认指纹 · 等待主机检查',
        ready_for_install: '主机检查通过 · 尚未安装',
        installation_queued: '首次安装执行中',
        reconcile_queued: '正在只读补查原首装记录',
        running: '任务执行中',
        installed_stopped: '已安装 · 保持停止/待配置',
        failed: '作业失败',
        cancelled: '已取消接入 · 未安装软件',
        unknown: '结果未知 · 请核对原作业',
        host_key_conflict: '主机指纹冲突 · 已停止接入',
      }[state] || '状态尚未确认';
    },
  },
};
</script>

<style scoped>
.host-enrollment { padding: 8px; color: #202a3d; }
.host-enrollment p { line-height: 1.65; }
.input-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 4px 16px; }
.muted { color: #6b788c; }
.warning-text { color: #986614; }
.enrollment-progress {
  display: flex;
  flex-wrap: wrap;
  gap: 10px 18px;
  list-style: none;
  padding: 0;
  margin: 0 0 16px;
}
.enrollment-progress li {
  display: flex;
  align-items: center;
  gap: 6px;
  color: #6b788c;
  font-size: 13px;
}
.enrollment-progress li span {
  display: inline-flex;
  width: 24px;
  height: 24px;
  border: 1px solid #cbd5e3;
  border-radius: 50%;
  align-items: center;
  justify-content: center;
}
.enrollment-progress .current { color: #245da8; font-weight: 600; }
.enrollment-progress .current span { border-color: #245da8; background: #edf4fc; }
.enrollment-progress .completed { color: #247447; }
.next-step { background: #f4f6f9; border-left: 3px solid #8ba2bd; padding: 10px 12px; }
.setup-summary { display: grid; grid-template-columns: 90px minmax(0, 1fr); gap: 10px; }
.setup-summary dd { margin: 0; overflow-wrap: anywhere; }
.host-keys { list-style: none; padding: 0; }
.host-keys li { margin: 12px 0; }
.host-keys code { display: block; overflow-wrap: anywhere; white-space: normal; margin-top: 5px; }
.steps, .host-enrollment section {
  margin-top: 22px; border-top: 1px solid #e0e5ec; padding-top: 18px;
}
.package-controls, .enrollment-actions { display: flex; gap: 8px; flex-wrap: wrap; }
@media (max-width: 600px) { .input-grid { grid-template-columns: minmax(0, 1fr); } }
</style>
