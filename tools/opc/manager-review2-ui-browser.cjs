// Run ONLY on 46 after building manager/web/dist. This serves the real built
// frontend on a random loopback port; every API response is an in-memory
// fixture. No live manager, OPC endpoint, serial device or SSH target is used.
// RCD_BROWSER_OUTPUT must name a new absolute evidence directory.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');

const dist = path.resolve(__dirname, '../../manager/web/dist');
const output = process.env.RCD_BROWSER_OUTPUT;
assert.ok(fs.existsSync(path.join(dist, 'index.html')), 'Build the frontend on 46 first');
assert.ok(output && path.isAbsolute(output) && !fs.existsSync(output), 'New absolute evidence directory required');

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

async function awaitSignal(signal, label) {
  let timer;
  try {
    await Promise.race([signal.promise, new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`Timed out waiting for ${label}`)), 15000);
    })]);
  } finally { clearTimeout(timer); }
}

const errors = [], fixtureErrors = [], blocked = [], requests = [], checks = [], deviceVersionLayout = [];
const group = { id: 'fixture-line', name: '隔离逻辑组', default_source_id: 'fixture-a', revision: 3,
  source_count: 2, pending_command_count: 0, total_points: 1, good_points: 1, bad_points: 0 };
const sources = ['fixture-a', 'fixture-b'].map((id) => ({ id, name: id === 'fixture-a' ? '隔离来源 A' : '隔离来源 B',
  group_id: group.id, endpoint: `opc.tcp://${id}.invalid:4841`, enabled: true, revision: 7,
  security_policy: 'None', security_mode: 'None', has_password: false, catalog_state: 'Ready',
  connection_state: 'Connected', session_state: 'Active', subscription_state: 'Active',
  available_group_ids: [group.id], monitored_count: 1, good_points: 1, bad_points: 0, total_points: 1 }));
const station = { source_id: 'fixture-a', source_name: '隔离来源 A', source_revision: 7,
  port_id: '/fixture/PORT0', port_name: '隔离串口', device_id: '11', device_name: '隔离站点 11',
  configured_module_type: 'unknown', detected_module_type: 'unknown', version_state: 'never_queried',
  station_revision: '1', version_supported: true, source_online: true, port_enabled: true,
  port_applied_enabled: true, port_open: true, device_read_enabled: true, applied_device_read_enabled: true,
  communication_ok: true, control_pending: false, pending_commands: 0 };
let groupRevision = 3;
let closing = false;
const sourceAccepted = deferred(), releaseSource = deferred(), sourceFulfilled = deferred();
let configDocument = { source_id: 'fixture-a', source_revision: 7, name: 'config.toml', kind: 'toml',
  revision: 'file-1', active_revision: 'file-1', restart_required: false, size_bytes: 40,
  content: '# preserve fixture comment\n[settings]\ncommand_retry_count = 3\n', backups: [] };

// Static server deliberately cannot proxy API requests or perform mutations.
const server = http.createServer((req, res) => {
  if (!['GET', 'HEAD'].includes(req.method) || new URL(req.url, 'http://localhost').pathname.startsWith('/api/')) {
    fixtureErrors.push(`Request escaped fixture routing: ${req.method} ${req.url}`);
    res.writeHead(405).end(); return;
  }
  const relative = new URL(req.url, 'http://localhost').pathname;
  const candidate = path.resolve(dist, '.' + relative);
  if (candidate !== dist && !candidate.startsWith(dist + path.sep)) { res.writeHead(403).end(); return; }
  const file = fs.existsSync(candidate) && fs.statSync(candidate).isFile() ? candidate : path.join(dist, 'index.html');
  const contentType = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' }[path.extname(file)] || 'application/octet-stream';
  res.writeHead(200, { 'Content-Type': contentType, 'Cache-Control': 'no-store' });
  if (req.method === 'HEAD') { res.end(); return; }
  fs.createReadStream(file).on('error', (error) => { fixtureErrors.push(error.message); res.destroy(); }).pipe(res);
});

(async () => {
  fs.mkdirSync(output, { recursive: false, mode: 0o700 });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const base = `http://127.0.0.1:${server.address().port}`;
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1366, height: 768 }, locale: 'zh-CN', serviceWorkers: 'block' });
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (error) => errors.push(error.message));
  await context.route('**/*', async (route) => {
    const req = route.request(), url = new URL(req.url()), method = req.method(), p = url.pathname;
    if (url.origin !== base) {
      blocked.push({ method, url: url.href });
      await route.abort('blockedbyclient'); return;
    }
    if (!p.startsWith('/api/')) {
      if (!['GET', 'HEAD'].includes(method)) {
        blocked.push({ method, url: url.href }); await route.abort('blockedbyclient'); return;
      }
      await route.continue(); return;
    }
    const item = { method, path: p, query: url.search };
    if (method !== 'GET') { item.if_match = req.headers()['if-match']; item.body = req.postDataJSON(); }
    requests.push(item);
    const json = (value, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) });
    try {
      const configPrefix = '/api/v1/sources/fixture-a/operations';
      if (method === 'POST' && p === configPrefix + '/config/prepare') {
        const edits = item.body.edits;
        assert.equal(item.body.source_revision, 7);
        for (const edit of edits) assert.deepEqual(edit, { path: ['settings', 'command_interval_ms'], value: 500 });
        await json({ source_id: 'fixture-a', source_revision: 7,
          content: edits.length ? item.body.content + 'command_interval_ms = 500\n' : item.body.content,
          fields: [{ path: ['settings', 'command_interval_ms'], key: 'settings.command_interval_ms',
            label: '控制命令最小间隔(ms)', hint: '0..60000 毫秒，保存后重启下级生效；不改变轮询和同条重试',
            kind: 'integer', value: item.body.content.includes('command_interval_ms') ? 500 : 0,
            options: [], inherited: !item.body.content.includes('command_interval_ms'), readonly: false },
            { path: ['logging', 'http_push', 'queue_capacity'], key: 'logging.http_push.queue_capacity', label: 'logging.http_push.queue_capacity', hint: '保存后重启生效；完整校验以下级返回为准。', kind: 'integer', value: 10000, options: [], inherited: false, readonly: false }] }); return;
      }
      if (method === 'POST' && p === configPrefix + '/config/validate') {
        assert.ok(item.body.content.includes('command_interval_ms = 500'));
        await json({ valid: true, errors: [], warnings: ['rcd_example.csv: 第 83 行 EXAMPLE_AI4: byte_index=20 超出状态数据长度 1..=19'], restart_required: true }); return;
      }
      if (method === 'PUT' && p === configPrefix + '/config') {
        assert.equal(item.if_match, '"file-1"');
        assert.equal(item.body.source_revision, 7);
        assert.equal(item.body.name, 'config.toml');
        assert.ok(item.body.content.startsWith('# preserve fixture comment'));
        assert.ok(item.body.content.includes('command_interval_ms = 500'));
        configDocument = { ...configDocument, content: item.body.content, revision: 'file-2', restart_required: true };
        await json(configDocument); return;
      }
      if (method === 'PUT' && p === '/api/v1/sources/fixture-a') {
        assert.equal(requests.filter((entry) => entry.method === 'PUT' && entry.path === p).length, 1, 'No repeated fixture source update');
        sourceAccepted.resolve();
        await releaseSource.promise;
        await json({ ...sources[0], ...item.body, revision: 8 });
        sourceFulfilled.resolve(); return;
      }
      if (method === 'PUT' && p === '/api/v1/groups/' + group.id) {
        await json({ error: { code: 'revision_conflict', message: '分组 revision 冲突（隔离夹具）' } }, 409); return;
      }
      if (method === 'PUT' && p === '/api/v1/sources/fixture-a/operations/device-module-type') {
        await json({ error: { code: 'revision_conflict', message: '站点版本已变化（隔离夹具）' } }, 409); return;
      }
      assert.equal(method, 'GET', `Unexpected fixture write ${method} ${p}`);
      if (p === configPrefix + '/capabilities') {
        await json({ source_id: 'fixture-a', source_revision: 7, supported: true, schema_version: 1,
          features: ['configuration', 'configuration_forms'], configuration_apply: 'restart', max_config_bytes: 2097152 }); return;
      }
      if (p === '/api/v1/sources/fixture-a/process/binding') {
        await json({ source_id: 'fixture-a', source_revision: 7, revision: 0, agent_url: '', instance_id: '' }); return;
      }
      if (p === configPrefix + '/configs') {
        await json({ source_id: 'fixture-a', source_revision: 7, items: [{ ...configDocument, content: undefined, backups: undefined }] }); return;
      }
      if (p === configPrefix + '/config') { await json(configDocument); return; }
      if (p === '/api/v1/events') {
        await route.fulfill({ contentType: 'text/event-stream', body: 'retry: 60000\n: isolated\n\n' }); return;
      }
      if (p === '/api/v1/auth/session') { await json({ authenticated: true, username: 'isolated-fixture' }); return; }
      if (p === '/api/v1/overview') {
        await json({ manager_state: 'Running', version: 'isolated-frontend', point_count: 1,
          data_quality: { good: 1, bad: 0, total: 1 }, source_count: 2, connected_sources: 2 }); return;
      }
      if (p === '/api/v1/groups') { await json([{ ...group, revision: groupRevision }]); return; }
      if (p === '/api/v1/sources') { await json(sources); return; }
      if (p === '/api/v1/sessions') { await json([]); return; }
      if (/^\/api\/v1\/sources\/fixture-[ab]\/topology$/.test(p)) {
        await json({ source_id: p.split('/')[4], group_id: group.id, supported: true, revision: 7,
          catalog_state: 'Ready', schema_version: 1, ports: [{ port_id: station.port_id, port_name: station.port_name,
            devices: [{ device_id: '11', device_address: 11, name: station.device_name, group_id: group.id, point_count: 1 }] }] }); return;
      }
      if (p === '/api/v1/device-versions') {
        await json({ items: [station], total: 1, page: 1, page_size: 50,
          summary: { total: 1, domestic: 0, legacy_russian: 0, unknown: 1, mismatch: 0, failed: 0, offline_cached: 0, lower_software_versions: [] } }); return;
      }
      if (p === '/api/v1/points') {
        const offset = Number(url.searchParams.get('offset') || 0);
        const source = url.searchParams.get('route_source_id') || 'fixture-a';
        const point = { node_id: 'FIXTURE_VALUE', browse_name: 'FIXTURE_VALUE', group_id: group.id,
          node_class: 'Variable', data_type: 'Double', value: source === 'fixture-b' ? 22 : 11,
          status_code: 0, source_present: true, selected_source_id: source };
        await json({ items: offset ? [] : [point], total: 1, limit: 50, offset }); return;
      }
      throw new Error(`No fixture for ${method} ${p}`);
    } catch (error) {
      if (!closing) fixtureErrors.push(error.stack || error.message);
      await route.abort('failed').catch(() => {});
    }
  });

  const capture = async (name, checkDeviceVersionLayout = false) => {
    for (const width of [1366, 1920]) {
      await page.setViewportSize({ width, height: width === 1366 ? 768 : 1080 });
      await page.waitForFunction(() => document.documentElement.scrollWidth <= innerWidth + 1, null, { timeout: 5000 });
      if (checkDeviceVersionLayout) {
        const table = page.locator('.device-version-table');
        const row = table.locator('tbody tr').first();
        const scroll = table.locator('..');
        const widths = await scroll.evaluate((element) => ({ client: element.clientWidth, scroll: element.scrollWidth, left: element.scrollLeft }));
        assert.ok(widths.scroll <= widths.client + 1, `Device versions must not need horizontal table scrolling at ${width}: ${JSON.stringify(widths)}`);
        assert.equal(widths.left, 0, 'Actions must be visible without moving the horizontal scroll position');
        const operation = row.locator('.device-version-operation.operation-error');
        await operation.scrollIntoViewIfNeeded();
        const visibility = await row.evaluate((element) => {
          const container = element.closest('.table-scroll').getBoundingClientRect();
          const targets = [...element.querySelectorAll('.device-version-actions button, .device-version-operation.operation-error')];
          return targets.map((target) => {
            const rect = target.getBoundingClientRect();
            const range = document.createRange(); range.selectNodeContents(target);
            const text = range.getBoundingClientRect();
            return { label: target.textContent, left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom,
              text_left: text.left, text_right: text.right, container_left: container.left, container_right: container.right,
              within_viewport: rect.left >= 0 && rect.right <= innerWidth + 1 && rect.top >= 0 && rect.bottom <= innerHeight + 1,
              within_table: rect.left >= container.left - 1 && rect.right <= container.right + 1,
              text_within_table: text.left >= container.left - 1 && text.right <= container.right + 1 };
          });
        });
        assert.equal(visibility.length, 3, 'Check both actions and the complete operation error');
        assert.ok(visibility.every((target) => target.within_viewport && target.within_table && target.text_within_table), `Clipped device actions/error at ${width}: ${JSON.stringify(visibility)}`);
        deviceVersionLayout.push({ width, widths, visibility });
      }
      await page.screenshot({ path: path.join(output, `${name}-${width}.png`), animations: 'disabled' });
    }
  };
  const save = (name, value) => fs.writeFileSync(path.join(output, name), JSON.stringify(value, null, 2), { mode: 0o600 });
  try {
    await page.goto(base + '/#/sources');
    await page.getByRole('button', { name: '编辑 fixture-a', exact: true }).click();
    let dialog = page.getByRole('dialog', { name: '编辑下级 OPC', exact: true });
    await dialog.getByLabel('显示名称', { exact: true }).fill('正在提交的来源 A');
    await dialog.getByRole('button', { name: '保存并应用', exact: true }).click();
    await awaitSignal(sourceAccepted, 'held source update');
    // The request can reach the fixture before React commits mutation pending
    // to the DOM. Wait for the actual disabled control, not a fixed sleep.
    await dialog.locator('label:has(> span:text-is("显示名称")) input:disabled').waitFor({ state: 'attached' });
    assert.ok(await dialog.getByLabel('显示名称', { exact: true }).isDisabled());
    await dialog.getByRole('button', { name: '关闭（请求继续执行）', exact: true }).click();
    await page.getByRole('button', { name: '编辑 fixture-b', exact: true }).click();
    dialog = page.getByRole('dialog', { name: '编辑下级 OPC', exact: true });
    await dialog.getByLabel('显示名称', { exact: true }).fill('来源 B 未保存草稿');
    const completionReads = Promise.all(['/api/v1/sources', '/api/v1/groups', '/api/v1/overview'].map((pathname) =>
      page.waitForResponse((response) => new URL(response.url()).pathname === pathname && response.request().method() === 'GET')));
    releaseSource.resolve();
    await awaitSignal(sourceFulfilled, 'old source update response');
    await completionReads;
    // The old callback invalidates several list queries before it could close
    // the new editor. Observe a subsequent source refresh and drain rendering.
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await dialog.getByLabel('Source ID', { exact: true }).inputValue(), 'fixture-b');
    assert.equal(await dialog.getByLabel('显示名称', { exact: true }).inputValue(), '来源 B 未保存草稿');
    await capture('new-source-editor-survives-old-save');
    await dialog.getByRole('button', { name: '取消', exact: true }).click();
    checks.push({ name: 'old-source-save-does-not-close-new-editor', passed: true });

    await page.goto(base + '/#/groups');
    await page.getByRole('button', { name: `编辑 ${group.id}`, exact: true }).click();
    dialog = page.getByRole('dialog', { name: '编辑分组', exact: true });
    await dialog.getByLabel('显示名称', { exact: true }).fill('分组未保存草稿');
    groupRevision = 4;
    const refreshed = page.waitForResponse((response) => new URL(response.url()).pathname === '/api/v1/groups' && response.request().method() === 'GET');
    await page.evaluate(() => document.querySelector('.context-actions button').click());
    await refreshed;
    assert.equal(await dialog.getByLabel('显示名称', { exact: true }).inputValue(), '分组未保存草稿');
    await dialog.getByRole('button', { name: '保存', exact: true }).click();
    await dialog.getByText('分组 revision 冲突（隔离夹具）', { exact: true }).waitFor();
    const groupWrite = requests.filter((entry) => entry.method === 'PUT' && entry.path === '/api/v1/groups/' + group.id);
    assert.equal(groupWrite.length, 1);
    assert.equal(groupWrite[0].if_match, '"3"', 'Background refresh must not upgrade the editing revision');
    assert.equal(groupWrite[0].body.name, '分组未保存草稿');
    await capture('group-original-revision-conflict');
    await dialog.getByRole('button', { name: '取消', exact: true }).click();
    checks.push({ name: 'group-draft-keeps-original-revision-after-refresh', passed: true, submitted_revision: 3, current_revision: 4 });

    await page.goto(base + '/#/device-versions');
    await page.getByRole('button', { name: '设置类型', exact: true }).click();
    dialog = page.getByRole('dialog', { name: '设置站点模块类型', exact: true });
    await dialog.locator('label:has(> span:text-is("模块类型")) select').selectOption('legacy_russian');
    await dialog.getByRole('button', { name: '确认保存', exact: true }).click();
    await page.getByText(/操作冲突，请刷新并重新核对目标；未自动重试/).waitFor();
    await page.waitForTimeout(150);
    const versionWrites = requests.filter((entry) => entry.path.endsWith('/device-module-type') && entry.method === 'PUT');
    assert.equal(versionWrites.length, 1, 'Do not automatically retry the old station intent');
    assert.equal(requests.filter((entry) => entry.path.endsWith('/operations/status')).length, 0, 'Do not silently read and rebase to a newer station revision');
    await capture('station-conflict-without-retry', true);
    checks.push({ name: 'station-conflict-is-explicit-without-rebase-or-resubmit', passed: true, write_count: 1 });

    for (const compare of [false, true]) {
      const route = `/#/points?group=${group.id}&page=9${compare ? '&compare=1&left=fixture-a&right=fixture-b' : ''}`;
      await page.goto(base + route);
      await page.getByRole('button', { name: '返回末页', exact: true }).click();
      await page.getByText('FIXTURE_VALUE', { exact: true }).waitFor();
      await page.getByText('1 / 1 页', { exact: true }).waitFor();
      assert.ok(!new URL(page.url()).hash.includes('page=9'));
      await capture(compare ? 'comparison-empty-page-recovered' : 'points-empty-page-recovered');
      checks.push({ name: compare ? 'comparison-empty-page-recovers' : 'points-empty-page-recovers', passed: true });
    }
    await page.goto(base + '/#/sources');
    await page.getByRole('button', { name: '运维 fixture-a', exact: true }).click();
    const interval = page.getByLabel('控制命令最小间隔(ms)', { exact: true });
    await interval.waitFor();
    assert.equal(await page.getByLabel('日志内存队列容量（条）', { exact: true }).count(), 0, 'Default form must hide advanced logging internals');
    assert.equal(await page.getByRole('button', { name: '生成配置草稿', exact: true }).count(), 0, 'No extra draft-generation step');
    await page.getByLabel('参数显示范围').selectOption('日志');
    assert.equal(await page.getByLabel('日志内存队列容量（条）', { exact: true }).inputValue(), '10000');
    await capture('configuration-advanced-logging');
    await page.getByLabel('参数显示范围').selectOption('常用参数');
    await interval.fill('500');
    assert.ok(await page.getByRole('button', { name: '保存到下级', exact: true }).isDisabled());
    await page.getByRole('button', { name: '校验当前草稿', exact: true }).click();
    const saveConfig = page.getByRole('button', { name: '保存到下级', exact: true });
    await saveConfig.locator('xpath=self::*[not(@disabled)]').waitFor();
    await interval.waitFor();
    assert.equal(await interval.inputValue(), '500');
    await page.getByText('配置可以保存，以下提示不阻止保存', { exact: true }).waitFor();
    await capture('lower-command-interval-before-save');
    await page.setViewportSize({ width: 390, height: 844 });
    await saveConfig.scrollIntoViewIfNeeded();
    assert.ok(await saveConfig.isVisible());
    await page.waitForFunction(() => document.documentElement.scrollWidth <= innerWidth + 1, null, { timeout: 5000 });
    await page.screenshot({ path: path.join(output, 'configuration-mobile-390.png'), animations: 'disabled' });
    await page.setViewportSize({ width: 1366, height: 768 });
    await saveConfig.click();
    await page.getByText('配置已保存到下级文件；需要手动重启下级后生效，本操作未重启程序。', { exact: true }).waitFor();
    assert.equal(requests.filter((entry) => entry.method === 'PUT' && entry.path.endsWith('/operations/config')).length, 1);
    assert.ok(!requests.some((entry) => entry.path.includes('/process/') && entry.method !== 'GET'), 'Saving must never restart a process');
    await capture('lower-command-interval-saved-pending-restart');
    checks.push({ name: 'lower-command-interval-one-validation-cas-save-with-point-warning-without-restart', passed: true, interval_ms: 500, configuration_viewports: [390, 1366, 1920] });
    assert.deepEqual(errors, [], 'No browser page errors');
    assert.deepEqual(fixtureErrors, [], 'Every API request must have an explicit deterministic fixture');
    assert.deepEqual(blocked, [], 'No request may leave the random loopback origin');
    save('result.json', { passed: true, checks, requests, page_errors: errors, fixture_errors: fixtureErrors,
      blocked_requests: blocked, fixtures: true, real_device_acceptance: false, live_writes: 0,
      fixture_writes: requests.filter((entry) => !['GET', 'HEAD'].includes(entry.method)).length,
      viewports: [1366, 1920], device_version_layout: deviceVersionLayout });
    console.log(JSON.stringify({ passed: true, checks, real_device_acceptance: false, live_writes: 0 }));
  } catch (error) {
    await page.screenshot({ path: path.join(output, 'failure.png') }).catch(() => {});
    save('failure-layout.json', await page.evaluate(() => ({ width: innerWidth, scrollWidth: document.documentElement.scrollWidth, elements: [...document.querySelectorAll('body *')].map((node) => { const rect = node.getBoundingClientRect(); return { tag: node.tagName, class: node.className, text: node.textContent.slice(0, 100), left: rect.left, right: rect.right, width: rect.width }; }).filter((node) => node.right > innerWidth + 1 || node.left < -1).slice(0, 100) })).catch(() => ({})));
    save('failure.json', { error: error.stack, checks, requests, errors, fixture_errors: fixtureErrors, blocked_requests: blocked });
    throw error;
  } finally {
    closing = true;
    releaseSource.resolve();
    await context.close();
    await browser.close();
    await new Promise((resolve) => server.close(resolve));
  }
})().catch((error) => { console.error(error); server.close(); process.exitCode = 1; });
