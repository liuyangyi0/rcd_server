<template>
  <section class="component-history">
    <v-btn small outlined :loading="loading" @click="load">版本历史</v-btn>
    <p v-if="error" class="rcd-warning" role="alert">{{ error }}</p>
    <p v-if="error && items.length" class="rcd-warning" role="status">
      第 {{ pendingPage }} 页未能确认，以下保留上次读取的第 {{ page }} 页；请重试，不能据此恢复。
    </p>
    <template v-if="opened">
      <p class="muted">兼容性依据最近一次检查，安装前再次核对；恢复保留当前配置和数据。</p>
      <p v-if="!items.length && !loading && !error" class="muted">
        没有此组件的成功安装记录。未校验的旧文件不会伪装成可恢复版本。
      </p>
      <div v-for="row in items" :key="row.operation_id" class="history-row">
        <div>
          <strong>{{ row.package.manifest ? row.package.manifest.version : '包已不可用' }}</strong
          ><small>{{ new Date(row.installed_at).toLocaleString('zh-CN', { hour12: false }) }}</small
          ><small v-if="historyNote(row)">{{ historyNote(row) }}</small
          ><small v-if="restoreReason(row)" class="rcd-warning">{{
            restoreReason(row)
          }}</small>
        </div>
        <v-btn
          v-if="canRun"
          small
          outlined
          :disabled="loading || !!error || !!restoreReason(row)"
          :title="restoreReason(row) || '以新任务恢复此版本，保留当前配置和数据'"
          @click="restore(row.package)"
          >恢复此版本</v-btn
        >
      </div>
      <footer>
        <span>共 {{ total }} 条安装记录 · 第 {{ totalPages ? page : 0 }} / {{ totalPages }} 页</span>
        <v-select
          v-model="pageSize"
          :items="[20, 50, 100]"
          label="每页条数"
          dense
          outlined
          hide-details
          @change="load(1)"
        />
        <v-btn
          small
          text
          :disabled="page <= 1 || loading"
          @click="load(page - 1)"
          >上一页</v-btn
        >
        <v-btn
          small
          text
          :disabled="page >= totalPages || loading"
          @click="load(page + 1)"
          >下一页</v-btn
        >
      </footer>
    </template>
    <ReleaseWizard
      v-model="wizard"
      :project-id="projectId"
      :initial-device-ids="[device.id]"
      :initial-package="chosen"
      action="restore"
    />
  </section>
</template>
<script>
import axios from 'axios';
import PermissionsCheck from '@/components/PermissionsCheck';
import { USER_PERMISSIONS } from '@/lib/constants';
import ReleaseWizard from './ReleaseWizard.vue';

export default {
  components: { ReleaseWizard },
  mixins: [PermissionsCheck],
  props: { projectId: Number, device: Object, component: String },
  data() {
    return {
      items: [],
      loading: false,
      opened: false,
      error: '',
      chosen: null,
      wizard: false,
      total: 0,
      page: 1,
      pendingPage: 1,
      pageSize: 20,
      totalPages: 0,
      sequence: 0,
    };
  },
  computed: {
    canRun() {
      return this.can(USER_PERMISSIONS.runProjectTasks);
    },
    contextKey() {
      return `${this.projectId}:${this.device.id}:${this.component}`;
    },
  },
  watch: {
    contextKey() {
      this.sequence += 1;
      this.items = [];
      this.total = 0;
      this.totalPages = 0;
      this.page = 1;
      this.pendingPage = 1;
      this.opened = false;
      this.loading = false;
      this.error = '';
      this.wizard = false;
      this.chosen = null;
    },
  },
  beforeDestroy() {
    this.sequence += 1;
  },
  methods: {
    historyNote(row) {
      return row.installation_kind === 'first_install_verified_start'
        ? '首次安装后启动确认；上述时间为启动验收时间。' : '';
    },
    async load(requestedPage) {
      const page = Number.isInteger(requestedPage) && requestedPage > 0
        ? requestedPage : this.pendingPage;
      this.pendingPage = page;
      this.sequence += 1;
      const sequence = this.sequence;
      this.loading = true;
      this.opened = true;
      this.error = '';
      try {
        const { data } = await axios.get(
          `/api/project/${this.projectId}/rcd/devices/${this.device.id}/components/${this.component}/history`,
          { params: { page, page_size: this.pageSize }, timeout: 15000 },
        );
        if (sequence !== this.sequence) return;
        this.items = data.items;
        this.total = data.total;
        this.totalPages = data.total_pages;
        if (page > Math.max(1, this.totalPages)) {
          this.load(Math.max(1, this.totalPages));
          return;
        }
        this.page = page;
      } catch (e) {
        if (sequence === this.sequence) {
          this.error = e.response?.status === 401
            ? '登录已失效，请重新登录。'
            : e.response?.data?.error || '历史记录读取失败，请重试。';
        }
      } finally {
        if (sequence === this.sequence) this.loading = false;
      }
    },
    restore(p) {
      if (this.loading || this.error) return;
      this.chosen = p;
      this.wizard = true;
    },
    restoreReason(row) {
      if (row.restore_disabled_reason) return row.restore_disabled_reason;
      if (!row.package?.manifest) return '软件包不可用，不能恢复';
      const current = (this.device.components || []).find((c) => c.component === this.component);
      if (this.device.state === 'checked' && current) {
        let image = '';
        if (current.state === 'running') image = current.running_sha256;
        if (current.state === 'stopped') image = current.disk_sha256;
        if (image && image === row.package.manifest.binary_sha256) return '已是此程序版本，无需恢复';
      }
      return '';
    },
  },
};
</script>
<style scoped>
.component-history {
  margin-top: 16px;
}
.history-row {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  padding: 14px 0;
  border-bottom: 1px solid #e8ebf0;
}
.history-row small {
  display: block;
  color: #6b788c;
}
.muted {
  color: #6b788c;
}
.rcd-warning {
  color: #995b16;
}
.v-btn {
  text-transform: none;
  letter-spacing: 0;
}
footer {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  margin-top: 14px;
}
footer .v-select {
  max-width: 110px;
}
</style>
