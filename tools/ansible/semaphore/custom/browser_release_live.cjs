// Explicit live UI acceptance. Run on 46 with a current control-pause authorization.
// Exactly one upload OR one single-target upgrade; never rerun an ambiguous upgrade.
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');

(async () => {
  const [output, mode, component, version, target, confirmation] = process.argv.slice(2);
  assert.equal(confirmation, '--confirm-paused');
  assert.ok(['upload', 'verify-upload', 'upgrade'].includes(mode));
  assert.ok(['agent', 'manager', 'lower'].includes(component));
  assert.ok(output && path.isAbsolute(output) && !fs.existsSync(output),
    'New absolute evidence directory required; inspect prior receipt instead of replaying');
  if (mode === 'upgrade') assert.ok(['30', '58', '46'].includes(target));
  else assert.ok(path.isAbsolute(target) && target.endsWith('.rcd.tar.gz') && fs.statSync(target).isFile());
  let input = '';
  for await (const chunk of process.stdin) input += chunk;
  const credentials = JSON.parse(input); input = '';
  fs.mkdirSync(output, { recursive: true, mode: 0o700 });
  const save = (name, value) => fs.writeFileSync(path.join(output, name), JSON.stringify(value, null, 2), { mode: 0o600 });
  const platform = 'http://192.168.10.46:3000';
  const base = platform + '/api/project/1/rcd';
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1366, height: 768 }, locale: 'zh-CN' });
  const page = await context.newPage();
  page.setDefaultTimeout(20000);
  const errors = []; const writes = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('request', request => {
    if (request.method() === 'GET' || request.url().includes('/auth/')) return;
    const record = { method: request.method(), url: request.url() };
    if (request.url() === base + '/operations') {
      record.body = request.postDataJSON();
      record.idempotency_key = request.headers()['idempotency-key'];
    }
    writes.push(record); save('requests.json', writes);
  });
  const get = async url => {
    const response = await context.request.get(url);
    assert.ok(response.ok(), `Read failed ${response.status()} ${url}`);
    return response.json();
  };
  const delay = () => new Promise(resolve => setTimeout(resolve, 2000));
  try {
    await page.goto(platform + '/auth/login');
    await page.getByTestId('auth-username').fill('admin');
    await page.getByTestId('auth-password').fill(credentials.platform);
    credentials.platform = '';
    await Promise.all([
      page.waitForResponse(r => r.url().endsWith('/api/auth/login') && r.status() === 204),
      page.getByTestId('auth-signin').click(),
    ]);
    await page.waitForURL(url => !url.pathname.startsWith('/auth/'));
    await page.waitForLoadState('networkidle');
    await page.goto(platform + '/project/1/rcd/packages');
    await page.getByRole('heading', { name: '软件包', exact: true }).waitFor();
    if (mode !== 'upgrade') {
      save('intent.json', { mode, component, version, package: path.basename(target) });
      if (mode === 'upload') {
        await page.getByRole('button', { name: '上传软件包', exact: true }).click();
        const dialog = page.getByRole('dialog').filter({ hasText: '上传软件包' });
        await dialog.locator('input[type=file]').setInputFiles(target);
        const reply = page.waitForResponse(r => r.url() === base + '/packages' && r.request().method() === 'POST', { timeout: 120000 });
        await dialog.getByRole('button', { name: '上传并校验', exact: true }).click();
        const response = await reply;
        save('upload-response.json', { status: response.status() });
        assert.ok(response.ok(), 'Upload rejected; inspect UI error');
        await dialog.locator('.v-card').waitFor({ state: 'hidden' });
      }
      // Large request bodies can evict Chromium inspector response buffers.
      // Confirm the uploaded archive through a read, never re-upload on ambiguity.
      const list = await get(base + '/packages?page_size=100&component=' + component + '&q=' + encodeURIComponent(version));
      const archiveHash = crypto.createHash('sha256').update(fs.readFileSync(target)).digest('hex');
      const item = list.items.find(p => p.archive_sha256 === archiveHash);
      assert.ok(item, 'Exact archive has not been confirmed; no automatic upload retry');
      save('upload.json', item);
      assert.equal(item.manifest.component, component);
      assert.equal(item.manifest.version, version);
      assert.equal(writes.filter(w => w.url === base + '/operations').length, 0);
      await page.screenshot({ path: path.join(output, 'uploaded.png'), fullPage: true });
      save('result.json', { passed: true, fixtures: false, mode, package_id: item.id });
      console.log(JSON.stringify({ passed: true, mode, component, package_id: item.id }));
      return;
    }
    const packages = (await get(base + '/packages?page_size=100&component=' + component + '&q=' + encodeURIComponent(version))).items;
    const pkg = packages.find(p => p.manifest.version === version && p.enabled);
    assert.ok(pkg, 'No exact validated package');
    const devices = (await get(base + '/devices?page_size=100')).items;
    const device = devices.find(d => d.address === '192.168.10.' + target);
    assert.ok(device && device.state === 'checked' && !device.active_operation_id);
    assert.notEqual(device.components.find(c => c.component === component)?.running_build?.version, version,
      'Already installed; inspect original operation instead of submitting again');
    save('devices-before.json', devices);
    await page.getByLabel('搜索版本或发布编号', { exact: true }).fill(version);
    await page.getByRole('button', { name: '查询', exact: true }).click();
    const label = { agent: 'RCD Agent', manager: 'OPC 管理端', lower: '下级 OPC' }[component];
    const row = page.locator('tbody tr').filter({ hasText: version }).filter({ hasText: label });
    await row.getByRole('button', { name: '选择设备升级', exact: true }).click();
    const dialog = page.getByRole('dialog').filter({ has: page.locator('.rcd-release-dialog') });
    await dialog.getByRole('button', { name: '下一步', exact: true }).click();
    const checkbox = dialog.getByRole('checkbox', { name: device.name + ' · ' + device.address, exact: true });
    await dialog.locator('.release-device').filter({ hasText: device.address })
      .locator('input[type=checkbox]:not([disabled])').waitFor({ timeout: 30000 });
    await dialog.getByText(device.name + ' · ' + device.address, { exact: true }).click();
    assert.ok(await checkbox.isChecked());
    await dialog.getByRole('button', { name: '下一步', exact: true }).click();
    const submit = dialog.getByRole('button', { name: '确认升级', exact: true });
    assert.ok(await submit.isDisabled(), 'Current pause confirmation must be required');
    await dialog.getByText('已确认本次暂停所有相关管理端及直连客户端的控制命令', { exact: true }).click();
    assert.ok(await dialog.getByRole('checkbox').isChecked());
    for (const width of [1366, 1920]) {
      await page.setViewportSize({ width, height: width === 1366 ? 768 : 1080 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
      await page.screenshot({ path: path.join(output, `confirm-${width}.png`) });
    }
    save('intent.json', { mode, component, version, device_id: device.id, address: device.address, package_id: pkg.id });
    const reply = page.waitForResponse(r => r.url() === base + '/operations' && r.request().method() === 'POST', { timeout: 60000 });
    await submit.click();
    const response = await reply; const accepted = await response.json();
    save('accepted.json', accepted);
    assert.ok(response.ok(), JSON.stringify(accepted));
    assert.equal(accepted.request.package_id, pkg.id);
    assert.deepEqual(accepted.request.targets.map(t => t.device_id), [device.id]);
    console.log(JSON.stringify({ accepted: accepted.id, task_id: accepted.task_id, component, target }));
    let final = accepted; const end = Date.now() + 360000;
    while (['queued', 'running'].includes(final.state) && Date.now() < end) {
      await delay(); final = await get(base + '/operations/' + accepted.id); save('latest-operation.json', final);
    }
    save('result-operation.json', final);
    assert.equal(final.state, 'succeeded', 'Inspect original operation and retained evidence; never replay automatically');
    let actual; const inspectionEnd = Date.now() + 120000;
    do {
      actual = await get(base + '/devices/' + device.id);
      if (actual.components.some(c => c.component === component && c.state === 'running'
          && c.running_build?.version === version && c.running_sha256 === pkg.manifest.binary_sha256
          && !c.disk_differs)) break;
      await delay();
    } while (Date.now() < inspectionEnd);
    save('device-after.json', actual);
    const running = actual.components.find(c => c.component === component);
    assert.equal(running.state, 'running');
    assert.equal(running.running_build.version, version);
    assert.equal(running.running_sha256, pkg.manifest.binary_sha256);
    assert.equal(running.disk_differs, false);
    for (const original of device.components.filter(c => c.component !== component)) {
      const after = actual.components.find(c => c.component === original.component);
      assert.equal(after.running_sha256, original.running_sha256, 'Unselected binary changed');
    }
    await page.goto(platform + '/project/1/rcd/operations?operation=' + accepted.id);
    await page.getByRole('heading', { name: '执行过程', exact: true }).waitFor();
    await page.screenshot({ path: path.join(output, 'result.png'), fullPage: true });
    assert.equal(writes.filter(w => w.url === base + '/operations').length, 1);
    assert.deepEqual(errors, []);
    save('result.json', { passed: true, fixtures: false, component, target, operation_id: final.id, task_id: final.task_id,
      running_version: running.running_build.version, running_sha256: running.running_sha256 });
    console.log(JSON.stringify({ passed: true, component, target, task_id: final.task_id }));
  } catch (error) {
    await page.screenshot({ path: path.join(output, 'failure.png'), fullPage: true }).catch(() => {});
    throw error;
  } finally { save('page-errors.json', errors); await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
