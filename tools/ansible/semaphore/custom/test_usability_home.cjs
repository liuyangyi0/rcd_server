// Run only on 46. Pure Vue lifecycle regressions; no browser, SSH or task writes.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function deferred() {
  let resolve;
  const promise = new Promise((accept) => { resolve = accept; });
  return { promise, resolve };
}
function home(get) {
  const source = fs.readFileSync(path.join(__dirname, 'files/web/src/views/rcd/Home.vue'), 'utf8');
  const code = source.match(/<script>([\s\S]*?)<\/script>/)[1]
    .replace(/^import .*;\r?$/gm, '').replace('export default', 'module.exports =');
  const context = { module: { exports: {} }, axios: { get } };
  vm.runInNewContext(code, context);
  const definition = context.module.exports;
  const routes = [];
  const state = { ...definition.data(), $route: { params: { projectId: '1' } },
    $router: { replace: async (route) => { routes.push(route); } } };
  for (const [key, getValue] of Object.entries(definition.computed)) {
    Object.defineProperty(state, key, { get: getValue.bind(state) });
  }
  for (const [key, method] of Object.entries(definition.methods)) state[key] = method.bind(state);
  return { state, definition, routes };
}

test('opening the project supports retry after a failed read without submitting a job', async () => {
  let calls = 0;
  const { state, routes } = home(async (url) => {
    assert.equal(url, '/api/info');
    if (++calls === 1) throw new Error('network disconnected');
    return { data: { rcd_packages: true } };
  });
  await state.openProject();
  assert.match(state.error, /重新读取/); assert.equal(state.busy, false);
  await state.openProject();
  assert.equal(state.error, ''); assert.deepEqual(routes, ['/project/1/rcd/devices']);
});

test('an expired login is distinguished from a generic read failure', async () => {
  const { state } = home(async () => { throw { response: { status: 401 } }; });
  await state.openProject();
  assert.equal(state.loginExpired, true); assert.match(state.error, /重新登录/);
});

test('an unmounted home cannot pull the user back after a late read', async () => {
  const response = deferred(); const { state, definition, routes } = home(() => response.promise);
  const request = state.openProject(); definition.beforeDestroy.call(state);
  response.resolve({ data: { rcd_packages: true } }); await request;
  assert.deepEqual(routes, []);
});

test('a previous project response cannot replace a newer destination', async () => {
  const previous = deferred(); const current = deferred(); let calls = 0;
  const { state, routes } = home(() => (++calls === 1 ? previous.promise : current.promise));
  const oldRequest = state.openProject(); state.$route.params.projectId = '2';
  const newRequest = state.openProject(); previous.resolve({ data: { rcd_packages: false } });
  await oldRequest; assert.deepEqual(routes, []); assert.equal(state.busy, true);
  current.resolve({ data: { rcd_packages: true } }); await newRequest;
  assert.deepEqual(routes, ['/project/2/rcd/devices']);
});

test('the standard native history remains available when the extension is disabled', async () => {
  const { state, routes } = home(async () => ({ data: { rcd_packages: false } }));
  await state.openProject(); assert.deepEqual(routes, ['/project/1/history']);
});
