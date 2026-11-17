// The node marks a call the contract failed (a revert, a wrong argument, an
// unknown function): an event's error, a read's { error }. The client throws
// on both, and keeps string results. A small server stands in for the node.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { Ibc, addressOf, newP256Key, p256Signer } from '../dist/index.js';

async function fakeNode() {
  const streams = new Set();
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://x');
    if (url.pathname === '/stream') {
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      res.write('data: \n\n');
      streams.add(res);
      return;
    }
    let body = '';
    req.on('data', (c) => { body += c; });
    req.on('end', () => {
      const method = url.pathname.split('/').at(-1);
      const json = (v) => res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify(v));
      if (url.searchParams.get('action') === 'contract_read') {
        return json(method === 'bad' ? { error: 'bad: unexpected argument "x"' } : 'a string result');
      }
      // contract_write: the id now, the outcome on the stream.
      const id = `0x${method}`;
      json(id);
      const event = method === 'bad'
        ? { request: id, contract: '0xc', action: 'contract_write', reply: 'unknown method "bad"', error: 'unknown method "bad"' }
        : { request: id, contract: '0xc', action: 'contract_write', reply: 'a string result' };
      setTimeout(() => { for (const s of streams) s.write(`data: ${JSON.stringify(event)}\n\n`); }, 20);
    });
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  return {
    url: `http://127.0.0.1:${server.address().port}`,
    close: () => { for (const s of streams) s.destroy(); server.closeAllConnections(); return new Promise((r) => server.close(r)); },
  };
}

test('a failed call throws; a string result does not', async () => {
  const node = await fakeNode();
  const key = await newP256Key();
  const ibc = new Ibc(node.url, addressOf(key.publicKey));
  ibc.root = p256Signer(key);
  assert.equal(await ibc.read('0xc', 'good'), 'a string result');
  await assert.rejects(ibc.read('0xc', 'bad'), /unexpected argument/);
  assert.equal(await ibc.write('0xc', 'good'), 'a string result');
  await assert.rejects(ibc.write('0xc', 'bad'), /unknown method/);
  ibc.close();
  await node.close();
});
