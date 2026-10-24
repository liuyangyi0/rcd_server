// Read-only acceptance of the authorized 46 platform update. Credentials are
// provided on stdin, never in CLI, environment variables or evidence files.
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
  page.on('pageerror', (error) => errors.push(error.message));
  await page.route('**/api/**', async (route) => {
    const request = route.request(); const pathname = new URL(request.url()).pathname;
    requests.push({ method: request.method(), pathname });
    if (request.method() !== 'GET'
      && !(request.method() === 'POST' && pathname === '/api/auth/login')) {
      await route.abort(); throw new Error(`unexpected live write: ${pathname}`);
    }
    await route.continue();
  });
  try {
    await page.goto(`${base}/auth/login`);
    await page.getByTestId('auth-username').fill('admin');
    await page.getByTestId('auth-password').fill(input.password); input.password = '';
    await Promise.all([
      page.waitForResponse((response) => response.url().endsWith('/api/auth/login') && response.status() === 204),
      page.getByTestId('auth-signin').click(),
    ]);
    await page.goto(`${base}/project/1/rcd/devices`);
    await page.getByRole('heading', { name: '设备与版本', exact: true }).waitFor();
    const response = await context.request.get(`${base}/api/project/1/rcd/devices?page_size=20`);
    assert.ok(response.ok()); const devices = (await response.json()).items;
    assert.equal(devices.length, 3);
    for (const device of devices) {
      assert.equal(device.state, 'checked', device.address);
      assert.equal(device.last_error, '', device.address);
      assert.ok(device.components.some((component) => component.component === 'communicate'));
      assert.equal(device.host_status?.schema_version, 1, device.address);
      assert.equal(device.host_status.available, true, device.address);
      assert.ok(Date.now() - Date.parse(device.host_status.observed_at) < 180000);
      assert.equal(device.host_status.cpu.available, true);
      assert.equal(device.host_status.memory.available, true);
    }
    for (const width of [1366, 1920, 390]) {
      await page.setViewportSize({ width, height: width === 390 ? 844 : 900 });
      await page.waitForFunction(() => document.documentElement.scrollWidth <= innerWidth + 1,
        null, { timeout: 5000 });
      await page.screenshot({ path: path.join(output, `live-hosts-${width}.png`), fullPage: true, animations: 'disabled' });
    }
    checks.push('three fresh real four-component/resource snapshots; desktop and mobile layout');
    await page.setViewportSize({ width: 1366, height: 768 });
    await page.getByRole('row').filter({ hasText: '192.168.10.46' })
      .getByRole('button', { name: '查看详情', exact: true }).click();
    await page.getByRole('heading', { name: '主机资源快照', exact: true }).waitFor();
    await page.getByRole('heading', { name: '网络接口', exact: true }).waitFor();
    await page.getByText('192.168.10.46/24', { exact: false }).waitFor();
    await page.getByRole('heading', { name: '网络接口', exact: true }).scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(output, 'live-host-network-detail.png'), animations: 'disabled' });
    await page.getByRole('button', { name: '关闭详情', exact: true }).click();
    checks.push('real network detail retains CIDR, counters, links and resource observation time');
    await page.getByRole('button', { name: '添加主机', exact: true }).click();
    const enrollment = page.getByRole('dialog').filter({ hasText: '添加主机' });
    await enrollment.getByLabel('SSH登录账号', { exact: true }).waitFor();
    await enrollment.getByText(/首次安装写入系统目录/).waitFor();
    await page.screenshot({ path: path.join(output, 'live-add-host.png'), animations: 'disabled' });
    await enrollment.getByRole('button', { name: '关闭', exact: true }).click();
    checks.push('real encrypted credential options and controlled source are readable; no host submitted');
    await page.goto(`${base}/project/1/rcd/packages`);
    await page.getByRole('button', { name: '上传软件包', exact: true }).waitFor();
    await page.getByRole('button', { name: '添加软件种类', exact: true }).click();
    const definitions = page.getByRole('dialog').filter({ hasText: '添加软件种类' });
    await definitions.getByLabel('软件种类ID', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'live-add-software-kind.png'), animations: 'disabled' });
    await definitions.getByRole('button', { name: '关闭', exact: true }).click();
    await page.getByLabel('组件', { exact: true }).click();
    await page.getByRole('option', { name: '串口通信程序', exact: true }).waitFor();
    checks.push('new component, standard package upload and software registration are visible without mutation');
    assert.deepEqual(errors, []);
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ passed: true, checks,
      requests, page_errors: errors, business_writes: 0,
      devices: devices.map((device) => ({ address: device.address,
        observed_at: device.host_status.observed_at,
        components: device.components.map((component) => ({ component: component.component, state: component.state })) })),
    }, null, 2));
    console.log(JSON.stringify({ passed: true, checks }));
  } catch (error) {
    await page.screenshot({ path: path.join(output, 'failure.png') }).catch(() => {});
    fs.writeFileSync(path.join(output, 'failure.json'), JSON.stringify({ error: error.stack, errors, checks }, null, 2));
    throw error;
  } finally { await context.close(); await browser.close(); }
})().catch((error) => { console.error(error); process.exitCode = 1; });
