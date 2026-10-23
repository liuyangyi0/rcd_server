<template>
  <main class="rcd-packages">
    <header class="rcd-heading">
      <div>
        <v-btn class="rcd-mobile-menu" small outlined title="打开项目导航" @click="openNavigation">
          <v-icon left small>mdi-menu</v-icon>菜单
        </v-btn>
        <h1>软件包</h1>
        <p>上传内置组件或已登记软件种类的版本包，校验后再选择设备升级。上传不会自动安装。</p>
      </div>
      <div class="rcd-heading-actions">
      <v-btn outlined :disabled="!canManage" title="先登记受管软件种类，不安装或启动程序"
        @click="definitionsOpen = true">添加软件种类</v-btn>
      <v-btn color="primary" depressed :disabled="!canManage || uploading"
        :title="canManage ? '上传软件版本包，上传不会自动部署' : '需要项目资源管理权限才能上传'"
        @click="openUpload">
        <v-icon left>mdi-upload</v-icon>上传软件包
      </v-btn>
      </div>
    </header>
    <p v-if="!canManage || !canRun" class="rcd-muted" role="status">
      <template v-if="!canManage">当前账号可查看软件包；上传和启停用需要项目资源管理权限。</template>
      <template v-if="!canRun">当前账号没有执行任务权限，不能发起升级或恢复。</template>
    </p>
    <v-alert v-if="error" type="error" text role="alert">{{ error }}</v-alert>
    <p v-if="error && items.length" class="rcd-warning" role="status">
      本次筛选或第 {{ pendingPage }} 页尚未确认，下方保留上次成功读取的第 {{ page }} 页；请刷新重试。
    </p>
    <v-alert v-if="notice" type="success" text role="status">
      {{ notice }}
      <template v-if="uploadedPackage">
        <p>
          {{ componentName(uploadedPackage.manifest.component) }} ·
          {{ uploadedPackage.manifest.version }}
        </p>
        <v-btn v-if="canRun && uploadedPackage.enabled" outlined small
          @click="upgradePackage = uploadedPackage; wizard = true;">选择设备升级此软件包</v-btn>
        <p v-if="!uploadedPackage.enabled">此包已停用；请在详情中核对原因，启用前不能升级。</p>
      </template>
    </v-alert>
    <form class="rcd-filters" @submit.prevent="searchNow">
      <v-text-field
        v-model="search"
        label="搜索版本或发布编号"
        outlined
        dense
        hide-details
        clearable
        @click:clear="searchNow"
      />
      <v-select
        v-model="component"
        :items="componentChoices"
        label="组件"
        outlined
        dense
        hide-details
        @change="searchNow"
      />
      <v-select
        v-model="enabled"
        :items="states"
        label="软件包状态"
        outlined
        dense
        hide-details
        @change="searchNow"
      />
      <v-btn type="submit" outlined :disabled="loading">查询</v-btn>
      <v-btn outlined :loading="loading" title="重新读取软件包列表，不发起远端检查" @click="load"
        >刷新列表</v-btn
      >
    </form>
    <p v-if="loading" class="rcd-muted" role="status">正在读取软件包…</p>
    <div class="rcd-table-wrap" :aria-busy="loading">
      <table class="rcd-table">
        <thead>
          <tr>
            <th>组件 / 版本</th>
            <th>适用平台</th>
            <th>说明</th>
            <th>状态</th>
            <th>上传时间</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="item in items" :key="item.id">
            <td>
              <strong>{{ componentName(item.manifest.component) }}</strong
              ><span>{{ item.manifest.version }}</span>
            </td>
            <td>
              {{ item.manifest.target }}<span class="rcd-muted">{{ size(item.archive_size) }}</span>
            </td>
            <td class="rcd-notes">{{ item.manifest.release_notes || '未填写发布说明' }}</td>
            <td>
              <span :class="item.enabled ? 'rcd-valid' : 'rcd-muted'">{{
                item.enabled ? '已校验' : '已停用'
              }}</span>
            </td>
            <td>
              {{ date(item.uploaded_at) }}<span class="rcd-muted">{{ item.uploaded_by }}</span>
            </td>
            <td>
              <v-btn small outlined @click="selected = item">查看详情</v-btn
              ><v-btn
                v-if="canRun"
                small
                outlined
                :disabled="loading || !listConfirmed || !item.enabled"
                @click="
                  upgradePackage = item;
                  wizard = true;
                "
                >选择设备升级</v-btn
              >
            </td>
          </tr>
          <tr v-if="!loading && !items.length">
            <td colspan="6" class="rcd-empty">
              <strong>{{ error ? '列表暂时无法读取' : '没有符合条件的软件包' }}</strong>
              <p>
                {{
                  error
                    ? '请重试；不要将读取失败视为软件包已删除。'
                    : '上传 .rcd.tar.gz 发布包，或调整筛选条件。'
                }}
              </p>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <footer class="rcd-pagination">
      <span>共 {{ total }} 个 · 第 {{ totalPages ? page : 0 }} / {{ totalPages }} 页</span>
      <div>
        <v-select
          v-model="pageSize"
          :items="[20, 50, 100]"
          label="每页条数"
          outlined
          dense
          hide-details
          @change="searchNow"
        /><v-btn outlined :disabled="loading || page <= 1" @click="changePage(-1)">上一页</v-btn>
        <v-btn outlined :disabled="loading || page >= totalPages" @click="changePage(1)"
          >下一页</v-btn
        >
      </div>
    </footer>
    <p class="rcd-footnote">
      “已校验”表示文件格式和摘要通过检查，不表示已部署或程序正在运行。配置、数据库与 PKI
      不在软件包中。
    </p>

    <v-dialog v-model="uploadOpen" max-width="640" :persistent="uploading">
      <v-card class="rcd-dialog">
        <v-card-title>上传软件包</v-card-title>
        <v-card-text>
          <p>上传软件版本包：每包只包含一个受管组件，最大 256 MiB，上传不重启程序。</p>
          <p>支持四个内置组件及已登记的单二进制软件。新软件请先“添加软件种类”，不能通过上传包执行任意安装脚本。</p>
          <details class="rcd-package-help">
            <summary>没有发布包？查看格式和生成方式</summary>
            <p>
              内置组件先用现有release.py生成带版本身份的产物，再用
              tools/release/rcd_package.py生成.rcd.tar.gz单组件发布包。
              已登记的自定义软件使用同一工具的--binary及--metadata入口，封装Linux二进制与公开构建信息。
              工具核对构建清单、适配器/数据兼容声明及摘要；现场人员无需手填哈希或安装目录。
            </p>
            <p>
              包中只有 manifest.json 和 binary 两个根目录文件，不能自行压缩整个安装目录，
              也不能把 EXE、ZIP 改后缀冒充。包里不得放配置、数据库、日志、密码或 PKI。
            </p>
            <p>上传只增加软件版本。新主机请到“设备与版本 → 添加主机”接入，首装成功后保持停止、等待业务配置。</p>
          </details>
          <div class="rcd-dropzone" @dragover.prevent @drop.prevent="dropFile">
            <v-icon size="30">mdi-package-variant-closed</v-icon>
            <p>将发布包拖到这里，或点击选择文件</p>
            <v-file-input
              v-model="file"
              accept=".rcd.tar.gz"
              label="选择 .rcd.tar.gz 软件包"
              outlined
              dense
              :disabled="uploading"
              show-size
              :error-messages="fileError ? [fileError] : []"
            />
          </div>
          <p v-if="file && !fileError" class="rcd-muted" role="status">
            已选择 {{ file.name }}（{{ size(file.size) }}）。上传后仍须服务端校验，不会自动安装。
          </p>
          <v-progress-linear
            v-if="uploading"
            :value="progress"
            :indeterminate="progress === 100"
            class="mt-5"
          />
          <p v-if="uploading" role="status">
            {{ progress === 100 ? '文件已送达，正在校验…' : `正在上传 ${progress}%` }}
          </p>
          <v-alert v-if="uploadError" type="error" text role="alert" class="mt-4">{{
            uploadError
          }}</v-alert>
        </v-card-text>
        <v-card-actions
          ><v-spacer /><v-btn text :disabled="uploading" @click="uploadOpen = false">关闭</v-btn>
          <v-btn color="primary" depressed :disabled="!file || !!fileError || uploading"
            @click="upload"
            >上传并校验</v-btn
          >
        </v-card-actions>
      </v-card>
    </v-dialog>

    <v-dialog :value="!!selected" max-width="740" scrollable @input="closeDetails">
      <v-card v-if="selected" class="rcd-dialog">
        <v-card-title>软件包详情</v-card-title>
        <v-card-text>
          <h2>
            {{ componentName(selected.manifest.component) }} · {{ selected.manifest.version }}
          </h2>
          <p class="rcd-detail-notes">
            {{ selected.manifest.release_notes || '未填写发布说明' }}
          </p>
          <dl class="rcd-details">
            <dt>发布编号</dt>
            <dd>{{ selected.manifest.release_id }}</dd>
            <dt>适用平台</dt>
            <dd>{{ selected.manifest.target }}</dd>
            <dt>构建时间</dt>
            <dd>{{ date(selected.manifest.built_at) }}</dd>
            <dt>源码提交</dt>
            <dd>{{ selected.manifest.commit }}</dd>
            <dt>源码包含未提交修改</dt>
            <dd>{{ selected.manifest.dirty ? '是（已记录源码摘要）' : '否' }}</dd>
            <dt>程序 SHA-256</dt>
            <dd>{{ selected.manifest.binary_sha256 }}</dd>
            <dt>最低适配器版本</dt>
            <dd>{{ selected.manifest.compatibility.adapter_min_version }}</dd>
            <dt>数据格式兼容范围</dt>
            <dd>
              <div v-for="(range, key) in selected.manifest.compatibility.state_formats" :key="key">
                {{ key }}：{{ range.min }}–{{ range.max }}
              </div>
            </dd>
          </dl>
          <v-alert type="info" text
            >兼容范围由发布器结合目标机器的实际状态再次检查。停用保留文件及历史记录，不卸载已部署程序。</v-alert
          >
          <v-alert v-if="detailError" type="error" text role="alert">{{ detailError }}</v-alert>
        </v-card-text>
        <v-card-actions
          ><v-btn outlined :disabled="busy" @click="download">下载软件包</v-btn>
          <v-btn text :disabled="busy" @click="refreshDetail">刷新详情</v-btn>
          <v-btn v-if="canRun" color="primary" depressed :disabled="busy || !selected.enabled"
            @click="upgradePackage = selected; selected = null; wizard = true;">选择设备升级</v-btn>
          <v-btn v-if="canManage" outlined :disabled="busy" @click="toggleOpen = true">{{
            selected.enabled ? '停用软件包' : '启用软件包'
          }}</v-btn>
          <v-spacer /><v-btn text @click="closeDetails(false)">关闭详情</v-btn></v-card-actions
        >
      </v-card>
    </v-dialog>
    <v-dialog v-model="toggleOpen" max-width="480" :persistent="busy">
      <v-card v-if="selected"
        ><v-card-title>{{ selected.enabled ? '停用软件包？' : '重新启用软件包？' }}</v-card-title>
        <v-card-text
          >不删除文件，不重启机器，不改变已发布的程序。记录更改使用当前版本条件，避免覆盖他人的操作。</v-card-text
        >
        <v-card-actions
          ><v-spacer /><v-btn text :disabled="busy" @click="toggleOpen = false">取消</v-btn>
          <v-btn color="primary" depressed :loading="busy" @click="toggle"
            >确认</v-btn
          ></v-card-actions
        >
      </v-card>
    </v-dialog>
    <ReleaseWizard v-model="wizard" :project-id="projectId" :initial-package="upgradePackage" />
    <SoftwareDefinitions v-model="definitionsOpen" :project-id="projectId" :allowed="canManage"
      @registered="definitionRegistered" @upload="uploadDefinedVersion" />
  </main>
</template>

<script>
import axios from 'axios';
import EventBus from '@/event-bus';
import PermissionsCheck from '@/components/PermissionsCheck';
import { USER_PERMISSIONS } from '@/lib/constants';
import ReleaseWizard from './ReleaseWizard.vue';
import SoftwareDefinitions from './SoftwareDefinitions.vue';

export default {
  components: { ReleaseWizard, SoftwareDefinitions },
  mixins: [PermissionsCheck],
  props: { projectId: Number },
  data() {
    return {
      items: [],
      total: 0,
      totalPages: 0,
      page: 1,
      pendingPage: 1,
      listConfirmed: false,
      pageSize: 20,
      search: '',
      component: '',
      enabled: '',
      loading: false,
      error: '',
      notice: '',
      applications: [],
      applicationSequence: 0,
      definitionsOpen: false,
      uploadedPackage: null,
      sequence: 0,
      selected: null,
      wizard: false,
      upgradePackage: null,
      detailError: '',
      busy: false,
      detailSequence: 0,
      toggleOpen: false,
      uploadOpen: false,
      file: null,
      uploading: false,
      uploadSequence: 0,
      progress: 0,
      uploadError: '',
      previousTitle: '',
      components: [
        { text: '全部组件', value: '' },
        { text: '下级 OPC', value: 'lower' },
        { text: 'OPC 管理端', value: 'manager' },
        { text: 'RCD Agent', value: 'agent' },
        { text: '串口通信程序', value: 'communicate' },
      ],
      states: [
        { text: '全部状态', value: '' },
        { text: '可用', value: 'true' },
        { text: '已停用', value: 'false' },
      ],
    };
  },
  computed: {
    fileError() {
      if (!this.file) return '';
      return !this.file.name.endsWith('.rcd.tar.gz') || !this.file.size
        || this.file.size > 256 * 1024 * 1024
        ? '请选择不超过 256 MiB 的 .rcd.tar.gz 发布包。' : '';
    },
    componentChoices() {
      const known = new Set(this.components.map((item) => item.value));
      return [...this.components, ...this.applications.filter((app) => !known.has(app.component))
        .map((app) => ({ text: app.name, value: app.component }))];
    },
    canRun() {
      return this.can(USER_PERMISSIONS.runProjectTasks);
    },
    endpoint() {
      return `/api/project/${this.projectId}/rcd/packages`;
    },
    canManage() {
      return this.can(USER_PERMISSIONS.manageProjectResources);
    },
  },
  watch: {
    file() { this.uploadError = ''; },
    projectId() {
      this.items = [];
      this.selected = null;
      this.detailSequence += 1;
      this.busy = false;
      this.uploadOpen = false;
      this.uploadSequence += 1;
      this.uploading = false;
      this.file = null; this.progress = 0; this.uploadError = '';
      this.toggleOpen = false;
      this.wizard = false;
      this.uploadedPackage = null;
      this.notice = '';
      this.definitionsOpen = false;
      this.applications = [];
      this.loadApplications();
      this.searchNow();
    },
    selected() {
      this.detailError = '';
    },
  },
  created() {
    this.previousTitle = document.title;
    document.title = '软件包 - Semaphore UI';
    this.load();
    this.loadApplications();
  },
  beforeDestroy() {
    document.title = this.previousTitle;
    this.sequence += 1;
    this.applicationSequence += 1;
    this.uploadSequence += 1;
    this.detailSequence += 1;
  },
  methods: {
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
          this.error = `${this.message(e)} 软件种类未刷新，内置组件仍可查看。`;
        }
      }
    },
    async definitionRegistered(app) {
      const project = this.projectId;
      await this.loadApplications();
      if (project !== this.projectId) return;
      this.notice = `已登记“${app.name}”，未上传版本包或安装任何主机。`;
      this.uploadedPackage = null;
    },
    uploadDefinedVersion(app) {
      this.component = app.component;
      this.openUpload();
    },
    openNavigation() {
      EventBus.$emit('i-show-drawer');
    },
    message(err) {
      const status = err.response && err.response.status;
      if (status === 401) return '登录已失效，请重新登录后再操作。';
      if (status === 403) return '当前账号没有此操作权限。';
      if (status === 409) return '发布编号内容冲突或记录已被修改。请刷新后重试，不要重复升级。';
      if (status === 413) return '软件包超过大小限制。';
      return (
        (err.response && err.response.data && err.response.data.error)
        || '请求未完成，请检查连接后刷新确认；不要重复提交发布。'
      );
    },
    componentName(value) {
      const option = this.componentChoices.find((item) => item.value === value);
      return option ? option.text : value;
    },
    size(value) {
      return `${(value / 1024 / 1024).toFixed(1)} MiB`;
    },
    date(value) {
      return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '—';
    },
    async load(requestedPage) {
      const page = Number.isInteger(requestedPage) && requestedPage > 0
        ? requestedPage : this.pendingPage;
      this.pendingPage = page;
      const sequence = this.sequence + 1;
      this.sequence = sequence;
      this.loading = true;
      this.listConfirmed = false;
      this.error = '';
      try {
        const { data } = await axios.get(this.endpoint, {
          params: {
            page,
            page_size: this.pageSize,
            q: this.search || '',
            component: this.component,
            enabled: this.enabled,
          },
        });
        if (sequence !== this.sequence) return;
        this.items = data.items;
        this.total = data.total;
        this.totalPages = data.total_pages;
        if (page > Math.max(1, this.totalPages)) {
          this.load(Math.max(1, this.totalPages));
          return;
        }
        this.page = page;
        this.listConfirmed = true;
      } catch (err) {
        if (sequence === this.sequence) this.error = this.message(err);
      } finally {
        if (sequence === this.sequence) this.loading = false;
      }
    },
    searchNow() {
      this.$nextTick(() => this.load(1));
    },
    async changePage(delta) {
      if (this.loading) return;
      await this.load(this.page + delta);
    },
    openUpload() {
      if (!this.canManage || this.uploading) return;
      this.file = null;
      this.uploadError = '';
      this.progress = 0;
      this.notice = '';
      this.uploadedPackage = null;
      this.uploadOpen = true;
    },
    dropFile(event) {
      if (this.uploading) return;
      if (event.dataTransfer.files.length !== 1) {
        this.file = null;
        this.$nextTick(() => { this.uploadError = '每次请选择一个软件包，不能同时上传多个文件。'; });
        return;
      }
      [this.file] = event.dataTransfer.files;
      this.uploadError = '';
    },
    async upload() {
      if (!this.canManage || !this.file || this.uploading) return;
      if (this.fileError) {
        this.uploadError = this.fileError;
        return;
      }
      this.uploading = true;
      this.uploadError = '';
      const endpoint = this.endpoint;
      this.uploadSequence += 1;
      const sequence = this.uploadSequence;
      const current = () => endpoint === this.endpoint && sequence === this.uploadSequence;
      try {
        const response = await axios.post(endpoint, this.file, {
          headers: { 'Content-Type': 'application/gzip' },
          onUploadProgress: (event) => {
            if (!current()) return;
            this.progress = event.total ? Math.round((100 * event.loaded) / event.total) : 0;
          },
        });
        if (!current()) return;
        this.uploadedPackage = response.data;
        this.uploadOpen = false;
        this.notice = response.status === 201
          ? '软件包已上传并通过校验。未触发升级。'
          : '此软件包已经存在，已返回原记录，未重复登记。';
        this.component = '';
        this.enabled = '';
        this.search = '';
        this.searchNow();
      } catch (err) {
        if (current()) this.uploadError = this.message(err);
      } finally {
        if (current()) this.uploading = false;
      }
    },
    closeDetails(value) {
      if (!value && !this.busy) {
        this.selected = null;
        this.toggleOpen = false;
      }
    },
    async refreshDetail() {
      if (!this.selected || this.busy) return;
      const item = this.selected;
      const endpoint = this.endpoint;
      this.detailSequence += 1;
      const sequence = this.detailSequence;
      const current = () => sequence === this.detailSequence && endpoint === this.endpoint
        && this.selected?.id === item.id;
      this.busy = true;
      this.detailError = '';
      try {
        const { data } = await axios.get(`${endpoint}/${item.id}`);
        if (current()) this.selected = data;
      } catch (err) {
        if (current()) {
          this.detailError = this.message(err);
        }
      } finally {
        if (current()) this.busy = false;
      }
    },
    async toggle() {
      if (!this.selected || this.busy) return;
      this.busy = true;
      this.detailError = '';
      const item = this.selected;
      const endpoint = this.endpoint;
      this.detailSequence += 1;
      const sequence = this.detailSequence;
      const current = () => sequence === this.detailSequence && endpoint === this.endpoint
        && this.selected?.id === item.id;
      try {
        const { data } = await axios.put(
          `${endpoint}/${item.id}/enabled`,
          { enabled: !item.enabled, revision: item.revision },
          { headers: { 'If-Match': `"${item.revision}"` } },
        );
        if (!current()) return;
        this.selected = data;
        this.toggleOpen = false;
        this.load();
      } catch (err) {
        if (current()) {
          this.toggleOpen = false;
          this.detailError = this.message(err);
        }
      } finally {
        if (current()) this.busy = false;
      }
    },
    async download() {
      if (!this.selected || this.busy) return;
      this.busy = true;
      this.detailError = '';
      const item = this.selected;
      const endpoint = this.endpoint;
      this.detailSequence += 1;
      const sequence = this.detailSequence;
      const current = () => sequence === this.detailSequence && endpoint === this.endpoint
        && this.selected?.id === item.id;
      try {
        const { data } = await axios.get(`${endpoint}/${item.id}/download`, {
          responseType: 'blob',
        });
        if (!current()) return;
        const url = URL.createObjectURL(data);
        const link = document.createElement('a');
        link.href = url;
        link.download = `${item.manifest.component}-${item.manifest.release_id}.rcd.tar.gz`;
        document.body.appendChild(link);
        link.click();
        link.remove();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      } catch (err) {
        if (!current()) return;
        // Axios retains responseType=blob for JSON errors as well.
        if (err.response?.data instanceof Blob) {
          try {
            err.response.data = JSON.parse(await err.response.data.text());
          } catch (_) {
            /* use safe fallback */
          }
        }
        if (current()) this.detailError = this.message(err);
      } finally {
        if (current()) this.busy = false;
      }
    },
  },
};
</script>

<style scoped>
.rcd-packages {
  padding: 32px;
  margin: 0 auto;
  max-width: 1540px;
  color: #202839;
}
.rcd-packages .v-btn--outlined {
  border-color: #ccd4e0;
  color: #324763;
}
.rcd-mobile-menu {
  display: none;
}
@media (max-width: 960px) {
  .rcd-mobile-menu {
    display: inline-flex;
    margin-bottom: 16px;
  }
}
.rcd-heading {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 24px;
  margin-bottom: 28px;
}
.rcd-heading h1 {
  font-size: 28px;
  line-height: 1.3;
  margin: 0 0 12px;
  font-weight: 650;
}
.rcd-heading p,
.rcd-muted {
  color: #6b7484;
}
.rcd-heading-actions { display: flex; flex-wrap: wrap; gap: 10px; }
.rcd-filters {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 24px;
  flex-wrap: wrap;
}
.rcd-filters > .v-input:first-child {
  flex: 2 1 240px;
}
.rcd-filters > .v-input {
  flex: 1 1 160px;
  max-width: 360px;
}
.rcd-table-wrap {
  border: 1px solid #dfe4ec;
  border-radius: 5px;
}
.rcd-table {
  width: 100%;
  border-collapse: collapse;
  table-layout: fixed;
  font-size: 14px;
}
.rcd-table th {
  background: #f5f6f8;
  text-align: left;
  font-weight: 600;
  padding: 16px;
}
.rcd-table th:first-child {
  width: 19%;
}
.rcd-table th:nth-child(2) {
  width: 16%;
}
.rcd-table th:nth-child(3) {
  width: 23%;
}
.rcd-table th:nth-child(4) {
  width: 10%;
}
.rcd-table th:nth-child(5) {
  width: 20%;
}
.rcd-table th:last-child {
  width: 12%;
}
.rcd-table td {
  padding: 18px 16px;
  border-top: 1px solid #e4e8ee;
  vertical-align: middle;
  overflow-wrap: anywhere;
}
.rcd-table td > span {
  display: block;
  margin-top: 5px;
}
.rcd-table td > .v-btn {
  margin: 4px 0;
}
.rcd-table .rcd-notes {
  white-space: pre-line;
}
.rcd-valid {
  color: #18704a;
}
.rcd-warning { color: #995b16; }
.rcd-empty {
  text-align: center;
  height: 180px;
}
.rcd-empty p {
  margin: 10px 0 0;
  color: #6b7484;
}
.rcd-pagination {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  margin: 24px 0;
  flex-wrap: wrap;
}
.rcd-pagination > div {
  display: flex;
  align-items: center;
  gap: 10px;
}
.rcd-pagination .v-input {
  max-width: 120px;
}
.rcd-footnote {
  border-top: 1px solid #e2e6ec;
  padding-top: 20px;
  color: #6b7484;
  font-size: 13px;
}
.rcd-dialog {
  padding: 8px;
}
.rcd-package-help {
  margin: 14px 0;
  padding: 12px;
  border: 1px solid #dce2ec;
  border-radius: 4px;
}
.rcd-package-help summary { cursor: pointer; color: #344e79; }
.rcd-package-help p { margin-top: 10px; }
.rcd-dropzone {
  background: #f8fafc;
  border: 1px dashed #9baabd;
  border-radius: 5px;
  padding: 24px;
  text-align: center;
}
.rcd-details {
  display: grid;
  grid-template-columns: 160px 1fr;
  gap: 14px;
  padding: 16px 0;
}
.rcd-details dt {
  color: #6b7484;
}
.rcd-details dd {
  margin: 0;
  overflow-wrap: anywhere;
}
.rcd-detail-notes {
  white-space: pre-wrap;
  margin-top: 16px;
}
@media (max-width: 1150px) {
  .rcd-packages {
    padding: 24px;
  }
  .rcd-table th:nth-child(3),
  .rcd-table td:nth-child(3) {
    display: none;
  }
  .rcd-table th:last-child {
    width: 17%;
  }
  .rcd-table th:nth-child(4) {
    width: 12%;
  }
}
@media (max-width: 700px) {
  .rcd-packages {
    padding: 16px;
  }
  .rcd-heading {
    flex-direction: column;
  }
  .rcd-table th:nth-child(2),
  .rcd-table td:nth-child(2),
  .rcd-table th:nth-child(5),
  .rcd-table td:nth-child(5) {
    display: none;
  }
  .rcd-table th:first-child {
    width: 43%;
  }
  .rcd-table th:nth-child(4) {
    width: 23%;
  }
  .rcd-table th:last-child {
    width: 34%;
  }
  .rcd-table td,
  .rcd-table th {
    padding: 14px 10px;
  }
  .rcd-details {
    grid-template-columns: 1fr;
  }
  .rcd-pagination > div {
    flex-wrap: wrap;
  }
}
</style>
