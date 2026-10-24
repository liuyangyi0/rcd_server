// Run on 46 against isolated Semaphore only. Fixture routes never create a
// deployment, SSH connection or hardware command; native login remains real.
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
  const page = await context.newPage();
  const errors = []; const checks = []; const writes = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    await page.goto(`${config.url}/auth/login`);
    await page.getByTestId('auth-username').fill(config.auth);
    await page.getByTestId('auth-password').fill(config.password);
    await Promise.all([
      page.waitForResponse((r) => r.url().endsWith('/api/auth/login') && r.status() === 204),
      page.getByTestId('auth-signin').click(),
    ]);
    const created = await context.request.post(`${config.url}/api/projects`, {
      data: { name: '发布交互隔离回归', alert: false, max_parallel_tasks: 1 },
      headers: { Origin: config.url },
    });
    assert.ok(created.ok(), await created.text());
    const project = (await created.json()).id;
    const api = `/api/project/${project}/rcd`;
    const url = `${config.url}/project/${project}/rcd`;
    const now = new Date().toISOString(); const sha = 'a'.repeat(64);
    const pkg = { id: 'p-new', revision: 1, enabled: true, uploaded_at: now, uploaded_by: 'rcd-test',
      archive_size: 1024, manifest: { component: 'manager', version: 'usability-fixture', release_id: 'fixture-1',
        target: 'linux/amd64', binary_sha256: sha, release_notes: '隔离交互测试，不执行发布',
        compatibility: { adapter_min_version: '1.1.0', state_formats: { manager_db: { min: 16, max: 16 } } } } };
    const devices = ['46', '30', '58'].map((n) => ({ id: `d${n}`, name: `RCD ${n}`,
      address: `192.168.10.${n}`, revision: 2, state: n === '58' ? 'stale' : 'checked',
      template_id: 21, release_template_id: 22, control_template_id: 23, last_success_at: now,
      components: ['lower', 'manager', 'agent'].map((component) => ({ component, state: 'running',
        running_sha256: sha, disk_sha256: sha, running_build: { version: 'old-fixture' },
        disk_build: { version: 'old-fixture' } })) }));
    let pendingInspections = 0; let operation; let loseReply = true;
    let listFail = false; let detailFail = false; let detailReads = 0;
    let inspectionCalls = 0;
    await page.route(`**${api}/**`, async (route) => {
      const request = route.request(); const pathname = new URL(request.url()).pathname;
      const method = request.method();
      if (method !== 'GET') writes.push({ method, pathname, key: request.headers()['idempotency-key'],
        body: pathname.endsWith('/packages') ? '(fixture upload)' : request.postDataJSON() });
      if (method === 'POST' && pathname === `${api}/packages`) return route.fulfill({ status: 201, json: pkg });
      if (method === 'POST' && pathname === `${api}/devices/d58/inspections`) {
        inspectionCalls += 1; assert.equal(inspectionCalls, 1);
        assert.equal(request.headers()['if-match'], '"2"');
        pendingInspections = 2; devices[2].pending_task_id = 44;
        return route.fulfill({ json: { task_id: 44 } });
      }
      if (method === 'POST' && pathname === `${api}/operations`) {
        const body = request.postDataJSON();
        assert.deepEqual(body.targets.map((d) => d.device_id), ['d30', 'd58', 'd46']);
        operation = { id: 'original-op', request: body, package: pkg.manifest, state: 'running',
          revision: 1, actor: 'rcd-test', created_at: now, task_id: 901,
          targets: body.targets.map((t) => ({ device_id: t.device_id, name: t.device_id, state: 'running', stages: [] })) };
        if (loseReply) { loseReply = false; return route.fulfill({ status: 503, json: { error: '隔离回复丢失' } }); }
        return route.fulfill({ json: operation });
      }
      assert.equal(method, 'GET', `unexpected write: ${method} ${pathname}`);
      let value;
      if (pathname === `${api}/applications`) value = { items: [] };
      else if (pathname === `${api}/packages`) value = { items: [pkg], total: 1, total_pages: 1 };
      else if (pathname.endsWith('/compatibility')) value = { package_revision: 1,
        reasons: Object.fromEntries(devices.map((d) => [d.id, d.state === 'checked' ? '' : '状态已过期'])) };
      else if (pathname === `${api}/devices`) {
        if (pendingInspections > 0 && --pendingInspections === 0) {
          devices[2].state = 'checked'; devices[2].revision += 1;
          delete devices[2].pending_task_id;
        }
        value = { items: devices, total: 3, total_pages: 1 };
      } else if (pathname === `${api}/operations`) {
        if (listFail) return route.fulfill({ status: 503, json: { error: '隔离列表读取失败' } });
        value = { items: operation ? [operation] : [], total: operation ? 1 : 0, total_pages: operation ? 1 : 0 };
      } else if (pathname.endsWith('/operations/original-op')) {
        detailReads += 1;
        if (detailFail) return route.fulfill({ status: 503, json: { error: '隔离详情读取失败' } });
        value = operation;
      } else if (pathname.endsWith('/history')) {
        value = { items: [{ operation_id: 'historic-op', package: pkg, installed_at: now }], total: 1, total_pages: 1 };
      } else if (/\/devices\/d\d+$/.test(pathname)) value = devices.find((d) => pathname.endsWith(`/${d.id}`));
      else if (pathname.endsWith('/packages/p-new')) value = pkg;
      else throw new Error(`unhandled isolated route ${pathname}`);
      return route.fulfill({ json: value });
    });

    await page.goto(`${url}/packages`);
    await page.getByRole('button', { name: '上传软件包', exact: true }).click();
    await page.locator('input[type=file]').setInputFiles({ name: 'fixture.rcd.tar.gz', mimeType: 'application/gzip', buffer: Buffer.from('fixture only') });
    await page.getByRole('button', { name: '上传并校验', exact: true }).click();
    const direct = page.getByRole('button', { name: '选择设备升级此软件包', exact: true });
    await direct.waitFor(); assert.equal(writes.filter((w) => w.pathname.endsWith('/operations')).length, 0);
    await page.screenshot({ path: path.join(output, 'uploaded-next-step.png') });
    await direct.click(); const dialog = page.getByRole('dialog').filter({ has: page.getByText('升级软件', { exact: true }) });
    await dialog.getByText('隔离交互测试，不执行发布', { exact: true }).waitFor();
    await dialog.getByRole('button', { name: '下一步', exact: true }).click();
    assert.equal(await dialog.getByLabel('RCD 58 · 192.168.10.58', { exact: true }).isDisabled(), true);
    const row58 = dialog.locator('.release-device').filter({ hasText: '192.168.10.58' });
    await row58.getByRole('button', { name: '检查此设备状态', exact: true }).click();
    await dialog.getByText(/只读检查任务 #44 已接收/).waitFor();
    await page.waitForFunction(() => !document.querySelector('input[value="d58"]').disabled);
    for (const n of ['46', '30', '58']) await dialog.getByLabel(`RCD ${n} · 192.168.10.${n}`, { exact: true }).check({ force: true });
    await dialog.getByRole('button', { name: '下一步', exact: true }).click();
    assert.deepEqual((await dialog.locator('ol li').allTextContents()).map((v) => v.match(/192\.168\.10\.(\d+)/)[1]), ['30', '58', '46']);
    assert.equal(await dialog.getByRole('button', { name: '确认升级', exact: true }).isDisabled(), true);
    for (const width of [1366, 1920]) {
      await page.setViewportSize({ width, height: width === 1366 ? 768 : 1080 });
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true);
      await page.screenshot({ path: path.join(output, `confirm-order-${width}.png`) });
    }
    await dialog.getByLabel('已确认本次暂停所有相关管理端及直连客户端的控制命令', { exact: true }).check({ force: true });
    await dialog.getByRole('button', { name: '确认升级', exact: true }).click();
    await dialog.getByRole('button', { name: '确认原请求结果', exact: true }).waitFor();
    assert.equal(await dialog.getByRole('button', { name: '上一步', exact: true }).isDisabled(), true);
    await dialog.getByRole('button', { name: '确认原请求结果', exact: true }).click();
    await page.waitForURL(/\/operations\?operation=original-op/);
    const submissions = writes.filter((w) => w.pathname.endsWith('/operations'));
    assert.equal(submissions.length, 2); assert.equal(submissions[0].key, submissions[1].key);
    assert.deepEqual(submissions[0].body, submissions[1].body);
    checks.push('upload explicit next step; stale device inspection; executor order; pause checkbox; identical uncertain request');

    operation.state = 'succeeded'; operation.targets.forEach((t) => { t.state = 'succeeded'; });
    listFail = true;
    const details = page.locator('.details');
    await details.getByText('软件升级成功', { exact: true }).first().waitFor();
    await page.getByRole('alert').filter({ hasText: '隔离列表读取失败' }).waitFor();
    listFail = false; detailFail = true;
    await details.getByRole('button', { name: '刷新任务详情', exact: true }).click();
    await details.getByRole('alert').filter({ hasText: '隔离详情读取失败' }).waitFor();
    await details.getByText('下方保留上次结果，不能据此确认任务当前状态。', { exact: true }).waitFor();
    detailFail = false;
    await details.getByRole('button', { name: '刷新任务详情', exact: true }).click();
    await details.getByRole('alert').filter({ hasText: '隔离详情读取失败' }).waitFor({ state: 'hidden' });
    await details.getByText(/任务确认于/).waitFor();
    await page.screenshot({ path: path.join(output, 'recovered-task-detail.png') });
    assert.ok(detailReads >= 3);
    checks.push('list failure does not block task reconciliation; detail error recovers; last-confirmed time visible');

    await page.goto(`${url}/devices?device=d46`);
    const section = page.locator('.component-detail').filter({ has: page.getByRole('heading', { name: 'OPC 管理端', exact: true }) });
    await section.getByRole('button', { name: '版本历史', exact: true }).click();
    await section.getByText('已是此程序版本，无需恢复', { exact: true }).waitFor();
    assert.equal(await section.getByRole('button', { name: '恢复此版本', exact: true }).isDisabled(), true);
    await section.getByText(/共 1 条安装记录/).waitFor();
    await page.screenshot({ path: path.join(output, 'history-current-image.png') });
    checks.push('installation record count is honest; current actual image is not offered as a restoration');
    assert.deepEqual(errors, []);
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ passed: true, checks,
      fixture_requests: writes, page_errors: errors, hardware_commands: 0, native_release_tasks: 0 }, null, 2));
    process.stdout.write(JSON.stringify({ passed: true, checks }) + '\n');
  } catch (error) {
    await page.screenshot({ path: path.join(output, 'failure.png') }).catch(() => {});
    fs.writeFileSync(path.join(output, 'failure.json'), JSON.stringify({ error: error.stack, checks, page_errors: errors }, null, 2));
    throw error;
  } finally {
    await context.close(); await browser.close();
  }
})().catch((error) => { console.error(error); process.exitCode = 1; });
