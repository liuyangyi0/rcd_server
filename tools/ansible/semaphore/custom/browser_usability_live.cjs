// Post-update read-only UI checks on 46. Credentials arrive over stdin, never
// CLI/environment/evidence. Only native login may POST; no release is submitted.
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');

(async () => {
  const input = JSON.parse(fs.readFileSync(0, 'utf8'));
  const [output] = process.argv.slice(2);
  const base = 'http://192.168.10.46:3000';
  fs.mkdirSync(output, { recursive: false });
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1366, height: 768 }, locale: 'zh-CN' });
  const page = await context.newPage(); const errors = []; const requests = []; const checks = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.route('**/api/**', async (route) => {
    const req = route.request(); const pathname = new URL(req.url()).pathname;
    requests.push({ method: req.method(), pathname });
    if (req.method() !== 'GET' && !(req.method() === 'POST' && pathname === '/api/auth/login')) {
      await route.abort(); throw new Error(`unexpected live mutation ${req.method()} ${pathname}`);
    }
    await route.continue();
  });
  try {
    await page.goto(`${base}/auth/login`);
    await page.getByTestId('auth-username').fill('admin');
    await page.getByTestId('auth-password').fill(input.password);
    input.password = '';
    await Promise.all([
      page.waitForResponse((r) => r.url().endsWith('/api/auth/login') && r.status() === 204),
      page.getByTestId('auth-signin').click(),
    ]);
    await page.goto(`${base}/project/1/rcd/packages`);
    await page.getByLabel('搜索版本或发布编号', { exact: true }).fill('0.6.12-state-names.20261021.1');
    await page.getByRole('button', { name: '查询', exact: true }).click();
    const row = page.locator('tbody tr').filter({ hasText: '0.6.12-state-names.20261021.1' });
    await row.getByRole('button', { name: '查看详情', exact: true }).click();
    const detail = page.getByRole('dialog').filter({ hasText: '软件包详情' });
    await detail.getByRole('button', { name: '选择设备升级', exact: true }).click();
    const wizard = page.getByRole('dialog').filter({ hasText: '升级软件' });
    await wizard.getByRole('button', { name: '下一步', exact: true }).click();
    await wizard.getByRole('button', { name: '刷新设备与兼容性', exact: true }).waitFor();
    await wizard.getByRole('button', { name: '刷新设备与兼容性', exact: true }).click();
    await wizard.getByText('RCD 30 · 192.168.10.30', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'live-target-checks.png') });
    await wizard.getByRole('button', { name: '取消', exact: true }).click();
    checks.push('live package details open the correct upgrade wizard; target refresh is GET-only; cancelled without submission');

    await page.goto(`${base}/project/1/rcd/operations?operation=b2ceb857fbf300254749e18ee5e290dd98d99c0aac7f90517a8e7db9d2a64f70`);
    const drawer = page.locator('.details');
    await drawer.getByRole('button', { name: '刷新任务详情', exact: true }).click();
    await drawer.getByText(/任务确认于/).waitFor();
    await drawer.getByText('软件升级成功', { exact: true }).first().waitFor();
    for (const width of [1366, 1920]) {
      await page.setViewportSize({ width, height: width === 1366 ? 768 : 1080 });
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true);
      await page.screenshot({ path: path.join(output, `live-original-task-${width}.png`) });
    }
    checks.push('original successful task, manual detail refresh, actual stage results, 1366/1920 layout');
    await page.goto(`${base}/project/1/rcd/devices`);
    await page.getByRole('heading', { name: '设备与版本', exact: true }).waitFor();
    await page.getByText('192.168.10.30', { exact: true }).waitFor();
    await page.getByText('192.168.10.58', { exact: true }).waitFor();
    await page.getByText('192.168.10.46', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'live-devices.png') });
    checks.push('three registered devices and preserved real component snapshots');
    assert.deepEqual(errors, []);
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ passed: true, checks,
      requests, page_errors: errors, release_submissions: 0, hardware_commands: 0 }, null, 2));
    console.log(JSON.stringify({ passed: true, checks }));
  } catch (error) {
    await page.screenshot({ path: path.join(output, 'failure.png') }).catch(() => {});
    fs.writeFileSync(path.join(output, 'failure.json'), JSON.stringify({ error: error.stack,
      requests, page_errors: errors, checks }, null, 2));
    throw error;
  } finally {
    await context.close(); await browser.close();
  }
})().catch((error) => { console.error(error); process.exitCode = 1; });
