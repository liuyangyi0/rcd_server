// Run on 46 only against deployed managers. Reads documentation; no business
// writes, software operations, configuration saves or hardware commands.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');

(async () => {
  const input = JSON.parse(fs.readFileSync(0, 'utf8'));
  const output = process.argv[2];
  assert.ok(output && path.isAbsolute(output) && !fs.existsSync(output));
  const web = path.resolve(__dirname, '../../manager/web');
  const metadata = fs.readFileSync(path.join(web, 'src/features/help/screenshots.ts'), 'utf8');
  const images = [...new Set([...metadata.matchAll(/id: '((?:manager|platform)-[a-z-]+)'/g)].map((match) => match[1]))].sort();
  assert.equal(images.length, 19);
  fs.mkdirSync(output, { mode: 0o700 });
  const browser = await chromium.launch({ headless: true });
  const results = [];
  try {
    for (const host of ['30', '58', '46']) {
      const base = `http://192.168.10.${host}:8087`;
      const context = await browser.newContext({ viewport: { width: 1366, height: 768 }, locale: 'zh-CN', timezoneId: 'Asia/Shanghai' });
      try {
        const login = await context.request.post(base + '/api/v1/auth/login', {
          data: { username: 'admin', password: input.manager_password }, headers: { Origin: base },
        });
        assert.equal(login.status(), 200, `manager ${host} login failed`);
        const errors = [], blocked = [], seen = new Set();
        const page = await context.newPage();
        page.setDefaultTimeout(20000);
        page.on('pageerror', (error) => errors.push(error.message));
        await page.route('**/api/**', async (route) => {
          const request = route.request();
          if (!['GET', 'HEAD'].includes(request.method())) {
            blocked.push(`${request.method()} ${new URL(request.url()).pathname}`);
            return route.abort('blockedbyclient');
          }
          return route.continue();
        });
        await page.goto(base + '/#/help');
        await page.getByRole('heading', { name: '帮助中心', exact: true }).waitFor();
        const navigation = page.getByRole('navigation', { name: '帮助主题', exact: true });
        const topics = await navigation.getByRole('link').evaluateAll((links) => links.map((link) => ({ name: link.textContent, href: link.getAttribute('href') })));
        assert.equal(topics.length, 10);
        assert.equal(await page.getByRole('navigation', { name: '主导航', exact: true }).getByRole('link', { name: '帮助中心', exact: true }).count(), 1);
        for (const topic of topics) {
          await navigation.getByRole('link', { name: topic.name, exact: true }).click();
          await page.getByRole('heading', { name: topic.name, exact: true, level: 2 }).waitFor();
          for (const image of await page.locator('.help-screenshot img').all()) {
            await image.scrollIntoViewIfNeeded();
            await image.evaluate((element) => element.decode());
            assert.deepEqual(await image.evaluate((element) => [element.naturalWidth, element.naturalHeight]), [1440, 960]);
            const url = new URL(await image.getAttribute('src'), base);
            const name = path.basename(url.pathname, '.jpg');
            assert.ok(images.includes(name));
            const response = await context.request.get(url.href);
            assert.equal(response.status(), 200, url.href);
            assert.match(response.headers()['content-type'], /^image\/jpeg/);
            const actual = await response.body();
            const expected = fs.readFileSync(path.join(web, 'public/help/screenshots', name + '.jpg'));
            assert.equal(crypto.createHash('sha256').update(actual).digest('hex'), crypto.createHash('sha256').update(expected).digest('hex'), `image mismatch: ${host}/${name}`);
            seen.add(name);
          }
        }
        assert.deepEqual([...seen].sort(), images);
        for (const [width, height] of [[1366, 768], [1920, 1080]]) {
          await page.setViewportSize({ width, height });
          await page.goto(base + '/#/help?topic=relationships');
          await page.locator('#help-article-title').waitFor();
          assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${host} page overflow`);
          await page.screenshot({ path: path.join(output, `manager-${host}-help-${width}.png`), animations: 'disabled' });
        }
        const original = page.locator('.help-screenshot a').first();
        const [popup] = await Promise.all([page.waitForEvent('popup'), original.click()]);
        await popup.locator('img').waitFor();
        await popup.locator('img').evaluate((image) => image.decode());
        assert.equal(await popup.locator('img').evaluate((image) => image.naturalWidth), 1440);
        await popup.close();
        assert.deepEqual(errors, []);
        assert.deepEqual(blocked, []);
        const result = { host, url: base + '/#/help', topics: topics.length, images: seen.size, jpeg_sha256_matches: true, original_image_tab: true, page_errors: errors, blocked_writes: blocked, passed: true };
        results.push(result);
        fs.writeFileSync(path.join(output, `manager-${host}.json`), JSON.stringify(result, null, 2));
        console.log(JSON.stringify(result));
      } finally { await context.close(); }
    }
    fs.writeFileSync(path.join(output, 'result.json'), JSON.stringify({ passed: true, results }, null, 2));
  } finally { await browser.close(); }
})().catch((error) => { console.error(error); process.exitCode = 1; });
