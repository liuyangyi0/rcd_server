// Run on 46 only. No deployment, rollback, inspection or device commands submitted.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const {chromium} = require('/home/lenovo/code/rcd_manager_test/browser-tools/node_modules/playwright');
const base = 'http://192.168.10.46:3000';
const output = '/home/lenovo/code/rcd_semaphore_ui_20260930/native-evidence';
(async () => {
  fs.mkdirSync(output, {recursive:true});
  const browser = await chromium.launch({headless:true});
  try {
    const context = await browser.newContext({viewport:{width:1500,height:960}});
    const auth = await context.request.post(base+'/api/auth/login', {data:{auth:'admin',password:process.env.SEMAPHORE_ADMIN_PASSWORD}});
    assert.equal(auth.status(),204);
    const page = await context.newPage(), errors=[];
    page.on('pageerror',e=>errors.push(e.message));
    await page.route('**/api/project/1/tasks',async route=>{
      assert.notEqual(route.request().method(),'POST','No tasks may be submitted by this test');
      await route.continue();
    });
    const views = await (await context.request.get(base+'/api/project/1/views')).json();
    const names=['状态检查','软件升级','版本回退'];
    assert.deepEqual([...views].sort((a,b)=>a.position-b.position).slice(0,3).map(v=>v.title),names);
    for (const name of names) {
      const view=views.find(v=>v.title===name);
      const templates=await (await context.request.get(base+`/api/project/1/views/${view.id}/templates`)).json();
      assert.equal(templates.length,3);
      await page.goto(base+`/project/1/views/${view.id}/templates`,{waitUntil:'networkidle'});
      assert.equal(await page.locator('.ops-machines').count(),0);
      for(const template of templates)await page.getByText(template.name,{exact:true}).waitFor();
      await page.screenshot({path:output+'/'+view.id+'.png',fullPage:true});
    }
    for(const [id,name,field] of [[6,'46｜软件升级','软件包版本'],[7,'46｜版本回退','备份路径']]) {
      const template=await (await context.request.get(base+'/api/project/1/templates/'+id)).json();
      const vars=template.survey_vars;
      assert.equal(vars.find(v=>v.name==='execution').default_value,'preview');
      assert.equal(vars.find(v=>v.name==='command_pause').default_value,'not_confirmed');
      assert.match(vars.find(v=>v.name==='command_pause').values.find(v=>v.value==='confirmed').name,/30、46、58/);
      await page.goto(base+`/project/1/views/${template.view_id}/templates`,{waitUntil:'networkidle'});
      await page.getByText(name,{exact:true}).click();
      await page.getByRole('button',{name:'运行',exact:true}).click();
      await page.getByText(field,{exact:false}).first().waitFor();
      await page.getByText('仅检查，不重启',{exact:true}).waitFor();
      await page.getByText('未确认',{exact:true}).waitFor();
      await page.screenshot({path:output+'/'+id+'-form.png',fullPage:true});
    }
    // Native per-browser preferences; these do NOT change other users' browsers.
    const first=views.find(v=>v.title===names[0]);
    await page.goto(base+`/project/1/views/${first.id}/templates`,{waitUntil:'networkidle'});
    await page.evaluate(()=>{
      const columns={};
      for(const key of ['name','actions','status','last_task'])columns[key]={visible:true};
      for(const key of ['version','playbook','inventory_id','environment_ids','repository_id'])columns[key]={visible:false};
      localStorage.setItem('project__template__settings',JSON.stringify({columns}));
    });
    await page.reload({waitUntil:'networkidle'});
    const headers=await page.locator('.templates-table thead').innerText();
    assert(!headers.includes('版本')&&!headers.includes('剧本')&&!headers.includes('库存'));
    await page.screenshot({path:output+'/native-columns-example.png',fullPage:true});
    const tasks=await (await context.request.get(base+'/api/project/1/tasks?count=200')).json();
    assert.equal(tasks.length,21);
    assert.deepEqual(errors,[]);
    console.log(JSON.stringify({passed:true,groups:3,templates:9,tasks_preserved:21,task_submissions:0,column_preferences:'test browser only'}));
  } finally {await browser.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
