// Gateway tests replace pool nodes, never the upstream provider. SSE shapes
// mirror adapters/anthropic/adapter_test.go mixedSSE/completed fixtures.
const http = require('node:http');
const crypto = require('node:crypto');
const backend = process.env.TEST_BACKEND;
const server = http.createServer(async (req, res) => {
  let raw = '';
  for await (const chunk of req) raw += chunk;
  res.setHeader('X-Test-Backend', backend);
  if (req.headers['x-test-status']) {
    res.writeHead(Number(req.headers['x-test-status']));
    return res.end(backend);
  }
  if (req.url === '/v1/responses' && raw && JSON.parse(raw).model) {
    res.writeHead(200, {'Content-Type': 'text/event-stream'});
    const events = [
      {type: 'response.created', response: {id: 'resp_test'}},
      {type: 'response.output_item.added', output_index: 0, item: {type: 'message', id: 'm_test'}},
      {type: 'response.output_text.delta', output_index: 0, delta: backend},
      {type: 'response.output_item.done', output_index: 0, item: {type: 'message', id: 'm_test', content: [{type: 'output_text', text: backend}]}},
      {type: 'response.completed', response: {id: 'resp_test', usage: {input_tokens: 1, output_tokens: 1}}},
    ];
    return res.end(events.map(event => `data: ${JSON.stringify(event)}\n\n`).join(''));
  }
  res.end(JSON.stringify({backend, path: req.url}));
});
server.on('upgrade', (req, socket) => {
  const accept = crypto.createHash('sha1').update(req.headers['sec-websocket-key'] + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64');
  socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
  socket.once('data', frame => {
    const len = frame[1] & 127;
    const message = Buffer.from(frame.subarray(6, 6 + len));
    for (let i = 0; i < message.length; i++) message[i] ^= frame[2 + i % 4];
    const reply = Buffer.from(`${backend}:${message}`);
    socket.write(Buffer.concat([Buffer.from([0x81, reply.length]), reply]));
  });
  socket.on('error', () => {});
});
server.listen(8317, '0.0.0.0');
process.on('SIGTERM', () => process.exit(0));
