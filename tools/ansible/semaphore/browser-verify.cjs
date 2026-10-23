// Run only on 46. Credentials remain on the controller; no storageState export.
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('/home/lenovo/code/rcd_manager_test/browser-tools/node_modules/playwright');

(async () => {
  const config = JSON.parse(fs.readFileSync('/etc/semaphore/config.json', 'utf8'));
  const output = process.argv[2];
  if (!output || !path.isAbsolute(output)) throw new Error('explicit evidence directory required');
  fs.mkdirSync(output, { recursive: true });
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(config.web_host, { waitUntil: 'networkidle' });
    await page.locator('input[type="password"]').waitFor();
    await page.screenshot({ path: path.join(output, 'login.png'), fullPage: true });
    const candidates = page.locator('input:not([type="password"]):not([type="hidden"]):not([type="checkbox"])');
    await candidates.first().fill('admin');
    await page.locator('input[type="password"]').fill(process.env.SEMAPHORE_ADMIN_PASSWORD || fs.readFileSync('/etc/semaphore/initial-admin-password', 'utf8').trim());
    console.log('Login buttons:', await page.getByRole('button').allTextContents());
    await page.locator('input[type="password"]').press('Enter');
    await page.locator('input[type="password"]').waitFor({ state: 'hidden', timeout: 20000 });
    await page.waitForLoadState('networkidle');
    await page.screenshot({ path: path.join(output, 'overview.png'), fullPage: true });
    console.log('Authenticated page:', await page.locator('body').innerText());
    await page.getByText('#2', { exact: true }).click();
    await page.waitForLoadState('networkidle');
    await page.screenshot({ path: path.join(output, 'task-detail.png'), fullPage: true });
    const detail = await page.locator('body').innerText();
    if (!detail.includes('Started task #2') || !detail.includes('LOG')) throw new Error('task log viewer missing');
    console.log('Task detail and virtualized log viewer: passed');
    console.log(JSON.stringify({ login: 'passed', url: page.url(), page_errors: errors }));
    if (errors.length) throw new Error('browser page errors');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error.message); process.exitCode = 1; });
