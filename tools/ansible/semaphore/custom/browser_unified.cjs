// Run on 46 only, against isolated native Semaphore. Native login/project;
// RCD operation/observation fixtures never execute a real task or hardware call.
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs'); const path = require('node:path'); const assert = require('node:assert/strict');
(async () => {
  const [state, output] = process.argv.slice(2);
  const config = JSON.parse(fs.readFileSync(path.join(state, 'browser-credentials.json')));
  assert.match(config.url, /^http:\/\/127\.0\.0\.1:\d+$/);
  fs.mkdirSync(output, {recursive:true});
  const browser = await chromium.launch({headless:true});
  const context = await browser.newContext({viewport:{width:1366,height:768},locale:'zh-CN'});
  const page = await context.newPage(); const errors=[]; const checks=[]; const calls=[];
  page.on('pageerror', e => errors.push(e.message));
  try {
    await page.goto(config.url+'/auth/login');
    await page.getByTestId('auth-username').fill(config.auth);
    await page.getByTestId('auth-password').fill(config.password);
    await Promise.all([page.waitForResponse(r => r.url().endsWith('/api/auth/login') && r.status()===204),page.getByTestId('auth-signin').click()]);
    const created = await context.request.post(config.url+'/api/projects',{data:{name:'统一运维隔离验收',alert:false,max_parallel_tasks:1},headers:{Origin:config.url}});
    assert.ok(created.ok(),await created.text()); const project=(await created.json()).id;
    const base=`/api/project/${project}/rcd`; const location=`/project/${project}/rcd`;
    const hash='a'.repeat(64); const now=new Date().toISOString();
    const devices=['30','58','46'].map(n => ({id:`d${n}`,name:`RCD ${n}`,address:`192.168.10.${n}`,host_alias:`rcd${n}`,revision:2,
      state:'checked',control_template_id:21,release_template_id:22,template_id:23,last_success_at:now,last_task_id:1,
      opc_endpoint:`opc.tcp://192.168.10.${n}:4841`,agent_url:`http://192.168.10.${n}:8090`,web_url:`http://192.168.10.${n}:8087`,
      agent_instance_id:`agent-${n}`,gateway_instance_id:`gateway-${n}`,manager_instance_id:`manager-${n}`,
      components:['lower','manager','agent'].map(component => ({component,state:component==='agent'?'stopped':'running',disk_sha256:hash,running_sha256:hash,
        disk_build:{version:'0.6.3-ui-fixture'},running_build:{version:'0.6.3-ui-fixture'},compatibility:{instance_id:`${component}-${n}`,gateway_instance_id:`gateway-${n}`}}))}));
    // A fixture-only pending inspection selects the normal three-second poll;
    // it creates no native task and avoids advancing the browser's clock.
    devices[0].pending_task_id=123;
    let operation; let loseReply=true; let hideSelectedDevice=false; let detailUnavailable=false;
    await page.route(`**${base}/**`,async r => {
      const request=r.request(); const u=new URL(request.url()); const method=request.method();
      let value;
      if(method==='POST'&&u.pathname===base+'/operations'){
        const body=request.postDataJSON(); const key=request.headers()['idempotency-key'];
        calls.push({body,key}); assert.equal(body.kind,'service_control');assert.equal(body.component,'manager');assert.equal(body.targets[0].device_id,'d46');assert.equal(body.package_id,undefined);
        operation={id:'original-op',request:body,package:{component:'',version:''},state:'unknown',revision:1,created_at:now,actor:'rcd-test',targets:[{device_id:'d46',name:'RCD 46',state:'unknown',stages:[]}],error:'隔离示例：结果待确认，不重复执行'};
        if(loseReply){loseReply=false;return r.fulfill({status:503,json:{error:'隔离示例：回复丢失'}});}
        return r.fulfill({json:operation});
      }
      if(method==='PUT'&&u.pathname.endsWith('/machine-binding')){
        const body=request.postDataJSON();const d=devices[2];assert.equal(body.revision,d.revision);assert.equal(body.agent_instance_id,d.agent_instance_id);
        Object.assign(d,body,{revision:d.revision+1});return r.fulfill({json:d});
      }
      assert.equal(method,'GET',`unexpected write ${u.pathname}`);
      if(u.pathname===base+'/devices') {
        const items=hideSelectedDevice?devices.filter(d=>d.id!=='d46'):devices;
        value={items,total:items.length,total_pages:1,page:1,page_size:20};
      }
      else if(u.pathname===base+'/operations')value={items:operation?[operation]:[],total:operation?1:0,total_pages:operation?1:0,page:1,page_size:20};
      else if(u.pathname.endsWith('/operations/original-op'))value=operation;
      else if(u.pathname===base+'/applications')value={items:[]};
      else if(u.pathname===base+'/packages')value={items:[],total:0,total_pages:0,page:1,page_size:20};
      else if(/\/devices\/d\d+$/.test(u.pathname)) {
        if(u.pathname.endsWith('/d46')&&detailUnavailable) {
          return r.fulfill({status:503,json:{error:'隔离示例：设备详情读取失败'}});
        }
        value=devices.find(d=>u.pathname.endsWith('/'+d.id));
      }
      else if(u.pathname.endsWith('/history'))value={items:[],total:0,total_pages:0,page:1,page_size:20};
      else throw Error('unhandled isolated route '+u.pathname);
      await r.fulfill({json:value});
    });
    for(const width of [1366,1920]){
      await page.setViewportSize({width,height:width===1366?768:1080});
      await page.goto(config.url+location+'/devices?device=d46');
      await page.getByRole('heading',{name:'设备与版本',exact:true}).waitFor();
      await page.locator('.details h2').filter({hasText:'RCD 46'}).waitFor();
      const section=page.locator('.component-detail').filter({has:page.getByRole('heading',{name:'OPC 管理端',exact:true})});
      await section.getByRole('button',{name:'重启',exact:true}).scrollIntoViewIfNeeded();
      assert.equal(await section.getByRole('button',{name:'启动',exact:true}).isDisabled(),true);
      assert.equal(await section.getByRole('button',{name:'重启',exact:true}).isEnabled(),true);
      assert.match(await section.getByRole('button',{name:'重启',exact:true}).getAttribute('title'),/排空/);
      assert.equal(await page.locator('.component-detail').filter({has:page.getByRole('heading',{name:'下级 OPC',exact:true})}).getByRole('button',{name:'停止',exact:true}).count(),0);
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),true);
      await page.screenshot({path:path.join(output,`manager-control-${width}.png`)});
    }
    checks.push('1366/1920 layout, explicit manager-only controls, tooltips and stopped-state restrictions');

    // Keep the drawer open while the next normal list response excludes d46,
    // as happens after filtering or paging. Only its independent detail GET
    // carries the changed version/freshness; no component state is injected.
    hideSelectedDevice=true;
    devices[2]={...devices[2],state:'stale',last_success_at:'2026-10-09T05:00:00Z',
      components:devices[2].components.map(c=>c.component==='manager'
        ? {...c,running_build:{...c.running_build,version:'0.6.3-refreshed-ui-fixture'}}:c)};
    await page.waitForResponse(async response=>{
      if(new URL(response.url()).pathname!==base+'/devices'||response.status()!==200)return false;
      const body=await response.json();return !body.items.some(d=>d.id==='d46');
    },{timeout:15000});
    const details=page.locator('.details');
    const detailManager=details.locator('.component-detail').filter({has:page.getByRole('heading',{name:'OPC 管理端',exact:true})});
    await details.getByText('0.6.3-refreshed-ui-fixture',{exact:true}).waitFor();
    await details.getByText('192.168.10.46 · 状态已过期',{exact:true}).waitFor();
    await page.locator('.table-wrap tbody tr').filter({hasText:'192.168.10.46'}).waitFor({state:'hidden'});
    assert.equal(await detailManager.getByRole('button',{name:'重启',exact:true}).isDisabled(),true);
    await page.screenshot({path:path.join(output,'detail-filtered-out-stale.png')});
    checks.push('open drawer independently refreshes version and stale status after selected device leaves list');

    // First obtain checked evidence, then fail only the detail request. The
    // stale list is not allowed to silently restore the old checked status.
    devices[2].state='checked';devices[2].last_success_at=new Date().toISOString();
    await details.getByRole('button',{name:'刷新详情',exact:true}).click();
    await details.getByText('192.168.10.46 · 检查成功',{exact:true}).waitFor();
    assert.equal(await detailManager.getByRole('button',{name:'重启',exact:true}).isEnabled(),true);
    detailUnavailable=true;
    await details.getByRole('button',{name:'刷新详情',exact:true}).click();
    const detailWarning=details.getByRole('alert').filter({hasText:'隔离示例：设备详情读取失败'});
    await detailWarning.waitFor();
    assert.match(await detailWarning.innerText(),/历史快照[\s\S]*不能确认设备当前状态/);
    assert.equal(await details.getByText('0.6.3-refreshed-ui-fixture',{exact:true}).count(),1);
    assert.equal(await detailManager.getByRole('button',{name:'重启',exact:true}).isDisabled(),true);
    assert.equal(await detailManager.getByRole('button',{name:'停止',exact:true}).isDisabled(),true);
    assert.equal(await page.getByRole('dialog').filter({hasText:'重启 OPC 管理端'}).count(),0);
    assert.equal(calls.length,0,'detail reads must not submit a service operation');
    await page.screenshot({path:path.join(output,'detail-unavailable-history.png')});
    detailUnavailable=false;
    await details.getByRole('button',{name:'刷新详情',exact:true}).click();
    await details.getByText('192.168.10.46 · 检查成功',{exact:true}).waitFor();
    assert.equal(await detailWarning.count(),0);
    assert.equal(await detailManager.getByRole('button',{name:'重启',exact:true}).isEnabled(),true);
    assert.equal(calls.length,0);
    devices[0].pending_task_id=0;
    checks.push('detail failure retains labelled history and blocks service controls; successful retry clears warning');

    await page.getByRole('button',{name:'管理机器绑定',exact:true}).click();
    const binding=page.getByRole('dialog').filter({hasText:'统一机器绑定'});
    await binding.getByLabel('机器显示名称').fill('RCD 46 · 隔离名称');
    assert.equal(await binding.getByLabel('Agent 稳定 ID（只读检查确认）').evaluate(el=>el.readOnly),true);
    await binding.getByRole('button',{name:'核对身份并保存',exact:true}).click();
    await page.getByRole('heading',{name:'RCD 46 · 隔离名称',exact:true}).waitFor();
    checks.push('verified machine-binding form preserves fixed identities');
    const manager=page.locator('.component-detail').filter({has:page.getByRole('heading',{name:'OPC 管理端',exact:true})});
    await manager.getByRole('button',{name:'重启',exact:true}).click();
    const dialog=page.getByRole('dialog').filter({hasText:'重启 OPC 管理端'});
    assert.equal(await dialog.getByRole('button',{name:'确认重启',exact:true}).isDisabled(),true);
    await dialog.getByText('已确认本次暂停相关控制命令，允许执行所列操作',{exact:true}).click();
    await dialog.getByRole('button',{name:'确认重启',exact:true}).click();
    await dialog.getByText('隔离示例：回复丢失',{exact:true}).waitFor();
    assert.equal(await dialog.getByRole('checkbox').isDisabled(),true);
    await page.screenshot({path:path.join(output,'lost-reply-original-operation.png')});
    await dialog.getByRole('button',{name:'确认原请求结果',exact:true}).click();
    await page.getByRole('heading',{name:'操作任务',exact:true}).waitFor();
    await page.getByText('隔离示例：结果待确认，不重复执行',{exact:true}).waitFor();
    assert.equal(calls.length,2);assert.deepEqual(calls[0],calls[1]);
    assert.equal(calls[0].body.targets[0].revision,3);
    checks.push('pause confirmation, lost reply immutable target/revision/idempotency, original task detail');
    await page.screenshot({path:path.join(output,'service-task-unknown.png')});
    operation={...operation,revision:2,state:'partial_failed',
      request:{kind:'',action:'upgrade',component:''},package:{component:'manager',version:'isolated-partial'},
      error:'部分完成：1/3 台成功；其余目标失败或未开始，后续目标不再执行',
      targets:devices.map((d,i)=>({device_id:d.id,name:d.name,address:d.address,
        state:['succeeded','failed_before_install','not_started'][i],stages:[]}))};
    for(const width of [1366,1920]){
      await page.setViewportSize({width,height:width===1366?768:1080});
      await page.getByRole('button',{name:'关闭详情',exact:true}).click();
      await page.getByRole('button',{name:'刷新列表',exact:true}).click();
      const partialRow=page.locator('.table-wrap tbody tr').filter({hasText:'部分完成（有失败）'});
      await partialRow.waitFor();
      await partialRow.getByRole('button',{name:'查看过程',exact:true}).click();
      const targets=page.locator('.target-detail');
      await targets.nth(0).getByText('软件升级成功',{exact:true}).waitFor();
      await targets.nth(1).getByText('安装前失败',{exact:true}).waitFor();
      await targets.nth(2).getByText('未开始',{exact:true}).waitFor();
      assert.equal(await page.getByRole('button',{name:'补查原事务',exact:true}).count(),0);
      assert.equal(calls.length,2,'rendering partial results must not resubmit operations');
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),true);
      await page.screenshot({path:path.join(output,`release-partial-${width}.png`)});
    }
    checks.push('partial deployment preserves all three host outcomes and renders without resubmission or horizontal overflow');
    assert.deepEqual(errors,[]);
    fs.writeFileSync(path.join(output,'result.json'),JSON.stringify({checks,errors,fixtureOperations:true,liveTasksExecuted:0},null,2));
    console.log(JSON.stringify({passed:checks.length,output}));
  } finally {await browser.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
