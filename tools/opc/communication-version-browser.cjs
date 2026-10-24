// Read-only live check on 46. Password is supplied on stdin and never saved.
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

(async () => {
  const [output] = process.argv.slice(2);
  assert.ok(output && path.isAbsolute(output) && !fs.existsSync(output));
  let input = '';
  for await (const chunk of process.stdin) input += chunk;
  const credentials = JSON.parse(input); input = '';
  const base = 'http://192.168.10.46:3000';
  fs.mkdirSync(output, { mode: 0o700 });
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1366, height: 768 }, locale: 'zh-CN' });
  const errors = []; const blockedWrites = [];
  await context.route('**/*', async route => {
    const request = route.request();
    if (['GET', 'HEAD'].includes(request.method()) ||
        (request.method() === 'POST' && request.url() === base + '/api/auth/login')) {
      return route.continue();
    }
    blockedWrites.push({ method: request.method(), url: request.url() });
    return route.abort();
  });
  const page = await context.newPage();
  page.on('pageerror', error => errors.push(error.message));
  try {
    await page.goto(base + '/auth/login');
    await page.getByTestId('auth-username').fill('admin');
    await page.getByTestId('auth-password').fill(credentials.platform);
    credentials.platform = '';
    await Promise.all([
      page.waitForResponse(r => r.url().endsWith('/api/auth/login') && r.status() === 204),
      page.getByTestId('auth-signin').click(),
    ]);
    await page.waitForURL(url => !url.pathname.startsWith('/auth/'));
    await page.goto(base + '/project/1/rcd/devices');
    const row = page.locator('tbody tr').filter({ hasText: '192.168.10.46' });
    const communication = row.locator('.component-version').filter({ hasText: '串口通信程序' });
    await communication.getByText('0.2.0', { exact: true }).waitFor();
    await communication.getByText('程序运行', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'device-46-version.png'), fullPage: true });
    const response = await context.request.get(base + '/api/project/1/rcd/devices?page_size=100');
    assert.ok(response.ok());
    const device = (await response.json()).items.find(d => d.address === '192.168.10.46');
    const actual = device.components.find(c => c.component === 'communicate');
    assert.equal(device.state, 'checked');
    assert.equal(actual.state, 'running');
    assert.equal(actual.running_build.version, '0.2.0');
    assert.equal(actual.running_sha256, '32367696f6f480acfd7c8cd77e5b485d441503cf201d1b62e4399d989bdc2921');
    assert.equal(actual.disk_differs, false);
    assert.deepEqual(errors, []);
    assert.deepEqual(blockedWrites, []);
    const result = { passed: true, fixtures: false, version: '0.2.0',
      task_id: device.last_task_id, confirmed_at: device.last_success_at,
      page_errors: errors, blocked_writes: blockedWrites };
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify(result, null, 2), { mode: 0o600 });
    console.log(JSON.stringify(result));
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error.message); process.exitCode = 1; });
