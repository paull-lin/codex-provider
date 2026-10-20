const assert = require('node:assert/strict');
const base = 'http://gateway:9100';
const mode = process.argv[2];
const expected = process.argv[3] || 'connection-main';
async function request(path, options = {}) {
  return fetch(base + path, {...options, signal: AbortSignal.timeout(10000)});
}
async function route(path, backend, method = 'GET') {
  const response = await request(path, {method});
  assert.equal(response.headers.get('x-test-backend'), backend, `${method} ${path}`);
  console.log(`PASS ${method} ${path} -> ${backend}`);
}
async function ws(path) {
  await new Promise((resolve, reject) => {
    const socket = new WebSocket(base.replace('http:', 'ws:') + path);
    const timer = setTimeout(() => { socket.close(); reject(new Error('WS timeout')); }, 10000);
    socket.onopen = () => socket.send('ping');
    socket.onerror = reject;
    socket.onmessage = event => {
      clearTimeout(timer);
      try { assert.equal(event.data, `${expected}:ping`); resolve(); } catch (e) { reject(e); }
      socket.close();
    };
  });
  console.log(`PASS WS bidirectional ${path} -> ${expected}`);
}
(async () => {
  if (mode === 'selected') {
    await route('/v1/models', expected);
    return;
  }
  if (mode === 'mock') {
    for (const path of ['/v1/models', '/v1/responses']) await route(path, expected);
    for (const path of ['/v1/responses', '/v1/responses/compact', '/v1/chat/completions']) await route(path, expected, 'POST');
    for (const path of ['responses', 'responses/compact', 'models', 'images/generations', 'images/edits', 'memories/trace_summarize', 'alpha/search', 'realtime/calls']) {
      await route('/k/test-key/backend-api/codex/' + path, expected, path === 'models' ? 'GET' : 'POST');
    }
    await route('/v1/models?test=query', expected);
    for (const path of ['/', '/admin', '/admin/assets/app.js', '/admin/assets/app.css', '/admin/assets/logo.svg', '/admin/manifest.webmanifest', '/admin/api/public-dashboard', '/admin/api/state', '/healthz', '/internal/state', '/unknown', '/v1/unknown', '/v1/codex-pool/status', '/v1/responses/extra', '/v1/messages/extra', '/k/test-key/backend-api/codex/unknown']) await route(path, 'control');
    await route('/admin/api/login', 'control', 'POST');
    for (const status of [429, 500, 503]) {
      const response = await request('/v1/responses', {method: 'POST', headers: {'X-Test-Status': String(status)}});
      assert.equal(response.status, status);
      assert.equal(await response.text(), expected);
    }
    console.log('PASS HTTP 429/500/503 are not replayed on backup');
    await ws('/v1/responses');
    await ws('/k/test-key/backend-api/codex/responses');
  } else {
    for (const path of ['/', '/admin', '/admin/assets/app.js', '/admin/assets/app.css', '/admin/api/public-dashboard']) {
      const response = await request(path);
      assert.equal(response.status, 200, path);
      if (path === '/admin') assert.match(await response.text(), /password/i);
    }
    const login = await request('/admin/api/login', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({password: 'throwaway-test-password'})});
    assert.equal(login.status, 200);
    assert.ok(login.headers.get('set-cookie'));
    const state = await request('/admin/api/state', {headers: {Cookie: login.headers.get('set-cookie').split(';')[0]}});
    assert.equal(state.status, 200);
    assert.equal((await request('/healthz')).status, 401);
    assert.equal((await request('/healthz', {headers: {Authorization: 'Bearer throwaway-test-key'}})).status, 200);
    assert.equal((await request('/internal/state')).status, 404);
    assert.equal((await request('/v1/models', {headers: {Authorization: 'Bearer throwaway-test-key'}})).status, 200);
    console.log('PASS real control UI/assets/dashboard/login/session, health auth, broker isolation, connection models');
  }
  const headers = {'Content-Type': 'application/json', 'x-api-key': mode === 'mock' ? 'throwaway-test-key' : 'invalid-test-key'};
  const body = JSON.stringify({model: 'claude-sonnet-4-6', max_tokens: 16, messages: [{role: 'user', content: 'ping'}]});
  const messages = await request('/v1/messages', {method: 'POST', headers, body});
  assert.equal(messages.status, mode === 'mock' ? 200 : 401);
  const result = await messages.json();
  if (mode === 'mock') assert.equal(result.content[0].text, 'connection-backup');
  else assert.equal(result.type, 'error');
  const count = await request('/v1/messages/count_tokens', {method: 'POST', headers, body});
  assert.equal(count.status, 200);
  assert.ok((await count.json()).input_tokens > 0);
  console.log(`PASS adapter messages (${messages.status}) and count_tokens; ${mode === 'mock' ? 'inference forwarded to backup' : 'real backup rejects invalid key'}`);
})().then(() => process.exit(0)).catch(error => { console.error(error); process.exit(1); });
