// Function signatures and lookup in a source's ABIs, as the node does them
// (common.ABISet).
//
//   cd js && npm test
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { signatureOf, functionOf, functionsOf } from '../dist/ibc.js';
import { selectorOf } from '../dist/keys.js';

const fn = (name, inputs, extra = {}) => ({ type: 'function', name, inputs, outputs: [], stateMutability: 'nonpayable', ...extra });

test('signatures use canonical types, with tuples written out', () => {
  assert.equal(signatureOf(fn('pay', [{ name: 'to', type: 'address' }, { name: 'n', type: 'uint256' }])), 'pay(address,uint256)');
  assert.equal(signatureOf(fn('f', [{ name: 'xs', type: 'tuple[]', components: [{ name: 'a', type: 'uint8' }, { name: 'b', type: 'bytes' }] }])),
    'f((uint8,bytes)[])');
  assert.equal(selectorOf('transfer(address,uint256)'), '0xa9059cbb');
});

test('a function of the source, by name or signature', () => {
  const abis = {
    'A.sol:A': [fn('get', []), fn('set', [{ name: 'v', type: 'uint256' }])],
    'B.sol:B': [fn('get', []), fn('set', [{ name: 'v', type: 'bytes32' }])],
  };
  assert.deepEqual(functionOf(abis, 'get').contracts, ['A.sol:A', 'B.sol:B']);
  assert.equal(functionsOf(abis).length, 3);
  assert.throws(() => functionOf(abis, 'set'), /by its signature/);
  assert.deepEqual(functionOf(abis, 'set(bytes32)').contracts, ['B.sol:B']);
  assert.throws(() => functionOf(abis, 'nope'), /unknown method "nope"/);
});
