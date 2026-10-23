// Read-only acceptance, Chromium runs on 46. No configuration or business writes.
const { chromium } = require('/home/lenovo/code/rcd_manager_test/browser-tools/node_modules/playwright');
const fs = require('node:fs');
const assert = require('node:assert/strict');
(async () => {
  const [root, phase] = process.argv.slice(2);
  assert.ok(root.startsWith('/home/lenovo/code/rcd_names_release_'));
  assert.ok(['before', 'after', 'catalog-ready'].includes(phase));
  const output = `${root}/web-${phase}`;
  assert.ok(!fs.existsSync(output));
  fs.mkdirSync(output, {mode: 0o700});
  let input = ''; for await (const chunk of process.stdin) input += chunk;
  const credentials = JSON.parse(input); input = '';
  const browser = await chromium.launch({headless: true});
  const context = await browser.newContext({viewport: {width: 1366, height: 768}, locale: 'zh-CN'});
  const page = await context.newPage(); const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await context.route('**/api/**', route => {
    const r = route.request();
    if (!['GET','HEAD'].includes(r.method()) && !r.url().endsWith('/api/v1/auth/login')) return route.abort();
    return route.continue();
  });
  const base = 'http://192.168.10.46:8087';
  async function get(path) {
    const response = await context.request.get(base + path);
    assert.ok(response.ok(), `${path}: ${response.status()}`);
    return response.json();
  }
  try {
    const login = await context.request.post(base + '/api/v1/auth/login', {
      headers: {Origin: base}, data: {username: 'admin', password: credentials.password}
    });
    credentials.password = ''; assert.ok(login.ok(), 'login failed');
    const overview = await get('/api/v1/overview');
    const {items: sources} = await get('/api/v1/sources');
    const {items: groups} = await get('/api/v1/groups');
    const points = await get('/api/v1/points?source_id=communicate-46&limit=100&offset=0');
    fs.writeFileSync(output + '/snapshot.json', JSON.stringify({overview,sources,groups,points}, null, 2), {mode: 0o600});
    assert.equal(overview.pending_commands, 0, 'live control transactions exist; do not deploy');
    assert.equal(overview.catalog_conflicts, 0);
    assert.equal(sources.length, 4);
    for (const source of sources) assert.equal(source.connection_state, 'Connected', source.id);
    if (phase !== 'before') {
      // Hot catalog replacement intentionally retains old retired nodes until
      // manager restart. The clean post-upgrade catalog must contain only 58.
      assert.equal(sources.find(s => s.id === 'communicate-46').monitored_count, 58);
      if (phase === 'after') assert.equal(points.total, 58);
      const fire = points.items.find(p => p.node.key.identifier === 'DATA.Buildings.91UBH.Fire');
      assert.ok(fire, 'original receive name missing');
      assert.equal(fire.node.data_type, 'UInt32');
      assert.equal(fire.sample.status_code, 0x808d0000);
      assert.ok(points.items.some(p => p.node.key.identifier === 'DATA.PumpStationCtrl[0]'));
      assert.ok(!points.items.some(p => !p.node.retired && (p.node.key.identifier.startsWith('rx9.') || p.node.key.identifier.startsWith('RcdSync.'))));
    }
    if (phase === 'after') {
      assert.equal(overview.version, '0.6.11-names.20261020.1');
      await page.goto(base + '/#/points?catalog_source=communicate-46&q=DATA.Buildings.91UBH.Fire');
      await page.getByRole('heading', {name: '点位浏览', exact: true}).waitFor();
      await page.getByText('DATA.Buildings.91UBH.Fire', {exact: true}).waitFor();
      for (const [width,height] of [[1366,768],[1920,1080]]) {
        await page.setViewportSize({width,height});
        await page.screenshot({path: output + `/points-${width}.png`, fullPage: true});
      }
      await page.goto(base + '/#/sources?operations=communicate-46');
      await page.getByRole('tab', {name: '配置', exact: true}).click();
      await page.getByLabel('配置文件', {exact: true}).selectOption('configuration-set.toml');
      const editor = page.getByLabel('配置内容 configuration-set.toml', {exact: true});
      await editor.waitFor({timeout: 30000});
      await page.waitForFunction(() => [...document.querySelectorAll('textarea')].some(e => e.value.includes('DATA.Buildings.91UBH.Fire')));
      const content = await editor.inputValue();
      assert.ok(content.replaceAll('\\', '').includes('"name","task_id","data_type","description"'));
      assert.ok(content.includes('enabled = false'));
      assert.ok(content.includes('transmit_enabled = false'));
      assert.ok(!content.includes('rx9.DATA.'));
      await page.screenshot({path: output + '/configuration.png', fullPage: true});
    }
    assert.deepEqual(errors, []);
    fs.writeFileSync(output + '/result.json', JSON.stringify({passed:true,phase,point_count:points.total,page_errors:errors,hardware_writes:0,configuration_saved:false},null,2), {mode:0o600});
    console.log(JSON.stringify({passed:true,phase,point_count:points.total}));
  } catch (error) {
    await page.screenshot({path:output+'/failure.png',fullPage:true}).catch(()=>{});
    throw error;
  } finally { await browser.close(); }
})().catch(e => {console.error(e);process.exitCode=1;});
