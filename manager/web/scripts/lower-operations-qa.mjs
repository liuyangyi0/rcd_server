/**
 * Remote-only, real-UI lower operations regression for the disposable PTY pair.
 *
 * RCD_QA_REMOTE=1 RCD_QA_ALLOW_MUTATIONS=1 \
 * RCD_QA_URL=http://127.0.0.1:8085 \
 * RCD_QA_FIXTURE=/absolute/operations-fixture/Fixture.json \
 * RCD_QA_OUTPUT=/absolute/evidence/outside-the-checkout \
 * PLAYWRIGHT_MODULE=/absolute/browser-tools/node_modules/playwright/index.js \
 * node manager/web/scripts/lower-operations-qa.mjs
 *
 * All application writes use visible browser controls. HTTP helpers are GET
 * only. The sole direct fixture writes create uniquely named synthetic log
 * files, or rotate one of those exact files; real logs are never modified.
 * Config restoration and acquisition recovery also use visible UI controls.
 * Screenshots, reports, automatic backups and synthetic logs are retained.
 */
import assert from 'node:assert/strict';
import { createHash, randomUUID } from 'node:crypto';
import { lstat, mkdir, readdir, readFile, readlink, realpath, rename, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

assert.equal(process.env.RCD_QA_REMOTE, '1', 'Set RCD_QA_REMOTE=1 only on the remote test host');
assert.equal(process.env.RCD_QA_ALLOW_MUTATIONS, '1', 'Explicit RCD_QA_ALLOW_MUTATIONS=1 is required');
assert.equal(process.platform, 'linux', 'Do not execute this regression on the development workstation');
const baseURL = (process.env.RCD_QA_URL ?? 'http://127.0.0.1:8085').replace(/\/$/, '');
const parsedURL = new URL(baseURL);
assert.equal(parsedURL.protocol, 'http:');
assert.ok(['127.0.0.1', 'localhost'].includes(parsedURL.hostname), 'Use the remote loopback test manager');
assert.equal(parsedURL.port, '8085', 'This regression is restricted to the isolated manager on port 8085');
assert.equal(parsedURL.pathname, '/');
assert.ok(!parsedURL.search && !parsedURL.hash && !parsedURL.username && !parsedURL.password);

const fixtureRoot = '/home/lenovo/code/rcd_device_groups_20260828/runs/resume-fix-20260828-qZJnw7/operations-fixture';
const fixtureInput = process.env.RCD_QA_FIXTURE;
assert.ok(fixtureInput && path.isAbsolute(fixtureInput), 'RCD_QA_FIXTURE must be an explicit absolute Fixture.json');
const manifestPath = await realpath(fixtureInput);
assert.equal(manifestPath, path.join(fixtureRoot, 'Fixture.json'), 'Only the approved disposable operations fixture is in scope');
const manifest = JSON.parse(await readFile(manifestPath, 'utf8'));
assert.equal(manifest.state, 'ready', 'The disposable fixture must be ready before starting');
assert.equal(await realpath(manifest.root), fixtureRoot);
for (const [key, port] of [['a', 4860], ['b', 4861]]) {
  const source = manifest.sources[key];
  assert.equal(source.opc_port, port);
  assert.equal(new URL(source.endpoint).port, String(port));
  assert.equal(await realpath(source.run_dir), path.join(fixtureRoot, key));
  assert.ok(Number.isInteger(source.pid) && source.pid > 1, 'Fixture Rust PID must be explicit');
  assert.ok(source.pty_slave.startsWith('/dev/pts/'));
}
const logDir = await realpath(manifest.sources.a.log_dir);
assert.equal(logDir, path.join(fixtureRoot, 'a', 'logs'), 'Synthetic logs are restricted to fixture a/logs');

const outputInput = process.env.RCD_QA_OUTPUT;
assert.ok(outputInput && path.isAbsolute(outputInput), 'RCD_QA_OUTPUT must be absolute and outside the checkout');
const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = await realpath(process.env.RCD_QA_REPO_ROOT ?? path.resolve(scriptDirectory, '../../..'));
const inside = (parent, child) => {
  const relative = path.relative(parent, child);
  return relative === '' || (!relative.startsWith('..' + path.sep) && relative !== '..' && !path.isAbsolute(relative));
};
async function resolvedDestination(value) {
  try { return await realpath(value); } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    return path.join(await resolvedDestination(path.dirname(value)), path.basename(value));
  }
}
const outputDir = await resolvedDestination(path.resolve(outputInput));
assert.ok(!inside(repositoryRoot, outputDir), 'Evidence must remain outside the checkout, including through symlinks');
assert.notEqual(outputDir, path.parse(outputDir).root);
assert.ok(!inside(fixtureRoot, outputDir), 'Use a separate evidence directory, not a lower runtime directory');
await mkdir(outputDir, { recursive: true });

const requirePlaywright = createRequire(path.join(process.env.RCD_QA_PLAYWRIGHT_ROOT ?? path.resolve(scriptDirectory, '..'), 'package.json'));
if (process.env.PLAYWRIGHT_MODULE) assert.ok(path.isAbsolute(process.env.PLAYWRIGHT_MODULE), 'PLAYWRIGHT_MODULE must be absolute');
const { chromium } = requirePlaywright(process.env.PLAYWRIGHT_MODULE ?? 'playwright');
const runTag = Date.now() + '-' + process.pid + '-' + randomUUID().slice(0, 8);
const digest = (value) => createHash('sha256').update(value).digest('hex');
const list = (value) => Array.isArray(value) ? value : (value.items ?? value.data?.items ?? value.data ?? []);
const opsPath = (source, suffix) => '/sources/' + encodeURIComponent(source) + '/operations/' + suffix;
const apiPath = (suffix) => '/api/v1' + suffix;
const fixtureSource = (id) => manifest.sources[id === 'lower-a' ? 'a' : 'b'];
const sortedDefaults = (groups) => groups.map((group) => [group.id, group.default_source_id ?? '', group.revision]).sort((a, b) => a[0].localeCompare(b[0]));
const sortedBindings = (source) => (source.group_bindings ?? []).map((item) => [item.port_id, item.device_id, item.group_id]).sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)));
const report = {
  startedAt: new Date().toISOString(),
  environment: {
    baseURL, host: os.hostname(), platform: process.platform, outputDir, repositoryRoot, manifestPath,
    desktopViewport: { width: 1600, height: 1000 }, mobileViewport: { width: 390, height: 844 },
    browserAvailability: 'available-on-local-workstation',
    browserPath: 'remote Playwright Chromium',
    browserPathReason: 'Explicit user constraint: every build/test/browser must run on the remote Linux host; local Browser plugin is not used.',
    allowMutations: true,
  },
  flow: '下级运维 → 站点停读/恢复 → 真实串口释放/打开 → 配置校验/备份恢复 → 草稿保护 → 双页面真实409 → 有界日志/轮转 → 窄屏 → UI恢复',
  checks: [], flows: [], screenshots: [], writes: [], requests: [], readAssertions: [],
  consoleErrors: [], consoleWarnings: [], httpErrors: [], expectedFailures: [], deniedWrites: [],
  injections: [], syntheticLogs: [], downloads: [], restoration: [],
};
let browser;
let desktop;
let currentFlow = 'preflight';
let mutationsArmed = false;
let originalProcess;
let ownedProcesses;
let initialSources;
let initialGroups;
let initialStatus;
let initialConfig;
let initialOtherConfigs;
let originalControlHash;
let failure;
const sourceRevisions = new Map();
const pageLabels = new WeakMap();
const expectedFailures = [];
const pages = [];

async function readAPI(suffix) {
  assert.ok(suffix.startsWith('/') && !suffix.startsWith('//'));
  const response = await desktop.context.request.get(baseURL + apiPath(suffix), { timeout: 20_000 });
  report.readAssertions.push({ flow: currentFlow, method: 'GET', path: apiPath(suffix), status: response.status() });
  assert.ok(response.ok(), 'Read-only API ' + suffix + ': ' + response.status() + ' ' + (await response.text()).slice(0, 350));
  return response.json();
}
async function readStatus(id = 'lower-a') {
  const result = await readAPI(opsPath(id, 'status'));
  assert.equal(result.source_id, id);
  assert.equal(result.source_revision, sourceRevisions.get(id));
  assert.equal(result.ports.length, 1);
  assert.equal(result.ports[0].port_id, fixtureSource(id).port_id);
  return result;
}
const configRead = (id = 'lower-a') => readAPI(opsPath(id, 'config') + '?name=calc.toml');
async function eventually(label, check, timeout = 40_000) {
  const deadline = Date.now() + timeout;
  let lastError;
  do {
    try { const value = await check(); if (value) return value; } catch (error) { lastError = error; }
    await new Promise((resolve) => setTimeout(resolve, 250));
  } while (Date.now() < deadline);
  throw new Error('Timed out: ' + label + (lastError ? ' (' + lastError.message + ')' : ''));
}
async function processSnapshot(pid) {
  const text = await readFile('/proc/' + pid + '/stat', 'utf8');
  const fields = text.slice(text.lastIndexOf(')') + 2).trim().split(/\s+/);
  assert.ok(fields.length > 19);
  return { pid, executable: await readlink('/proc/' + pid + '/exe'), startTick: fields[19] };
}
async function checkProcesses() {
  const production = await processSnapshot(398713);
  assert.equal(production.executable, '/home/lenovo/code/rcd/rcd_server');
  assert.deepEqual(production, originalProcess, 'The original production lower must not restart or change');
  const currentManifest = JSON.parse(await readFile(manifestPath, 'utf8'));
  for (const key of ['a', 'b']) {
    assert.equal(currentManifest.sources[key].pid, manifest.sources[key].pid, 'This UI suite must never restart a lower');
    assert.deepEqual(await processSnapshot(manifest.sources[key].pid), ownedProcesses[key]);
  }
  return { production, owned: ownedProcesses };
}
async function hasSerialFD(key = 'a') {
  const source = manifest.sources[key];
  const entries = await readdir('/proc/' + source.pid + '/fd');
  for (const entry of entries) {
    try { if (await readlink('/proc/' + source.pid + '/fd/' + entry) === source.pty_slave) return true; } catch (error) {
      if (error.code !== 'ENOENT') throw error;
    }
  }
  return false;
}
async function pointSample(id, identifier) {
  const params = new URLSearchParams({ route_source_id: id, q: identifier, limit: '100' });
  const value = await readAPI('/points?' + params);
  const matches = value.items.filter((item) => item.node.key.identifier === identifier);
  assert.equal(matches.length, 1);
  assert.equal(matches[0].selected_source_id, id);
  return matches[0].sample;
}
async function sampleMatches(id, identifier, good, value) {
  const sample = await pointSample(id, identifier);
  return ((sample.status_code & 0xc0000000) === 0) === good && (value === undefined || sample.value === value);
}
function controlSummary(snapshot) {
  return snapshot.ports.map((port) => ({
    port_id: port.port_id, enabled: port.enabled, applied_enabled: port.applied_enabled, open: port.open,
    devices: port.devices.map((device) => ({ id: device.device_id, desired: device.read_enabled, applied: device.applied_read_enabled })),
  }));
}
async function lowerBUnchanged() {
  const status = await readStatus('lower-b');
  assert.equal(status.revision, initialStatus.b.revision);
  assert.deepEqual(controlSummary(status), controlSummary(initialStatus.b));
  assert.ok(await hasSerialFD('b'));
  assert.ok(await sampleMatches('lower-b', 'A_VALUE', true, 101));
  assert.ok(await sampleMatches('lower-b', 'B_VALUE', true, 202));
}
function expectFailure(page, suffix, method, status, reason) {
  const entry = { page: pageLabels.get(page), path: apiPath(suffix), method, status, reason, flow: currentFlow, registeredAt: Date.now() };
  expectedFailures.push(entry);
  report.expectedFailures.push(entry);
}
function expectedResponse(label, response) {
  const request = response.request();
  const pathname = new URL(response.url()).pathname;
  return expectedFailures.find((item) => item.page === label && item.path === pathname && item.method === request.method() && item.status === response.status());
}
async function createPage(label, viewport = report.environment.desktopViewport, options = {}) {
  const context = await browser.newContext({ viewport, locale: 'zh-CN', acceptDownloads: true, serviceWorkers: 'block' });
  // A guard, not a replacement backend: permitted business requests continue
  // unchanged. A regression must not accidentally edit groups or other sources.
  await context.route('**/api/v1/**', async (route) => {
    const request = route.request();
    if (['GET', 'HEAD', 'OPTIONS'].includes(request.method())) return route.continue();
    const pathname = new URL(request.url()).pathname;
    const allowed = mutationsArmed && options.readOnly !== true && (
      (request.method() === 'PUT' && ['port', 'device-read', 'config'].some((suffix) => pathname === apiPath(opsPath('lower-a', suffix)))) ||
      (request.method() === 'POST' && pathname === apiPath(opsPath('lower-a', 'config/validate')))
    );
    if (!allowed) {
      report.deniedWrites.push({ page: label, method: request.method(), path: pathname, flow: currentFlow });
      return route.abort('blockedbyclient');
    }
    return route.continue();
  });
  if (options.suppressSSE) {
    report.injections.push({ page: label, purpose: 'Force an authentic stale-file CAS request from a second real page; only this page SSE returns HTTP 204 to stop EventSource reconnects, application GET/PUT remain live.', route: '/api/v1/events', status: 204, injected: true });
    await context.route('**/api/v1/events', (route) => route.fulfill({
      // A finite 200 stream would reconnect and trigger the application's
      // onopen invalidation, accidentally refreshing the intended stale CAS.
      // HTTP 204 explicitly tells EventSource not to reconnect.
      status: 204, headers: { 'Cache-Control': 'no-cache' },
    }));
  }
  if (options.inject) await options.inject(context);
  const page = await context.newPage();
  pageLabels.set(page, label);
  page.setDefaultTimeout(20_000);
  page.setDefaultNavigationTimeout(30_000);
  page.on('console', (message) => {
    const entry = { page: label, flow: currentFlow, at: Date.now(), url: page.url(), location: message.location(), message: message.text(), kind: 'console' };
    if (message.type() === 'error') report.consoleErrors.push(entry);
    if (message.type() === 'warning') report.consoleWarnings.push(entry);
  });
  page.on('pageerror', (error) => report.consoleErrors.push({ page: label, flow: currentFlow, at: Date.now(), url: page.url(), message: error.stack ?? error.message, kind: 'pageerror' }));
  page.on('response', (response) => {
    if (response.status() < 400 || !response.url().startsWith(baseURL)) return;
    const expected = expectedResponse(label, response);
    report.httpErrors.push({ page: label, flow: currentFlow, at: Date.now(), method: response.request().method(), url: response.url(), status: response.status(), expected: Boolean(expected), reason: expected?.reason });
  });
  page.on('request', (request) => {
    if (!request.url().startsWith(baseURL + '/api/v1/')) return;
    const url = new URL(request.url());
    if (request.method() === 'GET' && url.pathname.includes('/operations/')) {
      report.requests.push({ page: label, flow: currentFlow, method: 'GET', path: url.pathname, query: Object.fromEntries(url.searchParams), at: Date.now() });
    }
    if (!['GET', 'HEAD', 'OPTIONS'].includes(request.method())) {
      let body;
      try { body = request.postDataJSON(); } catch { /* record a malformed request without leaking its body */ }
      report.writes.push({
        page: label, flow: currentFlow, method: request.method(), path: url.pathname,
        ifMatch: request.headers()['if-match'], sourceRevision: body?.source_revision, portId: body?.port_id,
        deviceId: body?.device_id, enabled: body?.enabled, name: body?.name,
        contentSha256: typeof body?.content === 'string' ? digest(body.content) : undefined,
        contentBytes: typeof body?.content === 'string' ? Buffer.byteLength(body.content) : undefined,
        transport: 'visible UI; no direct API mutation',
      });
    }
  });
  const result = { context, page, label };
  pages.push(result);
  return result;
}
async function sourcesPage(page) {
  await page.goto(baseURL + '/#/sources', { waitUntil: 'domcontentloaded' });
  await page.getByRole('heading', { name: '下级 OPC', exact: true }).waitFor();
}
async function openOperations(page, id = 'lower-a') {
  await sourcesPage(page);
  await page.getByRole('button', { name: '运维 ' + id, exact: true }).click();
  const drawer = page.locator('.operations-drawer.is-open');
  await drawer.getByRole('heading', { name: '下级运维 · ' + id, exact: true }).waitFor();
  return drawer;
}
async function chooseTab(page, label) {
  const drawer = page.locator('.operations-drawer.is-open');
  const tab = drawer.getByRole('tab', { name: label, exact: true });
  await eventually('tab enabled: ' + label, () => tab.isEnabled());
  await tab.click();
  await eventually('active tab: ' + label, async () => (await tab.getAttribute('aria-selected')) === 'true');
  return drawer;
}
async function openConfig(page) {
  const drawer = await chooseTab(page, '配置');
  const selector = drawer.getByLabel('配置文件', { exact: true });
  await selector.waitFor();
  await selector.selectOption('calc.toml');
  const editor = drawer.getByRole('textbox', { name: '配置内容 calc.toml', exact: true });
  await editor.waitFor();
  return { drawer, editor };
}
async function closeCleanDrawer(page) {
  const drawer = page.locator('.operations-drawer.is-open');
  await drawer.getByRole('button', { name: '关闭详情', exact: true }).click();
  await drawer.waitFor({ state: 'hidden' });
}
async function currentFileRevision(page) {
  const code = page.locator('.operations-file-meta > div').filter({ has: page.locator('dt', { hasText: '编辑基线版本' }) }).locator('code');
  return (await code.innerText()).trim();
}
async function currentAcquisitionRevision(page) {
  return (await page.locator('.operations-drawer p.operations-muted').filter({ hasText: '下级状态版本：' }).locator('code').innerText()).trim();
}
async function clickRequest(page, button, suffix, { method = 'PUT', revision, expectedStatus = 200, expectedBody } = {}) {
  await eventually('button enabled: ' + suffix, () => button.isEnabled());
  const [response] = await Promise.all([
    page.waitForResponse((candidate) => candidate.request().method() === method && new URL(candidate.url()).pathname === apiPath(suffix)),
    button.click(),
  ]);
  assert.equal(response.status(), expectedStatus, 'UI ' + method + ' ' + suffix + ': ' + (await response.text()).slice(0, 500));
  if (revision !== undefined) assert.equal(response.request().headers()['if-match'], JSON.stringify(String(revision)), 'UI must submit the revision it displayed');
  const submitted = response.request().postDataJSON();
  for (const [key, value] of Object.entries(expectedBody ?? {})) assert.deepEqual(submitted[key], value, 'Wrong UI mutation field ' + key);
  if (method === 'PUT') assert.equal(submitted.source_revision, sourceRevisions.get('lower-a'));
  return response.json();
}
async function validateConfig(page, expectedValid = true) {
  const drawer = page.locator('.operations-drawer.is-open');
  const content = await drawer.getByRole('textbox', { name: '配置内容 calc.toml', exact: true }).inputValue();
  const result = await clickRequest(page, drawer.getByRole('button', { name: '校验当前草稿', exact: true }), opsPath('lower-a', 'config/validate'), { method: 'POST', expectedBody: { name: 'calc.toml', content } });
  assert.equal(result.valid, expectedValid);
  await drawer.getByText(expectedValid ? '当前草稿校验通过' : '当前草稿校验未通过', { exact: true }).waitFor();
  return result;
}
async function saveConfig(page, expectedStatus = 200) {
  const drawer = page.locator('.operations-drawer.is-open');
  const revision = await currentFileRevision(page);
  const content = await drawer.getByRole('textbox', { name: '配置内容 calc.toml', exact: true }).inputValue();
  const result = await clickRequest(page, drawer.getByRole('button', { name: '保存到下级', exact: true }), opsPath('lower-a', 'config'), { revision, expectedStatus, expectedBody: { name: 'calc.toml', content } });
  if (expectedStatus === 200) {
    assert.equal(result.revision, digest(content));
    assert.equal(result.content, content);
    await eventually('saved editor baseline', async () => (await currentFileRevision(page)) === result.revision);
    await drawer.getByText('配置已保存到下级文件；需要手动重启下级后生效，本操作未重启程序。', { exact: true }).waitFor();
  }
  return result;
}
async function reloadConfigExplicitly(page) {
  const drawer = page.locator('.operations-drawer.is-open');
  const first = drawer.getByRole('button', { name: '放弃草稿并载入最新', exact: true });
  await eventually('discard control enabled', () => first.isEnabled());
  await first.click();
  await drawer.getByRole('button', { name: '确认放弃并载入最新', exact: true }).click();
  await drawer.getByText('已明确放弃原草稿，并载入下级当前文件。', { exact: true }).waitFor();
  const current = await configRead();
  await eventually('explicit reload content', async () => (await drawer.getByRole('textbox', { name: '配置内容 calc.toml', exact: true }).inputValue()) === current.content);
}
async function setDeviceRead(page, id, enabled) {
  const drawer = page.locator('.operations-drawer.is-open');
  const before = await readStatus();
  const port = before.ports[0];
  const device = port.devices.find((item) => item.device_id === id);
  assert.ok(device);
  if (device.read_enabled !== enabled) {
    await drawer.getByRole('button', { name: '刷新实际状态', exact: true }).click();
    const button = drawer.getByRole('button', { name: (enabled ? '恢复读取 ' : '停读 ') + port.port_id + ' ' + id, exact: true });
    await eventually('device action enabled', () => button.isEnabled());
    const revision = await currentAcquisitionRevision(page);
    await clickRequest(page, button, opsPath('lower-a', 'device-read'), { revision, expectedBody: { port_id: port.port_id, device_id: id, enabled } });
  }
  return eventually('station ' + id + ' applied ' + enabled, async () => {
    const snapshot = await readStatus();
    const currentPort = snapshot.ports[0];
    const current = currentPort.devices.find((item) => item.device_id === id);
    return !currentPort.control_pending && current.read_enabled === enabled && current.applied_read_enabled === enabled ? snapshot : undefined;
  });
}
async function setPortEnabled(page, enabled, evidenceName) {
  const drawer = page.locator('.operations-drawer.is-open');
  const before = await readStatus();
  const port = before.ports[0];
  if (port.enabled !== enabled) {
    await drawer.getByRole('button', { name: '刷新实际状态', exact: true }).click();
    // Rust reports the raw, possibly long absolute port path as port_name.
    // Never substitute Fixture.port_name, which is only an alias basename.
    const button = drawer.getByRole('button', { name: (enabled ? '启用串口 ' : '关闭串口 ') + (port.port_name || port.port_id), exact: true });
    await eventually('physical port action enabled', () => button.isEnabled());
    const revision = await currentAcquisitionRevision(page);
    if (enabled) {
      await clickRequest(page, button, opsPath('lower-a', 'port'), { revision, expectedBody: { port_id: port.port_id, enabled: true } });
    } else {
      await button.click();
      const dialog = page.getByRole('dialog', { name: '确认关闭物理串口', exact: true });
      await dialog.waitFor();
      const confirm = dialog.getByRole('button', { name: '确认关闭串口', exact: true });
      assert.ok(await confirm.isDisabled(), 'Closing a whole physical port requires confirmation');
      const checkbox = dialog.getByRole('checkbox', { name: '我确认停止该串口全部站的采集并释放串口', exact: true });
      await checkbox.check();
      assert.ok(await checkbox.isChecked());
      if (evidenceName) { await inspect(page, evidenceName, 1); await screenshot(page, evidenceName); }
      await clickRequest(page, confirm, opsPath('lower-a', 'port'), { revision, expectedBody: { port_id: port.port_id, enabled: false } });
      await dialog.waitFor({ state: 'hidden' });
    }
  }
  return eventually('physical port applied/open ' + enabled, async () => {
    const snapshot = await readStatus();
    const current = snapshot.ports[0];
    return !current.control_pending && current.enabled === enabled && current.applied_enabled === enabled && current.open === enabled && (await hasSerialFD()) === enabled ? snapshot : undefined;
  });
}
async function inspect(page, name, dialogs = 0) {
  const evidence = await page.evaluate(() => ({
    title: document.title, h1: document.querySelector('h1')?.textContent?.trim() ?? '',
    bodyLength: document.body.innerText.trim().length,
    viewportWidth: innerWidth, documentWidth: document.documentElement.scrollWidth, bodyWidth: document.body.scrollWidth,
    frameworkOverlay: Boolean(document.querySelector('vite-error-overlay, nextjs-portal, #webpack-dev-server-client-overlay')),
    visibleDialogs: [...document.querySelectorAll('[role="dialog"]')].filter((element) => element.getClientRects().length > 0).length,
    drawer: (() => { const element = document.querySelector('.operations-drawer.is-open'); const bounds = element?.getBoundingClientRect(); return bounds ? { left: bounds.left, right: bounds.right, width: bounds.width, scrollWidth: element.scrollWidth, clientWidth: element.clientWidth } : null; })(),
    portButtonTextFits: [...document.querySelectorAll('.operations-port-heading .button')].every((element) => {
      if (!element.getClientRects().length) return true;
      const bounds = element.getBoundingClientRect();
      const range = document.createRange();
      range.selectNodeContents(element);
      return [...range.getClientRects()].every((rect) => rect.left >= bounds.left - 1 && rect.right <= bounds.right + 1 && rect.top >= bounds.top - 1 && rect.bottom <= bounds.bottom + 1);
    }),
  }));
  const checks = {
    identity: new URL(page.url()).hash.startsWith('#/sources') && /RCD|管理/i.test(evidence.title) && evidence.h1 === '下级 OPC',
    nonblank: evidence.bodyLength > 150,
    noFrameworkOverlay: !evidence.frameworkOverlay,
    noDocumentOverflow: Math.max(evidence.documentWidth, evidence.bodyWidth) <= evidence.viewportWidth + 1,
    expectedDialogState: evidence.visibleDialogs === dialogs,
    portButtonTextInsideItsBox: evidence.portButtonTextFits,
    drawerWithinViewport: !evidence.drawer || (evidence.drawer.left >= -1 && evidence.drawer.right <= evidence.viewportWidth + 1 && evidence.drawer.scrollWidth <= evidence.drawer.clientWidth + 1),
  };
  report.checks.push({ name, page: pageLabels.get(page), ...checks, evidence });
  assert.ok(Object.values(checks).every(Boolean), 'Rendered checks failed for ' + name + ': ' + JSON.stringify(checks));
}
async function screenshot(page, name) {
  const destination = path.join(outputDir, name + '.png');
  await page.screenshot({ path: destination, fullPage: false });
  report.screenshots.push({ name, path: destination, url: page.url() });
}
async function flow(name, action) {
  currentFlow = name;
  const started = Date.now();
  try {
    await action();
    report.flows.push({ name, passed: true, durationMs: Date.now() - started });
    console.log('PASS ' + name);
  } catch (error) {
    report.flows.push({ name, passed: false, durationMs: Date.now() - started, error: error.stack ?? error.message });
    if (desktop?.page && !desktop.page.isClosed()) await screenshot(desktop.page, 'failed-' + name).catch(() => {});
    throw error;
  }
}
async function syntheticLog(name, text) {
  assert.ok(name.startsWith('rcd_server_fixture_operations-ui-' + runTag) && name.endsWith('.log'));
  assert.equal(path.basename(name), name);
  assert.equal(await realpath(logDir), path.join(fixtureRoot, 'a', 'logs'));
  const destination = path.join(logDir, name);
  await writeFile(destination, text, { encoding: 'utf8', flag: 'wx' });
  const metadata = await lstat(destination);
  assert.ok(metadata.isFile() && !metadata.isSymbolicLink());
  report.syntheticLogs.push({ name, path: destination, inode: String(metadata.ino), sizeBytes: metadata.size, sha256: digest(text), retained: true });
  return destination;
}
async function logResponseAfter(page, action, queryMatch = {}, status = 200) {
  const [response] = await Promise.all([
    page.waitForResponse((candidate) => {
      if (candidate.request().method() !== 'GET') return false;
      const url = new URL(candidate.url());
      return url.pathname === apiPath(opsPath('lower-a', 'log')) && candidate.status() === status && Object.entries(queryMatch).every(([key, value]) => url.searchParams.get(key) === String(value));
    }),
    action(),
  ]);
  const result = await response.json();
  if (status === 200) {
    assert.equal(result.source_id, 'lower-a');
    assert.equal(result.source_revision, sourceRevisions.get('lower-a'));
    assert.ok(Buffer.byteLength(result.text) <= Number(new URL(response.url()).searchParams.get('limit')));
    assert.ok(!result.text.includes('\ufffd'), 'UTF-8 page boundaries must not corrupt fixture text');
  }
  return result;
}
async function visibleLogEquals(page, result) {
  if (result.text) await eventually('visible log equals bounded server page', async () => (await page.getByLabel('日志内容', { exact: true }).textContent()) === result.text);
  else await page.getByText('当前日志页为空', { exact: true }).waitFor();
}
async function downloadVisible(page, button, expectedText, name) {
  const [download] = await Promise.all([page.waitForEvent('download'), button.click()]);
  const destination = path.join(outputDir, name);
  await download.saveAs(destination);
  assert.equal(await download.failure(), null);
  assert.equal(await readFile(destination, 'utf8'), expectedText);
  report.downloads.push({ path: destination, suggestedFilename: download.suggestedFilename(), bytes: Buffer.byteLength(expectedText), sha256: digest(expectedText) });
}

async function chooseLog(page, name) {
  const selector = page.getByLabel('日志文件', { exact: true });
  await selector.waitFor();
  return logResponseAfter(page, async () => {
    if (await selector.inputValue() === name) await page.getByRole('button', { name: '刷新当前页', exact: true }).click();
    else await selector.selectOption(name);
  }, { name });
}

try {
  browser = await chromium.launch({ headless: true });
  desktop = await createPage('desktop-live');
  const page = desktop.page;

  await flow('01-live-fixture-and-lazy-entry', async () => {
    originalProcess = await processSnapshot(398713);
    assert.equal(originalProcess.executable, '/home/lenovo/code/rcd/rcd_server');
    ownedProcesses = Object.fromEntries(await Promise.all(['a', 'b'].map(async (key) => [key, await processSnapshot(manifest.sources[key].pid)])));
    originalControlHash = digest(await readFile(path.join(fixtureRoot, 'control.json')));
    [initialSources, initialGroups] = await Promise.all([readAPI('/sources').then(list), readAPI('/groups').then(list)]);
    for (const id of ['lower-a', 'lower-b']) {
      const configured = initialSources.find((source) => source.id === id);
      assert.ok(configured, 'Missing fixture manager source ' + id);
      assert.equal(configured.endpoint, fixtureSource(id).endpoint, 'Never operate a different Endpoint through a reused source ID');
      assert.equal(configured.connection_state, 'Connected');
      sourceRevisions.set(id, configured.revision);
      const capabilities = await readAPI(opsPath(id, 'capabilities'));
      assert.equal(capabilities.supported, true);
      assert.equal(capabilities.source_revision, configured.revision);
      assert.equal(capabilities.schema_version, 1);
      assert.deepEqual([...capabilities.features].sort(), ['acquisition', 'configuration', 'logs']);
    }
    initialStatus = { a: await readStatus(), b: await readStatus('lower-b') };
    for (const key of ['a', 'b']) {
      const port = initialStatus[key].ports[0];
      assert.ok(port.enabled && port.applied_enabled && port.open && !port.control_pending && !port.pending_commands);
      assert.deepEqual(port.devices.map((device) => device.device_id).sort(), ['1', '2']);
      assert.ok(port.devices.every((device) => device.read_enabled && device.applied_read_enabled));
      assert.ok(await hasSerialFD(key));
    }
    initialConfig = await configRead();
    assert.equal(initialConfig.revision, digest(initialConfig.content));
    assert.equal(initialConfig.active_revision, initialConfig.revision, 'Begin with the restored active fixture config');
    assert.equal(initialConfig.restart_required, false);
    initialOtherConfigs = [];
    for (const id of ['lower-a', 'lower-b']) {
      const files = await readAPI(opsPath(id, 'configs'));
      for (const file of files.items) {
        if (id === 'lower-a' && file.name === 'calc.toml') continue;
        const document = await readAPI(opsPath(id, 'config') + '?' + new URLSearchParams({ name: file.name }));
        initialOtherConfigs.push({ source: id, name: file.name, revision: document.revision, activeRevision: document.active_revision });
      }
    }
    await eventually('all fixture values ready', async () => (await Promise.all([
      sampleMatches('lower-a', 'A_VALUE', true, 11), sampleMatches('lower-a', 'B_VALUE', true, 22),
      sampleMatches('lower-b', 'A_VALUE', true, 101), sampleMatches('lower-b', 'B_VALUE', true, 202),
    ])).every(Boolean));
    report.fixture = {
      sources: initialSources.map((source) => ({ id: source.id, revision: source.revision, endpoint: source.endpoint, bindings: sortedBindings(source) })),
      defaultSources: sortedDefaults(initialGroups), production: originalProcess, owned: ownedProcesses,
      ports: { a: initialStatus.a.ports[0].port_id, b: initialStatus.b.ports[0].port_id },
      originalCalc: { revision: initialConfig.revision, activeRevision: initialConfig.active_revision, bytes: Buffer.byteLength(initialConfig.content) },
    };
    // No mutation is permitted until every target, process and baseline check
    // above has succeeded. API inspection did not preload browser query state.
    mutationsArmed = true;
    await sourcesPage(page);
    assert.equal(report.requests.filter((item) => item.page === 'desktop-live').length, 0, 'Operations must not be fetched before the lazy entry is opened');
    await page.getByRole('button', { name: '运维 lower-a', exact: true }).click();
    const drawer = page.locator('.operations-drawer.is-open');
    await drawer.getByRole('heading', { name: '下级运维 · lower-a', exact: true }).waitFor();
    await drawer.getByRole('region', { name: '采集串口 ' + manifest.sources.a.port_id, exact: true }).waitFor();
    assert.equal(await drawer.getByRole('tab').count(), 3);
    const activeRequests = report.requests.filter((item) => item.page === 'desktop-live');
    assert.ok(activeRequests.some((item) => item.path.endsWith('/capabilities')));
    assert.ok(activeRequests.some((item) => item.path.endsWith('/status')));
    assert.ok(activeRequests.every((item) => item.path.endsWith('/capabilities') || item.path.endsWith('/status')), 'Inactive configuration/log tabs must not fetch');
    await inspect(page, 'desktop-acquisition-initial');
    await screenshot(page, 'desktop-acquisition-initial');
  });

  await flow('02-station-read-isolation', async () => {
    await setDeviceRead(page, '1', false);
    await eventually('only station 1 bad quality', async () => (await sampleMatches('lower-a', 'A_VALUE', false)) && (await sampleMatches('lower-a', 'B_VALUE', true, 22)));
    const stopped = await readStatus();
    assert.ok(stopped.ports[0].open && await hasSerialFD());
    assert.ok(stopped.ports[0].devices.find((device) => device.device_id === '2').applied_read_enabled);
    await lowerBUnchanged();
    assert.match(await page.locator('.operations-drawer').innerText(), /站点停读只停止数据采集，不禁止控制命令/);
    await screenshot(page, 'station-1-stopped-station-2-unaffected');
    await setDeviceRead(page, '1', true);
    await eventually('station 1 fresh data restored', () => sampleMatches('lower-a', 'A_VALUE', true, 11));
    await lowerBUnchanged();
  });

  await flow('03-physical-port-confirmation-and-fd', async () => {
    await setPortEnabled(page, false, 'desktop-close-port-confirmation');
    assert.equal(await hasSerialFD(), false, 'Closed port must release the actual lower process PTY descriptor');
    await eventually('both physical stations have bad quality', async () => (await sampleMatches('lower-a', 'A_VALUE', false)) && (await sampleMatches('lower-a', 'B_VALUE', false)));
    assert.equal((await readAPI('/sources/lower-a')).connection_state, 'Connected', 'Stopping serial collection must not stop OPC');
    await lowerBUnchanged();
    report.checks.push({ name: 'port-close-real-proc-fd', passed: true, lowerPid: manifest.sources.a.pid, pty: manifest.sources.a.pty_slave, retainedAfterClose: false });
    await screenshot(page, 'desktop-port-physically-closed');
    await setPortEnabled(page, true);
    await eventually('both physical stations recover fresh data', async () => (await sampleMatches('lower-a', 'A_VALUE', true, 11)) && (await sampleMatches('lower-a', 'B_VALUE', true, 22)));
    assert.equal(await hasSerialFD(), true);
    report.checks.push({ name: 'port-open-real-proc-fd', passed: true, lowerPid: manifest.sources.a.pid, pty: manifest.sources.a.pty_slave, reopened: true });
    await checkProcesses();
    await lowerBUnchanged();
  });

  await flow('04-config-validation-save-backup-restore', async () => {
    const { drawer, editor } = await openConfig(page);
    assert.equal(await editor.inputValue(), initialConfig.content);
    assert.match(await drawer.innerText(), /保存后需重启下级，不自动重启/);
    assert.ok(!report.requests.some((item) => item.page === 'desktop-live' && item.path.endsWith('/logs')), 'Logs must stay inactive on the config tab');
    await downloadVisible(page, drawer.getByRole('button', { name: '下载已读取配置', exact: true }), initialConfig.content, 'calc-original-download.toml');
    const invalid = initialConfig.content + "\n[[rules]]\nkks_calc = 'A_VALUE'\ndata_type = 'Double'\nexpression = '('\n";
    const beforeInvalid = report.writes.filter((item) => item.method === 'PUT' && item.path.endsWith('/config')).length;
    await editor.fill(invalid);
    const validation = await validateConfig(page, false);
    assert.ok(validation.errors.length);
    assert.ok(await drawer.getByRole('button', { name: '保存到下级', exact: true }).isDisabled());
    assert.equal((await configRead()).revision, initialConfig.revision);
    assert.equal(report.writes.filter((item) => item.method === 'PUT' && item.path.endsWith('/config')).length, beforeInvalid, 'Invalid validation must not auto-save');
    await screenshot(page, 'desktop-config-invalid-draft');

    const modified = initialConfig.content + '\n# visible UI operations regression ' + runTag + '\n';
    await editor.fill(modified);
    await validateConfig(page);
    const saved = await saveConfig(page);
    assert.equal(saved.active_revision, initialConfig.active_revision);
    assert.equal(saved.restart_required, true);
    const live = await configRead();
    assert.equal(live.content, modified);
    const priorBackupIds = new Set(initialConfig.backups.map((backup) => backup.id));
    const backup = live.backups.find((item) => !priorBackupIds.has(item.id));
    assert.ok(backup, 'Explicit save must create an automatic backup');
    await drawer.getByText('已保存，待重启', { exact: true }).waitFor();
    await drawer.locator('.operations-file-meta').scrollIntoViewIfNeeded();
    await inspect(page, 'desktop-config-saved-pending-restart');
    await screenshot(page, 'desktop-config-saved-pending-restart');
    await checkProcesses();

    const backupSelect = drawer.getByLabel('自动备份版本', { exact: true });
    await eventually('new backup is listed', async () => (await backupSelect.locator('option').allTextContents()).some((text) => text.includes(backup.id)));
    await backupSelect.selectOption(backup.id);
    const beforePreviewWrites = report.writes.filter((item) => item.method === 'PUT').length;
    const [previewResponse] = await Promise.all([
      page.waitForResponse((response) => response.request().method() === 'GET' && new URL(response.url()).pathname === apiPath(opsPath('lower-a', 'config')) && new URL(response.url()).searchParams.get('backup_id') === backup.id),
      drawer.getByRole('button', { name: '载入备份为草稿', exact: true }).click(),
    ]);
    assert.ok(previewResponse.ok());
    const preview = await previewResponse.json();
    assert.equal(preview.content, initialConfig.content);
    assert.equal(preview.revision, saved.revision, 'Backup preview must return current-file CAS, not the historical backup hash');
    assert.equal(preview.backup_revision, initialConfig.revision);
    await eventually('backup copied into draft', async () => (await editor.inputValue()) === initialConfig.content);
    assert.equal(await currentFileRevision(page), saved.revision);
    assert.equal(report.writes.filter((item) => item.method === 'PUT').length, beforePreviewWrites);
    assert.equal((await configRead()).revision, saved.revision, 'Backup loading is only a draft action');
    await validateConfig(page);
    const restored = await saveConfig(page);
    assert.equal(restored.revision, initialConfig.revision);
    assert.equal(restored.restart_required, false);
    await checkProcesses();
  });

  await flow('05-dirty-refresh-escape-and-explicit-discard', async () => {
    const drawer = page.locator('.operations-drawer.is-open');
    const editor = drawer.getByRole('textbox', { name: '配置内容 calc.toml', exact: true });
    const draft = initialConfig.content + '\n# unsaved protected draft ' + runTag + '\n';
    const writeCount = report.writes.filter((item) => item.method === 'PUT').length;
    await editor.fill(draft);
    await Promise.all([
      page.waitForResponse((response) => response.request().method() === 'GET' && new URL(response.url()).pathname === apiPath(opsPath('lower-a', 'config')) && !new URL(response.url()).searchParams.has('backup_id')),
      drawer.getByRole('button', { name: '刷新文件版本（保留草稿）', exact: true }).click(),
    ]);
    assert.equal(await editor.inputValue(), draft);
    assert.ok(await drawer.getByRole('tab', { name: '日志', exact: true }).isDisabled());
    await page.keyboard.press('Escape');
    const dialog = page.getByRole('dialog', { name: '放弃未保存的配置草稿？', exact: true });
    await dialog.waitFor();
    await inspect(page, 'desktop-dirty-close-confirmation', 1);
    await screenshot(page, 'desktop-dirty-close-confirmation');
    await dialog.getByRole('button', { name: '保留草稿，继续编辑', exact: true }).click();
    await dialog.waitFor({ state: 'hidden' });
    assert.equal(await editor.inputValue(), draft);
    await downloadVisible(page, drawer.getByRole('button', { name: '下载草稿', exact: true }), draft, 'unsaved-draft-download.toml');
    await page.keyboard.press('Escape');
    await dialog.waitFor();
    await dialog.getByRole('button', { name: '放弃草稿并关闭', exact: true }).click();
    await drawer.waitFor({ state: 'hidden' });
    assert.equal((await configRead()).content, initialConfig.content);
    assert.equal(report.writes.filter((item) => item.method === 'PUT').length, writeCount);
    await openOperations(page);
    const reopened = await openConfig(page);
    assert.equal(await reopened.editor.inputValue(), initialConfig.content, 'Explicitly discarded draft must not reappear');
  });

  await flow('06-two-real-pages-stale-cas-conflict', async () => {
    const second = await createPage('concurrent-live-sse-withheld', report.environment.desktopViewport, { suppressSSE: true });
    await openOperations(second.page);
    const firstEditor = page.getByRole('textbox', { name: '配置内容 calc.toml', exact: true });
    const { drawer: secondDrawer, editor: secondEditor } = await openConfig(second.page);
    assert.equal(await currentFileRevision(page), initialConfig.revision);
    assert.equal(await currentFileRevision(second.page), initialConfig.revision);
    const firstDraft = initialConfig.content + '\n# first real browser writer ' + runTag + '\n';
    const secondDraft = initialConfig.content + '\n# second stale browser draft ' + runTag + '\n';
    await firstEditor.fill(firstDraft);
    await secondEditor.fill(secondDraft);
    await Promise.all([validateConfig(page), validateConfig(second.page)]);
    const firstSave = await saveConfig(page);
    assert.equal(firstSave.revision, digest(firstDraft));
    assert.equal(await currentFileRevision(second.page), initialConfig.revision, 'Only the second page SSE is withheld; its stale revision is intentional');
    assert.equal(await secondEditor.inputValue(), secondDraft);
    expectFailure(second.page, opsPath('lower-a', 'config'), 'PUT', 409, 'Real backend compare-and-swap rejection of a second stale browser draft; only SSE was withheld');
    const conflict = await saveConfig(second.page, 409);
    assert.equal((conflict.error ?? conflict).code, 'conflict');
    await secondDrawer.getByText(/保存冲突：草稿及原版本已保留/).waitFor();
    assert.equal(await secondEditor.inputValue(), secondDraft);
    assert.equal(await currentFileRevision(second.page), initialConfig.revision, 'Conflict must retain original baseline');
    assert.equal((await configRead()).content, firstDraft);
    await inspect(second.page, 'desktop-real-two-page-409');
    await screenshot(second.page, 'desktop-real-two-page-409');
    await downloadVisible(second.page, secondDrawer.getByRole('button', { name: '下载草稿', exact: true }), secondDraft, 'conflicted-draft-download.toml');

    await reloadConfigExplicitly(page);
    await firstEditor.fill(initialConfig.content);
    await validateConfig(page);
    await saveConfig(page);
    await reloadConfigExplicitly(second.page);
    assert.equal(await secondEditor.inputValue(), initialConfig.content);
    await closeCleanDrawer(second.page);
    await second.context.close();
    await checkProcesses();
  });

  await flow('07-real-log-pagination-filter-download-and-rotation', async () => {
    const name = 'rcd_server_fixture_operations-ui-' + runTag + '.log';
    const emptyName = 'rcd_server_fixture_operations-ui-' + runTag + '-empty.log';
    const text = Array.from({ length: 1800 }, (_, index) => '2026-08-28T12:00:00 ['
      + (index % 7 === 0 ? 'WARN ' : 'INFO ') + '] 中文边界😀 operations-ui-marker line='
      + String(index).padStart(5, '0') + ' payload=' + 'x'.repeat(83) + '\n').join('');
    const syntheticPath = await syntheticLog(name, text);
    await syntheticLog(emptyName, '');
    await chooseTab(page, '日志');
    let log = await chooseLog(page, name);
    await visibleLogEquals(page, log);
    log = await logResponseAfter(page, () => page.getByLabel('日志每页大小', { exact: true }).selectOption('4096'), { name, limit: '4096' });
    await visibleLogEquals(page, log);
    assert.equal(log.eof, true, 'Default view is the finite tail');
    assert.ok(log.offset > 0 && Buffer.byteLength(log.text) <= 4096);
    const first = await logResponseAfter(page, () => page.getByRole('button', { name: '从文件开头读取', exact: true }).click(), { name, offset: '0', limit: '4096' });
    await visibleLogEquals(page, first);
    assert.equal(first.offset, 0);
    assert.equal(first.eof, false);
    const next = await logResponseAfter(page, () => page.getByRole('button', { name: '下一页', exact: true }).click(), { name, offset: first.next_offset, limit: '4096' });
    await visibleLogEquals(page, next);
    assert.equal(next.offset, first.next_offset);
    assert.ok(next.next_offset > next.offset);
    assert.equal(Buffer.concat([Buffer.from(first.text), Buffer.from(next.text)]).toString('utf8'), Buffer.from(text).subarray(0, next.next_offset).toString('utf8'), 'Consecutive unfiltered pages must neither duplicate nor omit UTF-8 bytes');
    const previous = await logResponseAfter(page, () => page.getByRole('button', { name: '上一页', exact: true }).click(), { name, limit: '4096' });
    await visibleLogEquals(page, previous);
    assert.equal(previous.offset, Math.max(0, next.offset - 4096));

    await page.getByLabel('日志关键字', { exact: true }).fill('operations-ui-marker');
    await page.getByLabel('日志等级', { exact: true }).selectOption('WARN');
    const filtered = await logResponseAfter(page, () => page.getByRole('button', { name: '应用日志筛选', exact: true }).click(), { name, q: 'operations-ui-marker', level: 'WARN', limit: '4096' });
    await visibleLogEquals(page, filtered);
    assert.ok(filtered.text);
    assert.ok(filtered.text.split('\n').filter(Boolean).every((line) => line.includes('[WARN ') && line.includes('operations-ui-marker')));
    await downloadVisible(page, page.getByRole('button', { name: '下载当前页', exact: true }), filtered.text, 'filtered-log-current-page.log');
    await page.getByLabel('日志关键字', { exact: true }).fill('');
    await page.getByLabel('日志等级', { exact: true }).selectOption('');
    const unfiltered = await logResponseAfter(page, () => page.getByRole('button', { name: '应用日志筛选', exact: true }).click(), { name, limit: '4096' });
    await visibleLogEquals(page, unfiltered);
    await page.getByLabel('日志内容', { exact: true }).scrollIntoViewIfNeeded();
    await inspect(page, 'desktop-logs-bounded-page');
    await screenshot(page, 'desktop-logs-bounded-page');

    const follow = page.getByRole('checkbox', { name: '尾部轮询（每 5 秒，最多 5 分钟；错误或离开标签即停止）', exact: true });
    assert.equal(await follow.isChecked(), false);
    // Enabling follow first refreshes the file generation asynchronously.
    // check() insists on immediate checked state, before that request settles;
    // click the visible control and verify both its eventual state and the
    // next actual polling request below.
    await logResponseAfter(page, () => follow.click(), { name, limit: '4096', tail: 'true' });
    await eventually('bounded follow enabled', () => follow.isChecked());
    const pollResponse = await page.waitForResponse((response) => response.request().method() === 'GET' && new URL(response.url()).pathname === apiPath(opsPath('lower-a', 'log')) && new URL(response.url()).searchParams.get('name') === name && response.status() === 200, { timeout: 12_000 });
    assert.ok(Buffer.byteLength((await pollResponse.json()).text) <= 4096);
    await follow.uncheck();
    assert.equal(await follow.isChecked(), false);
    const empty = await chooseLog(page, emptyName);
    assert.equal(empty.text, '');
    assert.equal(empty.size_bytes, 0);
    await visibleLogEquals(page, empty);
    await screenshot(page, 'desktop-logs-empty-page');
    log = await chooseLog(page, name);
    const oldGeneration = log.generation;
    const originalMetadata = await lstat(syntheticPath);
    assert.ok(originalMetadata.isFile() && !originalMetadata.isSymbolicLink());
    assert.equal(String(originalMetadata.ino), report.syntheticLogs.find((item) => item.path === syntheticPath).inode, 'Rotate only the file created by this run');
    const rotatedPath = path.join(logDir, 'rcd_server_fixture_operations-ui-' + runTag + '-rotated.log');
    assert.equal(path.dirname(rotatedPath), logDir);
    await assert.rejects(lstat(rotatedPath), { code: 'ENOENT' }, 'Do not overwrite any existing rotated file');
    await rename(syntheticPath, rotatedPath);
    report.syntheticLogs.push({ path: rotatedPath, rotatedFrom: syntheticPath, retained: true });
    const rotatedText = '2026-08-28T12:00:01 [INFO ] 新代次😀 operations-ui-new-generation ' + runTag + '\n';
    await syntheticLog(name, rotatedText);
    expectFailure(page, opsPath('lower-a', 'log'), 'GET', 409, 'Real rotation of this run synthetic file invalidates its old generation');
    await logResponseAfter(page, () => page.getByRole('button', { name: '刷新当前页', exact: true }).click(), { name, generation: oldGeneration }, 409);
    await page.getByText('日志已轮转或代次冲突。', { exact: true }).waitFor();
    assert.ok(await page.getByRole('button', { name: '下一页', exact: true }).isDisabled());
    assert.equal(await follow.isChecked(), false);
    await screenshot(page, 'desktop-real-log-generation-conflict');
    const reset = await logResponseAfter(page, () => page.getByRole('button', { name: '重新从尾部读取', exact: true }).click(), { name, tail: 'true' });
    assert.notEqual(reset.generation, oldGeneration);
    assert.equal(reset.text, rotatedText);
    await visibleLogEquals(page, reset);
    assert.ok(!(await page.getByLabel('日志内容', { exact: true }).textContent()).includes('line='));
    report.logEvidence = { name, emptyName, originalBytes: Buffer.byteLength(text), pageBytes: 4096, firstOffset: first.offset, nextOffset: next.offset, oldGeneration, newGeneration: reset.generation };
  });

  await flow('08-mobile-visible-three-tabs', async () => {
    const mobile = await createPage('mobile-live', report.environment.mobileViewport, { readOnly: true });
    const drawer = await openOperations(mobile.page);
    await drawer.getByRole('region', { name: '采集串口 ' + manifest.sources.a.port_id, exact: true }).waitFor();
    await inspect(mobile.page, 'mobile-acquisition-long-port-path');
    await screenshot(mobile.page, 'mobile-acquisition-long-port-path');
    const { editor } = await openConfig(mobile.page);
    assert.equal(await editor.inputValue(), initialConfig.content);
    await drawer.locator('.operations-file-meta').scrollIntoViewIfNeeded();
    await inspect(mobile.page, 'mobile-config');
    await screenshot(mobile.page, 'mobile-config');
    await chooseTab(mobile.page, '日志');
    const log = await chooseLog(mobile.page, report.logEvidence.name);
    await visibleLogEquals(mobile.page, log);
    await mobile.page.getByLabel('日志内容', { exact: true }).scrollIntoViewIfNeeded();
    await inspect(mobile.page, 'mobile-logs');
    await screenshot(mobile.page, 'mobile-logs');
    await closeCleanDrawer(mobile.page);
    await inspect(mobile.page, 'mobile-source-list-after-close');
    await mobile.context.close();
  });

  await flow('09-injected-legacy-and-offline-rendering-only', async () => {
    report.injections.push({ purpose: 'Secondary legacy/offline presentation smoke only; not evidence of real lower behavior or a substitute for the preceding live flows.', injected: true });
    const legacy = await createPage('injected-legacy-capability', report.environment.desktopViewport, {
      readOnly: true,
      inject: async (context) => context.route('**' + apiPath(opsPath('lower-a', 'capabilities')), (route) => route.fulfill({
        status: 200, contentType: 'application/json', body: JSON.stringify({
          source_id: 'lower-a', source_revision: sourceRevisions.get('lower-a'), supported: false,
          reason: 'Injected legacy capability presentation fixture; not a real lower outage',
          schema_version: 1, features: [], max_config_bytes: 2097152, max_log_bytes: 65536, configuration_apply: 'restart',
        }),
      })),
    });
    const legacyDrawer = await openOperations(legacy.page);
    await legacyDrawer.getByText('此下级未提供运维能力', { exact: true }).waitFor();
    assert.equal(await legacyDrawer.getByRole('tab').count(), 0);
    assert.ok(!report.requests.some((item) => item.page === legacy.label && item.path.endsWith('/status')));
    await inspect(legacy.page, 'injected-legacy-unsupported');
    await screenshot(legacy.page, 'injected-legacy-unsupported');
    await legacy.context.close();

    const offline = await createPage('injected-offline-error', report.environment.desktopViewport, {
      readOnly: true,
      inject: async (context) => {
        await context.route(/\/api\/v1\/sources(?:\?.*)?$/, async (route) => {
          const response = await route.fetch();
          const body = await response.json();
          const source = list(body).find((item) => item.id === 'lower-a');
          assert.ok(source);
          source.connection_state = 'Disconnected';
          await route.fulfill({ response, json: body });
        });
        await context.route('**' + apiPath(opsPath('lower-a', 'capabilities')), (route) => route.fulfill({
          status: 503, contentType: 'application/json', body: JSON.stringify({ error: { code: 'unavailable', message: 'Injected offline lower rendering check; fixture process is still live' } }),
        }));
      },
    });
    expectFailure(offline.page, opsPath('lower-a', 'capabilities'), 'GET', 503, 'Explicit injected offline UI presentation; no real process disconnected');
    const offlineDrawer = await openOperations(offline.page);
    await offlineDrawer.getByText('无法读取下级运维能力', { exact: true }).waitFor();
    await offlineDrawer.getByText(/下级 OPC 当前未连接/).waitFor();
    assert.equal(await offlineDrawer.getByRole('tab').count(), 0);
    await inspect(offline.page, 'injected-offline-error');
    await screenshot(offline.page, 'injected-offline-error');
    await offline.context.close();
    await checkProcesses();
  });
} catch (error) {
  failure = error;
  report.error = error.stack ?? error.message;
  console.error(error.stack ?? error.message);
} finally {
  currentFlow = 'finally-ui-restoration';
  // A fresh page avoids an abandoned dirty dialog and retains the same visible
  // controls/CAS rules as an operator. Never use API PUT as a cleanup shortcut.
  if (browser && desktop && mutationsArmed && initialConfig) {
    let recovery;
    try {
      await checkProcesses();
      const sources = list(await readAPI('/sources'));
      for (const id of ['lower-a', 'lower-b']) {
        const current = sources.find((source) => source.id === id);
        assert.equal(current?.endpoint, fixtureSource(id).endpoint, 'Refuse recovery through a changed Endpoint');
        assert.equal(current?.revision, sourceRevisions.get(id), 'Refuse recovery through a changed source revision');
      }
      // Config and acquisition recovery are independent: a failed file save
      // must not prevent a best-effort reopening of the physical serial port.
      try {
        const current = await configRead();
        if (current.content !== initialConfig.content) {
          recovery = await createPage('restoration-config-live');
          await openOperations(recovery.page);
          const { editor } = await openConfig(recovery.page);
          await editor.fill(initialConfig.content);
          await validateConfig(recovery.page);
          await saveConfig(recovery.page);
          report.restoration.push({ action: 'restore original calc.toml through visible UI', passed: true });
          await closeCleanDrawer(recovery.page);
        }
      } catch (error) {
        failure ??= error;
        report.restoration.push({ action: 'restore original config through visible UI', passed: false, error: error.message });
        if (recovery && !recovery.page.isClosed()) await screenshot(recovery.page, 'failed-config-restoration').catch(() => {});
      }
      recovery = await createPage('restoration-control-live');
      await openOperations(recovery.page);
      try { await setPortEnabled(recovery.page, true); } catch (error) {
        failure ??= error;
        report.restoration.push({ action: 'reopen physical port through visible UI', passed: false, error: error.message });
      }
      for (const device of initialStatus.a.ports[0].devices) {
        try { await setDeviceRead(recovery.page, device.device_id, true); } catch (error) {
          failure ??= error;
          report.restoration.push({ action: 'restore station ' + device.device_id + ' through visible UI', passed: false, error: error.message });
        }
      }
      await eventually('restored a samples', async () => (await sampleMatches('lower-a', 'A_VALUE', true, 11)) && (await sampleMatches('lower-a', 'B_VALUE', true, 22)));
      await lowerBUnchanged();
      const restoredConfig = await configRead();
      assert.equal(restoredConfig.content, initialConfig.content);
      assert.equal(restoredConfig.revision, initialConfig.revision);
      assert.equal(restoredConfig.active_revision, initialConfig.active_revision);
      assert.equal(restoredConfig.restart_required, false);
      for (const original of initialOtherConfigs) {
        const currentDocument = await readAPI(opsPath(original.source, 'config') + '?' + new URLSearchParams({ name: original.name }));
        assert.equal(currentDocument.revision, original.revision, 'Unrelated file changed: ' + original.source + '/' + original.name);
        assert.equal(currentDocument.active_revision, original.activeRevision);
      }
      const [finalGroups, finalSources] = await Promise.all([readAPI('/groups').then(list), readAPI('/sources').then(list)]);
      assert.deepEqual(sortedDefaults(finalGroups), sortedDefaults(initialGroups), 'Default routing must not change during physical operations');
      for (const source of initialSources) {
        const currentSource = finalSources.find((item) => item.id === source.id);
        assert.ok(currentSource);
        assert.equal(currentSource.endpoint, source.endpoint);
        assert.equal(currentSource.revision, source.revision);
        assert.deepEqual(sortedBindings(currentSource), sortedBindings(source));
      }
      assert.equal(digest(await readFile(path.join(fixtureRoot, 'control.json'))), originalControlHash, 'The fixture controller must not be changed');
      await checkProcesses();
      report.restoration.push({
        action: 'all acquisition enabled, original config restored, other files/source/default routes/processes unchanged',
        passed: true, calcRevision: restoredConfig.revision, controls: controlSummary(await readStatus()),
        originalProcess, backupsAndSyntheticLogsRetained: true,
      });
      await recovery.page.locator('.operations-port-heading').first().scrollIntoViewIfNeeded();
      await screenshot(recovery.page, 'final-restored-live-acquisition');
      await closeCleanDrawer(recovery.page);
    } catch (error) {
      report.restoration.push({ action: 'visible UI recovery', passed: false, error: error.stack ?? error.message });
      failure ??= error;
      if (recovery && !recovery.page.isClosed()) await screenshot(recovery.page, 'failed-ui-restoration').catch(() => {});
      console.error('RESTORATION FAILED: ' + (error.stack ?? error.message));
    }
  } else if (originalProcess && ownedProcesses) {
    try { await checkProcesses(); report.restoration.push({ action: 'preflight failed before writes; processes unchanged', passed: true }); } catch (error) {
      failure ??= error;
      report.restoration.push({ action: 'preflight process verification', passed: false, error: error.message });
    }
  }
  if (browser) await browser.close().catch((error) => { failure ??= error; report.closeError = error.message; });
  // Expected 409/503 browser resource errors are retained and tied to the
  // exact observed request. JavaScript exceptions and unrelated HTTP failures
  // are never hidden by the negative test flows.
  for (const entry of report.consoleErrors) {
    entry.expected = entry.kind === 'console' && /Failed to load resource/.test(entry.message) && report.httpErrors.some((response) =>
      response.expected && response.page === entry.page && response.url === entry.location?.url && Math.abs(response.at - entry.at) < 3000);
  }
  const unexpectedHTTP = report.httpErrors.filter((entry) => !entry.expected);
  const unexpectedConsole = report.consoleErrors.filter((entry) => !entry.expected);
  report.finishedAt = new Date().toISOString();
  report.summary = {
    flowsPassed: report.flows.filter((entry) => entry.passed).length,
    flowsFailed: report.flows.filter((entry) => !entry.passed).length,
    expectedNegativeResponses: report.httpErrors.filter((entry) => entry.expected).length,
    unexpectedHTTP: unexpectedHTTP.length, unexpectedConsole: unexpectedConsole.length,
    deniedWrites: report.deniedWrites.length,
    restorationPassed: report.restoration.length > 0 && report.restoration.every((entry) => entry.passed),
  };
  report.ok = !failure && report.summary.flowsFailed === 0 && unexpectedHTTP.length === 0 && unexpectedConsole.length === 0 && report.deniedWrites.length === 0 && report.summary.restorationPassed;
  await writeFile(path.join(outputDir, 'report.json'), JSON.stringify(report, null, 2) + '\n', 'utf8');
  console.log(JSON.stringify({ ok: report.ok, report: path.join(outputDir, 'report.json'), ...report.summary }));
  if (!report.ok) process.exitCode = 1;
}
