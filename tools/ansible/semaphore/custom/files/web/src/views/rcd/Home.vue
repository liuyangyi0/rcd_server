<template>
  <section class="pa-8" aria-live="polite">
    <p v-if="busy" role="status">正在打开项目…</p>
    <template v-else-if="error">
      <v-alert type="error" text role="alert">{{ error }}</v-alert>
      <v-btn outlined :disabled="busy" @click="openProject">重新读取</v-btn>
      <v-btn v-if="loginExpired" text to="/auth/login">重新登录</v-btn>
      <p class="mt-4">重新读取只打开页面，不会创建安装、升级或启停任务。</p>
    </template>
  </section>
</template>
<script>
import axios from 'axios';

export default {
  data() {
    return {
      error: '', busy: false, loginExpired: false, requestRevision: 0,
    };
  },
  computed: {
    projectId() { return this.$route.params.projectId; },
  },
  watch: {
    projectId() { this.openProject(); },
  },
  created() { this.openProject(); },
  beforeDestroy() { this.requestRevision += 1; },
  methods: {
    async openProject() {
      this.requestRevision += 1;
      const revision = this.requestRevision;
      const projectId = this.projectId;
      this.error = ''; this.loginExpired = false; this.busy = true;
      try {
        const { data } = await axios.get('/api/info');
        if (revision !== this.requestRevision || projectId !== this.projectId) return;
        const suffix = data.rcd_packages ? 'rcd/devices' : 'history';
        await this.$router.replace(`/project/${projectId}/${suffix}`);
      } catch (e) {
        if (revision !== this.requestRevision || projectId !== this.projectId) return;
        this.loginExpired = e.response?.status === 401;
        this.error = this.loginExpired ? '登录已失效，请重新登录后打开项目。'
          : '平台信息读取失败。可重新读取；这不会重新提交任何任务。';
      } finally {
        if (revision === this.requestRevision && projectId === this.projectId) this.busy = false;
      }
    },
  },
};
</script>
