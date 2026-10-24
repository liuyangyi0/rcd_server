<template>
  <!-- eslint-disable max-len -->
  <div class="rcd-operations">
    <header class="ops-bar"><router-link class="ops-brand" :to="base + '/operations'"><span class="ops-mark">R</span> RCD <span>软件维护</span></router-link>
      <nav aria-label="维护导航"><button :class="{selected:tab==='devices'}" @click="tab='devices'">设备与版本</button><button :class="{selected:tab==='history'}" @click="tab='history'">操作记录</button></nav>
      <router-link class="ops-advanced" :to="base + '/templates?advanced=1'">高级配置 ↗</router-link>
      <span class="ops-account">{{ user.name || user.username || '已登录' }}</span>
    </header>
    <main class="ops-content">
      <div class="ops-title"><div><span class="ops-eyebrow">RCD / OPERATIONS</span><h1>{{ tab==='devices'?'设备与软件':'操作记录' }}</h1><p>一处维护三台机器。业务点表、分组选源与计算规则请进入各自的 OPC 管理页面。</p></div><button class="ops-button" :disabled="loading" @click="refresh">{{ loading?'刷新中…':'刷新页面' }}</button></div>
      <div v-if="error" role="alert" class="ops-notice danger">{{ error }} <button @click="refresh">重试</button></div>
      <div v-if="message" role="status" class="ops-notice">{{ message }}</div>
      <p v-if="loading && !templates.length" role="status">正在读取设备维护信息…</p>
      <template v-if="tab==='devices'">
        <div class="ops-summary"><div><strong>03</strong><span>现场机器</span></div><div><strong>09</strong><span>计划管理组件</span></div><div><strong>01</strong><span>部署平台 · 位于 46</span></div><p>版本与进程状态来自只读检查，<br>不是实时设备通信状态。</p></div>
        <div class="ops-machines">
          <section v-for="host in hosts" :key="host" class="ops-machine" :aria-label="host + ' 号机器'">
            <div class="ops-machine-head"><div><span class="ops-node">{{ host }}</span><h2>{{ host }} 号机器</h2><span class="ops-address">192.168.10.{{ host }}</span></div><button class="ops-button" :disabled="!find(host,'inspect') || busyHost(host) || sending" @click="check(host)">{{ busyHost(host)?'任务处理中':'检查状态' }}</button></div>
            <p class="ops-checked">最近检查：{{ date(checked[host]) }}<span v-if="inspectErrors[host]" class="ops-error">{{ inspectErrors[host] }}</span></p>
            <div v-for="(label,role) in roles" :key="role" class="ops-component">
              <div class="ops-row"><h3>{{ label }}</h3><span class="ops-status" :class="observed(host,role).tone">{{ observed(host,role).text }}</span></div>
              <div class="ops-version">{{ version(host,role) }}</div>
              <p class="ops-release">{{ release(host,role) }}</p>
              <div class="ops-actions"><button class="ops-link" :disabled="!supports(host,role) || busyHost(host) || sending" @click="open(host,role,'deploy')">升级软件 <span aria-hidden="true">↗</span></button><button class="ops-link ops-secondary" @click="details(host,role)">详情与回退</button></div>
            </div>
            <a class="ops-business" :href="'http://192.168.10.'+host+':8087'" target="_blank" rel="noopener">进入 OPC 管理页面 ↗</a>
          </section>
        </div>
        <p class="ops-footnote">“检查时运行”仅代表检查瞬间的进程状态。超过 5 分钟显示为历史快照；升级任务完成不会自动把组件标记为在线。</p>
      </template>
      <section v-else class="ops-history">
        <div class="ops-row"><h2>最近任务</h2><label for="ops-host-filter">机器 <select id="ops-host-filter" v-model="hostFilter" @change="page=1"><option value="">全部</option><option v-for="h in hosts" :key="h">{{ h }}</option></select></label></div>
        <p class="ops-muted">显示最近 {{ tasks.length }} 条任务；“预检完成”不代表已升级。完整历史保留在高级记录中。</p>
        <div class="ops-table-wrap"><table><thead><tr><th>时间</th><th>目标</th><th>操作</th><th>任务结果</th><th>记录</th></tr></thead><tbody>
          <tr v-for="task in pageTasks" :key="task.id"><td>{{ date(task.created) }}</td><td>{{ taskHost(task) || '—' }} / {{ roles[env(task).component] || '整机' }}</td><td>{{ taskKind(task) }}</td><td><span class="ops-status" :class="task.status==='error'?'warn':'muted'">{{ statusText(task) }}</span></td><td><button class="ops-link" @click="showTask(task)">查看 #{{ task.id }}</button></td></tr>
          <tr v-if="!filteredTasks.length"><td colspan="5">暂无匹配记录</td></tr>
        </tbody></table></div>
        <div class="ops-pagination"><span>共 {{ filteredTasks.length }} 条 · 第 {{ page }} / {{ pages }} 页</span><button class="ops-button" :disabled="page<=1" @click="page--">上一页</button><button class="ops-button" :disabled="page>=pages" @click="page++">下一页</button><router-link :to="base+'/history?advanced=1'">完整历史 ↗</router-link></div>
      </section>
    </main>
    <v-dialog v-model="dialog" max-width="620" :persistent="sending" @input="dialogChanged">
      <section v-if="selection" class="ops-dialog" aria-label="软件维护操作">
        <div class="ops-row"><h2>{{ selection.action==='details'?'组件详情':selection.action==='rollback'?'版本回退':'软件升级' }}</h2><button class="ops-button" :disabled="sending" @click="dialog=false">关闭</button></div>
        <p>{{ selection.host }} 号机器 · 192.168.10.{{ selection.host }} · {{ roles[selection.role] }}</p>
        <template v-if="selection.action==='details'">
          <dl><dt>运行版本（最近检查）</dt><dd>{{ version(selection.host,selection.role) }}</dd><dt>磁盘程序版本</dt><dd>{{ (row(selection.host,selection.role).entry_build || {}).version || '未获取' }}</dd><dt>最近检查</dt><dd>{{ date(checked[selection.host]) }}</dd><dt>状态</dt><dd>{{ observed(selection.host,selection.role).text }}</dd></dl>
          <p class="ops-notice">回退只替换软件，保留当前配置和数据。数据库不兼容时后台会拒绝回退。</p>
          <button class="ops-button" :disabled="!find(selection.host,'rollback') || busyHost(selection.host)" @click="open(selection.host,selection.role,'rollback')">选择备份回退</button>
        </template>
        <form v-else @submit.prevent="submit">
          <label v-if="selection.action==='deploy'" class="ops-field" for="ops-release">目标软件包<select id="ops-release" v-model="chosenRelease" required><option disabled value="">请选择软件包</option><option v-for="v in releases" :key="v.value" :value="v.value">{{ v.value }}</option></select></label>
          <label v-else class="ops-field" for="ops-backup">备份路径<input id="ops-backup" v-model.trim="backup" required placeholder="从该组件原升级记录复制 recovery_backup 路径"><small>只允许本机该组件的备份；不会恢复覆盖业务数据库。</small></label>
          <fieldset><legend>执行方式</legend><label for="ops-preview"><input id="ops-preview" type="radio" v-model="mode" value="preview"> 仅检查条件，不重启</label><label for="ops-execute"><input id="ops-execute" type="radio" v-model="mode" value="execute"> 确认执行，会短暂中断所选程序</label></fieldset>
          <p class="ops-notice">{{ selection.role==='agent'?'升级 Agent 会同时重启这台机器的下级采集程序。':'操作仅作用于所选机器和组件。' }} 后台自动执行备份、替换与验收。</p>
          <label v-if="mode==='execute'" class="ops-confirm" for="ops-paused"><input id="ops-paused" type="checkbox" v-model="paused"> 已暂停 30、46、58 管理端及所有直连客户端的控制命令。</label>
          <p v-if="formError" role="alert" class="ops-error">{{ formError }}</p>
          <p v-if="uncertain" class="ops-notice danger">请求结果不明确，请先查看操作记录确认；本页面不会自动重发。</p>
          <div class="ops-actions end"><button type="button" class="ops-button" @click="dialog=false" :disabled="sending">取消</button><button class="ops-button ops-primary" type="submit" :disabled="sending || uncertain || (mode==='execute' && !paused)">{{ sending?'提交中…':mode==='execute'?'确认执行':'开始预检' }}</button></div>
        </form>
      </section>
    </v-dialog>
    <v-dialog v-model="logDialog" max-width="980"><section class="ops-dialog" aria-label="任务详情"><div class="ops-row"><h2>任务 #{{ logTask && logTask.id }} · {{ logTask ? taskKind(logTask) : '' }}</h2><button class="ops-button" @click="logDialog=false">关闭</button></div><p>{{ logTask ? statusText(logTask) : '' }} · {{ date(logTask && logTask.created) }}</p><p>任务成功仅代表所选步骤完成。预检不替换程序，运行状态请重新检查。</p><pre tabindex="0">{{ logText || '正在加载…' }}</pre></section></v-dialog>
  </div>
</template>

<script>
/* eslint-disable */
import axios from 'axios';
import {roles,hosts,active,environment,taskKind,statusText,templateFor,inspection,observation,cleanOutput,makeTask} from './model.mjs';
export default {
  props: {projectId:Number,user:{type:Object,default:()=>({})}},
  data:()=>({roles,hosts,templates:[],tasks:[],snapshots:{},checked:{},inspectErrors:{},loaded:{},loading:false,error:'',message:'',tab:'devices',hostFilter:'',page:1,dialog:false,selection:null,chosenRelease:'',mode:'preview',paused:false,backup:'',sending:false,uncertain:false,formError:'',logDialog:false,logTask:null,logText:'',timer:null,clock:Date.now(),alive:true}),
  computed:{
    base(){return `/project/${this.projectId}`;},
    filteredTasks(){return this.tasks.filter(t=>!this.hostFilter || this.taskHost(t)===this.hostFilter);},
    pages(){return Math.max(1,Math.ceil(this.filteredTasks.length/10));},
    pageTasks(){return this.filteredTasks.slice((this.page-1)*10,this.page*10);},
    releases(){return (this.find(this.selection?.host,'deploy')?.survey_vars || []).find(v=>v.name==='release_id')?.values || [];},
  },
  async mounted(){await this.refresh();this.timer=setInterval(()=>{this.clock=Date.now();if(!document.hidden && !this.loading && !this.sending)this.refresh();},15000);},
  beforeDestroy(){this.alive=false;clearInterval(this.timer);},
  methods:{environment,env:environment,taskKind,statusText,
    date(v){return v?new Date(v).toLocaleString('zh-CN',{hour12:false}):'尚未检查';},
    find(h,a){return templateFor(this.templates,h,a);},
    row(h,r){return (this.snapshots[h] || []).find(p=>p.component===r) || {};},
    observed(h,r){if(this.inspectErrors[h])return {text:'检查异常 · 待确认',tone:'warn'};const row=this.row(h,r);return observation(row.component?row:null,this.checked[h],this.clock);},
    version(h,r){const p=this.row(h,r);return p.running_build?.version || (p.running===false?'未运行':'未获取版本');},
    release(h,r){return this.row(h,r).running_build?.release_id || '运行版本由状态检查确认';},
    taskHost(t){return (this.templates.find(v=>v.id===t.template_id)?.name || t.tpl_alias || '').match(/^(30|46|58)｜/)?.[1];},
    busyHost(h){return this.tasks.some(t=>this.taskHost(t)===h && active(t));},
    supports(h,r){return this.find(h,'deploy')?.survey_vars?.find(v=>v.name==='component')?.values?.some(v=>v.value===r);},
    async api(path){return (await axios.get(`api${this.base}${path}`,{timeout:15000})).data;},
    async refresh(){
      if(this.loading)return;this.loading=true;
      try{
        const [templates,tasks]=await Promise.all([this.api('/templates'),this.api('/tasks?count=100')]);
        if(!this.alive)return;
        if(!Array.isArray(templates) || !Array.isArray(tasks))throw Error('平台数据格式不兼容');
        this.templates=templates;this.tasks=tasks;this.page=Math.min(this.page,this.pages);this.error='';
        await Promise.all(hosts.map(async h=>{
          const template=this.find(h,'inspect'),task=template?.last_task;
          if(!task)return;
          if(active(task))return;
          if(task.status!=='success'){this.$set(this.inspectErrors,h,'最近检查失败，保留历史快照；查看记录。');return;}
          if(this.loaded[h]===task.id)return;
          try{const output=await this.api(`/tasks/${task.id}/output`);const rows=inspection(output);
            if(!rows.length)throw Error('没有可识别的版本快照');
            this.$set(this.snapshots,h,rows);this.$set(this.checked,h,task.end);this.$set(this.loaded,h,task.id);this.$set(this.inspectErrors,h,'');
          }catch(e){this.$set(this.inspectErrors,h,'快照读取失败，请重新检查或查看记录。');}
        }));
      }catch(e){this.error=e.response?.status===403?'当前账号无权读取此项目，请联系管理员。':'维护信息读取失败，已保留上次内容。';}
      finally{this.loading=false;}
    },
    details(host,role){this.selection={host,role,action:'details'};this.dialog=true;},
    open(host,role,action){this.selection={host,role,action};this.mode='preview';this.paused=false;this.backup='';this.formError='';this.uncertain=false;this.chosenRelease='';this.dialog=true;
      if(action==='deploy'){const v=this.find(host,'deploy')?.survey_vars?.find(x=>x.name==='release_id');this.chosenRelease=v?.default_value || '';}
    },
    dialogChanged(value){if(!value){this.paused=false;this.mode='preview';}},
    async send(body){return (await axios.post(`api${this.base}/tasks`,body,{timeout:20000})).data;},
    async check(host){
      if(this.sending || this.busyHost(host))return;
      this.sending=true;this.message='';
      try{const t=await this.send(makeTask(this.find(host,'inspect'),{host,action:'inspect'}));this.message=`已提交 ${host} 状态检查 #${t.id}，不升级、不重启。`;}
      catch(e){this.message='检查提交未确认，请先查看操作记录；未自动重发。';}
      finally{this.sending=false;await this.refresh();}
    },
    async submit(){
      if(this.sending || this.uncertain)return;this.formError='';
      let body;
      try{body=makeTask(this.find(this.selection.host,this.selection.action),{...this.selection,release:this.chosenRelease,execute:this.mode==='execute',paused:this.paused,backup:this.backup,actor:this.user.username || this.user.name});}
      catch(e){this.formError=e.message;return;}
      this.sending=true;
      try{const t=await this.send(body);this.dialog=false;this.message=`已提交任务 #${t.id}。${this.mode==='preview'?'本次仅预检，不替换程序。':'执行结果请查看记录。'}`;this.tab='history';await this.refresh();}
      catch(e){this.formError=e.response?.status===403?'没有执行权限。':e.response?.data?.error || '任务提交失败或结果不明确。';this.uncertain=!e.response || e.response.status>=500;}
      finally{this.sending=false;}
    },
    async showTask(t){this.logTask=t;this.logText='';this.logDialog=true;try{const out=await this.api(`/tasks/${t.id}/output`);this.logText=cleanOutput(out) || '暂无输出';}catch{this.logText='日志加载失败，请在高级记录中查看。';}},
  },
};
</script>
<style src="./operator.css"></style>
