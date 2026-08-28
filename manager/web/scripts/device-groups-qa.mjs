/**
 * Real-UI regression against the isolated two-Rust-lower deployment.
 * Run ON THE REMOTE LINUX TEST HOST, never on the development workstation:
 *
 * RCD_QA_REMOTE=1 RCD_QA_ALLOW_MUTATIONS=1 \
 * RCD_QA_OUTPUT=/absolute/path/outside-the-checkout/device-groups-ui \
 * RCD_QA_PLAYWRIGHT_ROOT=/path/to/existing/browser-tools \
 * node manager/web/scripts/device-groups-qa.mjs
 * Alternatively PLAYWRIGHT_MODULE=/absolute/path/node_modules/playwright/index.js
 * loads an existing installation without changing project dependencies.
 *
 * RCD_QA_URL defaults to http://127.0.0.1:8083. An optional
 * RCD_QA_SESSION_ID chooses an already-open OPC Session for the isolation flow.
 * No OPC Sessions are created or closed here. Existing Sessions cause mapping
 * writes to be skipped and the mapping lock to be checked instead.
 * Every configuration write is performed by a visible UI control. Fetch is
 * read-only, used for fixture discovery and post-action assertions.
 */
import assert from 'node:assert/strict';
import { mkdir, realpath, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

assert.equal(process.env.RCD_QA_REMOTE, '1', 'Set RCD_QA_REMOTE=1 only on the remote test host');
assert.equal(process.platform, 'linux', 'This regression must run on the remote Linux deployment');
const baseURL = (process.env.RCD_QA_URL ?? 'http://127.0.0.1:8083').replace(/\/$/, '');
const allowMutations = process.env.RCD_QA_ALLOW_MUTATIONS === '1';
const outputInput = process.env.RCD_QA_OUTPUT;
assert.ok(outputInput && path.isAbsolute(outputInput), 'RCD_QA_OUTPUT must be an explicit absolute directory outside the checkout');
const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = await realpath(process.env.RCD_QA_REPO_ROOT ?? path.resolve(scriptDirectory, '../../..'));
const inside = (parent, child) => {
  const relative = path.relative(parent, child);
  return relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative));
};
async function resolvedDestination(value) {
  try { return await realpath(value); } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    return path.join(await resolvedDestination(path.dirname(value)), path.basename(value));
  }
}
const outputDir = await resolvedDestination(path.resolve(outputInput));
assert.ok(!inside(repositoryRoot, outputDir), 'Screenshots/reports must not be written inside the checkout (including through symlinks)');
assert.notEqual(outputDir, path.parse(outputDir).root, 'Use a dedicated evidence directory, not a filesystem root');
await mkdir(outputDir, { recursive: true });

const requirePlaywright = createRequire(path.join(process.env.RCD_QA_PLAYWRIGHT_ROOT ?? path.resolve(scriptDirectory, '..'), 'package.json'));
if (process.env.PLAYWRIGHT_MODULE) assert.ok(path.isAbsolute(process.env.PLAYWRIGHT_MODULE), 'PLAYWRIGHT_MODULE must be absolute');
const { chromium } = requirePlaywright(process.env.PLAYWRIGHT_MODULE ?? 'playwright');
const report = {
  startedAt: new Date().toISOString(),
  environment: {
    baseURL, host: os.hostname(), platform: process.platform, outputDir, repositoryRoot,
    desktopViewport: { width: 1600, height: 1000 }, mobileViewport: { width: 390, height: 844 },
    browserAvailability: 'available-on-local-workstation',
    browserPath: 'remote Playwright Chromium',
    browserPathReason: 'Explicit user constraint: every build/test/browser must run on the remote Linux host; local Browser plugin is not used.',
    allowMutations,
  },
  flow: '下级列表 → 串口/设备映射 → 默认源切换 → 映射往返恢复 → 串口/设备分页过滤 → 双源比较 → 空结果 → 可选目标Session切换 → 390px窄屏',
  checks: [], flows: [], screenshots: [], consoleErrors: [], consoleWarnings: [], httpErrors: [], writes: [], pointRequests: [], skips: [], restoration: [],
};
let browser;
let desktop;
let initialSources;
let initialGroups;
let initialSessions;
let topologies;
let mappingNeedsRestore = false;
let defaultNeedsRestore = false;
let sessionNeedsRestore;

const list = (value) => Array.isArray(value) ? value : (value.items ?? value.data?.items ?? value.data ?? []);
const sortedBindings = (source) => (source.group_bindings ?? []).map((binding) => [binding.port_id, binding.device_id, binding.group_id]).sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)));
const routes = (session) => session.routes ?? session.selected_sources ?? {};
const routeSource = (session, groupId) => typeof routes(session)[groupId] === 'string' ? routes(session)[groupId] : (routes(session)[groupId]?.source_id ?? '');
const routeRevision = (session, groupId) => routes(session)[groupId]?.revision ?? 1;
const ids = { sourceA: 'lower-a', sourceB: 'lower-b', groupA: 'device-a', groupB: 'device-b', shared: 'shared' };

async function readAPI(suffix) {
  const response = await desktop.context.request.get(`${baseURL}/api/v1${suffix}`);
  assert.ok(response.ok(), `Read-only API ${suffix} returned ${response.status()}: ${(await response.text()).slice(0, 400)}`);
  return response.json();
}
async function eventually(label, check, timeout = 30_000) {
  const end = Date.now() + timeout;
  let lastError;
  do {
    try { const value = await check(); if (value) return value; } catch (error) { lastError = error; }
    await new Promise((resolve) => setTimeout(resolve, 250));
  } while (Date.now() < end);
  throw new Error(`Timed out: ${label}${lastError ? ` (${lastError.message})` : ''}`);
}
async function createPage(viewport) {
  const context = await browser.newContext({ viewport, locale: 'zh-CN' });
  const page = await context.newPage();
  page.setDefaultTimeout(20_000);
  page.setDefaultNavigationTimeout(30_000);
  page.on('console', (message) => {
    if (message.type() === 'error') report.consoleErrors.push({ viewport, url: page.url(), message: message.text() });
    if (message.type() === 'warning') report.consoleWarnings.push({ viewport, url: page.url(), message: message.text() });
  });
  page.on('pageerror', (error) => report.consoleErrors.push({ viewport, url: page.url(), message: error.message }));
  page.on('response', (response) => {
    if (response.status() >= 400 && response.url().startsWith(baseURL)) report.httpErrors.push({ method: response.request().method(), url: response.url(), status: response.status() });
  });
  page.on('request', (request) => {
    if (request.method() === 'GET' && new URL(request.url()).pathname === '/api/v1/points') {
      report.pointRequests.push({ viewport, query: Object.fromEntries(new URL(request.url()).searchParams) });
      if (report.pointRequests.length > 1000) report.pointRequests.shift();
    }
    if (request.url().startsWith(`${baseURL}/api/v1/`) && !['GET', 'HEAD', 'OPTIONS'].includes(request.method())) {
      const body = request.postDataJSON();
      report.writes.push({ method: request.method(), path: new URL(request.url()).pathname, ifMatch: request.headers()['if-match'], groupBindings: body?.group_bindings, sourceId: body?.source_id });
    }
  });
  return { context, page };
}
async function navigate(page, route, heading) {
  // Hash navigation to the current route does not remount React. Close the
  // previous flow's overlays through their visible controls before navigating;
  // otherwise a topology drawer can cover the next source's table button.
  // Callers use this helper only after a save has been verified, or when the
  // flow explicitly abandons a read-only/locked draft (including recovery).
  while (await page.getByRole('dialog').count()) {
    const dialog = page.getByRole('dialog').last();
    await dialog.getByRole('button', { name: '关闭弹窗', exact: true }).click();
    await dialog.waitFor({ state: 'hidden' });
  }
  while (await page.locator('.detail-drawer.is-open').count()) {
    const drawer = page.locator('.detail-drawer.is-open').last();
    await drawer.getByRole('button', { name: '关闭详情', exact: true }).click();
    await drawer.waitFor({ state: 'hidden' });
  }
  // Production embeds a FileServer without an SPA pathname fallback. React
  // owns the fragment via HashRouter; /sources would be a filesystem 404.
  await page.goto(`${baseURL}/#${route}`, { waitUntil: 'domcontentloaded' });
  await page.getByRole('heading', { name: heading, exact: true }).waitFor();
}
function routeURL(page) {
  const documentURL = new URL(page.url());
  return new URL(documentURL.hash.slice(1) || '/', documentURL.origin);
}
async function inspect(page, name, expectedPath, modal = false) {
  const evidence = await page.evaluate(() => ({
    title: document.title,
    h1: document.querySelector('h1')?.textContent?.trim() ?? '',
    bodyLength: document.body.innerText.trim().length,
    viewportWidth: innerWidth,
    documentWidth: document.documentElement.scrollWidth,
    bodyWidth: document.body.scrollWidth,
    frameworkOverlay: Boolean(document.querySelector('vite-error-overlay, nextjs-portal, #webpack-dev-server-client-overlay')),
    visibleDialogs: [...document.querySelectorAll('[role="dialog"]')].filter((element) => element.getClientRects().length > 0).length,
  }));
  const checks = {
    pageIdentity: routeURL(page).pathname === expectedPath && /RCD|管理/i.test(evidence.title),
    nonblank: Boolean(evidence.h1) && evidence.bodyLength > 150,
    noFrameworkOverlay: !evidence.frameworkOverlay,
    noDocumentOverflow: Math.max(evidence.documentWidth, evidence.bodyWidth) <= evidence.viewportWidth + 1,
    expectedDialogState: modal ? evidence.visibleDialogs === 1 : evidence.visibleDialogs === 0,
  };
  report.checks.push({ name, url: page.url(), ...checks, evidence });
  assert.ok(Object.values(checks).every(Boolean), `Rendered checks failed for ${name}: ${JSON.stringify(checks)}`);
}
async function screenshot(page, name) {
  const destination = path.join(outputDir, `${name}.png`);
  await page.screenshot({ path: destination, fullPage: false });
  report.screenshots.push({ name, path: destination, url: page.url() });
}
async function flow(name, action) {
  const started = Date.now();
  try {
    await action();
    report.flows.push({ name, passed: true, durationMs: Date.now() - started });
  } catch (error) {
    report.flows.push({ name, passed: false, durationMs: Date.now() - started, error: error.stack ?? error.message });
    if (desktop?.page && !desktop.page.isClosed()) await screenshot(desktop.page, `failed-${name}`).catch(() => {});
    throw error;
  }
}
async function clickMutation(page, button, suffix, expectedRevision) {
  await eventually(`button enabled: ${suffix}`, () => button.isEnabled());
  const [response] = await Promise.all([
    page.waitForResponse((candidate) => candidate.request().method() === 'PUT' && new URL(candidate.url()).pathname === `/api/v1${suffix}`),
    button.click(),
  ]);
  assert.equal(response.request().headers()['if-match'], `"${expectedRevision}"`, 'The UI must submit the observed revision through If-Match');
  const result = await response.json();
  assert.ok(response.ok(), `UI mutation ${suffix}: ${response.status()} ${JSON.stringify(result)}`);
  return result;
}
async function openTopology(page, sourceId) {
  await navigate(page, '/sources', '下级 OPC');
  await page.getByRole('button', { name: `串口 / 设备映射 ${sourceId}`, exact: true }).click();
  const drawer = page.locator('.topology-drawer.is-open');
  await drawer.getByRole('heading', { name: `串口 / 设备映射 · ${sourceId}`, exact: true }).waitFor();
  const topology = topologies[sourceId];
  const port = topology.ports.find((item) => item.devices.some((device) => device.device_id === '1'));
  await drawer.getByLabel(`${port.port_id} 设备 1 逻辑组`, { exact: true }).waitFor();
  return drawer;
}
async function selectDialogSource(dialog, sourceId) {
  // SourceSwitchDialog intentionally hides its native input underneath the
  // styled radio. Click the visible label text as an operator would, then
  // verify the associated input rather than forcing a click through the skin.
  const card = dialog.locator(`label.source-option:has(input[type="radio"][value=${JSON.stringify(sourceId)}])`);
  const radio = card.locator('input[type="radio"]');
  await card.waitFor({ state: 'visible' });
  assert.equal(await radio.inputValue(), sourceId);
  await eventually(`source option enabled: ${sourceId}`, () => radio.isEnabled());
  await card.getByText(sourceId, { exact: true }).click();
  await eventually(`source card selection settled: ${sourceId}`, () => radio.isChecked(), 20_000);
  const confirmation = dialog.locator('label.confirm-check');
  const checkbox = confirmation.locator('input[type="checkbox"]');
  if (!await checkbox.isChecked()) await confirmation.getByText('确认已核对目标下级状态', { exact: true }).click();
  await eventually('source confirmation checkbox settled', () => checkbox.isChecked(), 20_000);
}
async function changeDefault(page, sourceId, evidenceName) {
  const before = list(await readAPI('/groups')).find((group) => group.id === ids.groupA);
  if (before.default_source_id === sourceId) return before;
  await navigate(page, '/groups', '分组与选源');
  await page.getByRole('button', { name: `查看 ${ids.groupA}`, exact: true }).click();
  const drawer = page.locator('.detail-drawer.is-open');
  await drawer.getByRole('button', { name: '设置新 Session 默认源', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: '设置逻辑组默认数据源', exact: true });
  await selectDialogSource(dialog, sourceId);
  if (evidenceName) {
    await inspect(page, evidenceName, '/groups', true);
    await screenshot(page, evidenceName);
  }
  await clickMutation(page, dialog.getByRole('button', { name: '确认设置', exact: true }), `/groups/${ids.groupA}/default-source`, before.revision);
  await dialog.waitFor({ state: 'hidden' });
  return eventually(`default source ${sourceId}`, async () => {
    const group = list(await readAPI('/groups')).find((item) => item.id === ids.groupA);
    return group.default_source_id === sourceId && group.revision === before.revision + 1 ? group : undefined;
  });
}
async function changeMapping(page, targetGroup, evidenceName) {
  const before = await readAPI(`/sources/${ids.sourceA}`);
  const drawer = await openTopology(page, ids.sourceA);
  const port = topologies[ids.sourceA].ports.find((item) => item.devices.some((device) => device.device_id === '1'));
  await drawer.getByLabel(`${port.port_id} 设备 1 逻辑组`, { exact: true }).selectOption(targetGroup);
  const saved = await clickMutation(page, drawer.getByRole('button', { name: '保存设备映射', exact: true }), `/sources/${ids.sourceA}`, before.revision);
  assert.equal(saved.revision, before.revision + 1);
  await eventually('mapping refresh settled', async () => {
    const source = await readAPI(`/sources/${ids.sourceA}`);
    return source.revision === saved.revision && await drawer.getByRole('button', { name: '保存设备映射', exact: true }).isDisabled();
  });
  if (evidenceName) { await inspect(page, evidenceName, '/sources'); await screenshot(page, evidenceName); }
  return saved;
}
async function ensureCatalogsReady(page) {
  await eventually('catalog conflict resolved', async () => list(await readAPI('/sources')).filter((source) => [ids.sourceA, ids.sourceB].includes(source.id)).every((source) => ['Ready', 'Cached'].includes(source.catalog_state)));
  const cached = list(await readAPI('/sources')).filter((source) => [ids.sourceA, ids.sourceB].includes(source.id) && source.catalog_state === 'Cached');
  if (cached.length) {
    await navigate(page, '/sources', '下级 OPC');
    for (const source of cached) {
      const button = page.getByRole('button', { name: `重新连接并发现目录 ${source.id}`, exact: true });
      await eventually('rediscover enabled', () => button.isEnabled());
      await button.click();
    }
  }
  await eventually('both catalogs Ready', async () => list(await readAPI('/sources')).filter((source) => [ids.sourceA, ids.sourceB].includes(source.id)).every((source) => source.catalog_state === 'Ready'), 45_000);
}
async function pointNames(page, selector = '.points-table') {
  return page.locator(`${selector} tbody tr td:first-child strong`).allTextContents();
}
function pointSelect(page, caption, toolbar = '.points-toolbar') {
  // PointsPage wraps each native select in a label with a visible caption
  // span. The label's full text also contains its options, so an exact
  // getByLabel(caption) does not identify these controls reliably.
  return page.locator(toolbar).locator('label.field')
    .filter({ has: page.getByText(caption, { exact: true }) }).locator('select');
}
async function setComparison(page, enabled) {
  const label = page.locator('.points-toolbar label.comparison-toggle');
  const checkbox = label.locator('input[type="checkbox"]');
  if (await checkbox.isChecked() !== enabled) await label.getByText('双来源对比', { exact: true }).click();
  // checked is derived from useSearchParams. The label click can resolve
  // before HashRouter commits the new controlled value; wait for both the
  // route and rendered input, without clicking again and reversing the intent.
  await eventually(`comparison route and checkbox settled: ${enabled}`, async () => {
    return (routeURL(page).searchParams.get('compare') === '1') === enabled && await checkbox.isChecked() === enabled;
  }, 20_000);
}
async function filterDeviceOne(page) {
  await navigate(page, '/points', '点位浏览');
  await pointSelect(page, '逻辑组').selectOption(ids.groupA);
  await pointSelect(page, '目录成员').selectOption(ids.sourceA);
  const port = topologies[ids.sourceA].ports.find((item) => item.devices.some((device) => device.device_id === '1'));
  await eventually('port selector loaded', async () => pointSelect(page, '串口').locator(`option[value=${JSON.stringify(port.port_id)}]`).count());
  await pointSelect(page, '串口').selectOption(port.port_id);
  await pointSelect(page, '设备').selectOption('1');
  await eventually('device-one point rows', async () => JSON.stringify((await pointNames(page)).sort()) === JSON.stringify(['A_VALUE', 'CALC_A']));
  assert.match(await page.locator('.pagination').innerText(), /共 2 条/);
  assert.equal(await page.getByRole('button', { name: '下一页', exact: true }).isDisabled(), true);
  const url = routeURL(page);
  assert.equal(url.searchParams.get('port'), port.port_id);
  assert.equal(url.searchParams.get('device'), '1');
  assert.ok(report.pointRequests.some(({ query }) => query.limit === '50' && query.offset === '0' && query.group_id === ids.groupA && query.source_id === ids.sourceA && query.port_id === port.port_id && query.device_id === '1'), 'The rendered point filter must issue a paginated server request with physical ownership filters');
}
async function checkSessionIsolation(page, target, nextSource, evidenceName) {
  const before = list(await readAPI('/sessions'));
  const original = before.find((session) => session.id === target);
  assert.ok(original, 'Target OPC Session must still exist');
  await navigate(page, '/groups', '分组与选源');
  await page.locator('.session-context-picker select').selectOption(target);
  await page.getByRole('button', { name: `查看 ${ids.groupA}`, exact: true }).click();
  const drawer = page.locator('.detail-drawer.is-open');
  await drawer.locator('.drawer-session-field select').selectOption(target);
  await drawer.getByRole('button', { name: '切换当前 Session 的整组来源', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: '切换整逻辑设备组', exact: true });
  assert.match(await dialog.innerText(), /其他组|其他设备组/);
  await selectDialogSource(dialog, nextSource);
  if (evidenceName) { await inspect(page, evidenceName, '/groups', true); await screenshot(page, evidenceName); }
  await clickMutation(page, dialog.getByRole('button', { name: '确认切换', exact: true }), `/sessions/${encodeURIComponent(target)}/groups/${ids.groupA}/selection`, routeRevision(original, ids.groupA));
  await dialog.waitFor({ state: 'hidden' });
  const after = list(await readAPI('/sessions'));
  for (const previous of before) {
    const current = after.find((session) => session.id === previous.id);
    assert.ok(current, `Session ${previous.id} ended during the isolation check`);
    for (const groupId of Object.keys(routes(previous))) {
      if (previous.id === target && groupId === ids.groupA) {
        assert.equal(routeSource(current, groupId), nextSource);
        assert.equal(routeRevision(current, groupId), routeRevision(previous, groupId) + 1);
      } else {
        assert.equal(routeSource(current, groupId), routeSource(previous, groupId), 'Other Session/group selection changed');
        assert.equal(routeRevision(current, groupId), routeRevision(previous, groupId), 'Other Session/group revision changed');
      }
    }
  }
}

try {
  browser = await chromium.launch({ headless: true });
  desktop = await createPage(report.environment.desktopViewport);
  const page = desktop.page;
  await flow('fixture-and-overview', async () => {
    await navigate(page, '/', '运行总览');
    [initialSources, initialGroups, initialSessions] = await Promise.all([readAPI('/sources').then(list), readAPI('/groups').then(list), readAPI('/sessions').then(list)]);
    for (const id of [ids.sourceA, ids.sourceB]) assert.ok(initialSources.some((source) => source.id === id), `Missing fixture source ${id}`);
    for (const id of [ids.shared, ids.groupA, ids.groupB]) assert.ok(initialGroups.some((group) => group.id === id), `Missing fixture group ${id}`);
    topologies = Object.fromEntries(await Promise.all([ids.sourceA, ids.sourceB].map(async (id) => [id, await readAPI(`/sources/${id}/topology`)])));
    for (const id of [ids.sourceA, ids.sourceB]) {
      const topology = topologies[id];
      assert.equal(topology.supported, true);
      const port = topology.ports.find((item) => item.devices.some((device) => device.device_id === '1') && item.devices.some((device) => device.device_id === '2'));
      assert.ok(port, `${id} must have device 1 and 2 on one serial port`);
      assert.equal(port.devices.find((device) => device.device_id === '1').group_id, ids.groupA);
      assert.equal(port.devices.find((device) => device.device_id === '2').group_id, ids.groupB);
    }
    report.fixture = { sessionsAtStart: initialSessions.length, sources: initialSources.map((source) => ({ id: source.id, revision: source.revision, groupBindings: source.group_bindings })), defaultSources: initialGroups.map((group) => ({ id: group.id, sourceId: group.default_source_id, revision: group.revision })) };
    await inspect(page, 'overview-desktop', '/');
    await screenshot(page, 'overview-desktop');
  });
  await flow('topology-and-noop', async () => {
    for (const sourceId of [ids.sourceA, ids.sourceB]) {
      const drawer = await openTopology(page, sourceId);
      const port = topologies[sourceId].ports.find((item) => item.devices.some((device) => device.device_id === '1'));
      assert.equal(await drawer.getByLabel(`${port.port_id} 设备 1 逻辑组`, { exact: true }).inputValue(), ids.groupA);
      assert.equal(await drawer.getByLabel(`${port.port_id} 设备 2 逻辑组`, { exact: true }).inputValue(), ids.groupB);
      assert.equal(await drawer.getByRole('button', { name: '保存设备映射', exact: true }).isDisabled(), true, 'Unchanged mappings must not create a redundant revision');
      assert.match(await drawer.innerText(), /分步保存第一个来源后/);
      await inspect(page, `mapping-${sourceId}-desktop`, '/sources');
      await screenshot(page, `mapping-${sourceId}-desktop`);
    }
  });
  await flow('default-source-and-mapping-roundtrip', async () => {
    if (!allowMutations) { report.skips.push({ flow: 'default and mapping writes', reason: 'Set RCD_QA_ALLOW_MUTATIONS=1 on the authorized test deployment' }); return; }
    const originalDefault = initialGroups.find((group) => group.id === ids.groupA).default_source_id;
    assert.equal(originalDefault, ids.sourceA, 'Fixture device-a default must start at lower-a for the reversible mutation flow');
    defaultNeedsRestore = true;
    await changeDefault(page, ids.sourceB, 'default-switch-dialog-desktop');
    const sessionsNow = list(await readAPI('/sessions'));
    for (const previous of initialSessions) {
      const current = sessionsNow.find((session) => session.id === previous.id);
      assert.ok(current, 'An existing Session ended during default-source validation');
      assert.deepEqual(routes(current), routes(previous), 'Default-source changes must not change an existing Session');
    }
    if (sessionsNow.length || process.env.RCD_QA_SKIP_MAPPING_SAVE === '1') {
      const drawer = await openTopology(page, ids.sourceA);
      if (sessionsNow.length) {
        const port = topologies[ids.sourceA].ports.find((item) => item.devices.some((device) => device.device_id === '1'));
        await drawer.getByLabel(`${port.port_id} 设备 1 逻辑组`, { exact: true }).selectOption('');
        assert.equal(await drawer.getByRole('button', { name: '保存设备映射', exact: true }).isDisabled(), true);
        await drawer.locator('.topology-lock').waitFor();
        await screenshot(page, 'mapping-locked-by-sessions');
      }
      report.skips.push({ flow: 'mapping write roundtrip', reason: sessionsNow.length ? `${sessionsNow.length} OPC Sessions are online; lock verified without closing them` : 'RCD_QA_SKIP_MAPPING_SAVE=1' });
    } else {
      mappingNeedsRestore = true;
      const intermediate = await changeMapping(page, '', 'mapping-temporary-conflict');
      assert.equal(intermediate.revision, initialSources.find((source) => source.id === ids.sourceA).revision + 1);
      await eventually('temporary cross-group catalog conflict', async () => {
        const sources = list(await readAPI('/sources'));
        return [ids.sourceA, ids.sourceB].every((id) => sources.find((source) => source.id === id)?.catalog_state === 'CatalogConflict');
      });
      await changeMapping(page, ids.groupA, 'mapping-restored-desktop');
      const restored = await readAPI(`/sources/${ids.sourceA}`);
      assert.deepEqual(sortedBindings(restored), sortedBindings(initialSources.find((source) => source.id === ids.sourceA)));
      assert.equal(restored.revision, initialSources.find((source) => source.id === ids.sourceA).revision + 2);
      mappingNeedsRestore = false;
      await ensureCatalogsReady(page);
    }
  });
  await flow('device-point-filter-and-comparison', async () => {
    await filterDeviceOne(page);
    const expectedDefault = list(await readAPI('/groups')).find((group) => group.id === ids.groupA).default_source_id;
    const selectedCells = await page.locator('.points-table tbody tr td:nth-child(4)').allTextContents();
    assert.ok(selectedCells.every((cell) => cell.includes(expectedDefault)), 'Directory source A must not override the displayed default route');
    await inspect(page, 'device-one-points-desktop', '/points');
    await screenshot(page, 'device-one-points-desktop');
    await setComparison(page, true);
    await pointSelect(page, '左侧', '.route-view-toolbar').selectOption(ids.sourceA);
    await pointSelect(page, '右侧', '.route-view-toolbar').selectOption(ids.sourceB);
    await eventually('same two KKS on both comparison sides', async () => JSON.stringify((await pointNames(page, '.comparison-table')).sort()) === JSON.stringify(['A_VALUE', 'CALC_A']));
    const comparisonRows = await page.locator('.comparison-table tbody tr').allTextContents();
    assert.ok(comparisonRows.every((row) => !/点位不存在|来源缺少该点/.test(row)));
    await inspect(page, 'same-kks-comparison-desktop', '/points');
    await screenshot(page, 'same-kks-comparison-desktop');
  });
  await flow('shared-calculations-and-empty-state', async () => {
    await setComparison(page, false);
    await pointSelect(page, '逻辑组').selectOption(ids.shared);
    const search = page.getByPlaceholder('搜索 Node ID 或 BrowseName', { exact: true });
    await search.fill('CALC');
    await page.getByRole('button', { name: '搜索', exact: true }).click();
    await eventually('cross-device and constant calculations remain shared', async () => JSON.stringify((await pointNames(page)).sort()) === JSON.stringify(['CALC_AB', 'CALC_CONST']));
    assert.ok((await page.locator('.points-table tbody tr td:nth-child(3)').allTextContents()).every((value) => value.trim() === ids.shared));
    await screenshot(page, 'shared-calculations-desktop');
    await search.fill('__RCD_QA_NO_SUCH_KKS__');
    await page.getByRole('button', { name: '搜索', exact: true }).click();
    await page.locator('.page-state-empty').getByText('没有匹配的点位', { exact: true }).waitFor();
    await inspect(page, 'empty-points-desktop', '/points');
    await screenshot(page, 'empty-points-desktop');
  });
  await flow('restore-default-and-optional-session-isolation', async () => {
    if (defaultNeedsRestore) {
      await changeDefault(page, initialGroups.find((group) => group.id === ids.groupA).default_source_id);
      defaultNeedsRestore = false;
    }
    const sessions = list(await readAPI('/sessions'));
    if (!allowMutations || !sessions.length) { report.skips.push({ flow: 'Session selection isolation', reason: !sessions.length ? 'No OPC Session is online; run again with the separate holdSession fixture' : 'Mutation opt-in is disabled' }); return; }
    const target = process.env.RCD_QA_SESSION_ID ? sessions.find((session) => session.id === process.env.RCD_QA_SESSION_ID) : sessions[0];
    assert.ok(target, 'RCD_QA_SESSION_ID is not an online OPC Session');
    const originalSource = routeSource(target, ids.groupA);
    assert.ok([ids.sourceA, ids.sourceB].includes(originalSource), 'Held target Session must already select lower-a or lower-b');
    sessionNeedsRestore = { id: target.id, sourceId: originalSource };
    await checkSessionIsolation(page, target.id, originalSource === ids.sourceA ? ids.sourceB : ids.sourceA, 'target-session-switch-dialog');
    await checkSessionIsolation(page, target.id, originalSource);
    sessionNeedsRestore = undefined;
    await inspect(page, 'session-group-drawer-restored', '/groups');
    await screenshot(page, 'session-group-drawer-restored');
  });
  await flow('mobile-390px', async () => {
    const mobile = await createPage(report.environment.mobileViewport);
    try {
      await navigate(mobile.page, '/', '运行总览');
      await inspect(mobile.page, 'overview-mobile', '/');
      await screenshot(mobile.page, 'overview-mobile');
      await mobile.page.getByRole('button', { name: '打开导航', exact: true }).click();
      await mobile.page.getByRole('link', { name: '下级 OPC', exact: true }).click();
      await mobile.page.getByRole('heading', { name: '下级 OPC', exact: true }).waitFor();
      await mobile.page.getByRole('button', { name: `串口 / 设备映射 ${ids.sourceA}`, exact: true }).click();
      const drawer = mobile.page.locator('.topology-drawer.is-open');
      await drawer.locator('.topology-table').first().waitFor();
      await drawer.locator('.topology-table').first().scrollIntoViewIfNeeded();
      await inspect(mobile.page, 'mapping-mobile', '/sources');
      await screenshot(mobile.page, 'mapping-mobile');
      await drawer.getByRole('button', { name: '关闭详情', exact: true }).click();
      await filterDeviceOne(mobile.page);
      await inspect(mobile.page, 'device-one-points-mobile', '/points');
      await screenshot(mobile.page, 'device-one-points-mobile');
    } finally { await mobile.context.close(); }
  });
} catch (error) {
  report.failure = error.stack ?? error.message;
} finally {
  // Recovery uses the same real UI as the assertions, never an API write. Each
  // flag remains set until read-back verifies the original state is restored.
  if (desktop && initialSources && initialGroups) {
    if (mappingNeedsRestore) {
      try {
        const source = await readAPI(`/sources/${ids.sourceA}`);
        if (JSON.stringify(sortedBindings(source)) !== JSON.stringify(sortedBindings(initialSources.find((item) => item.id === ids.sourceA)))) await changeMapping(desktop.page, ids.groupA);
        await ensureCatalogsReady(desktop.page);
        mappingNeedsRestore = false;
        report.restoration.push({ operation: 'device mapping', restored: true });
      } catch (error) { report.restoration.push({ operation: 'device mapping', restored: false, error: error.message }); }
    }
    if (defaultNeedsRestore) {
      try {
        await changeDefault(desktop.page, initialGroups.find((group) => group.id === ids.groupA).default_source_id);
        defaultNeedsRestore = false;
        report.restoration.push({ operation: 'group default', restored: true });
      } catch (error) { report.restoration.push({ operation: 'group default', restored: false, error: error.message }); }
    }
    if (sessionNeedsRestore) {
      try {
        const current = list(await readAPI('/sessions')).find((session) => session.id === sessionNeedsRestore.id);
        if (current && routeSource(current, ids.groupA) !== sessionNeedsRestore.sourceId) await checkSessionIsolation(desktop.page, sessionNeedsRestore.id, sessionNeedsRestore.sourceId);
        sessionNeedsRestore = undefined;
        report.restoration.push({ operation: 'Session selection', restored: true });
      } catch (error) { report.restoration.push({ operation: 'Session selection', restored: false, error: error.message }); }
    }
    try {
      const [sources, groups] = await Promise.all([readAPI('/sources').then(list), readAPI('/groups').then(list)]);
      for (const original of initialSources) assert.deepEqual(sortedBindings(sources.find((source) => source.id === original.id)), sortedBindings(original), `Final mappings differ for ${original.id}`);
      for (const original of initialGroups) assert.equal(groups.find((group) => group.id === original.id)?.default_source_id, original.default_source_id, `Final default differs for ${original.id}`);
      report.restoration.push({ operation: 'final read-only configuration comparison', restored: true });
    } catch (error) { report.restoration.push({ operation: 'final read-only configuration comparison', restored: false, error: error.message }); }
  }
  report.checks.push({ name: 'console-health', passed: report.consoleErrors.length === 0, errors: report.consoleErrors.length, warnings: report.consoleWarnings.length });
  report.checks.push({ name: 'http-health', passed: report.httpErrors.length === 0, errors: report.httpErrors.length });
  report.finishedAt = new Date().toISOString();
  report.ok = !report.failure && report.consoleErrors.length === 0 && report.httpErrors.length === 0 && report.restoration.every((item) => item.restored) && !mappingNeedsRestore && !defaultNeedsRestore && !sessionNeedsRestore;
  await browser?.close();
  await writeFile(path.join(outputDir, 'device-groups-qa.json'), `${JSON.stringify(report, null, 2)}\n`);
  console.log(JSON.stringify({ ok: report.ok, flows: report.flows.map(({ name, passed }) => ({ name, passed })), skips: report.skips, outputDir, failure: report.failure, restoration: report.restoration }, null, 2));
  if (!report.ok) process.exitCode = 1;
}
