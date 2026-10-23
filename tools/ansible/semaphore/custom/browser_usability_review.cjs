// 46 isolated Semaphore only. Native login/project creation is real; all RCD
// endpoints are fixtures, including cancellation. No SSH, install or service call.
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
  const routeErrors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  try {
    await page.goto(`${config.url}/auth/login`);
    await page.getByTestId('auth-username').fill(config.auth);
    await page.getByTestId('auth-password').fill(config.password);
    await Promise.all([
      page.waitForResponse((r) => r.url().endsWith('/api/auth/login') && r.status() === 204),
      page.getByTestId('auth-signin').click(),
    ]);
    const response = await context.request.post(`${config.url}/api/projects`, {
      data: { name: '现场使用流程精修隔离验收', alert: false, max_parallel_tasks: 1 },
      headers: { Origin: config.url },
    });
    assert.ok(response.ok(), await response.text()); const project = (await response.json()).id;
    const api = `/api/project/${project}/rcd`; const url = `${config.url}/project/${project}/rcd`;
    const now = new Date().toISOString(); const sha = 'a'.repeat(64);
    const devices = ['30', '58', '46'].map((suffix) => ({ id: `d${suffix}`, name: `RCD ${suffix}`,
      address: `192.168.10.${suffix}`, revision: 2, state: suffix === '30' ? 'check_failed' : 'checked',
      last_error: suffix === '30' ? 'SSH认证失败，请核对已保存凭据' : '',
      last_success_at: now, last_task_id: 81, template_id: 11, release_template_id: 12, control_template_id: 13,
      components: ['lower', 'manager', 'agent', 'communicate'].map((component) => ({ component,
        state: component === 'communicate' ? 'not_installed' : 'running',
        running_sha256: sha, disk_sha256: sha,
        running_build: { version: '0.6.12-state-names.20261021.1' },
      })),
      host_status: { schema_version: 1, observed_at: now, available: true, error: '',
        cpu: { available: true, utilization_percent: 22, logical_count: 4 },
        memory: { available: true, total_bytes: 16 * 1024 ** 3, available_bytes: 8 * 1024 ** 3, used_percent: 50 },
        disks: [], network: [] },
    }));
    const packages = Array.from({ length: 21 }, (_, i) => ({ id: `p${i + 1}`, revision: 1, enabled: true,
      uploaded_at: now, archive_size: 1024, uploaded_by: 'isolated reviewer', manifest: {
        component: 'manager', version: `review-v${i + 1}`, release_id: `review-release-${i + 1}`,
        target: 'linux/amd64', binary_sha256: sha, release_notes: '隔离页面验收，不安装软件',
        compatibility: { adapter_min_version: '1.1.0', state_formats: { manager_db: { min: 16, max: 16 } } },
      } }));
    const setups = Array.from({ length: 21 }, (_, i) => ({ id: `original-setup-${i + 1}`, revision: 7,
      state: 'unknown', phase: 'install', original_install_task_id: 123, task_id: 123,
      existing_device: true, device_id: 'd30', created_at: now, updated_at: now, steps: [], host_keys: [],
      request: { name: `接入记录 ${i + 1}`, address: '192.168.10.30', ssh_port: 22 },
      install: { component: 'manager', package_id: 'p1', package_revision: 1 },
      error: '原目标回复丢失，保留占用等待核对',
    }));
    let failSetupPage = false; let failPackagePage = false;
    let operation = { id: 'original-queued-operation', revision: 4, task_id: 901, state: 'queued',
      actor: 'isolated reviewer', created_at: now, request: { action: 'upgrade', component: 'manager', kind: 'release' },
      package: packages[0].manifest, targets: [{ device_id: 'd46', name: 'RCD 46', address: '192.168.10.46', state: 'queued', stages: [] }] };
    await page.route(`**${api}/**`, async (route) => {
      try {
        const request = route.request(); const u = new URL(request.url()); const name = u.pathname;
        const method = request.method(); const query = u.searchParams;
        if (method !== 'GET') {
          writes.push({ method, name, body: request.postDataJSON(), match: request.headers()['if-match'] });
          assert.equal(method, 'POST'); assert.equal(name, `${api}/operations/${operation.id}/cancel`);
          assert.equal(request.headers()['if-match'], '"4"');
          operation = { ...operation, revision: 5, state: 'cancelled',
            targets: operation.targets.map((target) => ({ ...target, state: 'not_started' })) };
          await route.fulfill({ json: operation }); return;
        }
        let data;
        if (name === `${api}/applications`) data = { items: [] };
        else if (name === `${api}/devices`) data = { items: devices, total: devices.length, total_pages: 1 };
        else if (/\/devices\/d\d+$/.test(name)) data = devices.find((d) => name.endsWith(`/${d.id}`));
        else if (name.endsWith('/history')) data = { items: [], total: 0, total_pages: 0 };
        else if (name === `${api}/host-setups/options`) data = { available: true, error: '', ssh_keys: [], become_keys: [] };
        else if (name === `${api}/host-setups`) {
          const n = Number(query.get('page') || 1); const size = Number(query.get('page_size') || 20);
          if (n === 2 && failSetupPage) { await route.fulfill({ status: 503, json: { error: '隔离接入记录分页失败' } }); return; }
          data = { items: setups.slice((n - 1) * size, n * size), total: setups.length, total_pages: Math.ceil(setups.length / size) };
        } else if (name.includes('/host-setups/')) data = setups.find((s) => name.endsWith(`/${s.id}`));
        else if (name === `${api}/packages`) {
          const n = Number(query.get('page') || 1); const size = Number(query.get('page_size') || 20);
          if (n === 2 && failPackagePage) { await route.fulfill({ status: 503, json: { error: '隔离软件包分页失败' } }); return; }
          data = { items: packages.slice((n - 1) * size, n * size), total: packages.length, total_pages: Math.ceil(packages.length / size) };
        } else if (name.endsWith('/compatibility')) data = { package_revision: 1,
          reasons: { d30: '检查失败', d58: '', d46: '' } };
        else if (name === `${api}/operations`) data = { items: [operation], total: 1, total_pages: 1 };
        else if (name === `${api}/operations/${operation.id}`) data = operation;
        else throw new Error(`unhandled fixture read ${name}`);
        assert.notEqual(data, undefined, name); await route.fulfill({ json: data });
      } catch (error) {
        routeErrors.push(error.stack); await route.fulfill({ status: 500, json: { error: 'isolated fixture rejected the request' } });
      }
    });

    await page.goto(`${url}/devices`);
    await page.getByText('SSH认证失败，请核对已保存凭据', { exact: true }).waitFor();
    await page.getByRole('row').filter({ hasText: '192.168.10.30' })
      .getByRole('button', { name: '查看检查日志', exact: true }).waitFor();
    const historicalState = page.getByRole('row').filter({ hasText: '192.168.10.30' })
      .getByText('上次：程序运行', { exact: true }).first();
    await historicalState.waitFor();
    assert.equal((await historicalState.getAttribute('class') || '').split(/\s+/).includes('valid'), false);
    for (const width of [1366, 1920, 390]) {
      await page.setViewportSize({ width, height: width === 390 ? 844 : 900 });
      await page.waitForFunction(() => document.documentElement.scrollWidth <= innerWidth + 1);
      await page.screenshot({ path: path.join(output, `review-hosts-${width}.png`), fullPage: true, animations: 'disabled' });
    }
    await page.setViewportSize({ width: 1366, height: 768 });
    await page.getByRole('button', { name: '接入与首装记录', exact: true }).click();
    const records = page.getByTestId('host-setup-records');
    await records.getByText(/共 21 条/).waitFor();
    failSetupPage = true;
    await records.getByRole('button', { name: '下一页', exact: true }).click();
    await records.getByText(/隔离接入记录分页失败/).waitFor();
    await records.getByText(/第 1\s*\/ 2 页/).waitFor();
    failSetupPage = false;
    await records.getByRole('button', { name: '下一页', exact: true }).click();
    await records.getByText('original-setup-21', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'host-records-paged.png'), animations: 'disabled' });
    await records.getByRole('button', { name: '查看原作业', exact: true }).click();
    const enrollment = page.getByRole('dialog').filter({ hasText: '首次安装软件' });
    await enrollment.getByText('original-setup-21', { exact: true }).waitFor();
    await enrollment.getByRole('button', { name: '补查原首装结果', exact: true }).waitFor();
    await page.getByTestId('host-enrollment-next-step').waitFor();
    await page.screenshot({ path: path.join(output, 'original-setup-recovered.png'), animations: 'disabled' });
    assert.equal(writes.length, 0);
    checks.push('failed check is actionable; paginated server records survive failure and recover the original job without browser storage or resubmission');

    await page.goto(`${url}/packages`);
    await page.getByRole('button', { name: '上传软件包', exact: true }).click();
    const upload = page.getByRole('dialog').filter({ hasText: '上传软件包' });
    await upload.locator('input[type=file]').setInputFiles({ name: 'wrong.zip', mimeType: 'application/zip', buffer: Buffer.from('invalid fixture') });
    await upload.getByText('请选择不超过 256 MiB 的 .rcd.tar.gz 发布包。', { exact: true }).waitFor();
    await assert.equal(await upload.getByRole('button', { name: '上传并校验', exact: true }).isDisabled(), true);
    await upload.locator('input[type=file]').setInputFiles({ name: 'fixture.rcd.tar.gz', mimeType: 'application/gzip', buffer: Buffer.from('not uploaded') });
    await upload.getByText(/已选择 fixture.rcd.tar.gz/).waitFor();
    await page.screenshot({ path: path.join(output, 'upload-selection-feedback.png'), animations: 'disabled' });
    await upload.getByRole('button', { name: '关闭', exact: true }).click();
    assert.equal(writes.length, 0);
    checks.push('file type is checked on selection; valid selection explains server validation and never auto-uploads');

    failPackagePage = true;
    await page.getByRole('button', { name: '下一页', exact: true }).click();
    await page.getByText(/隔离软件包分页失败/).waitFor();
    await page.getByText(/保留上次成功读取的第 1 页/).waitFor();
    failPackagePage = false;
    await page.getByRole('button', { name: '下一页', exact: true }).click();
    await page.getByText('review-v21', { exact: true }).waitFor();
    await page.getByText(/第 2\s*\/ 2 页/).waitFor();
    checks.push('a failed next-page request retains the confirmed page and retries page two rather than skipping it');

    await page.goto(`${url}/operations?operation=${operation.id}`);
    const detail = page.locator('.details');
    await detail.getByRole('button', { name: '取消尚未执行的任务', exact: true }).click();
    const cancellation = page.getByRole('dialog').filter({ hasText: '取消尚未执行的任务？' });
    await cancellation.getByText(/192.168.10.46/).waitFor();
    assert.equal(writes.length, 0);
    await cancellation.getByRole('button', { name: '保留任务', exact: true }).click();
    await detail.getByRole('button', { name: '取消尚未执行的任务', exact: true }).click();
    await page.screenshot({ path: path.join(output, 'cancel-original-confirmation.png'), animations: 'disabled' });
    await cancellation.getByRole('button', { name: '确认取消此任务', exact: true }).click();
    await detail.getByText('已取消', { exact: true }).waitFor(); assert.equal(writes.length, 1);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.waitForFunction(() => document.documentElement.scrollWidth <= innerWidth + 1);
    await page.screenshot({ path: path.join(output, 'task-detail-mobile.png'), animations: 'disabled' });
    checks.push('queued cancellation requires target confirmation and sends one revision-bound fixture request; task detail fits mobile');

    await page.setViewportSize({ width: 1366, height: 768 });
    await page.goto(`${url}/packages`);
    const row = page.getByRole('row').filter({ hasText: 'review-v1' }).first();
    await row.getByRole('button', { name: '选择设备升级', exact: true }).click();
    const wizard = page.getByRole('dialog').filter({ hasText: '升级软件' });
    await wizard.getByRole('button', { name: '下一步', exact: true }).click();
    await wizard.getByText('RCD 46 · 192.168.10.46', { exact: true }).click();
    assert.equal(await wizard.getByLabel('RCD 46 · 192.168.10.46', { exact: true }).isChecked(), true);
    await wizard.getByRole('button', { name: '下一步', exact: true }).click();
    await wizard.getByText('已确认本次暂停所有相关管理端及直连客户端的控制命令', { exact: true }).click();
    assert.equal(await wizard.getByLabel('已确认本次暂停所有相关管理端及直连客户端的控制命令', { exact: true }).isChecked(), true);
    assert.equal(await wizard.getByRole('button', { name: '确认升级', exact: true }).isDisabled(), false);
    await page.clock.install({ time: new Date(now) });
    await page.clock.runFor(181000);
    await wizard.getByText(/部分目标状态已过期/).waitFor();
    assert.equal(await wizard.getByRole('button', { name: '确认升级', exact: true }).isDisabled(), true);
    await page.screenshot({ path: path.join(output, 'expired-upgrade-confirmation.png'), animations: 'disabled' });
    assert.equal(writes.length, 1);
    checks.push('an open final confirmation expires after 180 seconds without a new report; no upgrade is submitted');

    assert.deepEqual(routeErrors, []); assert.deepEqual(errors, []);
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ passed: true, checks,
      fixture_writes: writes, route_errors: routeErrors, page_errors: errors,
      native_installations: 0, native_service_controls: 0, hardware_commands: 0 }, null, 2));
    console.log(JSON.stringify({ passed: true, checks }));
  } catch (error) {
    await page.screenshot({ path: path.join(output, 'failure.png'), animations: 'disabled' }).catch(() => {});
    fs.writeFileSync(path.join(output, 'failure.json'), JSON.stringify({ error: error.stack, errors, routeErrors, checks }, null, 2));
    throw error;
  } finally { await context.close(); await browser.close(); }
})().catch((error) => { console.error(error); process.exitCode = 1; });
