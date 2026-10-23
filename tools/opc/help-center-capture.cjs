// Run on 46 only. Capture the REAL deployed UIs for documentation, not fixtures.
// Credentials arrive as JSON on stdin and are never written to the evidence.
// No software/host/configuration/control action may be submitted.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');

(async () => {
  const input = JSON.parse(fs.readFileSync(0, 'utf8'));
  const output = process.argv[2];
  assert.ok(output && path.isAbsolute(output) && !fs.existsSync(output), 'New absolute output directory required');
  fs.mkdirSync(output, { mode: 0o700 });
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1440, height: 960 }, locale: 'zh-CN', timezoneId: 'Asia/Shanghai' });
  const page = await context.newPage();
  page.setDefaultTimeout(25000);
  const manifest = { captured_at: new Date().toISOString(), kind: 'real-deployed-ui', viewport: { width: 1440, height: 960 }, screenshots: [], blocked: [], errors: [] };
  await page.route('**/api/**', async (route) => {
    const req = route.request(); const url = new URL(req.url());
    const login = req.method() === 'POST' && ['/api/v1/auth/login', '/api/auth/login'].includes(url.pathname);
    // The deployed typed form loads via a read-only POST. Only an empty edit
    // list for this fixed file/target is allowed; PUT/save/control remain blocked.
    const body = req.method() === 'POST' ? req.postDataJSON() : undefined;
    const formRead = req.method() === 'POST' && url.port === '8087'
      && url.pathname === '/api/v1/sources/real-lower/operations/config/prepare'
      && body?.name === 'config.toml' && Array.isArray(body.edits) && body.edits.length === 0;
    if (!['GET', 'HEAD'].includes(req.method()) && !login && !formRead) {
      manifest.blocked.push({ method: req.method(), path: url.pathname });
      await route.abort('blockedbyclient'); return;
    }
    await route.continue();
  });
  async function shot(id, caption) {
    await page.evaluate(() => document.fonts.ready);
    const file = `${id}.jpg`;
    await page.screenshot({ path: path.join(output, file), type: 'jpeg', quality: 85, animations: 'disabled' });
    manifest.screenshots.push({ id, file, caption, url: page.url(), captured_at: new Date().toISOString(), sha256: crypto.createHash('sha256').update(fs.readFileSync(path.join(output, file))).digest('hex') });
    console.log(`CAPTURED ${id}`);
  }
  async function managerPage(route, heading, id, caption) {
    await page.goto(`http://192.168.10.46:8087/#/${route}`);
    await page.getByRole('heading', { name: heading, exact: true }).waitFor();
    await page.locator('.page .loading-state').waitFor({ state: 'hidden' }).catch(() => {});
    await shot(id, caption);
  }
  try {
    if (!input.platform_only) {
    await page.goto('http://192.168.10.46:8087/#/sources');
    await page.getByLabel('账号', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill(input.manager_password);
    input.manager_password = '';
    await page.getByRole('button', { name: '登录', exact: true }).click();
    await page.getByRole('heading', { name: '下级 OPC', exact: true }).waitFor();
    await page.getByRole('button', { name: '运维 real-lower', exact: true }).waitFor();
    await shot('manager-sources', '下级 OPC：先核对物理来源，再打开运维；不要点击重连或删除。');
    await page.getByRole('button', { name: '运维 real-lower', exact: true }).click();
    await page.getByRole('tab', { name: '配置', exact: true }).click();
    await page.getByLabel('配置文件', { exact: true }).selectOption('config.toml');
    await page.getByRole('region', { name: '配置编辑 config.toml', exact: true }).waitFor();
    await page.getByText('命令最大下发次数（含首次）', { exact: true }).first().waitFor();
    await shot('manager-configuration', '配置：选择文件，修改后校验再保存；本次截图未改参数、未保存。');
    await page.getByRole('tab', { name: '进程', exact: true }).click();
    await page.getByRole('button', { name: /启动|停止|重启/ }).first().waitFor();
    await shot('manager-process', '进程：由目标 Agent 安全启停采集；截图没有提交操作。');
    await managerPage('groups', '分组与选源', 'manager-groups', '分组与选源：先选目标 Session，再按整组选择来源。');
    await managerPage('commands', '命令记录', 'manager-commands', '命令记录：按端口查看队列、结果和执行过程，已受理不等于设备动作完成。');
    await managerPage('calculations', '计算中心', 'manager-calculations', '计算中心：实时结果按默认来源生成，Session 独立选源不改变计算结果。');
    await page.getByRole('button', { name: '规则配置', exact: true }).click();
    await page.getByText('保存草稿', { exact: true }).first().waitFor();
    await shot('manager-calculation-config', '规则配置：保存草稿、校验、激活是不同步骤；截图未修改或激活。');
    await managerPage('points', '点位浏览', 'manager-points', '点位浏览：完整 NodeId、数据类型及质量以实际节点为准。');
    await managerPage('device-versions', '设备版本', 'manager-device-versions', '设备版本：模块与 PLC 版本不同于程序软件版本；手动查询会访问串口。');
    await managerPage('lower-logs', '下级集中日志', 'manager-logs', '下级日志：按来源、等级和时间查看接收记录。');
    }

    await page.goto('http://192.168.10.46:3000/auth/login');
    await page.getByTestId('auth-username').fill('admin');
    await page.getByTestId('auth-password').fill(input.platform_password);
    input.platform_password = '';
    const response = page.waitForResponse((reply) => reply.url().endsWith('/api/auth/login') && reply.request().method() === 'POST');
    await page.getByTestId('auth-signin').click();
    assert.equal((await response).status(), 204, 'Platform login must succeed before documentation capture');
    await page.waitForURL((url) => !url.pathname.startsWith('/auth/'));
    await page.goto('http://192.168.10.46:3000/project/1/rcd/devices');
    await page.getByRole('heading', { name: '设备与版本', exact: true }).waitFor();
    await page.getByRole('row').filter({ hasText: '192.168.10.46' }).getByRole('button', { name: '查看详情', exact: true }).waitFor();
    await shot('platform-devices', 'Semaphore 设备与版本：检查状态才发起巡检，刷新页面只读取已有快照。');
    await page.getByRole('row').filter({ hasText: '192.168.10.46' }).getByRole('button', { name: '查看详情', exact: true }).click();
    await page.getByRole('heading', { name: '主机资源快照', exact: true }).waitFor();
    await shot('platform-device-detail', '设备详情：分别查看各组件状态、版本历史及服务操作；资源是带时间的快照。');
    if (input.history_only) {
      const component = page.locator('.component-detail').filter({ has: page.getByRole('heading', { name: 'OPC 管理端', exact: true }) });
      await component.getByRole('heading', { name: 'OPC 管理端', exact: true }).scrollIntoViewIfNeeded();
      await shot('platform-service-control', 'OPC 管理端组件：启动、停止、重启在平台执行，不连带停止下级；截图未点击。');
      await component.getByRole('button', { name: '版本历史', exact: true }).click();
      const restore = component.getByRole('button', { name: '恢复此版本', exact: true }).first();
      await restore.waitFor();
      await component.getByRole('button', { name: '版本历史', exact: true }).scrollIntoViewIfNeeded();
      await shot('platform-history', '版本历史：选择兼容且有证据的旧版本，恢复会创建新任务并保留当前配置和数据。');
      const available = component.locator('button:not([disabled])').filter({ hasText: /^恢复此版本$/ });
      if (await available.count()) {
        await available.first().click();
        const dialog = page.getByRole('dialog').filter({ hasText: '恢复历史版本' });
        await dialog.waitFor();
        await shot('platform-restore', '恢复历史版本向导：先核对组件和版本；本次只打开说明画面，没有确认执行。');
      } else {
        manifest.restore_unavailable = 'All displayed versions are disabled by the current compatibility/state checks; no bypass attempted';
      }
      assert.deepEqual(manifest.blocked, []);
      manifest.completed = true;
      return;
    }
    await page.getByRole('button', { name: '关闭详情', exact: true }).click();
    await page.getByRole('button', { name: '添加主机', exact: true }).click();
    const enrollment = page.getByRole('dialog').filter({ hasText: '添加主机' });
    await enrollment.getByLabel('SSH登录账号', { exact: true }).waitFor();
    await shot('platform-add-host', '添加主机：选择已有凭据，核对主机密钥；本次仅打开向导，没有提交。');
    await enrollment.getByRole('button', { name: '关闭', exact: true }).click();
    await page.goto('http://192.168.10.46:3000/project/1/rcd/packages');
    await page.getByRole('heading', { name: '软件包', exact: true }).waitFor();
    await page.getByRole('button', { name: '上传软件包', exact: true }).waitFor();
    await shot('platform-packages', '软件包：先上传单组件标准包，上传不会自动部署。');
    await page.getByRole('button', { name: '上传软件包', exact: true }).click();
    const upload = page.getByRole('dialog').filter({ hasText: '上传软件包' });
    await upload.locator('input[type=file]').waitFor({ state: 'attached' });
    await shot('platform-upload', '上传软件包：选择 .rcd.tar.gz；本次未选择文件、未上传。');
    await upload.getByRole('button', { name: '关闭', exact: true }).click();
    await page.getByRole('button', { name: '选择设备升级', exact: true }).first().click();
    await page.getByRole('dialog').filter({ has: page.locator('.rcd-release-dialog') }).waitFor();
    await shot('platform-upgrade', '升级向导：选择组件、版本和兼容目标，最后确认维护；本次未提交。');
    await page.goto('http://192.168.10.46:3000/project/1/rcd/operations');
    await page.getByRole('heading', { name: '操作任务', exact: true }).waitFor();
    await shot('platform-tasks', '操作任务：查看原操作与原生任务日志；结果未知时补查，不能盲目重投。');
    assert.deepEqual(manifest.blocked, [], 'Unexpected write attempted; capture is not complete');
    manifest.completed = true;
  } catch (error) {
    manifest.errors.push(error.message);
    manifest.completed = false;
    console.error(error.message);
    process.exitCode = 1;
  } finally {
    fs.writeFileSync(path.join(output, 'manifest.json'), JSON.stringify(manifest, null, 2));
    await browser.close();
  }
})();
