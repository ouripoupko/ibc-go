// Deploys the example contracts and calls them, with the client in ibc.mjs.
// In a browser you would sign with a browser wallet; here an ethers.js wallet signs.
//
//   cd examples/js && npm install          # once: installs ethers
//   node demo.mjs [node-url]
//
// The default node is http://localhost:2611. Run the node first (see the
// README). Set IBC_PRIVATE_KEY to use a fixed key; otherwise a new one is
// made for each run.
import { readFile } from 'node:fs/promises';
import { Wallet } from 'ethers';
import { Ibc } from './ibc.mjs';

const url = process.argv[2] ?? 'http://localhost:2611';
const wallet = process.env.IBC_PRIVATE_KEY ? new Wallet(process.env.IBC_PRIVATE_KEY) : Wallet.createRandom();
const build = new URL('../build/', import.meta.url);

async function load(name) {
  return {
    name,
    code: await readFile(new URL(`${name}.bin`, build), 'utf8'),
    abi: await readFile(new URL(`${name}.abi`, build), 'utf8'),
  };
}

const ibc = await Ibc.connect(url, wallet);
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
