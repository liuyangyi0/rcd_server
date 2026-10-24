// Run on 46 after building the Web application. Only a temporary loopback
// static server and read-only shell fixtures are used. Documentation images
// remain the actual deployed-page captures, never fixture screenshots.
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const assert = require('node:assert/strict');
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const dist = path.resolve(__dirname, '../../manager/web/dist');
const output = process.argv[2];
assert.ok(output && path.isAbsolute(output) && !fs.existsSync(output));
assert.ok(fs.existsSync(path.join(dist, 'index.html')));
const errors = [], blocked = [], requests = [], checks = [];
const server = http.createServer((req, res) => {
  const name = new URL(req.url, 'http://localhost').pathname;
  if (!['GET', 'HEAD'].includes(req.method) || name.startsWith('/api/')) {
    blocked.push(`${req.method} ${name}`); res.writeHead(405).end(); return;
  }
  const candidate = path.resolve(dist, '.' + name);
  if (!candidate.startsWith(dist + path.sep) && candidate !== dist) { res.writeHead(403).end(); return; }
  const file = name === '/' ? path.join(dist, 'index.html') : candidate;
  if (!fs.existsSync(file) || !fs.statSync(file).isFile()) { errors.push(`missing asset ${name}`); res.writeHead(404).end(); return; }
  const type = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.jpg': 'image/jpeg' }[path.extname(file)] || 'application/octet-stream';
  res.writeHead(200, { 'Content-Type': type, 'Cache-Control': 'no-store' });
  if (req.method === 'HEAD') res.end(); else fs.createReadStream(file).pipe(res);
});

(async () => {
  fs.mkdirSync(output, { mode: 0o700 });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const base = `http://127.0.0.1:${server.address().port}`;
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1366, height: 900 }, locale: 'zh-CN', permissions: ['clipboard-read', 'clipboard-write'] });
  const page = await context.newPage();
  page.on('pageerror', (error) => errors.push(error.message));
  await page.route('**/api/**', async (route) => {
    const req = route.request(); const name = new URL(req.url()).pathname;
    requests.push({ method: req.method(), path: name });
    if (req.method() !== 'GET') { blocked.push(`${req.method()} ${name}`); await route.abort(); return; }
    if (name === '/api/v1/auth/session') return route.fulfill({ json: { authenticated: true, username: 'isolated-doc-review' } });
    if (name === '/api/v1/overview') return route.fulfill({ json: { manager_state: 'Running', version: 'isolated-documentation', point_count: 0 } });
    if (name === '/api/v1/events') return route.fulfill({ contentType: 'text/event-stream', body: 'retry: 60000\n: isolated\n\n' });
    errors.push(`unexpected API ${name}`); return route.fulfill({ status: 404, json: {} });
  });
  async function noOverflow() {
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `Overflow at ${page.viewportSize().width}`);
  }
  try {
    await page.goto(base + '/#/help');
    await page.getByRole('heading', { name: '帮助中心', exact: true }).waitFor();
    const topics = page.getByRole('navigation', { name: '帮助主题', exact: true });
    assert.equal(await topics.getByRole('link').count(), 10);
    assert.equal(await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '帮助中心' }).count(), 1);
    for (const entry of await topics.getByRole('link').evaluateAll((links) => links.map((link) => ({ href: link.getAttribute('href'), name: link.textContent })))) {
      await topics.getByRole('link', { name: entry.name, exact: true }).click();
      await page.getByRole('heading', { name: entry.name, level: 2, exact: true }).waitFor();
      for (const img of await page.locator('.help-screenshot img').all()) {
        await img.scrollIntoViewIfNeeded();
        await img.evaluate((image) => image.decode());
        assert.equal(await img.evaluate((image) => image.naturalWidth), 1440);
      }
    }
    checks.push('all ten topics, real screenshot assets and existing sidebar entry');
    for (const width of [1366, 1920, 390]) {
      await page.setViewportSize({ width, height: width === 390 ? 844 : 960 });
      for (const id of ['relationships', 'platform', 'communication']) {
        await page.goto(base + '/#/help?topic=' + id);
        await page.locator('#help-article-title').scrollIntoViewIfNeeded();
        await noOverflow();
        await page.screenshot({ path: path.join(output, `help-${id}-${width}.png`), fullPage: false, animations: 'disabled' });
      }
    }
    checks.push('relationship, software-maintenance and OPC-reference layouts at 1366/1920/390');
    await page.setViewportSize({ width: 1366, height: 900 });
    await page.goto(base + '/#/help');
    const search = page.getByRole('searchbox', { name: '搜索说明、节点或参数' });
    await search.fill('上传 软件包');
    await page.getByRole('combobox', { name: '主题分类' }).selectOption('软件维护升级');
    await page.getByRole('heading', { name: '找到 1 个主题', exact: true }).waitFor();
    await page.locator('.help-results').getByRole('link', { name: /软件维护、升级与恢复/ }).click();
    await page.waitForFunction(() => document.activeElement?.id === 'help-article-title');
    await page.goBack();
    await page.getByRole('heading', { name: '找到 1 个主题', exact: true }).waitFor();
    await search.fill('没有这个帮助内容');
    await page.getByRole('heading', { name: '找到 0 个主题', exact: true }).waitFor();
    await page.getByRole('button', { name: '清除筛选' }).click();
    checks.push('full-content search, category intersection, empty state and browser history');
    await page.goto(base + '/#/help?topic=communication');
    const locationBeforeContents = page.url();
    await page.getByRole('navigation', { name: '本篇目录' }).getByRole('button', { name: '推荐：整包原子提交', exact: true }).click();
    assert.equal(page.url(), locationBeforeContents, 'In-document contents must not replace the app hash route');
    const row = page.locator('.help-node').filter({ has: page.locator('code').filter({ hasText: /^ns=2;s=DATA.Buildings.91UBH.Fire$/ }) });
    await row.getByRole('button', { name: '复制节点', exact: true }).click();
    assert.equal(await page.evaluate(() => navigator.clipboard.readText()), 'ns=2;s=DATA.Buildings.91UBH.Fire');
    await page.getByRole('button', { name: '复制完整 39 项 JSON 格式示例', exact: true }).click();
    const snapshot = JSON.parse(await page.evaluate(() => navigator.clipboard.readText()));
    assert.equal(Object.keys(snapshot.fields).length, 39);
    assert.equal(snapshot.fields['DATA.AKONs.1CYE18GH204.HW'].length, 1938);
    assert.equal(snapshot.fields['DATA.Buildings.1JMF34.Fire'], 1);
    checks.push('real browser clipboard preserves exact NodeId and complete typed 39-input snapshot');
    await page.goto(base + '/#/help?topic=deleted');
    await page.getByRole('heading', { name: '未找到这个帮助主题' }).waitFor();
    assert.deepEqual(errors, []);
    assert.deepEqual(blocked, []);
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ passed: true, checks, requests, writes: 0, page_errors: errors }, null, 2));
    console.log(JSON.stringify({ passed: true, checks, writes: 0 }));
  } finally { await browser.close(); server.close(); }
})().catch((error) => { console.error(error); server.close(); process.exitCode = 1; });
