import {test} from 'node:test';
import assert from 'node:assert/strict';
import {inspection,observation,taskKind,templateFor,makeTask} from './model.mjs';
const tpl={id:6,name:'46｜软件升级',app:'ansible',playbook:'tools/ansible/semaphore-release.yml',arguments:JSON.stringify(['--limit','rcd46','--extra-vars','{"rcd_workflow":"deploy"}']),survey_vars:[{name:'component',values:[{value:'lower'}]},{name:'release_id',values:[{value:'r1'}]}]};
test('preview success is not deployment success',()=>assert.match(taskKind({environment:'{"execution":"preview"}',status:'success'}),/未替换/));
test('unknown execution must not imply upgrade',()=>assert.match(taskKind({status:'success'}),/未记录/));
test('stale running sample is not online',()=>assert.equal(observation({running:true},'2026-01-01',Date.parse('2026-09-30')).tone,'muted'));
test('failed/missing inspection never means zero-version or running',()=>assert.equal(observation(null,new Date().toISOString()).text,'尚未检查'));
test('actual runtime and on-disk identities are separate',()=>{
 const data={schema_version:1,service:{ActiveState:'active'},processes:[{entry:'/opt/rcd/bin/rcd_server',entry_build:{component:'lower',version:'new'},running_build:{version:'old'},running:true,restart_required_for_entry:true}]};
 const rows=inspection([{output:'ok: [host] => '+JSON.stringify({msg:data})}]);
 assert.equal(rows[0].running_build.version,'old');assert.equal(observation(rows[0],new Date().toISOString()).tone,'warn');
});
test('malformed logs cannot fabricate status',()=>assert.deepEqual(inspection([{output:'{"msg":"success"}\n{"msg":{"processes":[]}}'}]),[]));
test('missing build identity must not hide running component',()=>{
 const rows=inspection([{output:JSON.stringify({msg:{schema_version:1,processes:[{entry:'/opt/rcd/bin/RcdManager',running:true,entry_build:null,running_build:null}]}})}]);
 assert.equal(rows[0].component,'manager');assert.equal(observation(rows[0],new Date().toISOString()).text,'检查时运行');
 assert.equal(observation({running:null},new Date().toISOString()).text,'无法确认运行状态');
});
test('template mapping refuses duplicate and wrong host',()=>{
 assert.equal(templateFor([tpl],'46','deploy').id,6);assert.equal(templateFor([tpl,tpl],'46','deploy'),null);assert.equal(templateFor([tpl],'30','deploy'),null);
});
test('preview is safe default; explicit pause required for actual task',()=>{
 const input={host:'46',role:'lower',action:'deploy',release:'r1'};
 assert.equal(JSON.parse(makeTask(tpl,input).environment).execution,'preview');
 assert.throws(()=>makeTask(tpl,{...input,execute:true}),/暂停/);
 assert.throws(()=>makeTask(tpl,{...input,release:'arbitrary'}),/登记/);
 assert.throws(()=>makeTask(tpl,{...input,role:'manager'}),/没有/);
 assert.equal(JSON.parse(makeTask(tpl,{...input,execute:true,paused:true,actor:'admin'}).environment).command_pause,'confirmed');
});
