// Run on 46 against the isolated preview, never the production manager.
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');

(async () => {
  const base = process.env.FLOW_URL || 'http://127.0.0.1:18087';
  const url = new URL(base);
  assert(['127.0.0.1', '192.168.10.46'].includes(url.hostname) && url.port === '18087', 'Use isolated preview port 18087 only');
  const out = process.env.FLOW_EVIDENCE || '/tmp/rcd-flow-evidence';
  await fs.mkdir(out, { recursive: true });
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  const context = await browser.newContext({ viewport: { width: 1920, height: 1080 } });
  const page = await context.newPage();
  const errors = [], requests = [];
  page.on('pageerror', (error) => errors.push(error.message));
  page.on('request', (request) => { if (request.url().includes('/api/')) requests.push({ method: request.method(), url: new URL(request.url()).pathname }); });
  try {
    await page.goto(`${base}/#/flow`);
    await page.getByText('监视连接正常', { exact: false }).waitFor();
    await page.waitForTimeout(2200);
    assert.equal(await page.getByLabel('密码', { exact: true }).count(), 0);
    assert.equal((await context.cookies()).length, 0);
    assert(requests.every((r) => r.method === 'GET' && r.url.startsWith('/api/v1/public/flow')));
    const snapshot = await (await page.request.get(`${base}/api/v1/public/flow`)).json();
    assert(snapshot.sources.length >= 3 && snapshot.sources.every((s) => typeof s.samples === 'string'));
    assert(!JSON.stringify(snapshot).includes('password'));
    const canvas = page.locator('.flow-canvas');
    const image = () => canvas.evaluate((el) => el.toDataURL());
    const movingA = await image();
    await page.waitForTimeout(650);
    assert.notEqual(await image(), movingA, 'rotating ring is not moving');
    await page.screenshot({ path: path.join(out, 'flow-1920.png') });
    await page.getByRole('button', { name: '暂停动画', exact: true }).click();
    await page.waitForTimeout(150);
    const pausedA = await image();
    await page.waitForTimeout(650);
    assert.equal(await image(), pausedA, 'pause did not stop visual animation');
    await page.getByRole('button', { name: '播放', exact: true }).click();
    const sources = page.locator('.flow-source');
    await sources.first().click();
    await page.getByRole('dialog').waitFor();
    await page.waitForFunction(() => document.querySelectorAll('.flow-points-table tbody tr').length > 0);
    await page.getByLabel('筛选串口').selectOption({ index: 1 });
    await page.waitForTimeout(400);
    assert.equal(await page.locator('.flow-detail [type=submit]').count(), 0);
    await page.screenshot({ path: path.join(out, 'flow-detail.png') });
    await page.keyboard.press('Escape');
    assert.equal(await page.getByRole('dialog').count(), 0);
    await page.setViewportSize({ width: 1366, height: 768 });
    await page.waitForTimeout(300);
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), 'page horizontally overflows');
    const bounds = await page.locator('.flow-client').evaluateAll((nodes) => nodes.map((n) => { const r = n.getBoundingClientRect(); return { right: r.right, bottom: r.bottom }; }));
    assert(bounds.every((b) => b.right <= 1366 && b.bottom < 768));
    await page.screenshot({ path: path.join(out, 'flow-1366.png') });
    await page.route('**/api/v1/public/flow', (route) => route.abort());
    await page.getByText('监视已断开 / 状态过期', { exact: false }).waitFor();
    assert(await page.getByRole('alert').count() > 0);
    await page.screenshot({ path: path.join(out, 'flow-offline.png') });
    await page.unroute('**/api/v1/public/flow');
    await page.getByText('监视连接正常', { exact: false }).waitFor();
    for (const endpoint of ['/api/v1/sources', '/api/v1/sessions', '/api/v1/commands', '/api/v1/configuration', '/api/v1/events']) {
      assert.equal((await page.request.get(base + endpoint)).status(), 401, endpoint);
    }
    assert.equal((await page.request.post(`${base}/api/v1/public/flow`)).status(), 401);
    await page.getByRole('link', { name: '进入管理 ↗' }).click();
    await page.getByLabel('密码', { exact: true }).waitFor();
    assert.deepEqual(errors, [], 'browser errors');
    const result = { checked_at: new Date().toISOString(), base, source_count: snapshot.sources.length, session_count: snapshot.sessions.length, checks: ['anonymous-read-only', 'public-api-allowlist', 'ring-animation', 'pause-resume', 'actual-points', 'station-filter', 'keyboard-dialog', '1920-layout', '1366-layout', 'disconnect-cache', 'reconnect', 'management-login'], errors };
    await fs.writeFile(path.join(out, 'result.json'), JSON.stringify(result, null, 2));
    console.log(JSON.stringify(result));
  } finally { await browser.close(); }
})().catch((error) => { console.error(error); process.exitCode = 1; });
