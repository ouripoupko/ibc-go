# @ouripoupko/ibc

The JavaScript client of [ibc](https://github.com/ouripoupko/ibc-go) nodes, for apps and tools.
It works in browsers and in Node.js 20 and later, from JavaScript or TypeScript (types included),
and has no dependencies.

```sh
npm install @ouripoupko/ibc
```

On ibc, every user has their own node, and signs with a passkey that only their node's site can use.
An app never holds the user's keys. It has a key of its own, kept in the browser, and works through
the user's node:

- **The user approves on their node.** When the app needs the user, it opens the node's approval
  page in a popup, where the user signs in (or creates an identity) and approves with their passkey.
- **The user decides what the app may do without asking:** read, and have the node call some
  functions, per contract source. Everything else, the app asks each time.

## An app: `IbcApp`

```js
import { IbcApp } from '@ouripoupko/ibc';

const app = await IbcApp.open('https://node.example');   // the user's node
// app.agent: the user's agent id, once the user approved the app; null before.

// From a click (browsers open the approval page only from a click):
const agent = await app.connect();      // who the user is: they sign in, or create an identity
const source = await (await fetch('/Chat.sol')).text();
const chat = await app.deploy({ source, constructor: { initialTopic: 'lunch' } });
// The user approves on their node: the deploy, and what the app may do without asking.

await app.write(chat, 'post', { text: 'hi' });             // silent, if the user allowed "post"
await app.read(chat, 'topic');                             // silent, if the user allowed reading
await app.write(chat, 'setTopic', { newTopic: 'dinner' }); // opens the node: the user approves
app.onEvent((event) => console.log(event));                 // events of the app's contracts
```

`read` and `write` try silently first, and open the approval page when the node says the user must
approve. Also: `app.permit(contract)` asks for permissions on an existing contract, and
`app.join(contract, memberNode, memberAgent, { source, guest })` asks the user to join one, as a
partner or as a guest.

## Joining

The app sends the contract's source with the join. The user reads it on their node, and approves
the join, the app's permissions and the public choice at once. The join names the source's hash:
the members refuse it if it is not the contract's. The approval window closes as soon as the join is
sent, and `join` resolves with its id; the outcome arrives later, as an event:

```js
const source = await (await fetch('/Chat.sol')).text();     // byte for byte the deployed source
const id = await app.join(chat, memberNode, memberAgent, { source });
app.onEvent((event) => {
  if (event.action !== 'join_contract' || event.request !== id) return;
  // event.reply: true once the user's node holds the contract.
  // event.source_check: 'verified' (the source compiles to the deployed code), 'unchecked' (the
  // node could not compile it: event.reason says why), or 'mismatch' (it did not join).
});
```

Open the event stream (`onEvent`) before the join, or read the contract again after: events sent
while the stream is closed are lost. `Ibc.join` (for tools) works the same, with `{ source }`.

## Public contracts

A user may let anyone read their copy of a contract, unsigned: its `view` and `pure` functions run
with `msg.sender` the zero address, so the contract decides what an unknown reader sees. The app
proposes it, and the user decides on the approval page:

```js
const board = await app.deploy({ source, public: true });   // the box starts ticked
await app.join(contract, memberNode, memberAgent, { source, public: true });
await app.permit(contract, { public: false });              // propose making it private again

// Anyone, with no key, in any page or in Node.js:
import { Ibc } from '@ouripoupko/ibc';
await Ibc.publicRead('https://node.example', agent, contract, 'topic');
```

The user's own client has `ibc.setPublic(contract, true | false)`, signed by the root.

## The event stream

Events (the outcomes of transactions) arrive on a stream from the node. `onEvent` opens it, and it
stays open: if it breaks (the network, the node restarting) or goes silent for 30 seconds, the client
opens it again, waiting longer after each failure, up to 30 seconds. `onStatus` tells the app:

```js
app.onStatus((status, error) => {
  // 'connecting' | 'open' | 'reconnecting' | 'closed'; error says why it is not open
  showOnline(status === 'open');
  if (status === 'open') refresh();      // events sent while it was down are lost: read again
});
app.streamStatus;                        // the current state
```

It stops, with `closed`, only after `close()`, when the node refuses the stream (for example an app
the user disconnected), or when reopening it would need the user's passkey (a client whose only key
is a passkey): then `connect()` opens it again, from a click. `connect()` resolves when the stream is
open, and waits while the node is unreachable. `Ibc` has the same `onStatus`, `streamStatus`,
`connect()` and `close()`; `streamTimeoutMs` and `reconnectDelayMs` tune it.

## A tool: `Ibc`

`Ibc` talks to a node with the agent's own keys: the root key signs transactions, and a key the node
knows (or the root) signs queries. The node's web interface uses it.

```js
import { Ibc, addressOf, newP256Key, p256Signer } from '@ouripoupko/ibc';

const rootKey = await newP256Key();     // in a browser: the root passkey (passkeySigner)
const agent = addressOf(rootKey.publicKey);
const ibc = await Ibc.connect('http://localhost:2611', { agent, root: p256Signer(rootKey) });
await ibc.register();

const compiled = await ibc.compile(source);    // the node compiles
const counter = await ibc.deploy({ name: 'counter', code: compiled.code, abi: compiled.abi,
  constructor: { start: 10 }, source, compiler: compiled.compiler });
await ibc.write(counter, 'increment', { amount: 5 });   // 15
await ibc.read(counter, 'count');                       // 15
ibc.close();
```

The signing functions (EIP-712 digests, passkeys, WebCrypto keys, batches) are exported too, and
from `@ouripoupko/ibc/keys` alone.

## Versions

The client checks the node's API version (`API_VERSION`), from `node_info` and from the approval
page, and refuses a node of another version.

## Documentation

- [Usage guide](https://github.com/ouripoupko/ibc-go/blob/main/docs/usage.md#call-ibc-from-javascript)
- [Identity and keys](https://github.com/ouripoupko/ibc-go/blob/main/docs/keys.md): apps,
  permissions, and the approval page
- [API reference](https://github.com/ouripoupko/ibc-go/blob/main/docs/api.md)

## Development

The source is TypeScript in `src/`; `dist/` is its build, committed because the node serves it to
its own web pages. After changing `src/`: `npm run build`. `npm test` checks that `dist/` is the
build of `src/`, and that the client computes the same digests and addresses as the node.

MIT license. The keccak-256 in `src/vendor` is from [noble-hashes](https://github.com/paulmillr/noble-hashes)
(MIT).
