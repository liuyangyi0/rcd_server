// Run on 46 against built manager assets. All APIs are local deterministic
// fixtures; this is UI fault regression, not real-device acceptance.
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const assert = require('node:assert/strict');
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const dist = path.resolve(__dirname, '../../manager/web/dist');
const output = process.env.RCD_BROWSER_OUTPUT;
assert.ok(output && path.isAbsolute(output) && !fs.existsSync(output));
const source = { id: 'fixture', name: '隔离来源', group_id: 'line', enabled: true,
  connection_state: 'Disconnected', session_state: 'Inactive', subscription_state: 'Inactive', catalog_state: 'Ready', revision: 1 };
const group = { id: 'line', name: '隔离组', default_source_id: 'fixture', selection_revision: 1 };
const server = http.createServer((req, res) => {
  const relative = new URL(req.url, 'http://localhost').pathname;
  const candidate = path.resolve(dist, '.' + relative);
  if (candidate !== dist && !candidate.startsWith(dist + path.sep)) { res.writeHead(403).end(); return; }
  const file = fs.existsSync(candidate) && fs.statSync(candidate).isFile() ? candidate : path.join(dist, 'index.html');
  res.writeHead(200, { 'Content-Type': { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css' }[path.extname(file)] || 'application/octet-stream' });
  fs.createReadStream(file).pipe(res);
});

(async () => {
  fs.mkdirSync(output, { recursive: false });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1366, height: 900 }, locale: 'zh-CN' });
  const page = await context.newPage();
  const errors = [], requests = [], alarms = [], checks = [];
  let calculationFailure = false, sourceReads = 0;
  page.on('pageerror', (error) => errors.push(error.message));
  await context.route('**/api/v1/**', async (route) => {
    const req = route.request(), url = new URL(req.url()), p = url.pathname;
    assert.equal(req.method(), 'GET', 'no configuration, publication or hardware writes');
    requests.push({ path: p, query: url.search });
    if (p.endsWith('/events')) { await route.fulfill({ contentType: 'text/event-stream', body: 'retry: 60000\n: isolated\n\n' }); return; }
    if (p.endsWith('/calculations/status') && calculationFailure) {
      await route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: { message: '隔离计算状态不可用' } }) }); return;
    }
    const offset = Number(url.searchParams.get('offset') || 0);
    let value = { items: [], total: 0, limit: 50, offset };
    if (p.endsWith('/auth/session')) value = { authenticated: true, username: 'fixture' };
    else if (p.endsWith('/overview')) value = { manager_state: 'Running', data_quality: { good: 0, bad: 0, total: 0 } };
    else if (p.endsWith('/groups')) value = [group];
    else if (p.endsWith('/sources')) value = [{ ...source, reconnect_count: ++sourceReads }];
    else if (p.endsWith('/sessions')) value = [];
    else if (p.endsWith('/calculations/status')) value = { active_revision: 0, rule_count: 0, bad_count: 0 };
    else if (p.endsWith('/commands')) value = { items: offset ? [] : [{ id: 'one', port: 'TEST', state: 'Received', status_code: 0, started_at: '2026-10-22T00:00:00Z' }], total: 1, limit: 50, offset };
    else if (p.endsWith('/alarms')) alarms.push(Object.fromEntries(url.searchParams));
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) });
  });
  const base = `http://127.0.0.1:${server.address().port}`;
  try {
    await page.goto(base + '/#/');
    const inactive = page.getByText('未激活', { exact: true });
    await inactive.first().waitFor();
    assert.equal(await inactive.count(), 2);
    for (const badge of await inactive.all()) assert.match(await badge.locator('..').getAttribute('class'), /status-bad/);
    checks.push('inactive Session and subscription are not green');
    calculationFailure = true;
    await page.reload();
    await page.getByText('部分运行数据尚未确认', { exact: true }).waitFor();
    assert.equal(await page.getByText('当前无需人工处理', { exact: true }).count(), 0);
    checks.push('partial status failure is explicit, not all-normal');
    calculationFailure = false;

    await page.goto(base + '/#/operations');
    const state = page.locator('label:has(> span:text-is("状态")) select');
    await state.selectOption('all');
    await page.waitForFunction(() => location.hash.includes('state=all'));
    await page.locator('label:has(> span:text-is("时间范围")) select').selectOption('1h');
    await page.getByRole('button', { name: '刷新', exact: true }).click();
    await page.waitForTimeout(150);
    assert.ok(alarms.length > 0);
    assert.equal(alarms[alarms.length - 1].status, undefined);
    assert.equal(alarms[alarms.length - 1].to, undefined, 'relative ranges must admit records produced after opening');
    checks.push('all-alarm filter and moving relative interval');

    await page.goto(base + '/#/groups');
    await page.getByRole('button', { name: '新建分组', exact: true }).first().click();
    const dialog = page.getByRole('dialog', { name: '新建分组', exact: true });
    const editor = dialog.getByLabel('显示名称', { exact: true });
    await editor.fill('保持焦点');
    await page.evaluate(() => document.querySelector('.context-actions button').click());
    await page.waitForTimeout(200);
    assert.ok(await editor.evaluate((element) => element === document.activeElement));
    await page.keyboard.type('继续输入');
    assert.equal(await editor.inputValue(), '保持焦点继续输入');
    await page.screenshot({ path: path.join(output, 'focused-modal.png'), animations: 'disabled' });
    await dialog.getByRole('button', { name: '取消', exact: true }).click();
    checks.push('background refresh preserves modal editing focus');

    await page.goto(base + '/#/commands?page=9');
    await page.getByRole('button', { name: '返回末页', exact: true }).click();
    await page.getByRole('button', { name: '执行过程', exact: true }).waitFor();
    assert.ok(!page.url().includes('page=9'));
    checks.push('empty out-of-range command page retains a recovery action');
    for (const width of [1366, 1920, 390]) {
      await page.setViewportSize({ width, height: width === 390 ? 844 : 900 });
      await page.waitForFunction(() => document.documentElement.scrollWidth <= innerWidth + 1, null, { timeout: 5000 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
      await page.screenshot({ path: path.join(output, `commands-${width}.png`), animations: 'disabled' });
    }
    assert.deepEqual(errors, []);
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ passed: true, checks, requests,
      page_errors: errors, fixtures: true, real_device_acceptance: false, business_writes: 0 }, null, 2));
    console.log(JSON.stringify({ passed: true, checks }));
  } catch (error) {
    await page.screenshot({ path: path.join(output, 'failure.png') }).catch(() => {});
    fs.writeFileSync(path.join(output, 'failure.json'), JSON.stringify({ error: error.stack, errors, checks, requests }, null, 2));
    throw error;
  } finally { await context.close(); await browser.close(); await new Promise((resolve) => server.close(resolve)); }
})().catch((error) => { console.error(error); server.close(); process.exitCode = 1; });
