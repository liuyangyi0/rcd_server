// Run on 46 only against isolated native Semaphore. Fixture reads never create
// inspections, software operations, SSH calls or hardware traffic.
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

(async () => {
  const [state, output] = process.argv.slice(2);
  const config = JSON.parse(fs.readFileSync(path.join(state, 'browser-credentials.json')));
  assert.match(config.url, /^http:\/\/127\.0\.0\.1:\d+$/);
  fs.mkdirSync(output, { recursive: false });
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1366, height: 768 }, locale: 'zh-CN' });
  const page = await context.newPage(); const errors = []; const checks = []; const writes = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    await page.goto(`${config.url}/auth/login`);
    await page.getByTestId('auth-username').fill(config.auth);
    await page.getByTestId('auth-password').fill(config.password);
    await Promise.all([
      page.waitForResponse((r) => r.url().endsWith('/api/auth/login') && r.status() === 204),
      page.getByTestId('auth-signin').click(),
    ]);
    const response = await context.request.post(`${config.url}/api/projects`, {
      data: { name: '四组件及主机快照隔离验收', alert: false, max_parallel_tasks: 1 },
      headers: { Origin: config.url },
    });
    assert.ok(response.ok(), await response.text()); const project = (await response.json()).id;
    const api = `/api/project/${project}/rcd`; const url = `${config.url}/project/${project}/rcd`;
    const now = new Date().toISOString(); const sha = 'a'.repeat(64);
    const metrics = (index) => ({ schema_version: 1, observed_at: now, available: true, error: '',
      sample_interval_seconds: 0.2, uptime_seconds: 90061,
      cpu: { available: true, utilization_percent: 12.5 + index, logical_count: 4, error: '' },
      memory: { available: true, total_bytes: 16 * 1024 ** 3, available_bytes: 8 * 1024 ** 3,
        used_bytes: 8 * 1024 ** 3, used_percent: 50, error: '' },
      disks: [{ path: '/', mount_point: '/', filesystem: 'ext4', available: true,
        total_bytes: 200 * 1024 ** 3, available_bytes: 100 * 1024 ** 3,
        used_bytes: 100 * 1024 ** 3, used_percent: 50, error: '' }],
      network: [{ name: 'eth0', operstate: 'up', link_up: true,
        ipv4_addresses: [`192.168.10.${['30', '58', '46'][index]}`],
        available: true, addresses_error: '', error: '', rx_bytes: 2048, tx_bytes: 1024,
        rx_bytes_per_second: 1024, tx_bytes_per_second: null }],
    });
    const devices = ['30', '58', '46'].map((n, i) => ({ id: `d${n}`, name: `RCD ${n}`,
      address: `192.168.10.${n}`, revision: 1, state: 'checked', template_id: 20,
      release_template_id: 21, control_template_id: 22, last_success_at: now,
      host_status: metrics(i), components: ['lower', 'manager', 'agent', 'communicate'].map((c) => ({
        component: c, state: c === 'communicate' && n !== '46' ? 'not_installed' : 'running',
        running_build: c === 'communicate' && n !== '46' ? null : { version: c === 'communicate' ? 'sync-fixture-v1' : 'software-fixture-v1' },
        disk_build: { version: 'disk-fixture-v1' }, running_sha256: sha, disk_sha256: sha,
      })) }));
    const pkg = { id: 'sync-pkg', revision: 1, enabled: true, uploaded_at: now, archive_size: 1024,
      manifest: { component: 'communicate', version: 'sync-fixture-v2', release_id: 'sync-fixture-v2',
        target: 'linux/amd64', release_notes: '只用于隔离界面检查',
        binary_sha256: 'b'.repeat(64), compatibility: { adapter_min_version: '1.1.0', state_formats: {} } } };
    let freezeDeviceReads = false;
    await page.route(`**${api}/**`, async (route) => {
      const request = route.request(); const pathname = new URL(request.url()).pathname;
      if (request.method() !== 'GET') writes.push({ method: request.method(), pathname });
      assert.equal(request.method(), 'GET', `unexpected isolated mutation ${pathname}`);
      // Hold subsequent device reads to prove freshness is local to time, not
      // obtained by replacing the snapshot or returning a stale server state.
      if (freezeDeviceReads && (pathname === `${api}/devices` || /\/devices\/d\d+$/.test(pathname))) return;
      let value;
      if (pathname === `${api}/devices`) value = { items: devices, total: 3, total_pages: 1 };
      else if (/\/devices\/d\d+$/.test(pathname)) value = devices.find((d) => pathname.endsWith(`/${d.id}`));
      else if (pathname.endsWith('/history')) value = { items: [], total: 0, total_pages: 0 };
      else if (pathname === `${api}/packages`) value = { items: [pkg], total: 1, total_pages: 1 };
      else if (pathname.endsWith('/compatibility')) value = { package_revision: 1, reasons: { d30: '未安装', d58: '未安装', d46: '' } };
      else if (pathname === `${api}/operations`) value = { items: [], total: 0, total_pages: 0 };
      else if (pathname === `${api}/applications`) value = { items: [] };
      else throw new Error(`unhandled isolated route ${pathname}`);
      await route.fulfill({ json: value });
    });

    for (const width of [1366, 1920, 390]) {
      await page.setViewportSize({ width, height: width === 1920 ? 1080 : 844 });
      await page.goto(`${url}/devices`);
      const row = page.locator('tbody tr').filter({ hasText: '192.168.10.46' });
      await row.getByText('sync-fixture-v1', { exact: true }).waitFor();
      await row.getByText('14.5%', { exact: true }).waitFor();
      await row.getByText('1/1 接口up', { exact: true }).waitFor();
      assert.equal(await page.locator('tbody tr').filter({ hasText: '192.168.10.30' })
        .getByText('未安装', { exact: true }).count(), 1);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true);
      await page.screenshot({ path: path.join(output, `four-components-host-${width}.png`), fullPage: true, animations: 'disabled' });
    }
    checks.push('four components, real zero/unknown semantics and host overview at 1366/1920/390 without horizontal scrolling');
    await page.setViewportSize({ width: 1366, height: 768 });
    await page.goto(`${url}/devices?device=d46`);
    const drawer = page.locator('.details');
    await drawer.getByRole('heading', { name: '主机资源快照', exact: true }).waitFor();
    const host = drawer.locator('.host-status');
    await host.getByText('eth0 · 链路up', { exact: true }).waitFor();
    await host.getByText(/接收 1.0 KiB\/s · 发送 速率未知/).waitFor();
    await host.getByText(/不证明网络、下级或物理站点可达/).waitFor();
    const communication = drawer.locator('.component-detail').filter({ has: page.getByRole('heading', { name: '串口通信程序', exact: true }) });
    await communication.getByText('sync-fixture-v1', { exact: true }).waitFor();
    assert.equal(await communication.getByRole('button', { name: /^(启动|停止|重启)$/ }).count(), 0);
    await page.screenshot({ path: path.join(output, 'host-detail-network.png'), fullPage: true, animations: 'disabled' });
    devices[2].host_status.observed_at = new Date(Date.now() - 181000).toISOString();
    await drawer.getByRole('button', { name: '刷新详情', exact: true }).click();
    await host.getByText('历史快照 · 已过期', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'host-expired-snapshot.png'), animations: 'disabled' });
    checks.push('network details and unknown rate, explicit stale snapshot, no communicate direct service controls');

    await page.goto(`${url}/packages`);
    await page.getByLabel('组件', { exact: true }).click();
    await page.getByRole('option', { name: '串口通信程序', exact: true }).click();
    await page.locator('tbody tr').filter({ hasText: 'sync-fixture-v2' })
      .getByRole('button', { name: '选择设备升级', exact: true }).click();
    const wizard = page.getByRole('dialog').filter({ hasText: '升级软件' });
    await wizard.getByRole('button', { name: '下一步', exact: true }).click();
    assert.equal(await wizard.getByLabel('RCD 30 · 192.168.10.30', { exact: true }).isDisabled(), true);
    assert.equal(await wizard.getByLabel('RCD 58 · 192.168.10.58', { exact: true }).isDisabled(), true);
    assert.equal(await wizard.getByLabel('RCD 46 · 192.168.10.46', { exact: true }).isEnabled(), true);
    await wizard.getByRole('button', { name: '取消', exact: true }).click();
    checks.push('communicate package filter and upgrade wizard reject blind installation on 30/58');
    const clockTime = new Date();
    devices[2].host_status.observed_at = clockTime.toISOString();
    devices[2].last_success_at = clockTime.toISOString();
    devices[2].components.find((c) => c.component === 'manager').state = 'stopped';
    devices[2].components.find((c) => c.component === 'communicate').state = 'not_installed';
    await page.clock.install({ time: clockTime });
    await page.goto(`${url}/devices?device=d46`);
    const freshDrawer = page.locator('.details');
    const manager = freshDrawer.locator('.component-detail').filter({
      has: page.getByRole('heading', { name: 'OPC 管理端', exact: true }),
    });
    await freshDrawer.locator('.host-status').getByText('巡检快照 · 非实时', { exact: true }).waitFor();
    assert.equal(await manager.getByRole('button', { name: '启动', exact: true }).isEnabled(), true);
    freezeDeviceReads = true;
    await page.clock.runFor(181000);
    await freshDrawer.locator('.host-status').getByText('历史快照 · 已过期', { exact: true }).waitFor();
    assert.equal(await manager.getByRole('button', { name: '启动', exact: true }).isDisabled(), true);
    const absent = freshDrawer.locator('.component-detail').filter({
      has: page.getByRole('heading', { name: '串口通信程序', exact: true }),
    });
    await absent.getByRole('button', { name: '首次安装', exact: true }).click();
    const initialInstall = page.locator('.host-enrollment');
    await initialInstall.getByText('需要180秒内的新鲜可信检查，先关闭此页并检查主机。', { exact: true }).waitFor();
    assert.equal(await initialInstall.getByRole('button', { name: '首次安装并保持停止', exact: true }).isDisabled(), true);
    await page.screenshot({ path: path.join(output, 'freshness-clock-expiry.png'), fullPage: true, animations: 'disabled' });
    checks.push('without new device responses, virtual time crosses 180s: metrics become stale, service control and first install are disabled');
    assert.deepEqual(writes, []); assert.deepEqual(errors, []);
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ passed: true, checks,
      page_errors: errors, fixture_writes: writes, native_tasks: 0, hardware_commands: 0 }, null, 2));
    console.log(JSON.stringify({ passed: true, checks }));
  } catch (error) {
    await page.screenshot({ path: path.join(output, 'failure.png'), animations: 'disabled' }).catch(() => {});
    fs.writeFileSync(path.join(output, 'failure.json'), JSON.stringify({ error: error.stack, checks, page_errors: errors }, null, 2));
    throw error;
  } finally { await context.close(); await browser.close(); }
})().catch((error) => { console.error(error); process.exitCode = 1; });
