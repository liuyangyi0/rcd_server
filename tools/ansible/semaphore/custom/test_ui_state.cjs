// State-machine regressions for the native Vue pages. Run only on the remote test host.
// No browser or release executor is used here; rendered coverage is separate.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { webcrypto } = require('node:crypto');

function deferred() {
  let resolve; let reject;
  const promise = new Promise((a, b) => { resolve = a; reject = b; });
  return { promise, resolve, reject };
}
function component(name, axios = {}) {
  const source = fs.readFileSync(path.join(__dirname, 'files/web/src/views/rcd', `${name}.vue`), 'utf8');
  const code = source.match(/<script>([\s\S]*?)<\/script>/)[1]
    .replace(/^import .*;\r?$/gm, '').replace('export default', 'module.exports =');
  const timers = [];
  const context = { module: { exports: {} }, axios, crypto: webcrypto, Blob,
    EventBus: { $emit() {} }, PermissionsCheck: {}, USER_PERMISSIONS: {},
    ReleaseWizard: {}, ComponentHistory: {}, ServiceControl: {}, MachineBinding: {}, HostStatus: {},
    SoftwareDefinitions: {}, HostEnrollment: {},
    sessionStorage: { getItem() { return null; }, setItem() {} },
    document: { title: '' },
    clearTimeout() {}, setTimeout(fn, ms) { timers.push({ fn, ms }); return timers.length; } };
  vm.runInNewContext(code, context, { filename: `${name}.vue` });
  const definition = context.module.exports;
  const state = { ...(definition.data ? definition.data() : {}), projectId: 1,
    initialDeviceIds: [], initialComponent: 'lower',
    initialPackage: null, action: 'upgrade', value: true, $emit() {},
    $route: { query: {} }, $router: { async replace() {}, async push() {} },
    $nextTick(fn) { return fn(); }, can() { return true; } };
  for (const [key, fn] of Object.entries(definition.methods || {})) state[key] = fn.bind(state);
  for (const [key, get] of Object.entries(definition.computed || {})) {
    Object.defineProperty(state, key, { get: get.bind(state) });
  }
  return { state, definition, timers };
}
function pkg(id = 'p1', comp = 'lower') { return { id, revision: 1, manifest: { component: comp, version: id } }; }
function device() { return { id: 'd1', revision: 2, state: 'checked', last_success_at: new Date().toISOString(), release_template_id: 1, components: [{ component: 'lower', state: 'running' }] }; }

test('partial completion is explicitly labelled, filterable and not a total pre-install failure', () => {
  const {state:s}=component('Operations');
  assert.equal(s.status('partial_failed','upgrade'),'部分完成（有失败）');
  assert.equal(s.statusClass('partial_failed'),'rcd-failure');
  assert.ok(s.states.some((item)=>item.value==='partial_failed' && item.text==='部分完成（有失败）'));
  assert.notEqual(s.status('partial_failed','upgrade'),s.status('failed_before_install','upgrade'));
});
function ready(axios) {
  const result = component('ReleaseWizard', axios); const s = result.state;
  s.packages = [pkg()]; s.packageId = 'p1'; s.devices = [device()];
  s.selectedIds = ['d1']; s.compatibility = { d1: '' }; s.paused = true; s.step = 2;
  return result;
}

test('service control has no package, pins one target and resolves with original idempotency key', async () => {
  const calls = [];
  const { state: s } = component('ServiceControl', { post: async (url, body, options) => {
    calls.push({url, body:JSON.stringify(body), key:options.headers['Idempotency-Key']});
    if (calls.length === 1) throw new Error('reply lost');
    return {data:{id:'original-operation'}};
  } });
  s.component = 'manager';s.allowed = true;s.device = {...device(),control_template_id:21,components:[{component:'manager',state:'running'}]};
  s.open('restart');s.paused = true;await s.submit();
  assert.equal(s.pending.action,'restart');assert.equal(s.opened,true);
  s.device = {...s.device,revision:99,active_operation_id:'original-operation'};
  await s.submit();assert.equal(s.pending,null);assert.equal(calls.length,2);
  assert.equal(calls[0].body,calls[1].body);assert.equal(calls[0].key,calls[1].key);
  assert.equal(JSON.parse(calls[0].body).package_id,undefined);
  assert.equal(JSON.parse(calls[0].body).targets[0].revision,2);
});

test('service controls refuse stale evidence, wrong state or missing permission', () => {
  const {state:s}=component('ServiceControl');s.component='agent';s.allowed=true;
  s.device={...device(),control_template_id:21,components:[{component:'agent',state:'running'}]};
  assert.notEqual(s.reason('recover'),'');s.device.components[0].state='stopped';assert.equal(s.reason('recover'),'');
  s.device.state='stale';assert.notEqual(s.reason('recover'),'');s.device.state='checked';s.allowed=false;assert.notEqual(s.reason('recover'),'');
});

test('closed device deep link stays closed when a delayed detail response arrives', async () => {
  const detail = deferred();
  const {state:s}=component('Devices',{get:async(url)=>url.endsWith('/d1')?detail.promise:{data:{items:[],total:0,total_pages:0}}});
  s.$route.query.device='d1';const loading=s.load();await Promise.resolve();await Promise.resolve();
  s.closeDetails(false);detail.resolve({data:device()});await loading;assert.equal(s.selected,null);
});

test('selected device refreshes independently when filters exclude it from the list', async () => {
  const calls = [];
  const latest = { ...device(), revision: 3, state: 'check_failed', last_error: 'SSH disconnected',
    last_success_at: '2026-10-09T05:00:00Z', components: [{ component: 'lower', state: 'stopped' }] };
  const { state: s } = component('Devices', { get: async (url, options) => {
    calls.push({ url, options });
    return url.endsWith('/d1') ? { data: latest } : { data: { items: [], total: 0, total_pages: 0 } };
  } });
  s.selected = device(); s.detailConfirmed = true; s.state = 'checked';
  await s.load();
  assert.equal(calls.filter((c) => c.url.endsWith('/d1')).length, 1);
  assert.equal(calls.find((c) => c.options).options.params.state, 'checked');
  assert.equal(s.items.length, 0); assert.equal(s.selected, latest);
  assert.equal(s.detailDevice.state, 'check_failed'); assert.equal(s.detailError, '');
  assert.equal(s.version(s.selected, 'lower'), '—');
});

test('failed detail refresh retains historical values but invalidates checked control evidence', async () => {
  let fail = true;
  const old = { ...device(), components: [{ component: 'manager', state: 'running', running_build: { version: 'old' } }] };
  const fresh = { ...old, last_success_at: '2026-10-09T06:00:00Z' };
  const { state: s } = component('Devices', { get: async (url) => {
    if (!url.endsWith('/d1')) return { data: { items: [old], total: 1, total_pages: 1 } };
    if (fail) throw { response: { status: 503, data: { error: '详情暂不可用' } } };
    return { data: fresh };
  } });
  s.selected = old; s.detailConfirmed = true;
  await s.load();
  assert.equal(s.error, ''); assert.equal(s.detailError, '详情暂不可用');
  assert.equal(s.selected, old); assert.equal(s.version(s.selected, 'manager'), 'old');
  assert.equal(s.detailDevice.state, 'stale'); assert.equal(s.detailLoading, false);
  fail = false; await s.refreshDetail();
  assert.equal(s.detailError, ''); assert.equal(s.detailDevice.state, 'checked');
  assert.equal(s.selected, fresh);
});

test('list failure does not prevent an independently successful detail update', async () => {
  const fresh = { ...device(), state: 'stale' };
  const { state: s } = component('Devices', { get: async (url) => {
    if (url.endsWith('/d1')) return { data: fresh };
    throw new Error('list unavailable');
  } });
  s.selected = device(); await s.load();
  assert.ok(s.error); assert.equal(s.detailError, ''); assert.equal(s.selected, fresh);
  assert.equal(s.detailDevice.state, 'stale'); assert.equal(s.detailConfirmed, true);
});

test('older device detail response cannot replace a newer result or its freshness', async () => {
  const old = deferred(); const latest = deferred(); let calls = 0;
  const { state: s } = component('Devices', { get: () => (++calls === 1 ? old.promise : latest.promise) });
  s.selected = device();
  const first = s.refreshDetail(); const second = s.refreshDetail();
  latest.resolve({ data: { ...device(), revision: 9, state: 'stale' } }); await second;
  old.resolve({ data: device() }); await first;
  assert.equal(s.selected.revision, 9); assert.equal(s.detailDevice.state, 'stale');
  assert.equal(s.detailError, ''); assert.equal(s.detailLoading, false);
});

test('switching device discards the previous drawer response and error', async () => {
  const old = deferred(); const latest = deferred();
  const { state: s } = component('Devices', { get: (url) => url.endsWith('/d1') ? old.promise : latest.promise });
  const first = s.showDetails(device());
  const second = s.showDetails({ ...device(), id: 'd2' });
  latest.resolve({ data: { ...device(), id: 'd2', revision: 7 } }); await second;
  old.reject(new Error('old target offline')); await first;
  assert.equal(s.selected.id, 'd2'); assert.equal(s.selected.revision, 7);
  assert.equal(s.detailError, ''); assert.equal(s.detailLoading, false);
});

test('project change invalidates outstanding device detail requests', async () => {
  const detail = deferred();
  const { state: s, definition } = component('Devices', { get: (url) => url.endsWith('/d1')
    ? detail.promise : Promise.resolve({ data: { items: [], total: 0, total_pages: 0 } }) });
  const pending = s.showDetails(device());
  s.projectId = 2; definition.watch.projectId.call(s);
  detail.resolve({ data: device() }); await pending;
  assert.equal(s.selected, null); assert.equal(s.detailConfirmed, false);
  assert.equal(s.detailError, ''); assert.equal(s.detailLoading, false);
});

test('unmounted device view cannot accept a delayed detail response', async () => {
  const detail = deferred();
  const { state: s, definition } = component('Devices', { get: () => detail.promise });
  const original = device(); const pending = s.showDetails(original);
  definition.beforeDestroy.call(s);
  detail.resolve({ data: { ...device(), revision: 8 } }); await pending;
  assert.equal(s.selected, original); assert.equal(s.detailConfirmed, false);
});

test('wrong detail identity is rejected without replacing the selected snapshot', async () => {
  const { state: s } = component('Devices', { get: async () => ({ data: { ...device(), id: 'wrong-device' } }) });
  await s.showDetails(device());
  assert.equal(s.selected.id, 'd1'); assert.equal(s.detailDevice.state, 'stale');
  assert.ok(s.detailError); assert.equal(s.detailLoading, false);
});

test('version selection clears immediately and rejects out-of-order component responses', async () => {
  const a = deferred(); const b = deferred(); let calls = 0;
  const { state: s } = ready({ get: () => (++calls === 1 ? a.promise : b.promise) });
  const first = s.loadPackages(); assert.equal(s.chosen, undefined); assert.equal(s.packagesLoading, true);
  s.component = 'manager'; const second = s.loadPackages();
  b.resolve({ data: { items: [pkg('new', 'manager')], total: 1 } }); await second;
  a.resolve({ data: { items: [pkg('old')], total: 1 } }); await first;
  assert.equal(s.packages[0].id, 'new'); assert.equal(s.packagesLoading, false);
});
test('version pagination keeps the selection and sends the next page', async () => {
  let params;
  const { state: s } = ready({ get: async (_, options) => { params = options.params; return { data: { items: [pkg('p2')], total: 2 } }; } });
  await s.loadPackages(true); assert.equal(params.page, 2); assert.equal(s.packages.length, 2); assert.equal(s.packageId, 'p1');
});
test('compatibility rejects changed revision and stale requests for the same package', async () => {
  const a = deferred(); const b = deferred(); let calls = 0;
  const { state: s } = ready({ get: () => (++calls === 1 ? a.promise : b.promise) });
  const first = s.loadCompatibility(); const second = s.loadCompatibility();
  b.resolve({ data: { package_revision: 2, reasons: { d1: '' } } }); await second;
  a.resolve({ data: { package_revision: 1, reasons: { d1: '' } } }); await first;
  assert.equal(s.compatibilityFailed, true); assert.notEqual(s.disabledReason(device()), '');
});
test('closing a wizard invalidates its pending initialization', async () => {
  const a = deferred();
  const { state: s, definition } = ready({ get: (url) => url.endsWith('/devices') ? a.promise : Promise.resolve({ data: { items: [], total: 0 } }) });
  const loading = s.open(); definition.watch.value.call(s, false);
  a.resolve({ data: { items: [device()] } }); await loading;
  assert.equal(s.devices.length, 0);
});
test('uncertain submission retries the identical request and prevents concurrent double-clicks', async () => {
  const a = deferred(); const requests = [];
  const { state: s } = ready({ post: (_, body, options) => { requests.push({ body: JSON.stringify(body), key: options.headers['Idempotency-Key'] }); return a.promise; } });
  const first = s.submit(); await s.submit(); assert.equal(requests.length, 1);
  a.reject(new Error('lost response')); await first;
  assert.ok(s.pendingRequest); s.selectedIds = []; s.packageId = ''; // retry must not consult mutable form fields
  await s.submit(); assert.equal(requests.length, 2); assert.deepEqual(requests[0], requests[1]);
});
test('definitive first rejection permits editing; auth failure after ambiguity does not', async () => {
  const { state: s } = ready({ post: async () => { throw { response: { status: 409 } }; } });
  await s.submit(); assert.equal(s.pendingRequest, null); assert.equal(s.requestKey, '');
  let calls = 0;
  const other = ready({ post: async () => {
    calls += 1;
    if (calls === 1) throw new Error('network');
    throw { response: { status: 401 } };
  } }).state;
  await other.submit(); const pending = other.pendingRequest;
  await other.submit(); assert.equal(other.pendingRequest, pending);
  assert.equal(other.loginRequired, true);
});
test('a stopped component never displays an old build as a running version', () => {
  const { state: s } = component('Devices');
  assert.equal(s.version({ components: [{ component: 'agent', state: 'stopped', running_build: { version: 'old' } }] }, 'agent'), '—');
});
test('history identity change clears prior records and ignores late responses', async () => {
  const a = deferred(); const { state: s, definition } = component('ComponentHistory', { get: () => a.promise });
  s.device = { id: 'old' }; s.component = 'lower'; const first = s.load();
  s.device = { id: 'new' }; definition.watch.contextKey.call(s);
  a.resolve({ data: { items: [{ operation_id: 'old-op' }], total: 1, total_pages: 1 } }); await first;
  assert.equal(s.items.length, 0); assert.equal(s.opened, false); assert.equal(s.loading, false);
});
test('device poll recovers after request failure instead of silently stopping', async () => {
  const { state: s, timers } = component('Devices', { get: async () => { throw Error('offline'); } });
  await s.load(); assert.ok(s.error); assert.equal(timers.length, 1); assert.equal(timers[0].ms, 30000);
});
test('closing task details while polling does not reopen the drawer', async () => {
  const detail = deferred(); const { state: s } = component('Operations', {
    get: (url) => url.endsWith('/op1') ? detail.promise : Promise.resolve({ data: { items: [], total: 0, total_pages: 0 } }),
  });
  s.selected = { id: 'op1' }; s.$route.query.operation = 'op1';
  const polling = s.load(); await Promise.resolve(); s.closeDetails(false);
  detail.resolve({ data: { id: 'op1' } }); await polling; assert.equal(s.selected, null);
});
test('cancel conflict is shown inside the task drawer', async () => {
  const { state: s } = component('Operations', { post: async () => { throw { response: { status: 409, data: { error: '已经开始' } } }; } });
  s.selected = { id: 'op1', revision: 1 }; await s.cancelOperation(); assert.equal(s.detailError, '已经开始');
});
test('package detail refresh resolves stale revisions', async () => {
  const { state: s } = component('Packages', { get: async () => ({ data: { ...pkg(), revision: 2, enabled: false } }) });
  s.selected = pkg(); await s.refreshDetail(); assert.equal(s.selected.revision, 2); assert.equal(s.selected.enabled, false);
});
test('upload rejects empty or multiple packages without HTTP calls', async () => {
  let calls = 0; const { state: s } = component('Packages', { post: async () => { calls += 1; } });
  s.file = { name: 'empty.rcd.tar.gz', size: 0 }; await s.upload(); assert.equal(calls, 0);
  s.dropFile({ dataTransfer: { files: [1, 2] } }); assert.match(s.uploadError, /一个软件包/);
});

test('upload success retains the returned package for a direct explicit upgrade, never deploys', async () => {
  const calls = []; const uploaded = { ...pkg('uploaded', 'manager'), enabled: true };
  const { state: s } = component('Packages', {
    post: async (url) => { calls.push(url); return { status: 201, data: uploaded }; },
    get: async () => ({ data: { items: [uploaded], total: 1, total_pages: 1 } }),
  });
  s.file = { name: 'manager.rcd.tar.gz', size: 1234 }; s.uploadOpen = true;
  await s.upload();
  assert.equal(s.uploadedPackage, uploaded); assert.equal(s.uploadOpen, false);
  assert.equal(s.wizard, false); assert.equal(calls.length, 1); assert.match(calls[0], /packages$/);
  s.openUpload(); assert.equal(s.uploadedPackage, null);
});

test('duplicate disabled package stays disabled at the upload next-step entry', async () => {
  const uploaded = { ...pkg(), enabled: false };
  const { state: s } = component('Packages', {
    post: async () => ({ status: 200, data: uploaded }),
    get: async () => ({ data: { items: [], total: 0, total_pages: 0 } }),
  });
  s.file = { name: 'old.rcd.tar.gz', size: 1 }; await s.upload();
  assert.equal(s.uploadedPackage.enabled, false); assert.match(s.notice, /已经存在/);
  assert.equal(s.wizard, false);
});

test('release confirmation sorts selected machines exactly as the executor, not list order', () => {
  const { state: s } = ready({});
  s.devices = ['46', '30', '58'].map((n) => ({ ...device(), id: `d${n}`, address: `192.168.10.${n}` }));
  s.selectedIds = s.devices.map((d) => d.id); s.compatibility = { d30: '', d46: '', d58: '' };
  assert.deepEqual(Array.from(s.selectedDevices, (d) => d.id), ['d30', 'd58', 'd46']);
  assert.deepEqual(s.devices.map((d) => d.id), ['d46', 'd30', 'd58']);
});

test('wizard refresh invalidates stale compatibility and requires renewed pause confirmation', async () => {
  const latest = { ...device(), revision: 9 }; const calls = [];
  const { state: s } = ready({ get: async (url, options) => {
    calls.push({ url, options });
    return { data: url.endsWith('/devices') ? { items: [latest] }
      : { package_revision: 1, reasons: { d1: '' } } };
  } });
  await s.refreshTargets();
  assert.equal(s.devices[0].revision, 9); assert.equal(s.paused, false);
  assert.equal(s.selectedDevices.length, 1); assert.equal(s.devicesError, '');
  assert.equal(calls[0].options.timeout, 15000);
});

test('failed wizard target refresh cannot reuse the previous compatible snapshot', async () => {
  const { state: s } = ready({ get: async () => { throw new Error('offline'); } });
  await s.refreshTargets();
  assert.ok(s.devicesError); assert.equal(s.selectedDevices.length, 0);
  assert.equal(s.devicesLoading, false); assert.equal(s.paused, false);
});

test('wizard checks only through revision-protected native inspections, never an upgrade', async () => {
  const calls = [];
  const { state: s } = ready({
    post: async (url, body, options) => { calls.push({ url, body, options }); return { data: { task_id: 44 } }; },
    get: async (url) => ({ data: url.endsWith('/devices') ? { items: [device()] }
      : { package_revision: 1, reasons: { d1: '' } } }),
  });
  await s.inspect(device());
  assert.equal(calls.length, 1); assert.match(calls[0].url, /\/devices\/d1\/inspections$/);
  assert.equal(calls[0].body.revision, 2); assert.equal(calls[0].options.headers['If-Match'], '"2"');
  assert.match(s.inspectionMessage, /#44/); assert.equal(s.checking, '');
});

test('late wizard refresh cannot replace a newer device result', async () => {
  const a = deferred(); const b = deferred(); let reads = 0;
  const { state: s } = ready({ get: (url) => url.endsWith('/devices')
    ? (++reads === 1 ? a.promise : b.promise)
    : Promise.resolve({ data: { package_revision: 1, reasons: { d1: '' } } }) });
  const first = s.refreshTargets(); const second = s.refreshTargets();
  b.resolve({ data: { items: [{ ...device(), revision: 7 }] } }); await second;
  a.resolve({ data: { items: [device()] } }); await first;
  assert.equal(s.devices[0].revision, 7); assert.equal(s.devicesLoading, false);
});

test('changing a chosen package or target revokes an earlier pause checkbox', () => {
  const { state: s, definition } = ready({ get: async () => ({ data: { package_revision: 1, reasons: { d1: '' } } }) });
  definition.watch.selectedIds.call(s); assert.equal(s.paused, false);
  s.paused = true; definition.watch.packageId.call(s); assert.equal(s.paused, false);
});

test('first revision conflict refreshes targets but never submits a second upgrade', async () => {
  let posts = 0;
  const { state: s } = ready({
    post: async () => { posts += 1; throw { response: { status: 409, data: { error: '设备revision已变更' } } }; },
    get: async (url) => ({ data: url.endsWith('/devices') ? { items: [{ ...device(), revision: 8 }] }
      : { package_revision: 1, reasons: { d1: '' } } }),
  });
  await s.submit(); assert.equal(posts, 1); assert.equal(s.step, 1);
  assert.equal(s.devices[0].revision, 8); assert.equal(s.paused, false); assert.equal(s.pendingRequest, null);
  assert.match(s.error, /重新确认/);
});

test('operation detail refresh survives list failure, then clears only its own recovered error', async () => {
  let listFail = true; let detailFail = false;
  const { state: s } = component('Operations', { get: async (url) => {
    if (url.endsWith('/op1')) {
      if (detailFail) throw new Error('detail offline');
      return { data: { id: 'op1', state: 'succeeded' } };
    }
    if (listFail) throw new Error('list offline');
    return { data: { items: [], total: 0, total_pages: 0 } };
  } });
  s.selected = { id: 'op1', state: 'running' }; await s.load();
  assert.ok(s.error); assert.equal(s.selected.state, 'succeeded'); assert.equal(s.detailError, '');
  listFail = false; detailFail = true; await s.load(); assert.equal(s.error, ''); assert.ok(s.detailError);
  detailFail = false; await s.load(); assert.equal(s.detailError, ''); assert.ok(s.detailUpdatedAt);
});

test('background operation refresh does not flash the blocking loading state', async () => {
  const a = deferred(); const { state: s } = component('Operations', { get: () => a.promise });
  const request = s.load(true); assert.equal(s.loading, false);
  a.resolve({ data: { items: [], total: 0, total_pages: 0 } }); await request;
  assert.equal(s.loading, false); assert.ok(s.lastUpdatedAt);
});

test('operation polling stops on login expiry and does not survive unmount', async () => {
  const expired = component('Operations', { get: async () => { throw { response: { status: 401 } }; } });
  await expired.state.load(); assert.equal(expired.timers.length, 0);
  const pending = deferred(); const unmounted = component('Operations', { get: () => pending.promise });
  const request = unmounted.state.load(); unmounted.definition.beforeDestroy.call(unmounted.state);
  pending.resolve({ data: { items: [], total: 0, total_pages: 0 } }); await request;
  assert.equal(unmounted.timers.length, 0);
});

test('history identifies an already-running exact image, but not a same-label different image', () => {
  const { state: s } = component('ComponentHistory'); s.component = 'manager';
  s.device = { state: 'checked', components: [{ component: 'manager', state: 'running', running_sha256: 'a'.repeat(64) }] };
  const row = { package: { manifest: { binary_sha256: 'a'.repeat(64) } } };
  assert.match(s.restoreReason(row), /已是此程序版本/);
  row.package.manifest.binary_sha256 = 'b'.repeat(64); assert.equal(s.restoreReason(row), '');
  s.device.state = 'stale'; row.package.manifest.binary_sha256 = 'a'.repeat(64);
  assert.equal(s.restoreReason(row), '');
  row.restore_disabled_reason = '数据格式不兼容'; assert.equal(s.restoreReason(row), '数据格式不兼容');
});

test('communicate is a named fourth component throughout package and release pages', () => {
  for (const name of ['Devices', 'Packages', 'ReleaseWizard']) {
    const { state: s } = component(name);
    assert.ok(s.components.some((c) => c.value === 'communicate' && c.text === '串口通信程序'));
  }
  const { state: operation } = component('Operations');
  assert.equal(operation.componentName('communicate'), '串口通信程序');
  const { state: wizard } = component('ReleaseWizard'); wizard.component = 'communicate';
  assert.match(wizard.impact, /独立串口通信程序/);
  assert.match(wizard.impact, /不停止原采集程序/);
  assert.match(wizard.impact, /拒绝活动通信/);
});

test('a trusted not-installed observation is distinct from stopped and missing evidence', () => {
  const { state: s } = component('Devices');
  const d = { components: [{ component: 'communicate', state: 'not_installed' }] };
  assert.equal(s.componentState(d, 'communicate'), '未安装');
  assert.equal(s.version(d, 'communicate'), '—');
  assert.equal(s.componentState({ components: [] }, 'communicate'), '尚未检查');
  assert.equal(s.componentState({ components: [{ component: 'communicate', state: 'stopped' }] }, 'communicate'), '已停止');
});

test('a machine without communicate cannot be selected for a blind first installation', () => {
  const { state: s } = ready({}); s.component = 'communicate';
  s.devices = [{ ...device(), components: [{ component: 'communicate', state: 'not_installed' }] }];
  assert.match(s.disabledReason(s.devices[0]), /未安装.*首次接入/);
  assert.equal(s.currentVersion(s.devices[0]), '未安装');
  assert.equal(s.selectedDevices.length, 0);
  s.devices[0].components[0] = { component: 'communicate', state: 'running', running_build: { version: 'sync-v1' } };
  assert.equal(s.disabledReason(s.devices[0]), '');
  assert.equal(s.currentVersion(s.devices[0]), 'sync-v1');
});

test('unsupported component cannot accidentally enter the manager service-control path', () => {
  const { state: s } = component('ServiceControl');
  s.component = 'communicate'; s.allowed = true;
  s.device = { ...device(), control_template_id: 21, components: [{ component: 'communicate', state: 'running' }] };
  assert.match(s.reason('restart'), /不提供/);
  s.open('restart'); assert.equal(s.opened, false); assert.equal(s.pending, null);
});

function hostStatus(overrides = {}) {
  return { schema_version: 1, observed_at: new Date().toISOString(), available: true, error: '',
    sample_interval_seconds: 0.2, uptime_seconds: 90061,
    cpu: { available: true, utilization_percent: 0, logical_count: 4 },
    memory: { available: true, total_bytes: 8 * 1024 ** 3, available_bytes: 8 * 1024 ** 3, used_percent: 0 },
    disks: [{ path: '/', mount_point: '/', filesystem: 'ext4', available: true, used_percent: 23.5 }],
    network: [{ name: 'eth0', link_up: true, ipv4_addresses: ['192.168.10.46'],
      rx_bytes: 0, tx_bytes: 1024, rx_bytes_per_second: 0, tx_bytes_per_second: null }], ...overrides };
}

test('host snapshot displays valid zero values, capacities and sampled network rates honestly', () => {
  const { state: s } = component('HostStatus'); s.device = { state: 'checked', host_status: hostStatus() };
  assert.equal(s.cpuSummary, '0.0%'); assert.equal(s.memorySummary, '0.0%');
  assert.equal(s.memoryCapacity, '8.0 GiB / 8.0 GiB');
  assert.equal(s.diskSummary, '最高 23.5%'); assert.equal(s.networkSummary, '1/1 接口up');
  assert.equal(s.rate(0), '0 B/s'); assert.equal(s.rate(null), '速率未知');
  assert.equal(s.bytes(null), '未知'); assert.equal(s.percent(NaN), '未知');
  assert.equal(s.uptime, '1天 1小时 1分'); assert.equal(s.interval, '0.20秒');
  assert.equal(s.snapshotLabel, '巡检快照 · 非实时');
  assert.equal(s.linkLabel(true), '链路up'); assert.equal(s.linkLabel(null), '链路状态未知');
});

test('missing or unavailable host metrics are never converted into reassuring zeros', () => {
  const { state: s } = component('HostStatus'); s.device = { state: 'checked' };
  assert.equal(s.snapshotLabel, '尚未获取主机指标');
  assert.equal(s.cpuSummary, '未获取'); assert.equal(s.diskSummary, '未获取');
  assert.equal(s.networkSummary, '链路状态未知');
  s.device.host_status = hostStatus({ available: false, cpu: { available: false, utilization_percent: null },
    memory: { available: false, used_percent: null }, network: [{ name: 'eth0', link_up: null }] });
  assert.equal(s.snapshotLabel, '本轮主机指标不可用'); assert.equal(s.cpuSummary, '未获取');
  assert.equal(s.memorySummary, '未获取'); assert.equal(s.networkSummary, '链路状态未知');
});

test('freshness and failed checks label historical host values instead of claiming current state', () => {
  const { state: s } = component('HostStatus');
  s.device = { state: 'checked', host_status: hostStatus({ observed_at: new Date(Date.now() - 181000).toISOString() }) };
  assert.equal(s.snapshotLabel, '历史快照 · 已过期'); assert.equal(s.isHistorical, true);
  s.device.host_status.observed_at = new Date().toISOString(); s.device.state = 'check_failed';
  assert.equal(s.snapshotLabel, '历史快照 · 当前检查未成功');
  s.device.state = 'checked'; s.device.host_status.observed_at = 'invalid';
  assert.equal(s.snapshotLabel, '观测时间无法确认 · 非实时'); assert.equal(s.age, null);
  s.device.host_status.observed_at = new Date(Date.now() + 60000).toISOString();
  assert.equal(s.snapshotLabel, '观测时间无法确认 · 非实时');
});

test('partial metrics keep valid CPU but expose disk, address and link uncertainty', () => {
  const { state: s } = component('HostStatus');
  s.device = { state: 'checked', host_status: hostStatus({ error: '部分目录无法读取',
    disks: [{ path: '/', available: false, used_percent: 0 }],
    network: [{ name: 'eth0', link_up: true }, { name: 'wlan0', link_up: null }] }) };
  assert.equal(s.cpuSummary, '0.0%'); assert.equal(s.diskSummary, '未获取');
  assert.equal(s.snapshotLabel, '巡检快照 · 部分指标不可用');
  assert.equal(s.networkSummary, '1/2 接口up，1个未知');
  assert.equal(s.addresses({ ipv4_addresses: [], addresses_error: 'permission denied' }), '地址无法获取');
  assert.equal(s.addresses({ ipv4_addresses: [] }), '未配置IPv4');
  assert.equal(s.percent(101), '未知'); assert.equal(s.rate(-1), '速率未知');
});

test('package upload remains visibly permission-protected and cannot open without resource rights', () => {
  const { state: s } = component('Packages'); s.can = () => false;
  s.openUpload(); assert.equal(s.uploadOpen, false); assert.equal(s.canManage, false);
  assert.equal(s.canRun, false);
});

test('custom software names are appended without replacing four builtin identities', async () => {
  const app = { component: 'plant-monitor', name: '厂区监视软件', revision: 1 };
  for (const name of ['Devices', 'Packages', 'ReleaseWizard', 'Operations']) {
    const { state: s } = component(name, { get: async () => ({ data: { items: [app] } }) });
    await s.loadApplications();
    if (name === 'Devices') {
      assert.equal(s.knownComponents({ components: [{ component: 'legacy-observed' }] }).length, 6);
      assert.ok(s.knownComponents({}).some((c) => c.value === app.component && c.text === app.name));
    } else assert.equal(s.componentName(app.component), app.name);
  }
});

function definitionReady(axios) {
  const result = component('SoftwareDefinitions', axios); const s = result.state;
  s.allowed = true; s.component = 'plant-monitor'; s.name = '厂区监视软件';
  s.gracefulShutdown = true; return result;
}

test('software registration only accepts immutable single-program arguments and an explicit shutdown confirmation', () => {
  const { state: s } = definitionReady({}); assert.equal(s.validation, '');
  s.gracefulShutdown = false; assert.match(s.validation, /正常退出/); s.gracefulShutdown = true;
  s.argumentsText = '--port=8080\n/config/site'; assert.equal(s.validation, '');
  for (const text of [';shutdown', '$(whoami)', '../secret', '%HOME%', 'one\ttwo']) {
    s.argumentsText = text; assert.match(s.validation, /启动参数/);
  }
  s.argumentsText = ''; s.component = 'communicate'; assert.match(s.validation, /内置组件/);
  s.component = '9test'; assert.match(s.validation, /有效/);
});

test('an ambiguous software registration locks original content and retries the same definition', async () => {
  const calls = []; const { state: s } = definitionReady({ post: async (url, body) => {
    calls.push(JSON.stringify(body)); if (calls.length === 1) throw new Error('reply lost');
    return { data: { ...body, revision: 1 }, status: 200 };
  } });
  await s.submit(); assert.ok(s.pending); assert.equal(s.registered, null);
  s.name = 'must not change request'; await s.submit();
  assert.equal(calls[0], calls[1]); assert.equal(s.registered.name, '厂区监视软件');
  assert.equal(JSON.parse(calls[0]).graceful_shutdown_confirmed, true);
  assert.match(s.resultMessage, /未重复登记或安装/);
});

function enrollmentReady(axios = {}) {
  const result = component('HostEnrollment', axios); const s = result.state;
  s.allowed = true; s.options = { available: true, ssh_keys: [{ id: 3, name: 'encrypted SSH', type: 'ssh' }],
    become_keys: [{ id: 4, name: 'sudo credential', type: 'login_password' }] };
  s.request = { name: 'RCD new', address: '192.168.10.99', ssh_port: 22, ssh_user: 'operator',
    ssh_key_id: 3, become_key_id: 4, service_account: 'operator' };
  s.accessConfirmed = true; return result;
}
function setup(state = 'awaiting_host_key') {
  return { id: 'setup-original', revision: 2, state, task_id: 1,
    request: { name: 'RCD new', address: '192.168.10.99', ssh_port: 22, ssh_user: 'operator', become_key_id: 4 },
    host_keys: [{ algorithm: 'ssh-ed25519', fingerprint: 'SHA256:first' },
      { algorithm: 'ssh-rsa', fingerprint: 'SHA256:second' }], steps: [] };
}

test('host enrollment takes encrypted credential references, not arbitrary paths or passwords', () => {
  const { state: s } = enrollmentReady(); assert.equal(s.validation, '');
  assert.equal(s.request.password, undefined); assert.equal(s.request.path, undefined);
  s.request.service_account = 'root'; assert.match(s.validation, /非root/);
  s.request.service_account = 'operator'; s.request.ssh_key_id = 999; assert.match(s.validation, /加密SSH凭据/);
  s.request.ssh_key_id = 3; s.request.address = 'https://host'; assert.match(s.validation, /固定IP/);
  s.request.address = '192.168.10.99'; s.request.ssh_port = 65536; assert.match(s.validation, /端口/);
});

test('ambiguous host registration preserves its target and idempotency key, including a later login failure', async () => {
  const calls = []; const { state: s } = enrollmentReady({ post: async (url, body, options) => {
    calls.push({ body: JSON.stringify(body), key: options.headers['Idempotency-Key'] });
    if (calls.length === 1) throw new Error('reply lost');
    if (calls.length === 2) throw { response: { status: 401 } };
    return { data: setup('keyscan_queued') };
  } });
  await s.create(); const original = s.pendingCreate; assert.ok(original);
  s.request.address = '192.168.10.100'; await s.create(); assert.equal(s.pendingCreate, original);
  await s.create(); assert.equal(s.setup.id, 'setup-original'); assert.equal(s.pendingCreate, null);
  assert.equal(new Set(calls.map((c) => c.key)).size, 1);
  assert.equal(new Set(calls.map((c) => c.body)).size, 1);
});

test('host trust confirmation pins the revision and requires all displayed fingerprints', async () => {
  const calls = []; const { state: s } = enrollmentReady({ post: async (url, body, options) => {
    calls.push({ url, body, options }); return { data: setup('ssh_probe_queued') };
  } });
  s.setup = setup(); await s.confirmKeys(); assert.equal(calls.length, 0);
  s.keysConfirmed = true; await s.confirmKeys();
  assert.deepEqual([...calls[0].body.fingerprints], ['SHA256:first', 'SHA256:second']);
  assert.equal(calls[0].body.confirmed, true); assert.equal(calls[0].options.headers['If-Match'], '"2"');
  s.setup = setup('host_key_conflict'); s.keysConfirmed = true; await s.confirmKeys(); assert.equal(calls.length, 1);
});

test('unknown host setup is queried without a new registration, key confirmation or install', async () => {
  const reads = []; const writes = [];
  const { state: s } = enrollmentReady({ get: async (url) => { reads.push(url); return { data: setup('unknown') }; },
    post: async (url) => { writes.push(url); } });
  s.setup = setup('unknown'); s.resumeId = s.setup.id; s.installConfirmed = true; s.keysConfirmed = true;
  await s.create(); await s.confirmKeys(); await s.install(); await s.refresh();
  assert.equal(writes.length, 0); assert.equal(reads.length, 1); assert.match(reads[0], /setup-original$/);
  s.newHost(); assert.equal(s.resumeId, 'setup-original'); assert.match(s.stateLabel('unknown'), /结果未知/);
});

test('initial installation freezes package revision and original request on lost acknowledgement', async () => {
  const calls = []; const { state: s } = enrollmentReady({ post: async (url, body, options) => {
    calls.push({ body: JSON.stringify(body), key: options.headers['Idempotency-Key'] });
    if (calls.length === 1) throw new Error('reply lost'); return { data: setup('installation_queued') };
  } });
  s.setup = setup('ready_for_install'); s.resumeId = s.setup.id;
  s.packages = [pkg('p1'), pkg('p2')]; s.packageId = 'p1'; s.installConfirmed = true;
  await s.install(); assert.ok(s.pendingInstall); s.packageId = 'p2'; await s.install();
  assert.equal(calls[0].key, calls[1].key); assert.equal(calls[0].body, calls[1].body);
  assert.equal(JSON.parse(calls[0].body).package_id, 'p1'); assert.equal(s.setup.state, 'installation_queued');
  assert.match(s.stateLabel('installed_stopped'), /待配置/);
});

test('saved host setup reference is resumed with GET and stale replies cannot switch host identity', async () => {
  const reply = deferred(); const { state: s, definition } = enrollmentReady({ get: async () => reply.promise });
  s.resumeId = 'setup-original'; const read = s.refresh();
  definition.beforeDestroy.call(s); reply.resolve({ data: setup('installed_stopped') }); await read;
  assert.equal(s.setup, null); assert.equal(s.resumeId, 'setup-original');
  assert.throws(() => s.accept({ ...setup(), id: 'other-host' }), /identity mismatch/);
});

test('failed metrics attempt keeps successful values explicitly historical with both timestamps', () => {
  const { state: s } = component('HostStatus');
  s.device = { state: 'checked', host_status: hostStatus({ observed_at: new Date(Date.now() - 1000).toISOString(),
    last_attempt_at: new Date().toISOString(), error: 'metrics file unavailable' }) };
  assert.equal(s.cpuSummary, '0.0%'); assert.equal(s.isHistorical, true);
  assert.equal(s.snapshotLabel, '历史快照 · 本轮指标不可用'); assert.ok(s.lastAttemptAt);
});

test('existing-device first install pins device and package revisions without creating another host', async () => {
  const calls = []; const { state: s } = enrollmentReady({ post: async (url, body, options) => {
    calls.push({ url, body: JSON.stringify(body), key: options.headers['Idempotency-Key'], match: options.headers['If-Match'] });
    if (calls.length === 1) throw new Error('reply lost');
    return { data: { ...setup('installation_queued'), existing_device: true, device_id: 'd1' } };
  } });
  s.initialDevice = { ...device(), last_success_at: new Date().toISOString(),
    components: [{ component: 'plant-viewer', state: 'not_installed' }] };
  s.component = 'plant-viewer'; s.packages = [pkg('p1', 'plant-viewer')]; s.packageId = 'p1'; s.installConfirmed = true;
  assert.equal(s.firstInstallReason, ''); await s.install(); assert.ok(s.pendingInstall);
  s.initialDevice = { ...s.initialDevice, revision: 99 }; await s.install();
  assert.match(calls[0].url, /devices\/d1\/first-installs$/); assert.equal(calls[0].match, '"2"');
  assert.equal(calls[0].body, calls[1].body); assert.equal(calls[0].key, calls[1].key);
  assert.equal(s.setup.device_id, 'd1');
});

test('first installation rejects unknown, stopped, stale or busy device evidence', () => {
  const { state: s } = enrollmentReady(); s.component = 'communicate';
  s.initialDevice = { ...device(), last_success_at: new Date().toISOString(), components: [] };
  assert.match(s.firstInstallReason, /未安装/);
  s.initialDevice.components = [{ component: 'communicate', state: 'stopped' }]; assert.match(s.firstInstallReason, /未安装/);
  s.initialDevice.components[0].state = 'not_installed'; s.initialDevice.state = 'stale';
  assert.match(s.firstInstallReason, /180秒/); s.initialDevice.state = 'checked';
  s.initialDevice.active_operation_id = 'previous-operation'; assert.match(s.firstInstallReason, /原任务/);
});

test('registered generic lifecycle requires its own impact confirmation and cannot control communicate', async () => {
  const calls = []; const { state: s } = component('ServiceControl', { post: async (url, body) => {
    calls.push(body); return { data: { id: 'service-app' } };
  } });
  s.component = 'plant-viewer'; s.allowed = true; s.application = { component: 'plant-viewer', name: '厂区可视软件' };
  s.device = { ...device(), control_template_id: 21, components: [{ component: 'plant-viewer', state: 'stopped' }] };
  assert.equal(s.reason('start'), ''); assert.match(s.reason('recover'), /不支持/);
  s.open('start'); s.paused = true; await s.submit(); assert.equal(calls.length, 0);
  s.applicationConfirmed = true; await s.submit(); assert.equal(calls.length, 1);
  assert.equal(calls[0].component, 'plant-viewer'); assert.equal(calls[0].action, 'start');
  assert.match(s.genericDescriptions.start, /开机自启/);
  s.component = 'communicate'; s.application = { component: 'communicate', name: 'attempt to spoof builtin' };
  assert.equal(s.isApplication, false); assert.match(s.reason('start'), /不提供/);
});

test('only unsubmitted host enrollment can be explicitly cancelled; ambiguous cancellation blocks install', async () => {
  const calls = []; const { state: s } = enrollmentReady({ post: async (url, body, options) => {
    calls.push({ url, body, match: options.headers['If-Match'] }); throw new Error('reply lost');
  } });
  s.setup = setup('awaiting_host_key'); assert.equal(s.canCancel, true);
  await s.cancel(); assert.equal(calls.length, 0); s.cancelConfirmed = true; await s.cancel();
  assert.equal(calls.length, 1); assert.equal(calls[0].match, '"2"'); assert.ok(s.pendingCancel);
  s.setup.state = 'ready_for_install'; assert.equal(s.canChooseInstall, false);
  s.setup.state = 'unknown'; await s.cancel(); assert.equal(calls.length, 1); assert.equal(s.canCancel, false);
  s.accept({ ...setup('cancelled'), revision: 3 }); assert.equal(s.pendingCancel, null);
  assert.match(s.stateLabel('cancelled'), /未安装软件/);
});

test('non-root without sudo can register and probe but cannot request privileged first installation', () => {
  const { state: s } = enrollmentReady(); s.request.become_key_id = 0;
  assert.equal(s.validation, ''); assert.match(s.installPrivilegeReason, /root登录/);
  s.setup = setup('ready_for_install'); s.setup.request.become_key_id = 0;
  s.packages = [pkg()]; s.packageId = 'p1'; s.installConfirmed = true;
  assert.equal(s.canSubmitInstall, false); assert.match(s.firstInstallReason, /当前只能只读/);
  s.setup.request.ssh_user = 'root'; assert.equal(s.installPrivilegeReason, '');
  assert.equal(s.canSubmitInstall, true);
});

test('a read started before installation cannot restore its obsolete ready-for-install state', async () => {
  const old = deferred();
  const { state: s } = enrollmentReady({ get: () => old.promise,
    post: async () => ({ data: { ...setup('installation_queued'), revision: 3 } }) });
  s.setup = setup('ready_for_install'); s.resumeId = s.setup.id;
  s.packages = [pkg()]; s.packageId = 'p1'; s.installConfirmed = true;
  const read = s.refresh(); await s.install();
  assert.equal(s.setup.state, 'installation_queued');
  old.resolve({ data: setup('ready_for_install') }); await read;
  assert.equal(s.setup.state, 'installation_queued'); assert.equal(s.setup.revision, 3);
  assert.equal(s.canChooseInstall, false); assert.equal(s.loading, false);
});

test('host setup reads are ordered and stale revisions cannot alter pending state', async () => {
  const first = deferred(); const last = deferred(); let count = 0;
  const { state: s } = enrollmentReady({ get: () => (++count === 1 ? first.promise : last.promise) });
  s.setup = setup('installation_queued'); s.resumeId = s.setup.id;
  const one = s.refresh(); const two = s.refresh();
  last.resolve({ data: { ...setup('installed_stopped'), revision: 5 } }); await two;
  first.reject(new Error('old timed-out read')); await one;
  assert.equal(s.setup.state, 'installed_stopped'); assert.equal(s.error, '');
  const pending = { key: 'original', body: { revision: 4, package_id: 'p1' } };
  s.pendingInstall = pending; s.accept({ ...setup('ready_for_install'), revision: 3 });
  assert.equal(s.setup.revision, 5); assert.equal(s.pendingInstall, pending);
});

test('failed load-more retries the same version page instead of skipping it', async () => {
  const pages = []; let fail = true;
  const { state: s } = enrollmentReady({ get: async (url, options) => {
    const page = options.params.page; pages.push(page);
    if (page === 2 && fail) { fail = false; throw new Error('page response lost'); }
    return { data: { items: [{ ...pkg(`p${page}`), enabled: true }], total_pages: 3 } };
  } });
  await s.loadPackages(); assert.equal(s.packagePage, 1);
  await s.loadPackages(true); assert.equal(s.packagePage, 1); assert.ok(s.packagesError);
  await s.loadPackages(true); assert.equal(s.packagePage, 2);
  assert.deepEqual(pages, [1, 2, 2]); assert.deepEqual([...s.packages.map((p) => p.id)], ['p1', 'p2']);
});

test('host freshness expires without a new response or a new snapshot object', () => {
  const { state: s } = component('HostStatus'); const snapshot = hostStatus();
  s.device = { state: 'checked', host_status: snapshot };
  s.now = Date.parse(snapshot.observed_at); assert.equal(s.snapshotLabel, '巡检快照 · 非实时');
  s.now += 181000;
  assert.equal(s.device.host_status, snapshot); assert.equal(s.snapshotLabel, '历史快照 · 已过期');
});

test('first installation and service controls expire cached evidence while a page remains open', () => {
  const { state: s } = enrollmentReady(); s.component = 'communicate';
  s.initialDevice = { ...device(), components: [{ component: 'communicate', state: 'not_installed' }] };
  s.now = Date.parse(s.initialDevice.last_success_at); assert.equal(s.firstInstallReason, '');
  s.now += 181000; assert.match(s.firstInstallReason, /180秒/);
  const { state: control } = component('ServiceControl'); control.component = 'manager'; control.allowed = true;
  control.device = { ...device(), control_template_id: 3, components: [{ component: 'manager', state: 'stopped' }] };
  control.now = Date.parse(control.device.last_success_at); assert.equal(control.reason('start'), '');
  control.now += 181000; assert.match(control.reason('start'), /已过期/);
});

test('switching project clears registration busy state and prevents old replies from affecting a new definition', async () => {
  const old = deferred(); const fresh = deferred(); let calls = 0;
  const { state: s, definition } = definitionReady({ post: () => (++calls === 1 ? old.promise : fresh.promise) });
  const previous = s.submit(); assert.equal(s.busy, true);
  s.projectId = 2; definition.watch.projectId.call(s);
  assert.equal(s.busy, false); assert.equal(s.pending, null); assert.equal(s.component, '');
  s.component = 'other-app'; s.name = '第二项目软件'; s.gracefulShutdown = true;
  const current = s.submit(); old.resolve({ data: { component: 'plant-monitor' }, status: 201 }); await previous;
  assert.equal(s.busy, true); assert.equal(s.registered, null);
  fresh.resolve({ data: { component: 'other-app' }, status: 201 }); await current;
  assert.equal(s.busy, false); assert.equal(s.registered.component, 'other-app');
});

test('old-project upload progress and completion cannot unlock or overwrite another upload', async () => {
  const old = deferred(); const fresh = deferred(); const calls = [];
  const { state: s, definition } = component('Packages', {
    post: (url, body, options) => { calls.push({ url, options }); return calls.length === 1 ? old.promise : fresh.promise; },
    get: async () => ({ data: { items: [], total: 0, total_pages: 0 } }),
  });
  s.file = { name: 'old.rcd.tar.gz', size: 100 }; const previous = s.upload();
  s.projectId = 2; definition.watch.projectId.call(s); assert.equal(s.uploading, false);
  s.file = { name: 'new.rcd.tar.gz', size: 100 }; const current = s.upload();
  calls[1].options.onUploadProgress({ loaded: 30, total: 100 }); assert.equal(s.progress, 30);
  calls[0].options.onUploadProgress({ loaded: 90, total: 100 }); assert.equal(s.progress, 30);
  old.resolve({ status: 201, data: pkg('old') }); await previous;
  assert.equal(s.uploading, true); assert.equal(s.uploadedPackage, null);
  fresh.resolve({ status: 201, data: pkg('new') }); await current;
  assert.equal(s.uploading, false); assert.equal(s.uploadedPackage.id, 'new');
});

test('late package detail completion cannot release a different project action', async () => {
  const old = deferred(); const fresh = deferred(); let detailReads = 0;
  const { state: s, definition } = component('Packages', { get: async (url) => {
    if (url.endsWith('/p1')) return (++detailReads === 1 ? old.promise : fresh.promise);
    return { data: { items: [], total: 0, total_pages: 0 } };
  } });
  s.selected = pkg(); const previous = s.refreshDetail(); assert.equal(s.busy, true);
  s.projectId = 2; definition.watch.projectId.call(s); assert.equal(s.busy, false);
  s.selected = pkg(); const current = s.refreshDetail();
  old.resolve({ data: { ...pkg(), revision: 99 } }); await previous;
  assert.equal(s.busy, true); assert.equal(s.selected.revision, 1);
  fresh.resolve({ data: { ...pkg(), revision: 2 } }); await current;
  assert.equal(s.busy, false); assert.equal(s.selected.revision, 2);
});

test('unmounted service control cannot navigate to an obsolete project after a late successful response', async () => {
  const response = deferred(); const routes = [];
  const { state: s, definition } = component('ServiceControl', { post: () => response.promise });
  s.component = 'manager'; s.allowed = true;
  s.device = { ...device(), control_template_id: 3, components: [{ component: 'manager', state: 'stopped' }] };
  s.$router.push = async (route) => { routes.push(route); };
  s.open('start'); s.paused = true; const request = s.submit();
  definition.beforeDestroy.call(s); response.resolve({ data: { id: 'obsolete-operation' } }); await request;
  assert.deepEqual(routes, []);
});

test('unknown first-install reconciliation is separate from install and a lost reply only refreshes its setup', async () => {
  const writes = []; const reads = [];
  const original = { ...setup('unknown'), revision: 7, phase: 'install', install: { package_id: 'p1' }, original_install_task_id: 23 };
  const { state: s } = enrollmentReady({
    post: async (url, body, options) => { writes.push({ url, body, match: options.headers['If-Match'] }); throw new Error('reply lost'); },
    get: async (url) => { reads.push(url); return { data: { ...original, revision: 8, phase: 'reconcile', state: 'reconcile_queued', task_id: 24 } }; },
  });
  s.setup = original; s.resumeId = original.id;
  await s.reconcile(); assert.equal(writes.length, 0);
  s.reconcileConfirmed = true; await s.reconcile(); assert.ok(s.pendingReconcile);
  assert.match(writes[0].url, /\/reconcile$/); assert.equal(writes[0].match, '"7"');
  assert.equal(writes[0].body.package_id, undefined); assert.equal(writes[0].body.confirmed, true);
  await s.reconcile(); assert.equal(writes.length, 1); assert.equal(reads.length, 1);
  assert.equal(s.setup.original_install_task_id, 23); assert.equal(s.setup.task_id, 24);
  assert.equal(s.setup.state, 'reconcile_queued'); assert.equal(s.pendingReconcile, null);
  assert.match(s.stateLabel(s.setup.state), /只读补查/); assert.equal(s.canChooseInstall, false);
});

test('native running state continues polling and preserves the explicit read-only reconciliation phase', async () => {
  const { state: s, timers } = enrollmentReady({ get: async () => ({
    data: { ...setup('running'), revision: 9, phase: 'reconcile', task_id: 24, original_install_task_id: 23 },
  }) });
  s.setup = { ...setup('reconcile_queued'), revision: 8 }; s.resumeId = s.setup.id;
  await s.refresh();
  assert.equal(s.stateLabel('running'), '任务执行中'); assert.equal(s.phaseLabel(s.setup.phase), '核对原首装结果');
  assert.equal(s.setup.original_install_task_id, 23); assert.ok(timers.some((timer) => timer.ms === 3000));
  assert.equal(s.canChooseInstall, false); assert.equal(s.canReconcile, false);
});

test('verified first-start history explains its time while repeated releases remain separate records', async () => {
  const shared = pkg('same-package');
  const rows = [
    { package: shared, operation_id: 'verified-start', installed_at: '2026-10-21T06:00:00Z', installation_kind: 'first_install_verified_start' },
    { package: shared, operation_id: 'later-release', installed_at: '2026-10-21T07:00:00Z' },
  ];
  const { state: s } = component('ComponentHistory', { get: async () => ({ data: { items: rows, total: 2, total_pages: 1 } }) });
  s.device = device(); s.component = 'plant-viewer'; await s.load();
  assert.equal(s.items.length, 2); assert.equal(s.total, 2);
  assert.match(s.historyNote(s.items[0]), /首次安装后启动确认/);
  assert.match(s.historyNote(s.items[0]), /启动验收时间/); assert.equal(s.historyNote(s.items[1]), '');
});
