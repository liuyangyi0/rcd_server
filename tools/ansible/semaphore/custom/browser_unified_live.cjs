// Authorized live acceptance, executed on remote 46 only. No fixtures/mocks.
// One explicit manager restart and one explicit lower restart, no hardware commands.
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');

(async () => {
  const [output, confirmation] = process.argv.slice(2);
  assert.equal(confirmation, '--confirm-paused');
  assert.ok(output && !fs.existsSync(output), 'Use a fresh evidence directory; never replay a failed run');
  let input = '';
  for await (const chunk of process.stdin) input += chunk;
  const credentials = JSON.parse(input); input = '';
  fs.mkdirSync(output, { recursive: true, mode: 0o700 });
  const save = (name, value) => fs.writeFileSync(path.join(output, name), JSON.stringify(value, null, 2), { mode: 0o600 });
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1366, height: 768 }, locale: 'zh-CN' });
  const page = await context.newPage(); const errors = []; const writes = [];
  context.on('page', p => p.on('pageerror', e => errors.push(e.message)));
  page.on('pageerror', e => errors.push(e.message));
  page.on('request', r => {
    if (r.method() !== 'GET' && !r.url().includes('/auth/')) {
      writes.push({ method: r.method(), url: r.url(), body: r.postDataJSON() });
      save('requests.json', writes);
    }
  });
  const platform = 'http://192.168.10.46:3000';
  const manager = 'http://192.168.10.46:8087';
  async function get(url) {
    const r = await context.request.get(url);
    assert.ok(r.ok(), `GET ${url}: ${r.status()}`);
    return r.json();
  }
  async function poll(url, terminal) {
    const end = Date.now() + 150000;
    let result;
    do {
      result = await get(url);
      if (terminal(result.state)) return result;
      await new Promise(resolve => setTimeout(resolve, 1500));
    } while (Date.now() < end);
    throw Error('Original operation still pending; inspect retained receipt, do not repeat');
  }
  try {
    await page.goto(platform + '/auth/login');
    await page.getByTestId('auth-username').fill('admin');
    await page.getByTestId('auth-password').fill(credentials.platform);
    await Promise.all([page.waitForResponse(r => r.url().endsWith('/api/auth/login') && r.status() === 204), page.getByTestId('auth-signin').click()]);
    const devices = (await get(platform + '/api/project/1/rcd/devices?page_size=100')).items;
    const target = devices.find(d => d.address === '192.168.10.30');
    assert.ok(target && !target.active_operation_id && target.state === 'checked');
    assert.equal(target.components.find(c => c.component === 'manager').state, 'running');
    save('devices-before.json', devices);
    for (const width of [1366, 1920]) {
      await page.setViewportSize({ width, height: width === 1366 ? 768 : 1080 });
      await page.goto(platform + '/project/1/rcd/devices?device=' + target.id);
      await page.locator('.details h2').filter({ hasText: target.name }).waitFor();
      const section = page.locator('.component-detail').filter({ has: page.getByRole('heading', { name: 'OPC 管理端', exact: true }) });
      await section.getByRole('button', { name: '重启', exact: true }).scrollIntoViewIfNeeded();
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true);
      assert.match(await section.getByRole('button', { name: '重启', exact: true }).getAttribute('title'), /排空/);
      await page.screenshot({ path: path.join(output, `platform-${width}.png`) });
    }
    const section = page.locator('.component-detail').filter({ has: page.getByRole('heading', { name: 'OPC 管理端', exact: true }) });
    await section.getByRole('button', { name: '重启', exact: true }).click();
    const dialog = page.getByRole('dialog').filter({ hasText: '重启 OPC 管理端' });
    assert.equal(await dialog.getByRole('button', { name: '确认重启', exact: true }).isDisabled(), true);
    await dialog.getByText('已确认本次暂停相关控制命令，允许执行所列操作', { exact: true }).click();
    save('manager-intent.json', { target: target.id, component: 'manager', action: 'restart' });
    const reply = page.waitForResponse(r => r.url().endsWith('/api/project/1/rcd/operations') && r.request().method() === 'POST');
    await dialog.getByRole('button', { name: '确认重启', exact: true }).click();
    const response = await reply;
    const operation = await response.json(); save('manager-accepted.json', operation);
    assert.ok(response.ok(), JSON.stringify(operation));
    assert.equal(operation.request.component, 'manager');
    assert.equal(operation.request.targets[0].device_id, target.id);
    const final = await poll(platform + '/api/project/1/rcd/operations/' + operation.id, s => !['queued', 'running'].includes(s));
    save('manager-result.json', final); assert.equal(final.state, 'succeeded');
    await page.goto(platform + '/project/1/rcd/operations?operation=' + operation.id);
    await page.getByRole('heading', { name: '执行过程', exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'platform-result.png') });

    await page.goto(manager + '/#/sources?operations=rcd-30');
    await page.getByLabel('账号', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill(credentials.manager);
    await page.getByRole('button', { name: '登录', exact: true }).click();
    await page.getByRole('tab', { name: '进程', exact: true }).click();
    const control = page.getByRole('region', { name: '下级进程状态', exact: true });
    await control.getByRole('button', { name: '重启下级', exact: true }).waitFor();
    for (const width of [1366, 1920]) {
      await page.setViewportSize({ width, height: width === 1366 ? 768 : 1080 });
      await control.getByRole('button', { name: '重启下级', exact: true }).scrollIntoViewIfNeeded();
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true);
      await page.screenshot({ path: path.join(output, `lower-${width}.png`) });
    }
    await control.getByRole('button', { name: '重启下级', exact: true }).click();
    const confirm = page.getByRole('dialog').filter({ hasText: '确认重启下级进程' });
    await confirm.getByRole('checkbox').check();
    save('lower-intent.json', { manager: '46', source: 'rcd-30', action: 'restart' });
    const lowerReply = page.waitForResponse(r => r.url().endsWith('/sources/rcd-30/process/actions') && r.request().method() === 'POST');
    await confirm.getByRole('button', { name: '确认重启进程', exact: true }).click();
    const lowerResponse = await lowerReply;
    const lower = await lowerResponse.json(); save('lower-accepted.json', lower);
    assert.ok(lowerResponse.ok(), JSON.stringify(lower));
    const lowerFinal = await poll(manager + '/api/v1/sources/rcd-30/process/operations/' + lower.request_id, s => !['accepted', 'running'].includes(s));
    save('lower-result.json', lowerFinal); assert.equal(lowerFinal.state, 'succeeded');
    await page.getByText('进程动作已确认完成；OPC 连接、串口采集与配置生效状态仍需分别核对。', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'lower-result.png') });
    assert.equal(writes.filter(w => w.url.endsWith('/process/actions')).length, 1);
    assert.equal(writes.filter(w => w.url.endsWith('/rcd/operations')).length, 1);
    assert.deepEqual(errors, []);
    save('result.json', { passed: true, fixtures: false, platform_task: final.task_id, lower_request: lower.request_id, errors });
    console.log(JSON.stringify({ passed: true, platform_task: final.task_id, lower_request: lower.request_id }));
  } finally { save('page-errors.json', errors); await browser.close(); }
})().catch(e => { console.error(e); process.exitCode = 1; });
