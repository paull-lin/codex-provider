// Operator-run public URL checks. Uses the container's existing API key without
// printing it. Responses + Messages perform two real, billable inference calls.
const assert = require('node:assert/strict');
const https = require('node:https');
const crypto = require('node:crypto');
const origin = process.env.CODEX_SMOKE_ORIGIN;
assert.ok(origin && new URL(origin).protocol === 'https:', 'set CODEX_SMOKE_ORIGIN to the public HTTPS origin');
const key = process.env.CODEX_POOL_API_KEY;
assert.ok(key, 'CODEX_POOL_API_KEY is required');
const headers = {Authorization: `Bearer ${key}`, 'Content-Type': 'application/json'};
async function request(path, options = {}) {
  const response = await fetch(origin + path, {...options, signal: AbortSignal.timeout(120000)});
  assert.equal(response.status, 200, `${path}: HTTP ${response.status}`);
  return response;
}
(async () => {
  await request('/healthz', {headers});
  const page = await request('/admin');
  assert.match(await page.text(), /password/i);
  await request('/');
  await request('/admin/assets/app.js');
  await request('/admin/api/public-dashboard');
  console.log('PASS public health, admin login page, root, assets and dashboard');
  const responses = await request('/v1/responses', {method: 'POST', headers, body: JSON.stringify({
    model: process.env.CODEX_SMOKE_MODEL || 'gpt-6.1-sol', stream: true, store: false,
    instructions: 'Reply with OK only.', input: [{role: 'user', content: [{type: 'input_text', text: 'ping'}]}],
  })});
  const events = (await responses.text()).split('\n').filter(line => line.startsWith('data: {')).map(line => JSON.parse(line.slice(6)));
  assert.ok(events.some(event => event.type === 'response.completed'), 'Responses did not complete');
  assert.ok(!events.some(event => ['error', 'response.failed'].includes(event.type)), 'Responses error');
  console.log('PASS public Codex POST /v1/responses completed');
  await new Promise((resolve, reject) => {
    const nonce = crypto.randomBytes(16).toString('base64');
    const req = https.request(new URL('/v1/responses', origin), {headers: {
      Authorization: `Bearer ${key}`, Connection: 'Upgrade', Upgrade: 'websocket',
      'Sec-WebSocket-Version': '13', 'Sec-WebSocket-Key': nonce,
      'OpenAI-Beta': 'responses_websockets=2026-02-06',
    }});
    const timer = setTimeout(() => req.destroy(new Error('WS handshake timeout')), 30000);
    req.on('error', error => { clearTimeout(timer); reject(error); });
    req.on('response', response => { clearTimeout(timer); response.resume(); reject(new Error(`WS HTTP ${response.statusCode}`)); });
    req.on('upgrade', (response, socket) => {
      clearTimeout(timer);
      try {
        assert.equal(response.statusCode, 101);
        assert.equal(response.headers['sec-websocket-accept'], crypto.createHash('sha1').update(nonce + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64'));
        resolve();
      } catch (error) { reject(error); }
      socket.destroy();
    });
    req.end();
  });
  console.log('PASS public WebSocket GET /v1/responses 101 handshake');
  const messages = await request('/v1/messages', {method: 'POST', headers, body: JSON.stringify({
    model: 'claude-sonnet-4-6', max_tokens: 16, messages: [{role: 'user', content: 'Reply with OK only.'}],
  })});
  const message = await messages.json();
  assert.equal(message.type, 'message');
  assert.ok(message.content.some(block => block.type === 'text' && block.text));
  console.log('PASS public Claude POST /v1/messages completed');
})().catch(error => { console.error(error.message); process.exitCode = 1; });
