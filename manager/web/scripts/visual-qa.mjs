import { chromium } from 'playwright';
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';

const baseURL = process.env.RCD_QA_URL ?? 'http://127.0.0.1:8082';
const outputDir = process.env.RCD_QA_OUTPUT ?? path.resolve('qa-artifacts');
await mkdir(outputDir, { recursive: true });

const browser = await chromium.launch({ headless: true });
const results = { baseURL, checks: [], consoleErrors: [] };

async function inspect(page, name) {
  const metrics = await page.evaluate(() => ({
    viewportWidth: window.innerWidth,
    bodyScrollWidth: document.body.scrollWidth,
    documentScrollWidth: document.documentElement.scrollWidth,
    title: document.querySelector('h1')?.textContent?.trim() ?? '',
    navVisible: Boolean(document.querySelector('.sidebar')),
    fixedOverlayCount: [...document.querySelectorAll('.overlay')].filter((node) => getComputedStyle(node).visibility !== 'hidden').length,
  }));
  results.checks.push({ name, ...metrics, horizontalOverflow: metrics.documentScrollWidth > metrics.viewportWidth + 1 });
}

async function createPage(viewport) {
  const context = await browser.newContext({ viewport, locale: 'zh-CN' });
  const page = await context.newPage();
  page.on('console', (message) => {
    if (message.type() === 'error') results.consoleErrors.push({ viewport, message: message.text() });
  });
  page.on('pageerror', (error) => results.consoleErrors.push({ viewport, message: error.message }));
  return { context, page };
}

const desktop = await createPage({ width: 1600, height: 1000 });
await desktop.page.goto(baseURL, { waitUntil: 'domcontentloaded' });
await desktop.page.getByRole('heading', { name: '运行总览' }).waitFor();
await inspect(desktop.page, 'overview-desktop');
await desktop.page.screenshot({ path: path.join(outputDir, 'overview-desktop.png'), fullPage: true });

await desktop.page.getByRole('link', { name: '分组与选源' }).click();
await desktop.page.getByRole('heading', { name: '分组与选源' }).waitFor();
await inspect(desktop.page, 'groups-desktop');
await desktop.page.screenshot({ path: path.join(outputDir, 'groups-desktop.png'), fullPage: true });

const detailButton = desktop.page.getByRole('button', { name: /查看 line-1/ });
if (await detailButton.count()) {
  await detailButton.first().click();
  await desktop.page.locator('.detail-drawer.is-open').waitFor();
  await inspect(desktop.page, 'group-detail-desktop');
  await desktop.page.screenshot({ path: path.join(outputDir, 'group-detail-desktop.png'), fullPage: true });
}
await desktop.context.close();

const mobile = await createPage({ width: 390, height: 844 });
await mobile.page.goto(baseURL, { waitUntil: 'domcontentloaded' });
await mobile.page.getByRole('heading', { name: '运行总览' }).waitFor();
await inspect(mobile.page, 'overview-mobile');
await mobile.page.screenshot({ path: path.join(outputDir, 'overview-mobile.png'), fullPage: true });
await mobile.page.getByRole('button', { name: '打开导航' }).click();
await mobile.page.getByRole('link', { name: '分组与选源' }).click();
await mobile.page.getByRole('heading', { name: '分组与选源' }).waitFor();
await mobile.page.waitForTimeout(250);
await inspect(mobile.page, 'groups-mobile');
await mobile.page.screenshot({ path: path.join(outputDir, 'groups-mobile.png'), fullPage: true });
await mobile.context.close();

await browser.close();
results.ok = results.consoleErrors.length === 0 && results.checks.every((check) => !check.horizontalOverflow && check.title && check.navVisible && check.fixedOverlayCount === 0);
await writeFile(path.join(outputDir, 'visual-qa.json'), `${JSON.stringify(results, null, 2)}\n`);
console.log(JSON.stringify(results, null, 2));
if (!results.ok) process.exitCode = 1;
