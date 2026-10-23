<template>
  <main class="rcd-devices">
    <header>
      <div>
        <v-btn class="mobile-menu" small outlined @click="openNavigation">菜单</v-btn>
        <h1>设备与版本</h1>
        <p>展示受管软件及主机巡检快照；串口、站点与业务配置请进入 OPC 管理网页。</p>
      </div>
      <div class="heading-actions">
        <v-btn v-if="canManage" outlined @click="openHostSetups">接入与首装记录</v-btn>
        <v-btn outlined :disabled="!canManage || !canInspect"
          title="通过已加密SSH凭据接入主机；首次安装后保持停止待配置"
          @click="addHost">添加主机</v-btn>
        <v-btn v-if="canInspect" color="primary" depressed @click="upgrade(null)">升级软件</v-btn>
        <v-btn outlined :loading="loading" title="读取已保存的结果，不发起远端检查" @click="load"
          >刷新列表</v-btn
        >
      </div>
    </header>
    <v-alert v-if="error" type="error" text role="alert">{{ error }}</v-alert>
    <p v-if="!canInspect" class="muted" role="status">
      当前账号没有执行任务权限，可查看版本与主机快照，不能发起检查、升级或启停。
    </p>
    <v-alert v-if="detailError && !selected" type="warning" text role="alert">
      {{ detailError }}
    </v-alert>
    <p v-if="error && items.length" class="rcd-warning">
      当前显示上次读取的历史快照，不能据此确认设备仍在线；连接恢复后自动重试。
    </p>
    <v-alert v-if="notice" type="info" text role="status">{{ notice }}</v-alert>
    <form @submit.prevent="searchNow">
      <v-text-field
        v-model="search"
        label="搜索设备名称或 IP"
        outlined
        dense
        hide-details
        clearable
        @click:clear="searchNow"
      />
      <v-select
        v-model="state"
        :items="states"
        label="检查状态"
        outlined
        dense
        hide-details
        @change="searchNow"
      />
      <v-btn type="submit" outlined :disabled="loading">查询</v-btn>
    </form>
    <p v-if="loading" role="status" class="muted">正在读取版本快照…</p>
    <div class="table-wrap" :aria-busy="loading">
      <table>
        <thead>
          <tr>
            <th>设备 / 地址</th>
            <th>运维检查</th>
            <th>软件版本</th>
            <th>主机状态</th>
            <th>最后确认</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="device in items" :key="device.id">
            <td data-label="设备">
              <strong>{{ device.name }}</strong
              ><span class="muted">{{ device.address }}</span>
            </td>
            <td data-label="运维检查">
              <span :class="device.state === 'checked' ? 'valid' : 'muted'">{{
                stateName(device.state)
              }}</span
              ><small v-if="device.pending_task_id">检查进行中 #{{ device.pending_task_id }}</small>
              <small v-if="device.last_error" class="rcd-warning">{{ device.last_error }}</small>
              <v-btn v-if="device.pending_task_id || device.last_task_id" small text
                @click="showTask(device.pending_task_id || device.last_task_id)">查看检查日志</v-btn>
              <router-link
                v-if="device.active_operation_id"
                :to="{
                  path: `/project/${projectId}/rcd/operations`,
                  query: { operation: device.active_operation_id },
                }"
                >操作中 / 待确认</router-link
              >
            </td>
            <td data-label="软件版本">
              <div class="component-versions">
                <section v-for="c in knownComponents(device)" :key="c.value"
                  class="component-version">
                  <small class="muted">{{ c.text }}</small>
                  <strong class="component-version-value" :title="version(device, c.value)">
                    {{ version(device, c.value) }}</strong>
                  <span :class="['program-state', componentTone(device, c.value)]">
                    {{ componentState(device, c.value) }}</span>
                  <small v-if="observation(device, c.value).disk_differs" class="rcd-warning">
                    磁盘与运行版本不同
                  </small>
                </section>
              </div>
            </td>
            <td data-label="主机状态"><HostStatus :device="device" /></td>
            <td data-label="最后确认">
              <span>{{ date(device.last_success_at) }}</span
              ><small
                v-if="device.state !== 'checked' && device.last_success_at"
                class="rcd-warning"
                >历史快照，非实时状态</small
              >
            </td>
            <td class="actions" data-label="操作">
              <v-btn small outlined @click="showDetails(device)">查看详情</v-btn>
              <v-btn v-if="canInspect" small outlined @click="upgrade(device)">升级软件</v-btn>
              <v-btn
                v-if="canInspect"
                small
                outlined
                :disabled="!!device.pending_task_id || checking === device.id"
                title="新建只读检查任务，不重启任何程序"
                @click="inspect(device)"
                >检查状态</v-btn
              >
              <small v-if="device.pending_task_id || checking === device.id" class="muted">
                原检查仍在执行，等待其结果；不会重复排队。
              </small>
            </td>
          </tr>
          <tr v-if="!loading && !items.length">
            <td colspan="6" class="empty">
              <strong>{{ emptyTitle }}</strong>
              <p>{{ emptyHint }}</p>
              <v-btn v-if="search || state" small outlined @click="clearFilters">清除筛选</v-btn>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <footer>
      <span>共 {{ total }} 台 · 第 {{ totalPages ? page : 0 }} / {{ totalPages }} 页</span>
      <div>
        <v-select
          v-model="pageSize"
          :items="[20, 50, 100]"
          label="每页条数"
          outlined
          dense
          hide-details
          @change="searchNow"
        />
        <v-btn outlined :disabled="loading || page <= 1" @click="changePage(-1)">上一页</v-btn>
        <v-btn outlined :disabled="loading || page >= totalPages" @click="changePage(1)"
          >下一页</v-btn
        >
      </div>
    </footer>
    <p class="footnote">
      检查成功不代表设备站点通信正常。超过 180 秒未获得新报告显示过期；刷新列表不会远程探测。
      主机指标是巡检采样，不是实时曲线；网卡链路up不代表网络或设备可达。
    </p>
    <v-navigation-drawer
      :value="!!selected"
      right
      temporary
      fixed
      width="620"
      @input="closeDetails"
    >
      <section v-if="selected" class="details">
        <div class="detail-heading">
          <h2>{{ selected.name }}</h2>
          <v-btn text @click="closeDetails(false)">关闭详情</v-btn>
        </div>
        <p>{{ selected.address }} · {{ stateName(detailDevice.state) }}</p>
        <v-btn small outlined :loading="detailLoading" @click="refreshDetail">刷新详情</v-btn>
        <p v-if="detailLoading" class="muted" role="status">正在独立核对该设备的最新版本快照…</p>
        <v-alert v-if="detailError" type="warning" text role="alert">
          详情读取失败，以下为历史快照，不能确认设备当前状态：{{ detailError }}
        </v-alert>
        <v-alert v-if="selected.last_error" type="warning" text>{{ selected.last_error }}</v-alert>
        <p v-if="detailDevice.state !== 'checked'" class="rcd-warning">
          下面可能是历史快照，请以最后确认时间为准。
        </p>
        <p>最后确认：{{ date(selected.last_success_at) }}</p>
        <p>
          最近检查任务：{{ selected.last_task_id ? `#${selected.last_task_id}` : '尚未检查' }}
        </p>
        <MachineBinding :key="`binding:${selected.id}`" :project-id="projectId"
          :device="detailDevice" :allowed="canManage" @changed="bindingChanged" />
        <dl v-if="selected.agent_instance_id" class="machine-binding">
          <dt>下级 OPC 地址</dt><dd>{{ selected.opc_endpoint }}</dd>
          <dt>Agent 地址</dt><dd>{{ selected.agent_url }}</dd>
          <dt>Agent 稳定 ID</dt><dd>{{ selected.agent_instance_id }}</dd>
          <dt>下级稳定 ID</dt><dd>{{ selected.gateway_instance_id }}</dd>
          <dt>管理端稳定 ID</dt><dd>{{ selected.manager_instance_id }}</dd>
        </dl>
        <a
          v-if="selected.web_url"
          :href="selected.web_url"
          target="_blank"
          rel="noopener noreferrer"
          >打开 OPC 管理网页</a
        >
        <HostStatus :device="detailDevice" detailed />
        <section v-for="c in knownComponents(selected)" :key="c.value" class="component-detail">
          <h3>{{ c.text }}</h3>
          <p>{{ componentState(selected, c.value) }}</p>
          <dl v-if="observation(selected, c.value).state !== 'not_installed'">
            <dt>运行版本</dt>
            <dd>{{ version(selected, c.value) }}</dd>
            <dt>磁盘版本</dt>
            <dd>{{ (observation(selected, c.value).disk_build || {}).version || '无法确认' }}</dd>
            <dt>运行发布编号</dt>
            <dd>{{ runningEvidence(selected, c.value, 'release_id') }}</dd>
            <dt>运行程序摘要</dt>
            <dd>{{ runningEvidence(selected, c.value, 'sha256') }}</dd>
            <dt>磁盘程序摘要</dt>
            <dd>{{ observation(selected, c.value).disk_sha256 || '—' }}</dd>
          </dl>
          <p v-if="observation(selected, c.value).error" class="rcd-warning">
            {{ observation(selected, c.value).error }}
          </p>
          <v-btn v-if="observation(selected, c.value).state === 'not_installed'" small outlined
            :disabled="!!firstInstallBlocked(c.value)"
            title="仅对已确认未安装的组件首次安装；保留停止，等待使用人完成配置"
            @click="firstInstall(c.value)">首次安装</v-btn>
          <p v-if="observation(selected, c.value).state === 'not_installed'
            && firstInstallBlocked(c.value)" class="rcd-warning" role="status">
            {{ firstInstallBlocked(c.value) }}
          </p>
          <ServiceControl v-if="['manager', 'agent'].includes(c.value) || application(c.value)"
            :key="`service:${selected.id}:${c.value}`"
            :project-id="projectId" :device="detailDevice" :component="c.value"
            :application="application(c.value)" :allowed="canInspect" />
          <p v-else-if="c.value === 'lower'">
            下级日常启停请打开 OPC 管理网页，由目标机器的 Agent 安全执行。
          </p>
          <p v-else-if="c.value === 'communicate'">
            串口通信程序独立发布，不由采集 Agent 守护；未安装的机器不能通过升级入口盲目安装。
            当前只支持串口/任务/发送均停用时受控升级，不能借此启动真实通信。
          </p>
          <p v-else>该软件尚未登记受控定义，不能根据任意服务名或命令启停。</p>
          <ComponentHistory
            v-if="observation(selected, c.value).state !== 'not_installed'"
            :key="`${projectId}:${selected.id}:${c.value}`"
            :project-id="projectId"
            :device="detailDevice"
            :component="c.value"
            :is-admin="isAdmin"
            :user-permissions="userPermissions"
          />
        </section>
      </section>
    </v-navigation-drawer>
    <ReleaseWizard v-model="wizard" :project-id="projectId" :initial-device-ids="upgradeIds" />
    <v-dialog v-model="hostSetupsOpen" max-width="1000">
      <v-card class="host-records" data-testid="host-setup-records">
        <v-card-title>接入与首装记录</v-card-title>
        <v-card-text>
          <p>记录保存在平台。更换浏览器或关闭向导后，可从这里找回原作业；打开记录不会安装或重投任务。</p>
          <v-alert v-if="hostSetupsError" type="warning" text role="alert">
            {{ hostSetupsError }}
          </v-alert>
          <p v-if="hostSetupsLoading" role="status">正在读取原作业记录…</p>
          <form class="record-filters" @submit.prevent="loadHostSetups(1)">
            <v-text-field v-model="hostSetupSearch" label="搜索主机或作业编号" outlined dense
              hide-details clearable maxlength="128"
              @click:clear="$nextTick(() => loadHostSetups(1))" />
            <v-select v-model="hostSetupFilter" :items="hostSetupFilters" label="接入状态"
              outlined dense hide-details @change="loadHostSetups(1)" />
            <v-btn type="submit" outlined :disabled="hostSetupsLoading">查询记录</v-btn>
          </form>
          <div class="table-wrap">
            <table class="records-table">
              <thead><tr><th>主机 / 地址</th><th>类型 / 状态</th><th>最近更新</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="setup in hostSetups" :key="setup.id">
                  <td data-label="主机 / 地址"><strong>{{ setup.request.name }}</strong>
                    <span class="muted">{{ setup.request.address }}</span>
                    <small class="muted">{{ setup.id }}</small></td>
                  <td data-label="类型 / 状态">
                    <span>{{ setup.existing_device ? '已有主机首次安装' : '新主机接入' }}</span>
                    <span :class="setup.state === 'unknown' ? 'rcd-warning' : ''">
                      {{ hostSetupState(setup.state) }}</span>
                    <small v-if="setup.error" class="rcd-warning">{{ setup.error }}</small>
                  </td>
                  <td data-label="最近更新">{{ date(setup.updated_at) }}</td>
                  <td class="actions" data-label="操作">
                    <v-btn small outlined @click="resumeHostSetup(setup)">查看原作业</v-btn>
                    <v-btn v-if="setup.task_id" small text @click="showTask(setup.task_id)">
                      任务日志
                    </v-btn>
                  </td>
                </tr>
                <tr v-if="!hostSetupsLoading && !hostSetups.length">
                  <td colspan="4" class="empty">{{ hostSetupsError
                    ? '记录读取失败，请重试；不要因此重复接入。' : '暂无符合条件的接入或首装记录。' }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <div class="record-pagination">
            <span>共 {{ hostSetupTotal }} 条 · 第 {{ hostSetupTotalPages ? hostSetupPage : 0 }}
              / {{ hostSetupTotalPages }} 页</span>
            <v-select v-model="hostSetupPageSize" :items="[20, 50, 100]" label="记录每页条数"
              outlined dense hide-details @change="loadHostSetups(1)" />
            <v-btn small outlined :disabled="hostSetupsLoading || hostSetupPage <= 1"
              @click="loadHostSetups(hostSetupPage - 1)">上一页</v-btn>
            <v-btn small outlined
              :disabled="hostSetupsLoading || hostSetupPage >= hostSetupTotalPages"
              @click="loadHostSetups(hostSetupPage + 1)">下一页</v-btn>
          </div>
        </v-card-text>
        <v-card-actions><v-btn text @click="hostSetupsOpen = false">关闭记录</v-btn><v-spacer />
          <v-btn outlined :loading="hostSetupsLoading" @click="loadHostSetups()">刷新记录</v-btn>
        </v-card-actions>
      </v-card>
    </v-dialog>
    <HostEnrollment
      :key="`enroll:${enrollmentSetupId || (enrollmentDevice ? enrollmentDevice.id : 'new')}`"
      v-model="enrollmentOpen" :project-id="projectId" :initial-device="enrollmentDevice"
      :initial-component="enrollmentComponent" :initial-setup-id="enrollmentSetupId"
      :allowed="canManage && canInspect" :components="knownComponents({})"
      @changed="enrollmentChanged" />
  </main>
</template>

<script>
import axios from 'axios';
import EventBus from '@/event-bus';
import PermissionsCheck from '@/components/PermissionsCheck';
import { USER_PERMISSIONS } from '@/lib/constants';
import ReleaseWizard from './ReleaseWizard.vue';
import ComponentHistory from './ComponentHistory.vue';
import ServiceControl from './ServiceControl.vue';
import MachineBinding from './MachineBinding.vue';
import HostStatus from './HostStatus.vue';
import HostEnrollment from './HostEnrollment.vue';

export default {
  components: {
    ReleaseWizard, ComponentHistory, ServiceControl, MachineBinding, HostStatus, HostEnrollment,
  },
  mixins: [PermissionsCheck],
  props: { projectId: Number },
  data() {
    return {
      items: [],
      total: 0,
      totalPages: 0,
      page: 1,
      requestedPage: 1,
      pageSize: 20,
      search: '',
      state: '',
      error: '',
      notice: '',
      loading: false,
      selected: null,
      detailLoading: false,
      detailError: '',
      detailConfirmed: false,
      detailSequence: 0,
      wizard: false,
      enrollmentOpen: false,
      enrollmentDevice: null,
      enrollmentComponent: '',
      enrollmentSetupId: '',
      hostSetupsOpen: false,
      hostSetups: [],
      hostSetupsLoading: false,
      hostSetupsError: '',
      hostSetupSequence: 0,
      hostSetupSearch: '',
      hostSetupFilter: '',
      hostSetupPage: 1,
      hostSetupPageSize: 20,
      hostSetupRequestedPage: 1,
      hostSetupTotal: 0,
      hostSetupTotalPages: 0,
      hostSetupQueryKey: '',
      hostSetupFilters: [
        { text: '全部记录', value: '' },
        { text: '结果未知', value: 'unknown' },
        { text: '待核对主机指纹', value: 'awaiting_host_key' },
        { text: '检查通过，尚未安装', value: 'ready_for_install' },
        { text: '首次安装执行中', value: 'installation_queued' },
        { text: '任务执行中', value: 'running' },
        { text: '正在只读补查', value: 'reconcile_queued' },
        { text: '已安装，保持停止', value: 'installed_stopped' },
        { text: '作业失败', value: 'failed' },
        { text: '主机指纹冲突', value: 'host_key_conflict' },
        { text: '已取消', value: 'cancelled' },
      ],
      upgradeIds: [],
      checking: '',
      sequence: 0,
      timer: null,
      previousTitle: '',
      requestedTaskId: 0,
      closedDeviceLink: '',
      components: [
        { text: '下级 OPC', value: 'lower' },
        { text: 'OPC 管理端', value: 'manager' },
        { text: 'RCD Agent', value: 'agent' },
        { text: '串口通信程序', value: 'communicate' },
      ],
      applications: [],
      applicationSequence: 0,
      states: [
        { text: '全部状态', value: '' },
        { text: '检查成功', value: 'checked' },
        { text: '状态已过期', value: 'stale' },
        { text: '检查失败', value: 'check_failed' },
        { text: '尚未检查', value: 'never_checked' },
      ],
    };
  },
  computed: {
    endpoint() {
      return `/api/project/${this.projectId}/rcd/devices`;
    },
    canInspect() {
      return this.can(USER_PERMISSIONS.runProjectTasks);
    },
    canManage() { return this.can(USER_PERMISSIONS.manageProjectResources); },
    emptyTitle() {
      if (this.error) return '设备列表暂时无法读取';
      return this.search || this.state ? '没有符合筛选条件的设备' : '尚未添加主机';
    },
    emptyHint() {
      if (this.error) return '请重试读取，不要因此重复接入已有主机。';
      return this.search || this.state ? '可调整条件或清除筛选；不会修改已登记的主机。'
        : '点击“添加主机”开始；之前受理的接入任务可从“接入与首装记录”找回。';
    },
    detailDevice() {
      // Retain useful historical values without passing stale "checked"
      // evidence to service controls when the independent detail read fails.
      if (!this.selected) return null;
      return this.detailConfirmed && !this.detailError
        ? this.selected : { ...this.selected, state: 'stale' };
    },
  },
  watch: {
    projectId() {
      this.items = [];
      this.selected = null;
      this.detailSequence += 1;
      this.detailLoading = false;
      this.detailError = '';
      this.detailConfirmed = false;
      this.closedDeviceLink = '';
      this.notice = '';
      this.enrollmentOpen = false;
      this.enrollmentDevice = null;
      this.enrollmentSetupId = '';
      this.hostSetupsOpen = false;
      this.hostSetups = [];
      this.hostSetupTotal = 0;
      this.hostSetupTotalPages = 0;
      this.hostSetupPage = 1;
      this.hostSetupRequestedPage = 1;
      this.hostSetupQueryKey = '';
      this.hostSetupsLoading = false;
      this.hostSetupsError = '';
      this.hostSetupSequence += 1;
      this.applications = [];
      this.loadApplications();
      this.searchNow();
    },
  },
  created() {
    this.previousTitle = document.title;
    document.title = '设备与版本 - Semaphore UI';
    this.load();
    this.loadApplications();
    if (this.$route.query.upgrade === '1') this.$nextTick(() => this.upgrade(null));
  },
  beforeDestroy() {
    clearTimeout(this.timer);
    this.sequence += 1;
    this.detailSequence += 1;
    this.applicationSequence += 1;
    this.hostSetupSequence += 1;
    document.title = this.previousTitle;
  },
  methods: {
    application(id) { return this.applications.find((app) => app.component === id) || null; },
    addHost() {
      this.enrollmentDevice = null; this.enrollmentComponent = ''; this.enrollmentSetupId = '';
      this.enrollmentOpen = true;
    },
    clearFilters() { this.search = ''; this.state = ''; this.searchNow(); },
    firstInstallBlocked(id) {
      if (!this.canManage || !this.canInspect) return '当前账号没有首次安装权限。';
      if (!this.detailConfirmed) return '等待独立详情确认后才能首次安装。';
      if (this.detailDevice?.state !== 'checked') return '需要可信的新鲜检查，请先检查状态。';
      if (this.detailDevice.active_operation_id || this.detailDevice.pending_task_id) {
        return '该主机已有操作或检查进行中，须先核对原任务。';
      }
      return this.observation(this.detailDevice, id).state !== 'not_installed'
        ? '尚未确认该组件未安装，不能通过首次安装覆盖程序。' : '';
    },
    firstInstall(id) {
      if (this.firstInstallBlocked(id)) return;
      this.enrollmentDevice = this.detailDevice;
      this.enrollmentComponent = id;
      this.enrollmentSetupId = '';
      this.enrollmentOpen = true;
    },
    async openHostSetups() {
      if (!this.canManage) return;
      this.hostSetupsOpen = true;
      await this.loadHostSetups();
    },
    async loadHostSetups(targetPage = this.hostSetupRequestedPage) {
      const project = this.projectId;
      const query = {
        page_size: this.hostSetupPageSize,
        q: (this.hostSetupSearch || '').trim(),
        state: this.hostSetupFilter,
      };
      const queryKey = JSON.stringify(query);
      const page = queryKey === this.hostSetupQueryKey ? targetPage : 1;
      this.hostSetupRequestedPage = page;
      this.hostSetupSequence += 1; const sequence = this.hostSetupSequence;
      this.hostSetupsLoading = true; this.hostSetupsError = '';
      try {
        const { data } = await axios.get(`/api/project/${project}/rcd/host-setups`, {
          params: { ...query, page }, timeout: 15000,
        });
        if (project !== this.projectId || sequence !== this.hostSetupSequence) return;
        this.hostSetups = data.items || [];
        this.hostSetupTotal = data.total;
        this.hostSetupTotalPages = data.total_pages;
        this.hostSetupPage = page;
        this.hostSetupQueryKey = queryKey;
        if (page > Math.max(1, this.hostSetupTotalPages)) {
          this.loadHostSetups(Math.max(1, this.hostSetupTotalPages));
        }
      } catch (e) {
        if (project === this.projectId && sequence === this.hostSetupSequence) {
          this.hostSetupsError = `${this.errorText(e)} 保留已读取记录，打开后仍会重新核对原作业。`;
        }
      } finally {
        if (project === this.projectId && sequence === this.hostSetupSequence) {
          this.hostSetupsLoading = false;
        }
      }
    },
    resumeHostSetup(setup) {
      if (!this.canManage || !setup.id) return;
      this.hostSetupsOpen = false; this.enrollmentDevice = null; this.enrollmentComponent = '';
      this.enrollmentSetupId = setup.id; this.enrollmentOpen = true;
    },
    showTask(id) { if (Number.isInteger(id) && id > 0) EventBus.$emit('i-show-task', { taskId: id }); },
    hostSetupState(state) {
      return {
        keyscan_queued: '等待获取指纹',
        awaiting_host_key: '待核对主机指纹',
        ssh_probe_queued: '等待主机检查',
        ready_for_install: '检查通过，尚未安装',
        installation_queued: '首次安装执行中',
        reconcile_queued: '正在只读补查',
        running: '任务执行中',
        unknown: '结果未知，需核对原作业',
        installed_stopped: '已安装，保持停止',
        failed: '作业失败',
        host_key_conflict: '主机指纹冲突',
        cancelled: '已取消',
      }[state] || '状态尚未确认';
    },
    enrollmentChanged(setup) {
      if (setup.state === 'installed_stopped' || setup.device_id) this.load();
    },
    knownComponents(device) {
      const result = [...this.components];
      const names = new Map(this.applications.map((app) => [app.component, app.name]));
      const observed = (device.components || []).map((c) => c.component);
      Array.from(new Set([...names.keys(), ...observed])).forEach((id) => {
        if (!result.some((c) => c.value === id)) {
          result.push({ value: id, text: names.get(id) || id });
        }
      });
      return result;
    },
    async loadApplications() {
      this.applicationSequence += 1;
      const sequence = this.applicationSequence;
      const project = this.projectId;
      try {
        const { data } = await axios.get(`/api/project/${project}/rcd/applications`, { timeout: 15000 });
        if (project === this.projectId && sequence === this.applicationSequence) {
          this.applications = data.items;
        }
      } catch (e) {
        if (project === this.projectId && sequence === this.applicationSequence) {
          this.notice = '软件种类暂未刷新，自定义软件暂按ID显示，不代表程序已停止。';
        }
      }
    },
    bindingChanged(device) {
      if (this.selected?.id !== device.id) return;
      this.selected = device;
      this.detailConfirmed = false;
      this.load();
    },
    showDetails(device) {
      this.selected = device;
      this.detailConfirmed = false;
      this.detailError = '';
      return this.refreshDetail();
    },
    async refreshDetail() {
      const id = this.selected?.id || (this.closedDeviceLink !== this.$route.query.device
        ? this.$route.query.device : '');
      if (!id) return;
      const project = this.projectId;
      this.detailSequence += 1;
      const seq = this.detailSequence;
      const current = () => seq === this.detailSequence && project === this.projectId
        && (this.selected ? this.selected.id === id : this.$route.query.device === id
          && this.closedDeviceLink !== id);
      this.detailLoading = true;
      // Keep a previous error visible until a successful read replaces it.
      try {
        const { data } = await axios.get(`${this.endpoint}/${encodeURIComponent(id)}`);
        if (!current()) return;
        if (data.id !== id) throw new Error('device identity mismatch');
        this.selected = data;
        this.detailConfirmed = true;
        this.detailError = '';
      } catch (err) {
        if (current()) {
          this.detailConfirmed = false;
          this.detailError = err.response?.status === 404
            ? '该设备已不存在或当前账号无法访问，请关闭详情并重新检查列表。'
            : this.errorText(err);
        }
      } finally {
        if (current()) this.detailLoading = false;
      }
    },
    upgrade(device) {
      this.upgradeIds = device ? [device.id] : [];
      this.wizard = true;
    },
    openNavigation() {
      EventBus.$emit('i-show-drawer');
    },
    closeDetails(value) {
      if (!value) {
        this.detailSequence += 1;
        this.selected = null;
        this.detailLoading = false;
        this.detailError = '';
        this.detailConfirmed = false;
        this.closedDeviceLink = this.$route.query.device || '';
        if (this.$route.query.device) {
          const query = { ...this.$route.query }; delete query.device;
          this.$router.replace({ query });
        }
      }
    },
    stateName(state) {
      return (
        {
          checked: '检查成功',
          stale: '状态已过期',
          check_failed: '检查失败',
          never_checked: '尚未检查',
        }[state] || '无法确认'
      );
    },
    observation(device, component) {
      return (device.components || []).find((c) => c.component === component) || {};
    },
    runningEvidence(device, component, field) {
      const value = this.observation(device, component);
      if (value.state !== 'running') return '—';
      if (field === 'sha256') return value.running_sha256 || '无法确认';
      return (value.running_build || {}).release_id || '无法确认';
    },
    version(device, component) {
      const v = this.observation(device, component);
      if (v.state !== 'running') return '—';
      return (v.running_build || {}).version || '—';
    },
    componentState(device, component) {
      const label = {
        running: '程序运行',
        running_version_unknown: '运行中，版本无法确认',
        stopped: '已停止',
        not_installed: '未安装',
        unknown: '状态无法确认',
      }[this.observation(device, component).state];
      if (!label) return '尚未检查';
      return this.componentHistorical(device) ? `上次：${label}` : label;
    },
    componentHistorical(device) {
      return !!device.last_success_at && ['checked', 'stale', 'check_failed'].includes(device.state)
        && (!!this.error || ['stale', 'check_failed'].includes(device.state));
    },
    componentTone(device, component) {
      if (this.componentHistorical(device)) return 'muted';
      const state = this.observation(device, component).state;
      if (state === 'running') return 'valid';
      return ['unknown', 'running_version_unknown'].includes(state) ? 'rcd-warning' : 'muted';
    },
    date(value) {
      return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '尚未确认';
    },
    searchNow() {
      this.requestedPage = 1;
      this.$nextTick(this.load);
    },
    changePage(delta) {
      this.load(this.page + delta);
    },
    errorText(err) {
      return err.response?.status === 401
        ? '登录已失效，请重新登录。'
        : err.response?.data?.error || '请求失败，请重试；没有因此执行升级。';
    },
    async load(targetPage) {
      const requestedPage = Number.isInteger(targetPage) ? targetPage : this.requestedPage;
      this.requestedPage = requestedPage;
      this.sequence += 1;
      const seq = this.sequence;
      clearTimeout(this.timer);
      this.loading = true;
      this.error = '';
      // A selected device is independent of the filtered/paginated list.
      // Start both reads so a list failure cannot freeze the open drawer.
      const detail = this.refreshDetail();
      try {
        const { data } = await axios.get(this.endpoint, {
          params: {
            page: requestedPage,
            page_size: this.pageSize,
            q: this.search || '',
            state: this.state,
          },
        });
        if (seq !== this.sequence) return;
        this.items = data.items;
        this.total = data.total;
        this.totalPages = data.total_pages;
        if (requestedPage > Math.max(1, this.totalPages)) {
          this.load(Math.max(1, this.totalPages));
          return;
        }
        this.page = requestedPage;
        if (this.items.some((d) => d.last_task_id === this.requestedTaskId)) {
          this.notice = '';
        }
      } catch (err) {
        if (seq === this.sequence) this.error = this.errorText(err);
      } finally {
        await detail;
        if (seq === this.sequence) {
          this.loading = false;
          this.timer = setTimeout(
            this.load,
            this.items.some((d) => d.pending_task_id) || this.selected?.pending_task_id
              ? 3000 : 30000,
          );
        }
      }
    },
    async inspect(device) {
      const project = this.projectId;
      this.checking = device.id;
      this.error = '';
      try {
        const { data } = await axios.post(
          `${this.endpoint}/${device.id}/inspections`,
          { revision: device.revision },
          { headers: { 'If-Match': `"${device.revision}"` } },
        );
        if (project !== this.projectId) return;
        this.requestedTaskId = data.task_id;
        this.notice = `只读检查任务 #${data.task_id} 已接收，不会重启程序；请等待结果。`;
        this.load();
      } catch (err) {
        if (project === this.projectId) this.error = this.errorText(err);
      } finally {
        this.checking = '';
      }
    },
  },
};
</script>

<style scoped>
.rcd-devices {
  padding: 30px 32px;
  color: #202a3d;
  background: #fff;
  min-height: 100%;
}
header,
.detail-heading,
footer,
footer > div {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
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
header {
  align-items: flex-start;
  margin-bottom: 26px;
}
.heading-actions { display: flex; gap: 10px; flex-wrap: wrap; justify-content: flex-end; }
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
  width: 100%;
  border-collapse: collapse;
  table-layout: fixed;
  font-size: 13px;
}
th {
  background: #f4f6f9;
  text-align: left;
}
th:first-child {
  width: 13%;
}
th:nth-child(2) {
  width: 12%;
}
th:nth-child(3) {
  width: 29%;
}
th:nth-child(4) {
  width: 24%;
}
th:nth-child(5) {
  width: 13%;
}
th:last-child {
  width: 9%;
}
.rcd-devices .v-btn--outlined {
  color: #344e79;
  border-color: #cbd5e3;
}
th,
td {
  padding: 16px 12px;
  border-bottom: 1px solid #e0e5ec;
  vertical-align: top;
  overflow-wrap: anywhere;
}
tbody tr:last-child td {
  border-bottom: 0;
}
td span,
td small,
td strong {
  display: block;
  margin-bottom: 5px;
}
.component-versions {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}
.component-version { min-width: 0; }
.component-version small, .component-version strong, .component-version span { display: block; }
.component-version strong { font-size: 12px; }
.component-version .component-version-value {
  white-space: nowrap; text-overflow: ellipsis; overflow: hidden; line-height: 1.7;
}
.program-state { font-size: 12px; line-height: 1.5; }
.valid {
  color: #247447;
}
.rcd-warning {
  color: #986614;
}
.actions .v-btn {
  margin-bottom: 9px;
  max-width: 100%;
  min-width: 0;
  padding-left: 7px;
  padding-right: 7px;
  font-size: 12px;
}
.empty {
  text-align: center;
  padding: 44px 20px;
  color: #6b788c;
}
footer {
  margin-top: 24px;
}
footer .v-input {
  max-width: 120px;
}
.footnote {
  margin-top: 28px;
  padding-top: 20px;
  border-top: 1px solid #e0e5ec;
  color: #6b788c;
  font-size: 13px;
}
.details {
  padding: 24px;
}
.record-filters { margin-top: 14px; margin-bottom: 16px; }
.records-table th:first-child { width: 34%; }
.records-table th:nth-child(2) { width: 29%; }
.records-table th:nth-child(3) { width: 21%; }
.records-table th:last-child { width: 16%; }
.record-pagination {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-top: 18px;
}
.record-pagination .v-input { max-width: 140px; flex: 0 0 140px; }
.component-detail {
  border-top: 1px solid #e0e5ec;
  margin-top: 24px;
  padding-top: 20px;
}
.component-detail dl {
  display: grid;
  grid-template-columns: 110px minmax(0, 1fr);
  gap: 10px;
  font-size: 14px;
}
.component-detail dd {
  overflow-wrap: anywhere;
  margin: 0;
}
.component-detail dt {
  color: #6b788c;
}
.machine-binding { font-size: 13px; overflow-wrap: anywhere; }
.machine-binding dd { margin: 0 0 8px; }
.mobile-menu {
  display: none;
}
@media (max-width: 1000px) {
  .rcd-devices {
    padding: 24px 18px;
  }
  th,
  td {
    padding: 12px 8px;
  }
  .component-versions { grid-template-columns: minmax(0, 1fr); }
}
@media (max-width: 960px) {
  .mobile-menu {
    display: inline-flex;
    margin-bottom: 15px;
  }
}
@media (max-width: 700px) {
  header,
  form,
  footer {
    flex-wrap: wrap;
  }
  form > .v-input {
    max-width: none;
    min-width: 200px;
  }
  h1 {
    font-size: 25px;
  }
  thead {
    display: none;
  }
  table,
  tbody,
  tr,
  td {
    display: block;
    width: 100%;
  }
  td {
    border: 0;
    display: grid;
    grid-template-columns: 100px minmax(0, 1fr);
    padding: 9px 14px;
  }
  td::before {
    content: attr(data-label);
    color: #6b788c;
    grid-row: 1 / span 3;
  }
  tr + tr {
    border-top: 1px solid #dce2ec;
  }
  td.empty {
    display: block;
  }
  .actions .v-btn {
    justify-self: start;
  }
  footer > div {
    flex-wrap: wrap;
  }
  .details {
    padding: 20px;
  }
  .component-detail dl {
    grid-template-columns: 90px minmax(0, 1fr);
  }
}
</style>
