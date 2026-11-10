// Deploys the example contracts and calls them, with the ibc client package
// (@ouripoupko/ibc). In a browser, the root key is a passkey; here, a
// WebCrypto P-256 key stands in for it, and signs everything.
//
//   npm install          # the package, from ../../js
//   node demo.mjs [node-url]
//
// The default node is http://localhost:2611. Run the node first (see the
// README). Each run makes a new agent.
import { readFile } from 'node:fs/promises';
import { Ibc, addressOf, newP256Key, p256Signer } from '@ouripoupko/ibc';

const url = process.argv[2] ?? 'http://localhost:2611';
const build = new URL('../build/', import.meta.url);

const rootKey = await newP256Key();
const agent = addressOf(rootKey.publicKey);
const root = p256Signer(rootKey);

async function load(name) {
  return {
    name,
    code: await readFile(new URL(`${name}.bin`, build), 'utf8'),
    abi: await readFile(new URL(`${name}.abi`, build), 'utf8'),
  };
}

const ibc = await Ibc.connect(url, { agent, root });
try {
  console.log('agent', ibc.agent, 'registered:', await ibc.register());

  const counter = await ibc.deploy({ ...(await load('Counter')), constructor: { start: 10 } });
  console.log('Counter', counter);
  console.log('  increment(5) ->', await ibc.write(counter, 'increment', { amount: 5 }));
  try {
    await ibc.write(counter, 'increment', { amount: 0 });
  } catch (err) {
    console.log('  increment(0) ->', err.message);
  }
  console.log('  count ->', await ibc.read(counter, 'count'));

  const poll = await ibc.deploy({
    ...(await load('Poll')),
    constructor: { text: 'Lunch?', choices: ['pizza', 'salad', 'soup'] },
  });
  console.log('Poll', poll);
  console.log('  vote(1) ->', await ibc.write(poll, 'vote', { option: 1 }));
  console.log('  results ->', JSON.stringify(await ibc.read(poll, 'results')));

  const board = await ibc.deploy(await load('Board'));
  console.log('Board', board);
  console.log('  post ->', await ibc.write(board, 'post', { text: 'hello from JavaScript' }));
  console.log('  getPost(0) ->', await ibc.read(board, 'getPost', { id: 0 }));
  console.log('  whoAmI ->', await ibc.read(board, 'whoAmI'));
} finally {
  ibc.close();
}
