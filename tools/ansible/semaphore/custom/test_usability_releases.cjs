// Release usability state tests. Run on the authorized remote test host only.
// HTTP is deterministic; no task, installer, SSH command or business process runs.
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
function page(name, axios = {}) {
  const source = fs.readFileSync(path.join(__dirname, 'files/web/src/views/rcd', `${name}.vue`), 'utf8');
  const script = source.match(/<script>([\s\S]*?)<\/script>/)[1]
    .replace(/^import .*;\r?$/gm, '').replace('export default', 'module.exports =');
  const timers = [];
  const context = { module: { exports: {} }, axios, crypto: webcrypto, Blob,
    PermissionsCheck: {}, USER_PERMISSIONS: {}, EventBus: { $emit() {} },
    ReleaseWizard: {}, SoftwareDefinitions: {}, document: { title: '' },
    setTimeout(fn, ms) { timers.push({ fn, ms }); return timers.length; }, clearTimeout() {} };
  vm.runInNewContext(script, context, { filename: `${name}.vue` });
  const definition = context.module.exports;
  const state = { ...definition.data(), projectId: 1, value: true, initialPackage: null,
    initialDeviceIds: [], initialComponent: 'lower', action: 'upgrade',
    $emit() {}, can() { return true; }, $nextTick(fn) { return fn(); },
    $route: { query: {} }, $router: { async replace() {}, async push() {} } };
  for (const [key, fn] of Object.entries(definition.methods || {})) state[key] = fn.bind(state);
  for (const [key, fn] of Object.entries(definition.computed || {})) {
    Object.defineProperty(state, key, { get: fn.bind(state) });
  }
  return { state, definition, timers, source };
}
const response = (id, total = 60) => ({ data: { items: [{ id }], total, total_pages: 3 } });

for (const name of ['Packages', 'Operations', 'ComponentHistory']) {
  test(`${name}: failed next page preserves committed rows/page and retries the same page`, async () => {
    const calls = [];
    const { state: s } = page(name, { get: async (url, config) => {
      const requested = config.params.page; calls.push(requested);
      if (calls.length === 2) throw new Error('connection lost');
      return response(`page-${requested}`);
    } });
    if (name === 'ComponentHistory') { s.device = { id: 'd1' }; s.component = 'manager'; }
    await s.load();
    if (name === 'Operations') await s.load(false, 2); else await s.load(2);
    assert.equal(s.page, 1); assert.equal(s.pendingPage, 2);
    assert.equal(s.items[0].id, 'page-1'); assert.ok(s.error);
    await s.load();
    assert.equal(s.page, 2); assert.equal(s.items[0].id, 'page-2');
    assert.equal(calls.join(','), '1,2,2');
  });
  test(`${name}: failed new filter cannot relabel the previous page as page one`, async () => {
    let failed = false;
    const { state: s } = page(name, { get: async (url, config) => {
      if (failed) throw new Error('offline');
      return response(`page-${config.params.page}`);
    } });
    if (name === 'ComponentHistory') { s.device = { id: 'd1' }; s.component = 'manager'; }
    if (name === 'Operations') await s.load(false, 3); else await s.load(3);
    failed = true; s.search = 'other version';
    if (name === 'Operations') await s.load(false, 1); else await s.load(1);
    assert.equal(s.page, 3); assert.equal(s.pendingPage, 1);
    assert.equal(s.items[0].id, 'page-3'); assert.ok(s.error);
  });
}

test('package late page result cannot replace the newer confirmed selection', async () => {
  const late = deferred();
  const { state: s } = page('Packages', { get: async (url, { params }) => (
    params.page === 2 ? late.promise : response('third-page')) });
  const pending = s.load(2); await s.load(3);
  late.resolve(response('late-page')); await pending;
  assert.equal(s.page, 3); assert.equal(s.items[0].id, 'third-page');
});

test('file choice validates extension, empty file and exact upload size boundary before POST', async () => {
  let posts = 0;
  const { state: s } = page('Packages', { post: async () => { posts += 1; } });
  for (const file of [{ name: 'wrong.zip', size: 1 }, { name: 'empty.rcd.tar.gz', size: 0 },
    { name: 'large.rcd.tar.gz', size: 256 * 1024 * 1024 + 1 }]) {
    s.file = file; assert.ok(s.fileError); await s.upload();
  }
  assert.equal(posts, 0);
  s.file = { name: 'maximum.rcd.tar.gz', size: 256 * 1024 * 1024 };
  assert.equal(s.fileError, '');
});

test('multiple-file drop clears the old valid selection rather than uploading the wrong old package', () => {
  const { state: s } = page('Packages');
  s.file = { name: 'old.rcd.tar.gz', size: 10 };
  s.dropFile({ dataTransfer: { files: [{}, {}] } });
  assert.equal(s.file, null); assert.match(s.uploadError, /一个软件包/);
});

function readyWizard(axios = {}) {
  const result = page('ReleaseWizard', axios); const s = result.state;
  s.packages = [{ id: 'p1', revision: 1, manifest: { component: 'lower', version: '1' } }];
  s.packageId = 'p1'; s.paused = true; s.step = 2;
  s.devices = [{ id: 'd1', address: '192.168.10.30', revision: 3, state: 'checked',
    last_success_at: new Date(s.now).toISOString(), release_template_id: 1,
    components: [{ component: 'lower', state: 'running' }] }];
  s.selectedIds = ['d1']; s.compatibility = { d1: '' };
  return result;
}
test('release confirmation expires by the local clock without HTTP writes', async () => {
  let posts = 0;
  const { state: s, timers } = readyWizard({ post: async () => { posts += 1; } });
  assert.equal(s.disabledReason(s.devices[0]), '');
  s.updateClock(); assert.ok(timers.some((t) => t.ms === 1000));
  s.now += 181000;
  assert.match(s.disabledReason(s.devices[0]), /过期/);
  await s.submit(); assert.equal(posts, 0);
});

test('one expired selected target blocks the whole confirmation rather than silently dropping it', async () => {
  let posts = 0;
  const { state: s } = readyWizard({ post: async () => { posts += 1; } });
  s.devices.push({ ...s.devices[0], id: 'd2', last_success_at: new Date(s.now - 181000).toISOString() });
  s.compatibility.d2 = ''; s.selectedIds.push('d2');
  assert.equal(s.selectedDevices.length, 1);
  await s.submit(); assert.equal(posts, 0);
});

test('expired evidence does not prevent resolving an already ambiguous request using its original identity', async () => {
  const calls = [];
  const { state: s } = readyWizard({ post: async (url, body, config) => {
    calls.push({ body: JSON.stringify(body), key: config.headers['Idempotency-Key'] });
    if (calls.length === 1) throw new Error('reply lost');
    return { data: { id: 'original-op' } };
  } });
  await s.submit(); assert.ok(s.pendingRequest);
  s.now += 181000; await s.submit();
  assert.equal(calls.length, 2); assert.equal(calls[0].body, calls[1].body);
  assert.equal(calls[0].key, calls[1].key); assert.equal(s.pendingRequest, null);
});

test('registered operation error keeps original request locked and exposes only a valid original identifier', async () => {
  const id = 'a'.repeat(64);
  const { state: s } = readyWizard({ post: async () => {
    throw { response: { status: 409, data: { error: 'registered, needs confirmation', operation_id: id } } };
  } });
  await s.submit();
  assert.equal(s.knownOperationId, id); assert.ok(s.pendingRequest); assert.ok(s.requestKey);
  assert.equal(s.step, 2);
});

test('late release rejection does not unlock a newly opened context or overwrite its message', async () => {
  const pending = deferred();
  const { state: s } = readyWizard({ post: () => pending.promise });
  const request = s.submit();
  s.generation += 1; s.pendingRequest = { marker: 'new-original' }; s.busy = true; s.error = 'new context';
  pending.reject({ response: { status: 409, data: { error: 'late old context' } } }); await request;
  assert.equal(s.pendingRequest.marker, 'new-original'); assert.equal(s.busy, true);
  assert.equal(s.error, 'new context');
});

test('service control registered-error preserves original request and idempotency key', async () => {
  const id = 'b'.repeat(64);
  const { state: s } = page('ServiceControl', { post: async () => {
    throw { response: { status: 409, data: { error: 'registered', operation_id: id } } };
  } });
  s.device = { id: 'd1', revision: 3, control_template_id: 1, state: 'checked',
    last_success_at: new Date(s.now).toISOString(), components: [{ component: 'manager', state: 'running' }] };
  s.allowed = true; s.component = 'manager'; s.open('restart'); s.paused = true;
  await s.submit();
  assert.equal(s.knownOperationId, id); assert.ok(s.pending); assert.ok(s.key);
});

function queuedOperation() {
  return { id: 'op1', revision: 7, state: 'queued', request: { action: 'upgrade', component: 'manager' },
    package: { component: 'manager', version: '1' }, targets: [{ name: 'RCD 30', address: '192.168.10.30' }] };
}
test('opening and declining task cancellation does not POST', () => {
  let posts = 0;
  const { state: s } = page('Operations', { post: async () => { posts += 1; } });
  s.selected = queuedOperation(); s.openCancel(); assert.equal(s.cancelTarget.id, 'op1');
  s.closeCancel(false); assert.equal(s.cancelTarget, null); assert.equal(posts, 0);
});

test('dependent cancellation dialog cannot dismiss its task drawer and declining retains original details', () => {
  const { state: s, source } = page('Operations');
  s.selected = queuedOperation(); s.openCancel();
  s.closeDetails(false);
  assert.equal(s.selected.id, 'op1'); assert.equal(s.cancelTarget.id, 'op1');
  s.closeCancel(false);
  assert.equal(s.selected.id, 'op1'); assert.equal(s.cancelTarget, null);
  // Reopening cancellation and then normal explicit drawer closing must work.
  s.openCancel(); assert.equal(s.cancelTarget.id, 'op1'); s.closeCancel(false);
  s.closeDetails(false); assert.equal(s.selected, null);
  // Vuetify discovers open dependent overlays through its component descendants.
  const drawerStart = source.indexOf('<v-navigation-drawer');
  const dialogStart = source.indexOf('<v-dialog :value="!!cancelTarget"');
  const dialogEnd = source.indexOf('</v-dialog>', dialogStart);
  const drawerEnd = source.indexOf('</v-navigation-drawer>', drawerStart);
  assert.ok(drawerStart < dialogStart && dialogEnd < drawerEnd);
  assert.match(source.slice(drawerStart, dialogStart), /:stateless="!!cancelTarget \|\| cancelling"/);
});

test('confirmation never cancels a changed task or changed revision', async () => {
  let posts = 0;
  const { state: s } = page('Operations', { post: async () => { posts += 1; } });
  for (const updated of [{ ...queuedOperation(), state: 'running' },
    { ...queuedOperation(), revision: 8 }, { ...queuedOperation(), id: 'op2' }]) {
    s.selected = queuedOperation(); s.openCancel(); s.selected = updated;
    await s.cancelOperation(); assert.match(s.cancelError, /状态已变化/);
  }
  assert.equal(posts, 0);
});

test('confirmed cancellation submits only its pinned operation and revision', async () => {
  const calls = [];
  const { state: s } = page('Operations', { post: async (url, body, config) => {
    calls.push({ url, revision: config.headers['If-Match'] });
    return { data: { ...queuedOperation(), state: 'cancelled' } };
  } });
  s.load = async () => {}; s.selected = queuedOperation(); s.openCancel();
  await s.cancelOperation();
  assert.equal(calls[0].url, '/api/project/1/rcd/operations/op1/cancel');
  assert.equal(calls[0].revision, '"7"'); assert.equal(s.cancelTarget, null);
});

test('late cancellation result cannot overwrite a different project or clear its request state', async () => {
  const pending = deferred();
  const { state: s, definition } = page('Operations', { post: () => pending.promise });
  s.selected = queuedOperation(); s.openCancel(); const request = s.cancelOperation();
  s.projectId = 2; s.loadApplications = () => {}; s.searchNow = () => {};
  definition.watch.projectId.call(s); s.selected = { ...queuedOperation(), id: 'new-op' };
  s.cancelling = true;
  pending.resolve({ data: { ...queuedOperation(), state: 'cancelled' } }); await request;
  assert.equal(s.selected.id, 'new-op'); assert.equal(s.cancelling, true);
});
