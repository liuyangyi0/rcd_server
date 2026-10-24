// Remote46 UI-only check against built assets. No production or device APIs.
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const assert = require('node:assert/strict');
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../..');
const dist = path.join(root, 'manager/web/dist');
const output = path.resolve(process.env.RCD_BROWSER_OUTPUT || path.join(root, 'artifacts/runs/process-maintenance-browser'));
const binding = { source_id: 'fixture', source_revision: 2, revision: 4, agent_url: 'http://fixture:8090', instance_id: 'fixture-agent', registry_device_id: 'device-fixture' };
const base = { source_id: 'fixture', source_revision: 2, binding_revision: 4, instance_id: 'fixture-agent', schema_version: 1, revision: 'r1', state: 'stopped', desired_state: 'stopped', executable: '/fixture/rcd_server', work_dir: '/fixture' };
const deployment = 'release-' + 'a'.repeat(120);
const agentIdentity = { build: { schema_version: 1, component: 'agent', version: '0.6.1-ui-test', commit: 'a'.repeat(40), built_at: '2026-09-28T00:00:00Z', release_id: 'isolated-agent-build', source_sha256: 'b'.repeat(64), dirty: true, target: 'linux/amd64' }, boot_id: 'isolated-agent-boot', started_at: '2026-09-28T01:00:00Z', reported_at: '2026-09-28T02:00:00Z' };
let status = { ...base, agent_identity: agentIdentity, maintenance_enabled: true, maintenance_deployment_id: deployment, maintenance_phase: 'stopped' };
let unavailable = false;
const eventStreams = new Set();
const server = http.createServer((req, res) => {
  if (new URL(req.url, 'http://localhost').pathname === '/api/v1/events') {
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' });
    res.write(': isolated event stream\n\n');
    eventStreams.add(res);
    req.on('close', () => eventStreams.delete(res));
    return;
  }
  const candidate = path.resolve(dist, '.' + new URL(req.url, 'http://localhost').pathname);
  if (!candidate.startsWith(dist + path.sep)) { res.writeHead(403).end(); return; }
  const file = fs.existsSync(candidate) && fs.statSync(candidate).isFile() ? candidate : path.join(dist, 'index.html');
  const type = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml' }[path.extname(file)] || 'application/octet-stream';
  res.writeHead(200, { 'Content-Type': type }); fs.createReadStream(file).pipe(res);
});
(async () => {
  fs.mkdirSync(output, { recursive: true });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const browser = await chromium.launch({ headless: true });
  const errors = [], mutations = [], checks = [];
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    page.on('pageerror', e => errors.push(e.message));
    await page.route('**/api/v1/**', async route => {
      if (route.request().method() !== 'GET') { mutations.push(route.request().url()); await route.abort(); return; }
      const url = new URL(route.request().url()), p = url.pathname;
      let value = { items: [], total: 0, limit: 20, offset: 0 };
      if (p.endsWith('/auth/session')) value = { authenticated: true, username: 'fixture' };
      else if (p === '/api/v1/fleet') value = { enabled: true, platform_url: 'http://platform/project/1/rcd/devices', last_sync_at: '2026-10-09T01:00:00Z', error: '平台离线（隔离样例）', sources: [{ source_id: 'fixture', device_id: 'device-fixture', platform_url: 'http://platform/project/1/rcd/devices?device=device-fixture', applied_at: '2026-10-09T01:00:00Z', machine: { name: '46 隔离来源', address: '192.168.10.46', active_operation_id: 'original-platform-operation' } }] };
      else if (p === '/api/v1/groups') value = [{ id: 'line', name: '隔离测试组' }];
      else if (p === '/api/v1/sources') value = { items: [{ id: 'fixture', name: '隔离界面测试', group_id: 'line', endpoint: 'opc.tcp://fixture:4841', enabled: true, revision: 2, connection_state: 'Connected' }], total: 1 };
      else if (p.endsWith('/process/binding')) value = binding;
      else if (p.endsWith('/process/status')) {
        if (unavailable) { await route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: { code: 'unavailable', message: 'isolated agent unavailable' } }) }); return; }
        value = status;
      } else if (p.endsWith('/process/operations')) value = { ...value, source_id: 'fixture', source_revision: 2 };
      else if (p.endsWith('/process/log')) value = { ...base, text: '隔离日志：维护期间仍可读取', size_bytes: 50, truncated: false };
      else if (p.endsWith('/operations/capabilities')) value = { source_id: 'fixture', source_revision: 2, schema_version: 1, supported: false, features: [] };
      await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) });
    });
    await page.route('**/healthz', route => route.fulfill({ contentType: 'application/json', body: '{}' }));
    // Keep one real named SSE connection. Reconnecting fixture streams would
    // invalidate every query and conceal a missing source.process listener.
    await page.route('**/events', route => route.continue());
    await page.goto(`http://127.0.0.1:${server.address().port}/index.html#/sources?operations=fixture`);
    await page.getByRole('tab', { name: '进程', exact: true }).click();
    const notice = page.getByRole('region', { name: '发布维护状态' });
    await notice.getByText('发布维护未结束', { exact: true }).waitFor();
    const identity = page.getByRole('region', { name: '守护代理运行版本' });
    await identity.getByText('0.6.1-ui-test', { exact: true }).waitFor();
    await identity.scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(output, 'agent-identity-desktop.png') });
    assert.equal(await identity.locator('details').getAttribute('open'), null);
    await identity.getByText('构建与启动详情', { exact: true }).click();
    await identity.getByText('isolated-agent-boot', { exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'agent-identity-details.png') });
    checks.push('running agent identity is labelled separately from lower and manager');
    for (const name of ['启动下级', '停止下级', '重启下级', '编辑守护绑定']) assert.equal(await page.getByRole('button', { name, exact: true }).isDisabled(), true);
    await notice.scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(output, 'maintenance-desktop.png') });
    checks.push('active stopped maintenance locks controls and binding');
    await page.getByRole('button', { name: '读取守护日志', exact: true }).click();
    await page.getByLabel('守护日志内容').waitFor();
    assert.match(await page.getByLabel('守护日志内容').innerText(), /隔离日志：维护期间仍可读取/);
    checks.push('logs remain readable');
    unavailable = true;
    await page.getByRole('button', { name: '刷新进程与作业', exact: true }).click();
    await notice.getByText('维护旧状态，仅供核对').waitFor();
    await identity.getByText('代理版本为旧记录，仅供核对；本次未取得最新状态。', { exact: true }).waitFor();
    checks.push('agent identity is retained and marked stale after 503');
    assert.equal(await page.getByRole('button', { name: '启动下级', exact: true }).isDisabled(), true);
    await notice.scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(output, 'maintenance-stale.png') });
    checks.push('503 preserves stale maintenance without enabling actions');
    unavailable = false; status = { ...status, maintenance_phase: 'future_phase' };
    await page.getByRole('button', { name: '刷新进程与作业', exact: true }).click();
    await notice.getByText(/暂不识别的维护阶段/).waitFor();
    await page.setViewportSize({ width: 390, height: 844 });
    await notice.scrollIntoViewIfNeeded();
    assert.equal(await notice.evaluate(el => el.scrollWidth <= el.clientWidth + 1), true, 'long deployment ID must wrap');
    await page.screenshot({ path: path.join(output, 'maintenance-mobile.png') });
    await identity.scrollIntoViewIfNeeded();
    assert.equal(await identity.evaluate(el => el.scrollWidth <= el.clientWidth + 1), true, 'agent source hash must wrap');
    await page.screenshot({ path: path.join(output, 'agent-identity-mobile.png') });
    checks.push('unknown phase remains fenced; 128-character ID wraps on narrow viewport');
    status = { ...base, maintenance_enabled: true, state: 'running', desired_state: 'running', pid: 140 };
    await page.getByRole('button', { name: '刷新进程与作业', exact: true }).click();
    await notice.getByText('已启用发布维护保护', { exact: true }).waitFor();
    assert.equal(await page.getByRole('button', { name: '重启下级', exact: true }).isDisabled(), true);
    checks.push('configured idle protection blocks ordinary restart');
    status = { ...base };
    await page.getByRole('button', { name: '刷新进程与作业', exact: true }).click();
    await notice.getByText('代理未报告维护能力', { exact: true }).waitFor();
    await identity.getByText(/此代理未报告运行版本/).waitFor();
    assert.equal(await page.getByRole('button', { name: '启动下级', exact: true }).isDisabled(), true);
    checks.push('legacy agent has no unsafe lifecycle fallback');
    status = { ...base, lifecycle_supported: true, gateway_instance_id: 'stable-gateway', manual_stopped: true };
    await page.getByRole('button', { name: '刷新进程与作业', exact: true }).click();
    const safe = page.getByRole('region', { name: '安全运维状态' });
    await safe.getByText('下级已人为停止', { exact: true }).waitFor();
    assert.equal(await page.getByRole('button', { name: '启动下级', exact: true }).isEnabled(), true);
    const registry = page.getByRole('region', { name: '统一机器台账' });
    await registry.getByText(/当前为缓存信息/).waitFor();
    assert.match(await registry.innerText(), /original-platform-operation/);
    assert.match(await registry.getByRole('link', { name: '软件升级 / 版本恢复' }).getAttribute('href'), /device=device-fixture/);
    for (const width of [1366, 1920]) {
      await page.setViewportSize({ width, height: width === 1366 ? 768 : 1080 });
      await safe.scrollIntoViewIfNeeded();
      assert.equal(await safe.evaluate(el => el.scrollWidth <= el.clientWidth + 1), true);
      await page.screenshot({ path: path.join(output, `unified-stopped-${width}.png`) });
    }
    checks.push('manual stop is distinct, explicit safe start enabled, offline registry and original operation remain visible');
    status = { ...status, manual_stopped: false, lifecycle: { request_id: 'original-request', action: 'restart', manager_instance_id: 'manager-30', actor: 'operator', state: 'unknown', phase: 'unknown', error: '回应丢失，核对原操作' } };
    await page.getByRole('button', { name: '刷新进程与作业', exact: true }).click();
    await safe.getByText('目标机器正在维护或结果待确认', { exact: true }).waitFor();
    assert.match(await safe.innerText(), /manager-30/);
    assert.equal(await page.getByRole('button', { name: '启动下级', exact: true }).isDisabled(), true);
    checks.push('another manager unknown operation is visible and blocks replay');
    binding.revision += 1;
    binding.agent_url = 'http://fixture-rebound:8090';
    status = { ...base, binding_revision: binding.revision, lifecycle_supported: true, gateway_instance_id: 'stable-gateway', manual_stopped: true, revision: 'after-external-binding' };
    assert.equal(eventStreams.size, 1, 'the original SSE connection must remain open');
    for (const stream of eventStreams) stream.write('event: source.process\ndata: ' + JSON.stringify({ type: 'source.process', source_id: 'fixture', data: { action: 'binding' } }) + '\n\n');
    await page.getByText('http://fixture-rebound:8090', { exact: true }).waitFor();
    await safe.getByText('下级已人为停止', { exact: true }).waitFor();
    assert.equal(await page.getByRole('button', { name: '启动下级', exact: true }).isEnabled(), true);
    await page.screenshot({ path: path.join(output, 'process-named-event-refresh.png') });
    checks.push('named source.process refreshes an open view without OPC events, SSE reconnect, navigation or manual refresh');
    assert.deepEqual(errors, []); assert.deepEqual(mutations, []);
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ checks, errors, mutations, hardwareAcceptance: false }, null, 2));
    console.log(JSON.stringify({ passed: checks.length, output }));
  } finally { await browser.close(); for (const stream of eventStreams) stream.end(); await new Promise(resolve => server.close(resolve)); }
})().catch(err => { console.error(err); process.exitCode = 1; server.close(); });
