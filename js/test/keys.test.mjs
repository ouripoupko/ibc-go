// Checks that the JavaScript client computes the same digests and addresses
// as the node. The vectors come from Go (identity/vectors_test.go).
//
//   cd js && npm test
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { digestOf, addressOf, fromHex, toHex, newP256Key, p256Signer, sign, signBatch } from '../dist/keys.js';

const vectors = JSON.parse(await readFile(new URL('../../identity/testdata/vectors.json', import.meta.url), 'utf8'));

test('EIP-712 digests match the node', () => {
  for (const { type, message, digest } of vectors.messages) {
    assert.equal(toHex(digestOf(type, message)), digest, type);
  }
});

test('agent addresses match the node, for secp256k1 and P-256 keys', () => {
  for (const { publicKey, address } of vectors.keys) {
    assert.equal(addressOf(fromHex(publicKey)), address);
  }
});

test('a P-256 signer makes envelopes the node can read', async () => {
  const key = await newP256Key();
  const signer = p256Signer(key);
  const message = vectors.messages.find((v) => v.type === 'Query').message;
  const { envelope } = await sign(signer, 'Query', message);
  assert.equal(envelope.scheme, 'p256');
  assert.equal(fromHex(envelope.sig).length, 64);
  assert.equal(envelope.key, toHex(key.publicKey));
  const batch = await signBatch(signer, [{ type: 'Query', message }, { type: 'Query', message: { ...message, nonce: 'x' } }]);
  assert.equal(batch.length, 2);
  assert.equal(batch[0].batch.length, 2);
  assert.equal(batch[0].sig, batch[1].sig);
});
