export const roles = { manager: 'OPC 管理端', lower: 'OPC 下级', agent: '进程 Agent' };
export const hosts = ['30', '46', '58'];
export const active = t => ['waiting', 'starting', 'running'].includes(t?.status);
export const statusText = t => ({success:'已完成',error:'失败',stopped:'已停止',waiting:'排队中',starting:'准备中',running:'执行中'}[t?.status] || '未知');
export function environment(task) {
  try { const e=JSON.parse(task.environment || '{}'); return e && typeof e==='object' ? e : {}; } catch { return {}; }
}
export function taskKind(task) {
  if(task.tpl_playbook==='tools/ansible/inspect.yml') return '状态检查';
  const e=environment(task);
  if(e.execution==='preview') return '升级 / 回退预检（未替换程序）';
  if(e.execution!=='execute') return '历史任务（执行方式未记录）';
  return (task.tpl_alias || '').includes('回退') ? '版本回退' : '软件升级';
}
export function templateFor(templates, host, action) {
  const matches=templates.filter(t=> {
    if(!t.name.startsWith(`${host}｜`) || t.app!=='ansible') return false;
    if(action==='inspect') return t.playbook==='tools/ansible/inspect.yml';
    if(t.playbook!=='tools/ansible/semaphore-release.yml') return false;
    try { const a=JSON.parse(t.arguments); const i=a.indexOf('--extra-vars');
      return JSON.parse(a[i+1]).rcd_workflow===action && a.includes(`rcd${host}`);
    } catch { return false; }
  });
  return matches.length===1 ? matches[0] : null;
}
export function cleanOutput(output) {
  return output.slice(0,20000).map(x=>String(x.output || '').replace(/\x1b\[[0-9;]*m/g,'')).join('\n');
}
// Ansible debug renders one JSON envelope per result; accept only our inspector schema.
export function inspection(output) {
  const text=cleanOutput(output), results=[];
  for(let i=0;i<text.length;i+=1) {
    if(text[i]!=='{') continue;
    let depth=0, quoted=false, escaped=false;
    for(let j=i;j<text.length;j+=1) {
      const c=text[j];
      if(quoted) { if(escaped) escaped=false; else if(c==='\\') escaped=true; else if(c==='"') quoted=false; }
      else if(c==='"') quoted=true;
      else if(c==='{') depth+=1;
      else if(c==='}' && --depth===0) {
        try { const value=JSON.parse(text.slice(i,j+1)); const msg=value.msg;
          if(msg?.schema_version===1 && Array.isArray(msg.processes)) results.push(msg);
        } catch { /* Non-JSON log text never becomes a status. */ }
        i=j; break;
      }
    }
  }
  return results.flatMap(result=>result.processes.map(p=>({
    ...p, component:({'/opt/rcd/bin/RcdManager':'manager','/opt/rcd/bin/RcdAgent':'agent','/opt/rcd/bin/rcd_server':'lower'})[p.entry],
    service:result.service, unit:result.unit,
  })));
}
export function observation(row, checked, now=Date.now()) {
  if(!row) return {text:'尚未检查',tone:'muted'};
  if(!checked || !Number.isFinite(Date.parse(checked)) || now-Date.parse(checked)>5*60*1000)
    return {text:'历史快照 · 待检查',tone:'muted'};
  if(row.restart_required_for_entry) return {text:'磁盘与运行版本不同',tone:'warn'};
  if(row.running===true) return {text:'检查时运行',tone:'good'};
  if(row.running!==false) return {text:'无法确认运行状态',tone:'warn'};
  return {text:'检查时未运行',tone:'warn'};
}
export function makeTask(template, {host,role,action,release,execute=false,paused=false,backup='',actor=''}) {
  if(!hosts.includes(host) || !template) throw Error('目标模板不可用，请联系维护人员');
  if(action==='inspect') {
    if(template.playbook!=='tools/ansible/inspect.yml') throw Error('不是状态检查模板');
    return {template_id:template.id,message:`状态检查 · ${host} · 不重启`,params:{dry_run:false}};
  }
  if(!['deploy','rollback'].includes(action) || !roles[role]) throw Error('操作或组件无效');
  const values=template.survey_vars || [];
  if(!values.find(v=>v.name==='component')?.values?.some(v=>v.value===role)) throw Error('目标没有该组件');
  if(action==='deploy' && !values.find(v=>v.name==='release_id')?.values?.some(v=>v.value===release)) throw Error('请选择已登记的软件包');
  if(action==='rollback' && !new RegExp(`^/var/lib/rcd-release/${role}/artifacts/[A-Za-z0-9][A-Za-z0-9_.-]*/backup-[a-f0-9]{32}$`).test(backup)) throw Error('请填写该组件升级记录中的完整备份路径');
  if(execute && (!paused || !actor)) throw Error('执行前必须确认所有控制命令已暂停');
  return {template_id:template.id, message:`${execute?'执行':'仅预检'} · ${host} · ${roles[role]}`,
    environment:JSON.stringify({component:role,release_id:release,backup_ref:backup,
      execution:execute?'execute':'preview',command_pause:execute?'confirmed':'not_confirmed',
      initiator:actor,restore_data:'keep'}),params:{dry_run:false}};
}
