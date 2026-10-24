// Run on 46 only. Pure Vue state regressions; no SSH, task queue or service control.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function deferred() {
  let resolve; let reject;
  const promise = new Promise((a, b) => { resolve = a; reject = b; });
  return { promise, resolve, reject };
}
function component(name, axios = {}) {
  const source = fs.readFileSync(path.join(__dirname, 'files/web/src/views/rcd', `${name}.vue`), 'utf8');
  const script = source.match(/<script>([\s\S]*?)<\/script>/)[1]
    .replace(/^import .*;\r?$/gm, '').replace('export default', 'module.exports =');
  const events = []; const stored = new Map();
  const context = { module: { exports: {} }, axios, document: { title: '' },
    EventBus: { $emit(...args) { events.push(args); } }, PermissionsCheck: {}, USER_PERMISSIONS: {},
    ReleaseWizard: {}, ComponentHistory: {}, ServiceControl: {}, MachineBinding: {}, HostStatus: {},
    HostEnrollment: {}, clearTimeout() {}, setTimeout() { return 1; },
    sessionStorage: { getItem(key) { return stored.get(key) || null; }, setItem(key, val) { stored.set(key, val); } },
  };
  vm.runInNewContext(script, context, { filename: `${name}.vue` });
  const definition = context.module.exports;
  const state = { ...definition.data(), projectId: 1, value: true, allowed: true,
    initialDevice: null, initialSetupId: '', components: [],
    $route: { query: {} }, $router: { async replace() {} }, $emit(...args) { events.push(args); },
    $nextTick(fn) { return fn(); }, can() { return true; },
  };
  for (const [key, fn] of Object.entries(definition.methods || {})) state[key] = fn.bind(state);
  for (const [key, getter] of Object.entries(definition.computed || {})) {
    Object.defineProperty(state, key, { get: getter.bind(state) });
  }
  return { state, definition, events, stored, source };
}
function host(id = 'original') {
  return { id, revision: 5, state: 'unknown', phase: 'install', existing_device: false,
    device_id: '', request: { name: '原主机', address: '192.168.10.99', ssh_port: 22 },
    install: { component: 'communicate' }, steps: [], host_keys: [], updated_at: new Date().toISOString() };
}
function device() {
  return { id: 'd1', revision: 7, name: 'RCD 46', address: '192.168.10.46',
    state: 'checked', last_success_at: new Date().toISOString(), components: [],
    opc_endpoint: 'opc.tcp://192.168.10.46:4841', agent_url: 'http://192.168.10.46:8090',
    agent_instance_id: 'agent-1', gateway_instance_id: 'gateway-1', manager_instance_id: 'manager-1' };
}

test('record finder uses explicit GET-only server pagination and never calls task creation', async () => {
  const calls = [];
  const { state: s } = component('Devices', { get: async (url, options) => {
    calls.push({ url, params: options.params });
    return { data: { items: [host()], total: 45, total_pages: 3, page: 1, page_size: 20 } };
  } });
  assert.equal(calls.length, 0);
  await s.openHostSetups();
  assert.equal(calls.length, 1); assert.match(calls[0].url, /\/host-setups$/);
  assert.equal(calls[0].params.page, 1); assert.equal(calls[0].params.page_size, 20);
  assert.equal(s.hostSetupTotal, 45); assert.equal(s.hostSetupTotalPages, 3);
  assert.equal(s.hostSetupsOpen, true);
});

test('record page failure preserves confirmed page and retries the same requested page', async () => {
  const pages = []; let fail = true;
  const { state: s } = component('Devices', { get: async (url, options) => {
    const page = options.params.page; pages.push(page);
    if (page === 2 && fail) { fail = false; throw new Error('reply lost'); }
    return { data: { items: [host(`page-${page}`)], total: 45, total_pages: 3 } };
  } });
  await s.loadHostSetups(); await s.loadHostSetups(2);
  assert.equal(s.hostSetupPage, 1); assert.equal(s.hostSetups[0].id, 'page-1');
  assert.ok(s.hostSetupsError);
  await s.loadHostSetups();
  assert.equal(s.hostSetupPage, 2); assert.equal(s.hostSetups[0].id, 'page-2');
  assert.deepEqual(pages, [1, 2, 2]);
});

test('record search and page-size changes begin from page one, even after a failed request', async () => {
  const queries = []; let fail = false;
  const { state: s } = component('Devices', { get: async (url, options) => {
    queries.push({ ...options.params });
    if (fail) { fail = false; throw new Error('connection lost'); }
    return { data: { items: [host()], total: 45, total_pages: 3 } };
  } });
  await s.loadHostSetups(); await s.loadHostSetups(2);
  s.hostSetupSearch = '  192.168.10.99  '; s.hostSetupFilter = 'unknown';
  s.hostSetupPageSize = 50; fail = true;
  await s.loadHostSetups(1); await s.loadHostSetups(3);
  assert.equal(queries.at(-1).page, 1);
  assert.equal(queries.at(-1).q, '192.168.10.99');
  assert.equal(queries.at(-1).state, 'unknown'); assert.equal(queries.at(-1).page_size, 50);
});

test('late record list cannot contaminate another project', async () => {
  const pending = deferred(); const { state: s, definition } = component('Devices', {
    get: async () => pending.promise,
  });
  const read = s.loadHostSetups(); s.projectId = 2;
  // Invalidate only the finder lifecycle without starting the page's unrelated reads.
  s.hostSetupSequence += 1; s.hostSetups = []; s.hostSetupsLoading = false;
  pending.resolve({ data: { items: [host()], total: 1, total_pages: 1 } });
  await read; assert.equal(s.hostSetups.length, 0); assert.equal(s.hostSetupsLoading, false);
  assert.equal(typeof definition.watch.projectId, 'function');
});

test('record finder opens only the exact original setup without creating an operation', () => {
  const { state: s } = component('Devices'); s.hostSetupsOpen = true;
  s.enrollmentDevice = device(); s.enrollmentComponent = 'lower';
  s.resumeHostSetup(host('original-unknown'));
  assert.equal(s.enrollmentSetupId, 'original-unknown'); assert.equal(s.enrollmentDevice, null);
  assert.equal(s.enrollmentComponent, ''); assert.equal(s.enrollmentOpen, true);
  assert.equal(s.hostSetupsOpen, false);
});

test('resuming from server records isolates local references and reads the original setup', async () => {
  const calls = []; const { state: s, stored } = component('HostEnrollment', {
    get: async (url) => {
      calls.push(url);
      if (url.endsWith('/options')) return { data: { available: true, ssh_keys: [], become_keys: [] } };
      return { data: host('original-unknown') };
    },
  });
  s.initialSetupId = 'original-unknown';
  stored.set('rcd-host-setup:1:new', JSON.stringify({ setup_id: 'different-job' }));
  await s.open();
  assert.equal(s.setup.id, 'original-unknown');
  assert.equal(s.storageKey(), 'rcd-host-setup:1:setup:original-unknown');
  assert.ok(calls.some((url) => url.endsWith('/original-unknown')));
  assert.ok(!calls.some((url) => url.endsWith('/different-job')));
  assert.equal(JSON.parse(stored.get('rcd-host-setup:1:new')).setup_id, 'different-job');
});

test('enrollment progress and next-step instructions reflect fingerprint, install and unknown states', () => {
  const { state: s } = component('HostEnrollment');
  assert.equal(s.currentStep, 'register'); assert.match(s.nextStepHint, /登记不会安装/);
  s.setup = { ...host(), install: null, state: 'awaiting_host_key', phase: 'keyscan' };
  assert.equal(s.currentStep, 'keys'); assert.match(s.nextStepHint, /核对全部指纹/);
  s.setup = { ...host(), state: 'unknown' };
  assert.equal(s.currentStep, 'result'); assert.match(s.nextStepHint, /只读补查/);
  s.setup = { ...host(), existing_device: true, state: 'installed_stopped' };
  assert.equal(s.isExistingTarget, true); assert.equal(s.enrollmentSteps.length, 2);
  assert.match(s.nextStepHint, /保持停止/);
});

test('binding provides actionable disable reasons and expires without receiving a new snapshot', () => {
  const { state: s } = component('MachineBinding'); s.device = device(); s.open();
  assert.equal(s.blockedReason, '');
  s.device.active_operation_id = 'unresolved'; assert.match(s.blockedReason, /原任务/);
  s.device.active_operation_id = ''; s.now += 181000;
  assert.match(s.blockedReason, /180秒/); assert.equal(s.blocked, true);
});

test('late binding save is ignored after a project switch without closing or modifying the new dialog', async () => {
  const pending = deferred(); const { state: s, definition, events } = component('MachineBinding', {
    put: async () => pending.promise,
  });
  s.device = device(); s.open(); const saving = s.save();
  s.projectId = 2; definition.watch.projectId.call(s);
  s.device = { ...device(), id: 'd2' }; s.open();
  pending.resolve({ data: device() }); await saving;
  assert.equal(s.opened, true); assert.equal(events.length, 0); assert.equal(s.error, '');
});

test('binding rejects a mismatched response identity while preserving the current target', async () => {
  const { state: s, events } = component('MachineBinding', { put: async () => ({ data: { id: 'other' } }) });
  s.device = device(); s.open(); await s.save();
  assert.equal(s.opened, true); assert.equal(events.length, 0); assert.match(s.error, /保存未确认/);
});

test('device paging does not advance when the next page fails', async () => {
  const pages = []; let fail = true;
  const { state: s } = component('Devices', { get: async (url, options) => {
    const page = options.params.page; pages.push(page);
    if (page === 2 && fail) { fail = false; throw new Error('page failed'); }
    return { data: { items: [{ ...device(), id: `page-${page}` }], total: 45, total_pages: 3 } };
  } });
  await s.load(); await s.load(2);
  assert.equal(s.page, 1); assert.equal(s.items[0].id, 'page-1');
  await s.load(); assert.equal(s.page, 2); assert.deepEqual(pages, [1, 2, 2]);
});

test('first-install disable reasons and compact versions do not hide full evidence', () => {
  const { state: s, source } = component('Devices'); s.selected = {
    ...device(), components: [{ component: 'communicate', state: 'not_installed' }],
  };
  assert.match(s.firstInstallBlocked('communicate'), /独立详情/);
  s.detailConfirmed = true; assert.equal(s.firstInstallBlocked('communicate'), '');
  s.selected.pending_task_id = 99; assert.match(s.firstInstallBlocked('communicate'), /原任务/);
  assert.match(source, /:title="version\(device, c.value\)"/);
  assert.match(source, /text-overflow: ellipsis/);
});

test('cached component states are historical, not green current-running evidence', () => {
  const { state: s } = component('Devices');
  const d = { ...device(), components: [{ component: 'lower', state: 'running',
    running_build: { version: 'cached-version' } }] };
  assert.equal(s.componentState(d, 'lower'), '程序运行');
  assert.equal(s.componentTone(d, 'lower'), 'valid');
  for (const state of ['stale', 'check_failed']) {
    d.state = state;
    assert.equal(s.componentState(d, 'lower'), '上次：程序运行');
    assert.equal(s.componentTone(d, 'lower'), 'muted');
    assert.equal(s.version(d, 'lower'), 'cached-version');
  }
  d.state = 'checked'; s.error = 'list read failed';
  assert.equal(s.componentState(d, 'lower'), '上次：程序运行');
  assert.equal(s.componentTone(d, 'lower'), 'muted');
  delete d.state;
  assert.equal(s.componentState(d, 'lower'), '程序运行');
  assert.equal(s.componentTone(d, 'lower'), 'valid');
  d.state = 'never_checked'; d.components = [];
  assert.equal(s.componentState(d, 'lower'), '尚未检查');
  d.state = 'stale';
  assert.equal(s.componentState(d, 'lower'), '尚未检查');
  d.components = [{ component: 'lower', state: 'running' }]; delete d.last_success_at;
  assert.equal(s.componentHistorical(d), false);
});
