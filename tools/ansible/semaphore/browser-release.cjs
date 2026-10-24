// Controller-only UI verification. Submits only a guarded preview, never release.
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('/home/lenovo/code/rcd_manager_test/browser-tools/node_modules/playwright');

(async () => {
  const output = process.argv[2];
  if (!output || !path.isAbsolute(output) || !process.env.SEMAPHORE_ADMIN_PASSWORD) throw new Error('evidence path and credential required');
  fs.mkdirSync(output, { recursive: true });
  const browser = await chromium.launch({headless:true, args:['--no-sandbox']});
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1050}});
    await page.goto('http://192.168.10.46:3000', {waitUntil:'networkidle'});
    await page.locator('input[type=password]').waitFor();
    await page.locator('input:not([type=password]):not([type=hidden]):not([type=checkbox])').first().fill('admin');
    await page.locator('input[type=password]').fill(process.env.SEMAPHORE_ADMIN_PASSWORD);
    await page.locator('input[type=password]').press('Enter');
    await page.locator('input[type=password]').waitFor({state:'hidden'});
    await page.goto('http://192.168.10.46:3000/project/1/templates', {waitUntil:'networkidle'});
    await page.screenshot({path:path.join(output,'templates.png'),fullPage:true});
    await page.getByText('46｜软件升级', {exact:true}).click();
    await page.waitForLoadState('networkidle');
    await page.getByRole('button', {name:'运行',exact:true}).click();
    await page.waitForLoadState('networkidle');
    await page.screenshot({path:path.join(output,'upgrade-form.png'),fullPage:true});
    await page.getByText('目标组件', {exact:false}).first().waitFor();
    const form = await page.locator('body').innerText();
    if (!form.includes('软件包版本') || !form.includes('仅检查，不重启') || !form.includes('未确认')) throw new Error('upgrade form defaults missing');
    await page.screenshot({path:path.join(output,'upgrade-form.png'),fullPage:true});
    console.log('Upgrade form verified; task NOT submitted.');
    await page.route('**/api/project/1/tasks', async route => {
      if (route.request().method() !== 'POST') return route.continue();
      const body = route.request().postDataJSON();
      const values = JSON.parse(body.environment || '{}');
      if (body.template_id !== 6 || values.execution !== 'preview' || values.command_pause !== 'not_confirmed') {
        await route.abort();
        throw new Error('refused any task other than unconfirmed preview');
      }
      await route.continue();
    });
    const request = page.waitForResponse(r=>r.url().endsWith('/api/project/1/tasks') && r.request().method()==='POST', {timeout:15000});
    await page.getByRole('button', {name:'运行',exact:true}).last().click();
    const response = await request;
    if (!response.ok()) throw new Error('preview submission failed');
    const task = await response.json();
    let status;
    for (let i=0; i<60; i++) {
      const result = await page.request.get('http://192.168.10.46:3000/api/project/1/tasks/' + task.id);
      status = (await result.json()).status;
      if (['success','error','stopped'].includes(status)) break;
      await new Promise(resolve=>setTimeout(resolve,1000));
    }
    if (status !== 'success') throw new Error('browser preview task failed: ' + status);
    console.log(JSON.stringify({browser_preview_task:task.id,status}));
    await page.goto('http://192.168.10.46:3000/project/1/templates', {waitUntil:'networkidle'});
    await page.getByText('46｜版本回退', {exact:true}).click();
    await page.getByRole('button', {name:'运行',exact:true}).click();
    await page.getByText('备份路径', {exact:false}).first().waitFor();
    await page.screenshot({path:path.join(output,'rollback-form.png'),fullPage:true});
    console.log('Rollback form verified; task NOT submitted.');
  } finally { await browser.close(); }
})().catch(error=>{console.error(error.message);process.exitCode=1;});
