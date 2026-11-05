// Deploys the example contracts and calls them, with the client in ibc.mjs.
//
//   node examples/js/demo.mjs [node-url] [agent]
//
// Defaults: http://localhost:2611 and a fresh agent id. Run the node first
// (see the README) and compile the examples (examples/scripts/compile.sh).
import { readFile } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import { Ibc } from './ibc.mjs';

const url = process.argv[2] ?? 'http://localhost:2611';
const agent = process.argv[3] ?? `demo-${randomUUID()}`;
const build = new URL('../build/', import.meta.url);

async function load(name) {
  return {
    name,
    code: await readFile(new URL(`${name}.bin`, build), 'utf8'),
    abi: await readFile(new URL(`${name}.abi`, build), 'utf8'),
  };
}

const ibc = new Ibc(url, agent);
try {
  console.log('agent', agent, 'registered:', await ibc.register());

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
