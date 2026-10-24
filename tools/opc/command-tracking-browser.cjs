// UI-only regression. Run on remote46 against built assets, never live APIs.
// RCD_PLAYWRIGHT_MODULE points at an installed Playwright module. All fixture
// requests stay in this process; no device or production server is contacted.
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const assert = require('node:assert/strict');
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../..');
const dist = path.join(root, 'manager/web/dist');
const output = path.resolve(process.env.RCD_BROWSER_OUTPUT || path.join(root, 'artifacts/runs/architecture-browser'));
const golden = JSON.parse(fs.readFileSync(path.join(root, 'contracts/command-tracking/v1/ack-received.json')));
const tracked = { id: golden.command_id, source_id: 'fixture', group_id: 'line', port: golden.port_id,
  kind: 'Write', node_id: 'TEST_Send', state: 'TimedOut', status_code: 0x800a0000, request: '1108', started_at: golden.started_at,
  tracking: { protocol: 'tracked', phase: 'AckReceived', attempt_count: 2, sent_count: 2, audit_incomplete: true } };
const detail = { command: tracked, trace: { protocol: 'tracked', gateway_instance_id: golden.gateway_instance_id,
  boot_id: golden.boot_id, history_gap: true, snapshot: { ...golden, audit_incomplete: true } } };
const server = http.createServer((req, res) => {
  const relative = new URL(req.url, 'http://localhost').pathname;
  const candidate = path.resolve(dist, '.' + relative);
  if (!candidate.startsWith(dist + path.sep)) { res.writeHead(403).end(); return; }
  const file = fs.existsSync(candidate) && fs.statSync(candidate).isFile() ? candidate : path.join(dist, 'index.html');
  const type = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml' }[path.extname(file)] || 'application/octet-stream';
  res.writeHead(200, { 'Content-Type': type }); fs.createReadStream(file).pipe(res);
});
(async () => {
  fs.mkdirSync(output, { recursive: true });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    const errors = []; page.on('pageerror', e => errors.push(e.message));
    page.on('pageerror', e => console.error('PAGE ERROR', e.message));
    await page.route('**/api/v1/**', async route => {
      assert.equal(route.request().method(), 'GET', 'browser check may not perform mutations');
      const url = new URL(route.request().url()); const p = url.pathname;
      let value = { items: [], total: 0, limit: 50, offset: 0 };
      if (p.endsWith('/auth/session')) value = { authenticated: true, username: 'fixture' };
      else if (p === '/api/v1/groups') value = [{ id: 'line', name: '测试组' }];
      else if (p === '/api/v1/sources') value = { items: [{ id: 'fixture', name: '隔离界面测试', group_id: 'line' }], total: 1 };
      else if (p === '/api/v1/overview') value = { manager_state: 'Running' };
      else if (p === '/api/v1/commands') value = { items: Number(url.searchParams.get('offset')) > 0 ? [{ ...tracked, id: 'legacy', tracking: undefined, state: 'Received' }] : [tracked], total: 51, limit: 50, offset: Number(url.searchParams.get('offset')) };
      else if (p === '/api/v1/commands/legacy') value = { command: { ...tracked, id: 'legacy', state: 'Received' } };
      else if (p.startsWith('/api/v1/commands/')) value = detail;
      await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) });
    });
    await page.route('**/healthz', route => route.fulfill({ contentType: 'application/json', body: JSON.stringify({ checks: { command_persistence: 'warning: isolated audit fault' } }) }));
    await page.route('**/events', route => route.fulfill({ contentType: 'text/event-stream', body: ': fixture\n\n' }));
    const url = `http://127.0.0.1:${server.address().port}/index.html#/commands`;
    await page.goto(url);
    await page.waitForTimeout(500);
    fs.writeFileSync(path.join(output, 'initial.html'), await page.content());
    await page.getByRole('heading', { name: '命令记录', exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'loaded.png'), fullPage: true });
    fs.writeFileSync(path.join(output, 'loaded.html'), await page.content());
    await page.getByText('命令审计降级，控制仍可执行', { exact: true }).waitFor();
    await page.getByRole('button', { name: '执行过程', exact: true }).click();
    await page.getByText(/原超时结果保留/).waitFor();
    await page.getByText(golden.ack_id, { exact: true }).waitFor();
    await page.waitForTimeout(220); // settle the shared drawer's 180 ms animation
    await page.screenshot({ path: path.join(output, 'tracked-desktop.png'), fullPage: true });
    await page.getByRole('button', { name: '关闭详情', exact: true }).click();
    await page.getByRole('button', { name: '下一页', exact: true }).first().click();
    await page.getByText('2 / 2 页', { exact: true }).waitFor();
    await page.getByRole('button', { name: '执行过程', exact: true }).click();
    await page.getByText('旧协议或历史记录，无法精确关联', { exact: true }).waitFor();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.waitForTimeout(220);
    const box = await page.locator('.detail-drawer.is-open').boundingBox();
    assert(box && box.x >= -1 && box.x + box.width <= 391, 'drawer must fit the viewport');
    await page.screenshot({ path: path.join(output, 'legacy-mobile.png') });
    assert.deepEqual(errors, []);
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ passed: true, realDeviceAcceptance: false, cases: ['audit-warning', 'late-ack', 'uint64-id', 'pagination', 'legacy-unknown', 'desktop-mobile'], javascriptErrors: errors }, null, 2));
    console.log('PASS isolated command tracking browser checks; no device acceptance claimed');
  } finally { await browser.close(); await new Promise(resolve => server.close(resolve)); }
})().catch(error => { console.error(error); server.close(); process.exitCode = 1; });
