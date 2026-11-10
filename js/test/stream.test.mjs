// The event stream: the client reconnects when it breaks, says so, and stops
// when the node refuses it. A small server stands in for the node's /stream.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { Ibc, addressOf, newP256Key, p256Signer } from '../dist/index.js';

/** A fake node: behave(n, res) answers the n-th stream request. */
async function fakeNode(behave) {
  let n = 0;
  const server = http.createServer((req, res) => {
    if (!req.url.startsWith('/stream')) return res.writeHead(404).end();
    behave(++n, res);
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const open = new Set();
  server.on('connection', (socket) => { open.add(socket); socket.on('close', () => open.delete(socket)); });
  return {
    url: `http://127.0.0.1:${server.address().port}`,
    count: () => n,
    close: () => { for (const s of open) s.destroy(); return new Promise((resolve) => server.close(resolve)); },
  };
}

const sse = (res) => res.writeHead(200, { 'Content-Type': 'text/event-stream' });

async function client(url) {
  const key = await newP256Key();
  const ibc = new Ibc(url, addressOf(key.publicKey));
  ibc.root = p256Signer(key); // signs the stream's query silently
  ibc.reconnectDelayMs = 20;
  const statuses = [];
  ibc.onStatus((status) => statuses.push(status));
  return { ibc, statuses };
}

const until = async (cond, ms = 3000) => {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error('timed out');
    await new Promise((r) => { setTimeout(r, 10); });
  }
};

test('reconnects when the node closes the stream, and reports it', async () => {
  const node = await fakeNode((n, res) => {
    sse(res);
    res.write('data: \n\n');
    if (n === 1) {
      res.write(`data: ${JSON.stringify({ request: 'a', contract: null, action: 'x', reply: 1 })}\n\n`);
      res.end(); // the first connection breaks
    }
  });
  const { ibc, statuses } = await client(node.url);
  const events = [];
  ibc.onEvent((e) => events.push(e.request));
  await ibc.connect();
  await until(() => node.count() === 2 && ibc.streamStatus === 'open');
  assert.deepEqual(events, ['a']);
  assert.deepEqual(statuses, ['connecting', 'open', 'reconnecting', 'open']);
  ibc.close();
  assert.equal(ibc.streamStatus, 'closed');
  await node.close();
});

test('reconnects when the stream goes silent', async () => {
  const node = await fakeNode((n, res) => {
    sse(res);
    res.write('data: \n\n'); // then nothing, not even keep-alives
  });
  const { ibc, statuses } = await client(node.url);
  ibc.streamTimeoutMs = 100;
  await ibc.connect();
  await until(() => node.count() >= 2 && ibc.streamStatus === 'open');
  assert.deepEqual(statuses.slice(0, 4), ['connecting', 'open', 'reconnecting', 'open']);
  ibc.close();
  await node.close();
});

test('waits for the node while it is down', async () => {
  const node = await fakeNode((n, res) => {
    if (n < 3) return res.writeHead(503).end('{"message":"starting"}'); // not up yet
    sse(res);
    res.write('data: \n\n');
  });
  const { ibc, statuses } = await client(node.url);
  await ibc.connect();
  assert.equal(node.count(), 3);
  assert.deepEqual(statuses, ['connecting', 'reconnecting', 'reconnecting', 'open']);
  ibc.close();
  await node.close();
});

test('stops when the node refuses the stream', async () => {
  const node = await fakeNode((n, res) => {
    res.writeHead(401, { 'Content-Type': 'application/json' }).end('{"message":"this key is not agent 0x…\'s"}');
  });
  const { ibc, statuses } = await client(node.url);
  await assert.rejects(ibc.connect(), /this key is not agent/);
  assert.deepEqual(statuses, ['connecting', 'closed']);
  await new Promise((r) => { setTimeout(r, 100); });
  assert.equal(node.count(), 1, 'no retry after a refusal');
  await node.close();
});

test('close() stops reconnecting', async () => {
  const node = await fakeNode((n, res) => res.writeHead(503).end('{}'));
  const { ibc, statuses } = await client(node.url);
  const opened = ibc.connect();
  await until(() => statuses.includes('reconnecting'));
  ibc.close();
  await assert.rejects(opened, /closed/);
  const count = node.count();
  await new Promise((r) => { setTimeout(r, 100); });
  assert.equal(node.count(), count);
  assert.equal(statuses.at(-1), 'closed');
  await node.close();
});
