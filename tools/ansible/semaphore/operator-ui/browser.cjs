// All executable browser checks run on 46. Never submit a deployment or rollback.
const assert=require('node:assert/strict');
const fs=require('node:fs');
const {chromium}=require('/home/lenovo/code/rcd_manager_test/browser-tools/node_modules/playwright');
const base=process.env.RCD_UI_URL || 'http://127.0.0.1:13000';
const output='/home/lenovo/code/rcd_semaphore_ui_20260930/evidence';
(async()=>{
 fs.mkdirSync(output,{recursive:true});
 const browser=await chromium.launch({headless:true});
 try{
  const context=await browser.newContext({viewport:{width:1440,height:1100}});
  const page=await context.newPage(),errors=[],submitted=[];
  page.on('pageerror',e=>errors.push(e.message));
  await page.route('**/api/project/1/tasks',async route=>{
   if(route.request().method()!=='POST')return route.continue();
   const body=route.request().postDataJSON();
   if(![1,2,3].includes(body.template_id)){
    const env=JSON.parse(body.environment || '{}');
    assert.equal(env.execution,'preview');assert.equal(env.command_pause,'not_confirmed');
    assert.equal(body.template_id,6);assert.equal(env.component,'manager');
   }
   submitted.push(body);await route.continue();
  });
  const auth=await context.request.post(base+'/api/auth/login',{data:{auth:'admin',password:process.env.SEMAPHORE_ADMIN_PASSWORD}});
  assert.equal(auth.status(),204);
  await page.goto(base+'/project/1/templates');
  await page.locator('.ops-machines').waitFor();
  assert.equal(new URL(page.url()).pathname,'/project/1/operations');
  assert.equal(await page.locator('.ops-component').count(),9);
  assert.equal(await page.locator('.NavDrawer:visible').count(),0);
  const machine=page.getByRole('region',{name:'46 号机器'});
  await machine.locator('.ops-component').first().getByRole('button',{name:/升级软件/}).click();
  const dialog=page.locator('.v-dialog--active');
  assert(await dialog.getByLabel('仅检查条件，不重启').isChecked());
  await dialog.getByLabel('确认执行，会短暂中断所选程序').check();
  assert(await dialog.getByRole('button',{name:'确认执行',exact:true}).isDisabled());
  await dialog.getByLabel(/已暂停 30、46、58/).check();
  assert(await dialog.getByRole('button',{name:'确认执行',exact:true}).isEnabled());
  await dialog.getByRole('button',{name:'取消',exact:true}).click();
  if(process.env.RCD_UI_PREVIEW_TASK==='1'){
   await machine.locator('.ops-component').first().getByRole('button',{name:/升级软件/}).click();
   const request=page.waitForResponse(r=>r.url().endsWith('/api/project/1/tasks') && r.request().method()==='POST');
   await dialog.getByRole('button',{name:'开始预检',exact:true}).click();
   const response=await request;assert(response.ok(),`${response.status()} ${await response.text()}`);const task=await response.json();
   let state;
   for(let i=0;i<60;i++){
    state=await (await context.request.get(base+'/api/project/1/tasks/'+task.id)).json();
    if(['success','error','stopped'].includes(state.status))break;
    await page.waitForTimeout(1000);
   }
   assert.equal(state.status,'success');console.log('UI preview only',task.id,state.status);
   await page.getByRole('button',{name:'设备与版本',exact:true}).click();
  }
  // Reopening never preserves the prior authorization checkbox.
  await machine.locator('.ops-component').first().getByRole('button',{name:/升级软件/}).click();
  assert(await dialog.getByLabel('仅检查条件，不重启').isChecked());
  await dialog.getByRole('button',{name:'取消',exact:true}).click();
  if(process.env.RCD_UI_READONLY_TASKS==='1'){
   for(const host of ['30','46','58']){
    const request=page.waitForResponse(r=>r.url().endsWith('/api/project/1/tasks') && r.request().method()==='POST');
    await page.getByRole('region',{name:host+' 号机器'}).getByRole('button',{name:'检查状态',exact:true}).click();
    const response=await request;assert(response.ok(),`${response.status()} ${await response.text()}`);const task=await response.json();
    let state;
    for(let i=0;i<60;i++){
     state=await (await context.request.get(base+'/api/project/1/tasks/'+task.id)).json();
     if(['success','error','stopped'].includes(state.status))break;
     await page.waitForTimeout(1000);
    }
    assert.equal(state.status,'success');console.log('read-only inspection',host,task.id,state.status);
    await page.getByRole('button',{name:'刷新页面',exact:true}).click();
    await page.waitForTimeout(600);
   }
  }
  await page.waitForTimeout(700);
  if(process.env.RCD_UI_READONLY_TASKS==='1')assert.equal(await page.locator('.ops-version').filter({hasText:'0.6.1-full.20260928'}).count(),9);
  await page.screenshot({path:output+'/devices.png',fullPage:true});
  await machine.locator('.ops-component').first().getByRole('button',{name:/升级软件/}).click();
  await page.waitForTimeout(400);
  await page.screenshot({path:output+'/upgrade.png',fullPage:true});
  await dialog.getByRole('button',{name:'取消',exact:true}).click();
  await page.getByRole('button',{name:'操作记录',exact:true}).click();
  await page.getByText(/第 1 \/ \d+ 页/).waitFor();
  await page.screenshot({path:output+'/history.png',fullPage:true});
  await page.getByRole('button',{name:/查看 #/}).first().click();
  await page.locator('.v-dialog--active pre').waitFor();
  await page.locator('.v-dialog--active').getByRole('button',{name:'关闭',exact:true}).click();
  await page.getByRole('button',{name:'设备与版本',exact:true}).click();
  await page.route('**/api/project/1/templates',r=>r.fulfill({status:503,body:'unavailable'}));
  await page.getByRole('button',{name:'刷新页面',exact:true}).click();
  await page.getByRole('alert').filter({hasText:'维护信息读取失败'}).waitFor();
  assert.equal(await page.locator('.ops-component').count(),9);
  await page.unroute('**/api/project/1/templates');
  await page.getByRole('button',{name:'刷新页面',exact:true}).click();
  await page.waitForTimeout(400);
  await page.setViewportSize({width:390,height:844});
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth));
  await page.screenshot({path:output+'/mobile.png',fullPage:true});
  await page.setViewportSize({width:1440,height:1100});
  await page.getByRole('link',{name:/高级配置/}).click();
  await page.waitForURL('**/project/1/templates?advanced=1');
  await page.locator('.NavDrawer:visible').waitFor();
  await page.getByRole('link',{name:'设备与软件',exact:true}).click();
  await page.locator('.ops-machines').waitFor();
  assert.deepEqual(errors,[]);
  console.log(JSON.stringify({passed:true,submitted_readonly_tasks:submitted.length,actual_release_requests:0,url:base}));
 }finally{await browser.close();}
})().catch(e=>{console.error(e);process.exitCode=1});
