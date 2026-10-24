// Run only on 46 against isolated native Semaphore. All RCD endpoints are
// intercepted below: fixture POSTs cannot create native jobs or reach SSH.
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
  const failedRoutes = []; page.on('pageerror', (e) => errors.push(e.message));
  try {
    await page.goto(`${config.url}/auth/login`);
    await page.getByTestId('auth-username').fill(config.auth);
    await page.getByTestId('auth-password').fill(config.password);
    await Promise.all([
      page.waitForResponse((r) => r.url().endsWith('/api/auth/login') && r.status() === 204),
      page.getByTestId('auth-signin').click(),
    ]);
    const response = await context.request.post(`${config.url}/api/projects`, {
      data: { name: '软件登记与主机接入隔离验收', alert: false, max_parallel_tasks: 1 },
      headers: { Origin: config.url },
    });
    assert.ok(response.ok(), await response.text()); const project = (await response.json()).id;
    const api = `/api/project/${project}/rcd`; const base = `${config.url}/project/${project}/rcd`;
    const now = new Date().toISOString(); let apps = [];
    const pkg = { id: 'new-app-v1', revision: 7, enabled: true, uploaded_at: now, archive_size: 1024,
      manifest: { component: 'plant-viewer', version: '1.0', release_id: 'fixture-app-release', target: 'linux/amd64',
        release_notes: '隔离界面测试，不执行软件安装', binary_sha256: 'a'.repeat(64),
        compatibility: { adapter_min_version: '1.1.0', state_formats: {} } } };
    const existingDevice = { id: 'existing-30', revision: 11, name: '已有主机30', address: '192.168.10.30',
      state: 'checked', last_success_at: now, release_template_id: 31, control_template_id: 32,
      components: ['lower', 'manager', 'agent'].map((component) => ({ component, state: 'stopped' })).concat([
        { component: 'communicate', state: 'not_installed' }, { component: 'plant-viewer', state: 'not_installed' },
      ]) };
    let existingSetup = null; let serviceOperation = null;
    let hostSetup = null; let createCount = 0; let installCount = 0;
    await page.route(`**${api}/**`, async (route) => {
      try {
        const request = route.request(); const url = new URL(request.url()); const name = url.pathname;
        const method = request.method(); const body = method === 'GET' ? null : request.postDataJSON();
        if (method !== 'GET') writes.push({ method, name, body,
          key: request.headers()['idempotency-key'], if_match: request.headers()['if-match'] });
        let data;
        if (name === `${api}/applications` && method === 'GET') data = { items: apps };
        else if (name === `${api}/applications` && method === 'POST') {
          assert.equal(body.component, 'plant-viewer'); assert.equal(body.graceful_shutdown_confirmed, true);
          assert.deepEqual(body.arguments, []);
          apps = [{ ...body, revision: 1, install_path: '/opt/rcd/apps/plant-viewer/bin/plant-viewer',
            work_dir: '/var/lib/rcd/apps/plant-viewer', service_unit: 'rcd-app-plant-viewer.service' }];
          await route.fulfill({ status: 201, json: apps[0] }); return;
        } else if (name === `${api}/devices` && method === 'GET') data = { items: [existingDevice], total: 1, total_pages: 1 };
        else if (name === `${api}/devices/existing-30` && method === 'GET') data = existingDevice;
        else if (name.endsWith('/history') && method === 'GET') data = { items: [], total: 0, total_pages: 0 };
        else if (name === `${api}/packages` && method === 'GET') {
          const include = !url.searchParams.get('component') || url.searchParams.get('component') === 'plant-viewer';
          data = { items: include ? [pkg] : [], total: include ? 1 : 0, total_pages: include ? 1 : 0 };
        } else if (name === `${api}/host-setups/options` && method === 'GET') data = {
          available: true, error: '', ssh_keys: [{ id: 31, name: '已有加密SSH凭据', type: 'ssh' }],
          become_keys: [{ id: 41, name: '已有加密sudo凭据', type: 'login_password' }],
        };
        else if (name === `${api}/host-setups` && method === 'POST') {
          createCount += 1;
          assert.equal(body.password, undefined); assert.equal(body.private_key, undefined);
          assert.equal(body.ssh_key_id, 31); assert.equal(body.become_key_id, 41);
          if (!hostSetup) hostSetup = { id: 'fixture-host-original', revision: 2, state: 'awaiting_host_key',
            task_id: 0, device_id: '', request: body, updated_at: now, steps: [],
            host_keys: [{ algorithm: 'ssh-ed25519', fingerprint: 'SHA256:fixtureKeyOne' },
              { algorithm: 'ecdsa-sha2-nistp256', fingerprint: 'SHA256:fixtureKeyTwo' }] };
          if (createCount === 1) { await route.abort('failed'); return; }
          data = hostSetup;
        } else if (name === `${api}/host-setups/fixture-host-original` && method === 'GET') data = hostSetup;
        else if (name === `${api}/devices/existing-30/first-installs` && method === 'POST') {
          assert.equal(body.revision, 11); assert.equal(body.package_id, pkg.id); assert.equal(body.package_revision, 7);
          assert.equal(request.headers()['if-match'], '"11"'); assert.ok(request.headers()['idempotency-key']);
          existingSetup = { id: 'fixture-existing-install', revision: 1, state: 'installation_queued', phase: 'install',
            existing_device: true, device_id: existingDevice.id, request: { name: existingDevice.name,
              address: existingDevice.address, ssh_port: 22 }, steps: [], host_keys: [], updated_at: now };
          data = existingSetup;
        } else if (name === `${api}/host-setups/fixture-existing-install` && method === 'GET') {
          existingSetup = { ...existingSetup, revision: 2, state: 'installed_stopped' };
          existingDevice.revision = 12;
          existingDevice.components.find((c) => c.component === 'plant-viewer').state = 'stopped';
          data = existingSetup;
        } else if (name === `${api}/operations` && method === 'POST') {
          assert.equal(body.kind, 'service_control'); assert.equal(body.component, 'plant-viewer');
          assert.equal(body.action, 'start'); assert.equal(body.commands_paused, true);
          assert.deepEqual(body.targets, [{ device_id: 'existing-30', revision: 12 }]);
          serviceOperation = { id: 'fixture-generic-start', revision: 1, state: 'succeeded', request: body,
            package: {}, targets: [{ name: '已有主机30', address: '192.168.10.30', device_id: 'existing-30', state: 'succeeded', stages: [] }],
            actor: 'isolated browser', created_at: now };
          data = serviceOperation;
        } else if (name === `${api}/operations` && method === 'GET') data = {
          items: serviceOperation ? [serviceOperation] : [], total: serviceOperation ? 1 : 0, total_pages: serviceOperation ? 1 : 0,
        };
        else if (name === `${api}/operations/fixture-generic-start` && method === 'GET') data = serviceOperation;
        else if (name.endsWith('/fixture-host-original/host-key') && method === 'POST') {
          assert.deepEqual(body.fingerprints, ['SHA256:fixtureKeyOne', 'SHA256:fixtureKeyTwo']);
          assert.equal(body.confirmed, true); assert.equal(body.revision, 2);
          assert.equal(request.headers()['if-match'], '"2"');
          hostSetup = { ...hostSetup, state: 'ready_for_install', revision: 3, platform: 'linux/amd64' };
          data = hostSetup;
        } else if (name.endsWith('/fixture-host-original/install') && method === 'POST') {
          installCount += 1; assert.equal(body.revision, 3); assert.equal(body.package_id, pkg.id);
          assert.equal(body.package_revision, 7); assert.equal(body.confirmed, true);
          assert.ok(request.headers()['idempotency-key']);
          hostSetup = { ...hostSetup, state: 'unknown', revision: 4, phase: 'install',
            install: { package_id: pkg.id }, original_install_task_id: 901, task_id: 901,
            error: '隔离注入：安装回执丢失' };
          await route.abort('failed'); return;
        } else if (name.endsWith('/fixture-host-original/reconcile') && method === 'POST') {
          assert.deepEqual(body, { revision: 4, confirmed: true });
          assert.equal(request.headers()['if-match'], '"4"');
          hostSetup = { ...hostSetup, phase: 'reconcile', state: 'reconcile_queued', revision: 5, task_id: 902, error: '' };
          data = hostSetup;
        } else throw new Error(`unhandled fixture route ${method} ${name}`);
        await route.fulfill({ json: data });
      } catch (e) { failedRoutes.push(e.message); await route.fulfill({ status: 500, json: { error: e.message } }); }
    });
    await page.goto(`${base}/packages`);
    await page.getByRole('button', { name: '添加软件种类', exact: true }).click();
    const definitions = page.locator('.software-definitions');
    await definitions.getByLabel('软件种类ID', { exact: true }).fill('plant-viewer');
    await definitions.getByLabel('显示名称', { exact: true }).fill('厂区可视软件');
    await definitions.getByLabel('启动参数（可选，每行一个参数）', { exact: true }).fill('$(whoami)');
    await definitions.getByText('已确认软件支持SIGTERM正常退出；平台不核实其设备事务排空', { exact: true }).click();
    assert.equal(await definitions.getByRole('button', { name: '登记软件种类', exact: true }).isDisabled(), true);
    await definitions.getByLabel('启动参数（可选，每行一个参数）', { exact: true }).fill('');
    await definitions.getByRole('button', { name: '登记软件种类', exact: true }).click();
    await definitions.getByText('已登记软件种类，尚未上传或安装。', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'software-definition-receipt.png') });
    await definitions.getByRole('button', { name: '上传该软件的版本包', exact: true }).click();
    const upload = page.getByRole('dialog').filter({ has: page.getByText('上传软件版本包：', { exact: false }) });
    await upload.getByText('没有发布包？查看格式和生成方式', { exact: true }).click();
    await upload.getByText(/--binary及--metadata/).waitFor();
    await page.screenshot({ path: path.join(output, 'upload-standard-package-help.png') });
    await upload.getByRole('button', { name: '关闭', exact: true }).click();
    await page.getByLabel('组件', { exact: true }).click();
    await page.getByRole('option', { name: '厂区可视软件', exact: true }).click();
    checks.push('register immutable software definition, reject shell arguments, show real upload help and dynamic component option');

    await page.goto(`${base}/devices`);
    await page.getByRole('button', { name: '添加主机', exact: true }).click();
    const host = page.locator('.host-enrollment');
    await host.getByLabel('主机名称', { exact: true }).fill('隔离主机99');
    await host.getByLabel('固定IP地址', { exact: true }).fill('192.168.10.99');
    await host.getByLabel('SSH登录账号', { exact: true }).fill('operator');
    await host.getByLabel('目标机已有的非root服务账户', { exact: true }).fill('operator');
    await host.getByLabel('已加密SSH凭据', { exact: true }).click();
    await page.getByRole('option', { name: '已有加密SSH凭据（ssh）', exact: true }).click();
    await host.getByLabel('sudo凭据', { exact: true }).click();
    await page.getByRole('option', { name: '已有加密sudo凭据（login_password）', exact: true }).click();
    await host.getByText('已获准访问该主机，确认IP及账号正确；后续仍需核对SSH主机指纹', { exact: true }).click();
    assert.equal(await host.locator('input[type="password"]').count(), 0);
    await host.getByRole('button', { name: '登记并获取主机指纹', exact: true }).click();
    await host.getByText('登记结果待确认，目标已锁定。请确认原请求，不能换编号重复创建作业。', { exact: true }).waitFor();
    await host.getByRole('button', { name: '确认原登记结果', exact: true }).click();
    await host.getByRole('heading', { name: '核对全部SSH主机指纹', exact: true }).waitFor();
    assert.equal(await host.getByRole('button', { name: '确认指纹并检查主机', exact: true }).isDisabled(), true);
    await host.getByText('SHA256:fixtureKeyOne', { exact: true }).waitFor();
    await host.getByText('SHA256:fixtureKeyTwo', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'host-key-all-fingerprints.png') });
    await host.getByText('已通过受信任渠道核对以上全部指纹，均与目标机一致', { exact: true }).click();
    await host.getByRole('button', { name: '确认指纹并检查主机', exact: true }).click();
    await host.getByRole('heading', { name: '主机检查通过 · 选择首次安装的软件', exact: true }).waitFor();
    await host.getByLabel('软件种类', { exact: true }).click();
    await page.getByRole('option', { name: '厂区可视软件', exact: true }).click();
    await host.getByLabel('软件版本包', { exact: true }).click();
    await page.getByRole('option', { name: '1.0 · linux/amd64 · fixture-app-release', exact: true }).click();
    await host.getByText('确认首次安装此组件；安装后保持停止，配置和串口须由使用人确认后再启用', { exact: true }).click();
    await page.screenshot({ path: path.join(output, 'host-initial-install-confirmation.png') });
    await host.getByRole('button', { name: '首次安装并保持停止', exact: true }).click();
    await host.getByText('原安装请求待确认，软件与版本已锁定；不会另建安装任务。', { exact: false }).waitFor();
    await host.getByRole('button', { name: '关闭', exact: true }).click();
    await page.getByRole('button', { name: '添加主机', exact: true }).click();
    await host.getByText('结果未知 · 请核对原作业', { exact: true }).waitFor();
    assert.equal(await host.getByRole('button', { name: '首次安装并保持停止', exact: true }).count(), 0);
    assert.equal(await host.getByRole('button', { name: '接入另一台主机', exact: true }).count(), 0);
    assert.equal(installCount, 1); assert.equal(createCount, 2);
    const creations = writes.filter((w) => w.name === `${api}/host-setups`);
    assert.equal(creations[0].key, creations[1].key); assert.deepEqual(creations[0].body, creations[1].body);
    await page.screenshot({ path: path.join(output, 'host-unknown-no-retry.png') });
    checks.push('encrypted credential references, all SSH keys pinned, lost registration uses same request, unknown install never requeues');
    assert.equal(await host.getByRole('button', { name: '补查原首装结果', exact: true }).isDisabled(), true);
    await host.getByText('确认仅核对原首装记录，不重新安装、不启停', { exact: true }).click();
    await host.getByRole('button', { name: '补查原首装结果', exact: true }).click();
    await host.getByText('正在只读补查原首装记录', { exact: true }).waitFor();
    await host.getByRole('button', { name: '查看原安装任务 #901 日志', exact: true }).waitFor();
    await host.getByRole('button', { name: '查看只读补查任务 #902 日志', exact: true }).waitFor();
    assert.equal(installCount, 1);
    await page.screenshot({ path: path.join(output, 'host-readonly-reconcile-separate-tasks.png') });
    checks.push('explicit read-only first-install reconciliation keeps original install log and separate read-only task, never installs again');
    hostSetup = { ...hostSetup, state: 'installed_stopped', revision: 6, error: '', device_id: '' };
    await host.getByRole('button', { name: '刷新原作业', exact: true }).click();
    await host.getByText('已安装 · 保持停止/待配置', { exact: true }).waitFor();
    await host.getByText(/已安装，保持停止、等待配置。软件安装成功不表示已开始采集/).waitFor();
    assert.equal(await host.getByText('程序运行', { exact: true }).count(), 0);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.waitForFunction(() => document.documentElement.scrollWidth <= innerWidth + 1,
      null, { timeout: 5000 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true);
    await page.screenshot({ path: path.join(output, 'host-installed-stopped-mobile.png'), fullPage: true });
    checks.push('initial install reports installed but stopped/configuration pending; mobile layout stays within viewport');
    await host.getByRole('button', { name: '关闭', exact: true }).click();
    await page.setViewportSize({ width: 1366, height: 768 });
    await page.goto(`${base}/devices?device=existing-30`);
    const appSection = page.locator('.component-detail').filter({ has: page.getByRole('heading', { name: '厂区可视软件', exact: true }) });
    await appSection.getByRole('button', { name: '首次安装', exact: true }).click();
    await host.getByText('已有主机30 · 192.168.10.30', { exact: true }).waitFor();
    assert.equal(await host.getByLabel('SSH登录账号', { exact: true }).count(), 0);
    await host.getByLabel('软件版本包', { exact: true }).click();
    await page.getByRole('option', { name: '1.0 · linux/amd64 · fixture-app-release', exact: true }).click();
    await host.getByText('确认首次安装此组件；安装后保持停止，配置和串口须由使用人确认后再启用', { exact: true }).click();
    await host.getByRole('button', { name: '首次安装并保持停止', exact: true }).click();
    await host.getByText('已安装 · 保持停止/待配置', { exact: true }).waitFor();
    await host.getByRole('button', { name: '关闭', exact: true }).click();
    // The native temporary drawer can close while its separate modal is open.
    // Reopen the same device as a user would, and require a fresh stopped read
    // before an explicit start; installation itself must never auto-start.
    await page.locator('tbody tr').filter({ hasText: '192.168.10.30' })
      .getByRole('button', { name: '查看详情', exact: true }).click();
    await appSection.getByText('已停止', { exact: true }).waitFor();
    await appSection.getByRole('button', { name: '启动', exact: true }).waitFor();
    await appSection.getByRole('button', { name: '启动', exact: true }).click();
    const startDialog = page.getByRole('dialog').filter({ hasText: '启动 厂区可视软件' });
    await startDialog.getByText('已确认本次暂停相关控制命令，允许执行所列操作', { exact: true }).click();
    assert.equal(await startDialog.getByRole('button', { name: '确认启动', exact: true }).isDisabled(), true);
    await startDialog.getByText('已完成必要配置，并确认本次启停安全；平台不核实设备事务排空', { exact: true }).click();
    await page.screenshot({ path: path.join(output, 'registered-software-start-confirmation.png') });
    await startDialog.getByRole('button', { name: '确认启动', exact: true }).click();
    await page.waitForURL(/\/operations\?operation=fixture-generic-start$/);
    await page.getByText('操作编号：fixture-generic-start', { exact: true }).waitFor();
    assert.equal(writes.filter((w) => w.name.endsWith('/first-installs')).length, 1);
    assert.equal(writes.filter((w) => w.name === `${api}/host-setups`).length, 2);
    checks.push('existing device installs one verified-absent component without creating another host; explicit generic start uses native operation contract');
    assert.deepEqual(failedRoutes, []); assert.deepEqual(errors, []);
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ passed: true, checks,
      page_errors: errors, fixture_post_count: writes.length, native_jobs: 0, ssh_calls: 0, hardware_commands: 0 }, null, 2));
    console.log(JSON.stringify({ passed: true, checks }));
  } catch (error) {
    await page.screenshot({ path: path.join(output, 'failure.png') }).catch(() => {});
    const layout = await page.evaluate(() => ({ width: innerWidth, scroll_width: document.documentElement.scrollWidth,
      overflow: [...document.querySelectorAll('body *')].map((element) => {
        const rect = element.getBoundingClientRect();
        return { tag: element.tagName, class_name: typeof element.className === 'string' ? element.className : '',
          left: rect.left, right: rect.right, width: rect.width };
      }).filter((element) => element.width > 0 && (element.right > innerWidth + 1 || element.left < -1)).slice(0, 30),
    })).catch(() => null);
    fs.writeFileSync(path.join(output, 'failure.json'), JSON.stringify({ error: error.stack, checks,
      page_errors: errors, fixture_route_errors: failedRoutes, layout }, null, 2));
    throw error;
  } finally { await context.close(); await browser.close(); }
})().catch((error) => { console.error(error); process.exitCode = 1; });
