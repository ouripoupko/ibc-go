# Usage guide

This guide takes you from a running node to a contract you share with other agents. It assumes a
node is running at `http://localhost:2611`; the [README](../README.md) shows how to start one.
Every call used here is described in full in the [API reference](api.md).

- [Concepts](#concepts)
- [Your keys](#your-keys)
- [The web interface](#the-web-interface)
- [Write a contract](#write-a-contract)
- [Compile it](#compile-it)
- [Call ibc from the shell](#call-ibc-from-the-shell)
- [Call ibc from JavaScript](#call-ibc-from-javascript)
- [The example contracts](#the-example-contracts)
- [Sharing a contract](#sharing-a-contract)
- [Publishing: public contracts](#publishing-public-contracts)
- [Without the helpers: curl](#without-the-helpers-curl)
- [Common errors](#common-errors)

## Concepts

- **Agent.** You, as ibc sees you: your identity key (the *root key*). Your agent id is its
  address, for example `0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851`. You register once on the node
  you choose.
- **Node.** The server your agent lives on. It runs your contracts and talks to your partners'
  nodes, but it never holds any of your keys: everything you do is signed by you.
- **Contract.** A Solidity program you deploy. Deploying returns the contract's id, which every
  later call uses. Each contract has its own state.
- **Partners.** The agents who share a contract. Each partner's node keeps its own copy, and the
  partners agree on the order of every transaction with the [consensus protocol](consensus.md).
- **Guests.** Agents who use a contract without taking part in its consensus, if the contract lets
  them. A guest's node keeps no copy: it sends the guest's transactions and reads to a partner's
  node, its *host*, which the guest trusts with them. See [Joining as a guest](#joining-as-a-guest).
- **Writes and reads.** A *write* (`contract_write`) is a signed transaction: it can change the
  contract's state, all partners apply it, and its result arrives on the event stream. A *read*
  (`contract_read`) runs a `view` function, changes nothing and answers at once. You read your own
  copy; the contract sees you as `msg.sender`.
- **Apps.** Websites that act for you through your node. You decide what each may do without asking
  you: read, and have your node call some functions for you. Everything else, they send you to your
  node to approve.
- **Event stream.** A long-lived HTTP connection (`/stream`) on which the node reports the outcome
  of every deploy, write, join and leave. Open it before you send a write.

## Your keys

Two keys sign in your name ([Identity and keys](keys.md) has the design):

- **Your root key** is your identity. It stays on your device: in the web interface it is a
  *passkey*, kept in your phone's or computer's security hardware. It signs everything you approve:
  your registration, each transaction, and what you let apps do.
- **Your node's key** belongs to your node. It signs consensus blocks, and the calls you let apps
  make without asking you, and nothing else: partners refuse anything else it signs in your name.

From the shell, the `ibc` program keeps the root key in a file instead:

```sh
./ibc key new ~/.ibc/alice.key        # your root key; prints your agent id
./ibc key address ~/.ibc/alice.key    # prints it again
```

The file holds a private key. Keep it private (it is created readable by you only), and keep a
backup: losing it means losing your agent.

## The web interface

The node serves a web interface at its own address, for example `http://localhost:2611`. You need
no wallet and nothing to install: your root key is a **passkey**, which your browser keeps with your
phone, your computer, or a security key. The node never sees it.

- **Create your identity.** Enter a name (it labels your passkey) and click **Create my identity**.
  You approve twice: the passkey is created, and it signs your registration.
- **Sign in on another device.** Click **Sign in with my passkey** and choose it (synced to the
  device, or on your phone nearby). You approve twice.
- **Your contracts.** The page lists your contracts on this node.
- **Deploy.** Choose the contract's Solidity source (`.sol`), or paste it, and click **Compile**: your
  node compiles it. Fill in the constructor's arguments and click **Deploy**.
- **Join.** Paste an invite from a member, or enter the contract id, the member's node URL and the
  member's agent id, and the contract's **source** (the member sends it with the invite: **Copy the
  source** on the contract's page). You read what you join: the members refuse the join if the
  source is not the contract's, and your node checks that it compiles to the contract's code. Tick
  **Join as a guest** to join as a [guest](#joining-as-a-guest). The contract's own rules decide.
  The page tells you when the join is done; a contract whose source your node could not check is
  marked **source not checked**.
- **A contract's page.** Click a contract to see its details, its code (behind **Show the code**),
  and a form for each function in its ABI. *Read* runs a `view` function and shows the result at
  once; *Send* sends a transaction and shows its result once all members have applied it. Reverts
  are shown in red. The page also shows an **invite** to give to people who should join, and a
  button to leave.
- **Connected apps.** The apps you approved, what each may do without asking you, and a
  **Disconnect** button.

The bottom of each page shows the node's storage: the size of its data folder.

Arguments are typed as in [Arguments](#arguments): numbers and addresses as plain text, arrays and
structs as JSON.

**Each passkey request is named.** While your passkey waits, the page shows a box such as *Sign
request: increment 7*. It closes when the passkey answers. **Each transaction asks for your
passkey**; reading does not ask. **Sign out** forgets your identity in this browser (your passkey
stays on your devices).

- **Your passkey belongs to this node's address**: your node represents you on the internet. Use the
  node at a stable address.
- Passkeys need a secure page: `https`, or `http://localhost`. They do not work at an IP address
  such as `http://127.0.0.1:2611`; use `http://localhost:2611`.

### Apps

An app on another site never holds your keys. It has a key of its own, and works through your node:

0. **Connecting.** To know who you are, the app opens your node's **approval page** ("… wants to know
   who you are"). If that browser does not know you yet, you sign in there with your passkey, or, if
   you are new, create your identity there. You approve, and the app learns your agent id, nothing
   more: it may do nothing without asking you yet. An app may skip this step; its first request then
   connects it.
1. **The first time it uses a contract code** (when it creates or joins a contract, for example), it
   opens your node's **approval page**. You see what it asks (and the code, if you want), and choose what it may
   do without asking you:
   - **read these contracts:** on by default;
   - **call these functions:** off by default; tick the ones you trust it with, such as `post` in a
     chat. Your node then signs those calls for you.

   You approve with your passkey, once. When the app asks you to join a contract, it sends the
   contract's source: you read it, and approve the join and these permissions together.
2. **From then on**, it reads and makes the calls you ticked without asking you. Creating or joining
   another contract of the same source needs only your approval of the deploy or the join itself.
3. **For anything else**, a transfer for example, it opens the approval page again, and you sign that
   transaction with your passkey.
4. **On another computer**, the app's first request that needs your node opens the approval page
   (sign in there if that browser does not know you yet). Approving it also connects that browser,
   with the permissions you already gave the app: nothing to choose again.

Your node keeps each app by its website. You can change what an app may do at any time, on a
contract's page on your node (its **Apps** card: one approval; it applies to every contract of the
same code). Disconnect an app on your node's page, and it can do nothing more, on any computer.

## Write a contract

Contracts are ordinary Solidity. Here is the smallest of the [examples](../examples/contracts):

```solidity
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

contract Counter {
    address public owner;
    uint256 public count;
    mapping(address => uint256) public contributions;

    constructor(uint256 start) {
        owner = msg.sender; // the agent that deployed the contract
        count = start;
    }

    function increment(uint256 amount) external returns (uint256) {
        require(amount > 0, "amount must be positive");
        count += amount;
        contributions[msg.sender] += amount;
        return count;
    }

    function reset() external {
        require(msg.sender == owner, "only the owner can reset");
        count = 0;
    }

    function myContribution() external view returns (uint256) {
        return contributions[msg.sender];
    }
}
```

### What is different on ibc

| In Solidity | On ibc |
|---|---|
| `msg.sender`, `tx.origin` | The agent who signed the transaction: its key's address |
| `block.timestamp` | The time of the consensus block that carried the transaction, in seconds |
| `block.number` | How many records the contract has processed |
| Return values | A write's return value arrives on the event stream; a read's comes back in the HTTP reply |
| `require` / `revert` messages, custom errors | Become the reply, for example `execution reverted: amount must be positive` or `execution reverted: AlreadyVoted(1)` |
| `msg.value`, `payable`, sending ether | There is no ether. `msg.value` is always 0, and sending any amount fails |
| `emit` | Allowed, but events are not reported anywhere yet |
| `keccak256` | Works |
| `sha256` | Reverts: ibc has no precompiled contracts |
| `ecrecover`, `ripemd160` | **Return zero without failing.** Do not use them |
| `block.coinbase`, `block.prevrandao`, `block.basefee`, `blockhash` | Always zero |
| Gas | Each call may use up to 100 million gas. Nobody pays for it; it only stops runaway code |

Every partner runs the same transactions in the same order and must reach the same state, so a
contract cannot see anything that differs between nodes. That is why the fields above are fixed.

Solidity features up to the Osaka fork work, so recent compilers can be used with their default
settings.

### Who may join, and who may remove

A contract can decide its own membership by defining these functions. ibc calls them when a join or
a removal is ordered:

```solidity
/// Called as the joining agent. Return true to let it join.
function approve_partner(address partner) external view returns (bool);

/// Called as an agent joining as a guest. Return true to let it in.
function approve_guest(address guest) external view returns (bool);

/// Called as the member who asks to remove another. Return true to remove it.
function approve_removal(address member) external view returns (bool);
```

- **Without `approve_partner`**, every join is approved.
- **Without `approve_guest`**, no one may join as a guest.
- **Without `approve_removal`**, members cannot remove each other. Any member can always leave on
  its own.

[`testdata/Club.sol`](../testdata/Club.sol) is an example: the owner invites agents with
`invite(address)`, only invited agents may join, and only the owner may remove others.

### Tips

- **Return what the client needs.** A write's return value is the only result the client gets
  back from it, so return the new id or the new value.
- **Mark reads `view` (or `pure`).** Only those can be read with `contract_read`; every other
  function is a transaction, ordered with the other members. The web interface lists them under
  Read and Transactions accordingly.
- **Use `msg.sender` for "me".** A function like `myVote()` or `whoAmI()` lets an agent ask the
  contract about itself.
- **Decide who may read.** A read's `msg.sender` is whoever signed it, and anyone can sign one. To
  keep something private, check the reader in the `view` function, for example
  `require(isMember[msg.sender], "members only")`. A function without a check is public.
- **Name your parameters.** Arguments are passed by the names in the ABI, so a parameter called
  `amount` is passed as `{"amount": 5}`.

## Compile it

**Your node compiles.** You always give the Solidity source, in the web interface, through apps and
from the shell; your node compiles it with the Solidity version this release of ibc pins (0.8.38),
and the deploy transaction carries the source, the compiler's version and settings, and the
bytecode. A deploy must carry its source: whoever joins the contract reads it, and their node
compiles it again with the same version and compares. The node uses the `-solc` compiler if it is
that version, or else downloads solc-js of that version and runs it with Node.js. The API call is
[`compile`](api.md#compile).

**To compile yourself**, to look at the bytecode and ABI (the Go tests use these files), the
examples come with a script that compiles them with solc. It needs Node.js, and downloads the
compiler the first time:

```sh
examples/scripts/compile.sh               # every contract in examples/contracts
examples/scripts/compile.sh Counter.sol   # one contract
```

Each contract produces two files in `examples/build`:

- `Counter.bin`: the bytecode, as hex text.
- `Counter.abi`: the interface, as JSON. ibc uses it to encode your arguments and decode the
  results.

To compile your own contract anywhere else, run solc yourself:

```sh
npx -p solc@0.8.38 solcjs --bin --abi --optimize -o build MyContract.sol
```

solcjs names its output files after the source path and the contract, for example
`MyContract_sol_MyContract.bin`. A native `solc --bin --abi` works just as well.

## Call ibc from the shell

[`examples/scripts/ibc.sh`](../examples/scripts/ibc.sh) is a small client built on `curl`, `jq`
and the `ibc` program, which signs with your root key file. It waits for each write's result on the event stream and
prints just the result.

```sh
export IBC_URL=http://localhost:2611   # the node (this is the default)
export IBC_KEY=~/.ibc/alice.key        # your root key file
export IBC_BIN=./ibc                   # the ibc program
alias ibc=examples/scripts/ibc.sh

ibc newkey                             # once: creates IBC_KEY
ibc register
# registered 0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851 at http://localhost:2611

C=$(ibc deploy Counter '{"start": 10}')       # the node compiles contracts/Counter.sol; prints the contract id

ibc write "$C" increment '{"amount": 5}'
# 15
ibc write "$C" increment '{"amount": 0}'
# "execution reverted: amount must be positive"

ibc read "$C" count
# 15
ibc read "$C" myContribution
# 5
```

Other commands:

```sh
ibc whoami                 # your agent id
ibc exists                 # are you registered on the node?
ibc contracts              # your contracts
ibc contract "$C"          # one contract's details
ibc stream                 # follow your events (Ctrl-C to stop)
```

Run `examples/scripts/ibc.sh` without arguments for the full list.

## Call ibc from JavaScript

The JavaScript client is the npm package [`@ouripoupko/ibc`](../js), written in TypeScript and
published as ES modules with type declarations, so it works the same from JavaScript and TypeScript.
It runs in browsers and in Node.js 20 and later, and has no dependencies. The node's own web pages
use the same code.

```sh
npm install @ouripoupko/ibc
```

It has two parts.

**`IbcApp`, for an app on your own site.** The app never touches the user's keys: it makes a key of
its own (kept in the browser, which cannot export it), and opens the user's node when the user must
approve something.

```js
import { IbcApp } from '@ouripoupko/ibc';

const app = await IbcApp.open('https://node.example');   // the user's node
// app.agent is the user's agent id once the user approved the app here; null before.

// On a click (browsers open the node's page only from a click):
const agent = await app.connect();   // the user signs in (or creates an identity) on their node
const source = await (await fetch('/Chat.sol')).text();
const chat = await app.deploy({ source, constructor: { initialTopic: 'lunch' } });
// The user approves on their node: the deploy, and what the app may do without asking.

await app.write(chat, 'post', { text: 'hi' });            // silent, if the user ticked "post"
await app.read(chat, 'topic');                            // silent, if the user allowed reading
await app.write(chat, 'setTopic', { newTopic: 'dinner' }); // opens the node: the user approves
app.onEvent((event) => console.log(event));                // events of the app's contracts
```

`read` and `write` try silently first, and open the node's page when the node says the user must
approve. The event stream reconnects by itself when it breaks; `app.onStatus((status, error) => …)`
says when it is `open`, `reconnecting` or `closed`, and events sent while it was down are lost, so
read again when it reopens (see the [package's README](../js/README.md#the-event-stream)). `app.permit(contract)` asks for permissions on an existing contract (one the user joined),
and `app.join(contract, memberNode, memberAgent, { source })` asks the user to join one, of that
source (`{ source, guest: true }` as a guest). `join` resolves with the join's id once it is sent; the
outcome arrives later, as a `join_contract` event for that id, with `reply` and the user's node's
check of the source (`source_check`: `verified`, `unchecked` or `mismatch`). `deploy`, `join` and
`permit` also take `public`, to propose letting anyone read the user's copy, and anyone reads one
with `Ibc.publicRead(node, agent, contract, method, values)`: see
[Publishing](#publishing-public-contracts).

**`Ibc`, the user's own client**, with the user's keys: the node's web page uses it, and so can
tools. The root key signs transactions; a key the node knows (or the root) signs queries.

```js
import { Ibc, addressOf, newP256Key, p256Signer } from '@ouripoupko/ibc';

const rootKey = await newP256Key();                    // in a browser: the root passkey (passkeySigner)
const agent = addressOf(rootKey.publicKey);
const ibc = await Ibc.connect('http://localhost:2611', { agent, root: p256Signer(rootKey) });
await ibc.register();

const compiled = await ibc.compile(source);            // the node compiles
const counter = await ibc.deploy({ name: 'my-counter', code: compiled.code, abi: compiled.abi,
  constructor: { start: 10 }, source, compiler: compiled.compiler });
await ibc.write(counter, 'increment', { amount: 5 });  // 15
await ibc.read(counter, 'count');                      // 15
ibc.close();
```

[`examples/js/demo.mjs`](../examples/js/demo.mjs) deploys and calls all three examples from
Node.js, with a WebCrypto key standing in for the passkey. Nothing to install:

```sh
node examples/js/demo.mjs http://localhost:2611
```

> **Large numbers in JavaScript.** Results are exact JSON numbers. `JSON.parse` keeps integers
> exactly only up to 2^53. For values that can grow beyond that, return them as `string` from the
> contract, or parse the reply with a big-number-aware parser. When you *send* a large integer,
> pass it as a string: `{"amount": "1000000000000000000000"}`.

## The example contracts

| Contract | Shows |
|---|---|
| [Counter](../examples/contracts/Counter.sol) | State, `msg.sender`, a revert reason, an owner-only function |
| [Poll](../examples/contracts/Poll.sol) | Array arguments, several return values, custom errors |
| [Board](../examples/contracts/Board.sol) | Structs returned as JSON objects, strings, `block.timestamp`, `whoAmI()` |

### Poll

```sh
P=$(ibc deploy Poll '{"text": "Lunch?", "choices": ["pizza", "salad", "soup"]}')

ibc write "$P" vote '{"option": 1}'
# 1
ibc write "$P" vote '{"option": 2}'
# "execution reverted: AlreadyVoted(1)"
ibc write "$P" vote '{"option": 7}'
# "execution reverted: NoSuchOption(7, 3)"

ibc read "$P" results
# ["Lunch?",["pizza","salad","soup"],[0,1,0]]
ibc read "$P" myVote
# [true,1]
```

A function with several return values answers with a JSON array. Custom errors are reported with
their name and arguments.

A deployment can fail too. The constructor requires at least two options:

```sh
ibc deploy Poll '{"text": "Lunch?", "choices": ["pizza"]}'
# ibc.sh: deploy failed: execution reverted: a poll needs at least two options
```

### Board

```sh
B=$(ibc deploy Board)

ibc write "$B" post '{"text": "hello ibc"}'
# 0
ibc read "$B" getPost '{"id": 0}'
# {"author":"0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851","text":"hello ibc","time":1793900673}
ibc read "$B" whoAmI
# "0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851"
```

A struct comes back as a JSON object keyed by its field names, and an array of structs as an array
of objects.

### Chat

[`Chat.sol`](../examples/contracts/Chat.sol) is a contract for an app: users let the app `post` and
`like` without approving each message, while changing the topic (`setTopic`) asks every time. Nothing
in the contract says so; each user decides, on their node, when the app first uses it ([Apps](#apps)).
`web/e2e/app.html` is a small app that does this: it creates a chat from the source, posts silently,
and asks for the topic. Chat also lets anyone join as a guest (`approve_guest` returns `true`): see
[Joining as a guest](#joining-as-a-guest). And if a user makes their copy public, anyone reads its
topic and count, but `latest` refuses an unknown reader: see
[Publishing](#publishing-public-contracts).

### Arguments

Pass arguments as a JSON object keyed by the parameter names in the ABI:

| Solidity type | Send | Receive |
|---|---|---|
| `uint256`, `int64`, … | `5`, or a string: `"5"`, `"0x05"` | `5` |
| `bool` | `true` | `true` |
| `string` | `"hello"` | `"hello"` |
| `address` | `"0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851"` | the same, checksummed |
| `bytes`, `bytes32` | `"0x00ff…"` | `"0x00ff…"` |
| `string[]`, `uint256[3]` | `["a", "b"]` | `["a", "b"]` |
| struct | `{"author": "0x…", "text": "hi"}` | the same |

A parameter without a name, such as the key of a `public` mapping's getter, is passed by its
position: `ibc read "$C" contributions '{"0": "0x86D6…"}'`.

## Sharing a contract

Two people, each with an agent on their own node: alice on node 1, bob on node 2.

```sh
# alice, on node 1
export IBC_URL=http://node1:2611 IBC_KEY=~/.ibc/alice.key
C=$(ibc deploy Counter '{"start": 10}')
ibc whoami                       # alice tells bob the contract id and her agent id

# bob, on node 2: he reads Counter.sol, which alice sent him too
export IBC_URL=http://node2:2611 IBC_KEY=~/.ibc/bob.key
ibc join "$C" http://node1:2611 0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851 examples/contracts/Counter.sol
# joined 0x67bb… (its source compiles to its code)
ibc write "$C" increment '{"amount": 7}'
# 22

# alice sees bob's write
ibc read "$C" count
# 22
```

What happens:

1. Bob's join is a transaction signed by bob, naming the hash of the source he read. His node passes
   it to alice's node, and the contract's partners order it like any other transaction.
2. When it is ordered, the partners refuse it if the source is not the contract's. Otherwise the
   contract decides with its `approve_partner` function (Counter has none, so the join is
   approved).
3. The members' nodes welcome bob's node, telling it where the join left the contract. Bob's node
   fetches the history from alice's node (or from another member, if alice's does not check),
   checks every signature, replays it to the state the members agree on, and from then on takes
   part in consensus.
4. Bob's node compiles the contract's source with the compiler the deploy names, downloading that
   version if needed, and compares it with the deployed code. If they differ, it does not join. If
   it cannot get that compiler, it joins, and warns.

   Bob's stream reports `join_contract` with `true` and `source_check`; a refused join, or one bob's
   node gave up, reports `false` with the `reason`.

While a membership change is being decided, the contract pauses other transactions; they are
processed right after. To leave:

```sh
ibc leave "$C"                    # bob leaves
ibc leave "$C" 0x…                # ask to remove another member (needs approve_removal)
```

Keep in mind:

- **Partners must be online.** A contract makes progress only while a supermajority of its
  partners' nodes are reachable; with two partners, both. See the
  [consensus design](consensus.md#1-goals).
- **Partners' nodes must reach each other** at the addresses the agents registered.

### Joining as a guest

If the contract lets guests in (its `approve_guest` function), you can use it without taking part in
its consensus. Your node keeps no copy: it sends your transactions and reads to the member's node you
joined through (your *host*), and passes you the contract's events. Partners still check your
signature on every transaction, so your host cannot act in your name; but you trust it for what you
read. A guest adds nothing to the contract's consensus: its partners do not slow down, and the
contract does not pause for your join.

```sh
# alice, on node 1
C=$(ibc deploy Chat '{"initialTopic": "lunch"}')

# bob, on node 2
ibc guest "$C" http://node1:2611 0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851 examples/contracts/Chat.sol
# joined 0x7bf7… (its source compiles to its code)
ibc write "$C" post '{"text": "hi from bob"}'
# 0
ibc read "$C" count                # read on alice's node, as bob
# 1
ibc contracts                      # Chat is listed with "role": "guest"
ibc leave "$C"                     # always approved; bob's node forgets Chat
# true
```

As a guest you use the same calls, apps and permissions as a partner; your node sends them on. In
the web interface, a guest's contract page shows its hosts and has no invite: others join through a
partner.

## Publishing: public contracts

To publish information, you may let anyone read your copy of a contract: no sign-in, no key, no
signature. It is your choice, for your copy, on your node; the other partners each decide for theirs.
A public read changes nothing and acts for no one: only `view` and `pure` functions run, with
`msg.sender` set to the zero address.

So the contract decides what a stranger sees. In a contract that mixes public and private
information, the private functions refuse an unknown reader. Chat does it for its messages:

```solidity
function latest(uint256 limit) external view returns (Message[] memory page) {
    require(msg.sender != address(0), "sign in to read the messages");
    …
}
```

Public is off until you turn it on: on the contract's page in the web interface ("Make it public"),
when an app asks you to deploy or join (the box "Anyone may read this contract" starts ticked if the
app proposes it), or from the shell:

```sh
ibc.sh public "$C"                                   # true
ibc.sh anyone "$(ibc.sh whoami)" "$C" topic          # anyone, unsigned: "lunch"
ibc.sh anyone "$(ibc.sh whoami)" "$C" latest '{"limit": 5}'
# ibc.sh: execution reverted: sign in to read the messages
ibc.sh private "$C"                                  # false
```

Anyone reads with a plain GET; the URL names your agent, since a node keeps one copy per agent:

```sh
curl -G "http://localhost:2611/ibc/app/<agent>/<contract>/topic" --data-urlencode action=public_read
curl -G "http://localhost:2611/ibc/app/<agent>/<contract>/latest" --data-urlencode action=public_read \
     --data-urlencode 'values={"limit": 5}'
```

The node limits public reads per client address and in total ([Limits](api.md#limits)). A guest
keeps no copy, so has none to make public.

## Without the helpers: curl

Every call is one HTTP request. A write carries a signed transaction, and a read or the event
stream a signed query. The `ibc` program signs; `jq` builds the JSON. Open the event stream in one
terminal (the signed query goes in the URL):

```sh
curl -sN -G http://localhost:2611/stream \
  --data-urlencode "auth=$(./ibc sign query -key ~/.ibc/alice.key -action stream)"
```

and send the requests from another:

```sh
IBC=http://localhost:2611
KEY=~/.ibc/alice.key   # your root key: signs everything
ME=$(./ibc key address $KEY)
NODE_KEY=$(curl -s "$IBC/ibc/app?action=node_info" | jq -r .nodeKey)

# Register: a registration signed with your root key.
./ibc sign registration -key $KEY -node $IBC -node-key $NODE_KEY |
  curl -s -X PUT "$IBC/ibc/app/$ME?action=register_agent" -H 'Content-Type: application/json' -d @-

# Deploy: the node compiles the source; a deploy transaction carries the source and the compiler.
# The reply is the new contract's id.
COMPILED=$(jq -cn --rawfile src examples/contracts/Counter.sol '{source: $src}' |
  curl -s -X POST "$IBC/ibc/app?action=compile" -H 'Content-Type: application/json' -d @-)
BODY=$(jq -cn --arg node $IBC --arg nodeKey $NODE_KEY --rawfile src examples/contracts/Counter.sol --argjson c "$COMPILED" \
  '{name: "my-counter", code: $c.code, abi: $c.abi, constructor: {start: 10}, node: $node, nodeKey: $nodeKey,
    source: $src, compiler: $c.compiler}')
CONTRACT=$(./ibc sign tx -key $KEY -kind deploy -body "$BODY" |
  curl -s -X PUT "$IBC/ibc/app/$ME?action=deploy_contract" -H 'Content-Type: application/json' -d @- | jq -r .)

# Write: a call transaction. The reply is its id; the result appears on the event stream.
./ibc sign tx -key $KEY -kind call -contract $CONTRACT -body '{"method": "increment", "values": {"amount": 5}}' |
  curl -s -X POST "$IBC/ibc/app/$ME/$CONTRACT/increment?action=contract_write" -H 'Content-Type: application/json' -d @-

# Read: a signed query. The reply is the result.
./ibc sign query -key $KEY -action contract_read -contract $CONTRACT -body '{"method": "count", "values": {}}' |
  curl -s -X POST "$IBC/ibc/app/$ME/$CONTRACT/count?action=contract_read" -H 'Content-Type: application/json' -d @-
```

On the stream, each result is one line:

```
data: {"request":"0x5d2c…","contract":null,"action":"register_agent","reply":true,"agent":"0x86D6…"}
data: {"request":"0x67bb…","contract":"0x67bb…","action":"deploy_contract","reply":true,"state_root":"0x…","agent":"0x86D6…"}
data: {"request":"0x9a00…","contract":"0x67bb…","action":"contract_write","reply":15,"state_root":"0x…","agent":"0x86D6…"}
```

Match `request` with the id you got when you sent the request. Empty `data:` lines are
keep-alives.

## Common errors

| Reply | Meaning |
|---|---|
| `{"message": "… signature does not match the agent"}` | The transaction was changed after it was signed, or signed by another key |
| `{"message": "transaction signed by 0x…, sent as 0x…"}` | The agent in the URL is not the one who signed |
| `{"message": "registration authorizes node key 0x…, this node's key is 0x…"}` | Sign the registration with the `nodeKey` from this node's `node_info` |
| `{"message": "calling setTopic needs your approval on your node's page"}` | An app asked for a call the user did not let it make silently: open the approval page (`IbcApp` does) |
| `{"message": "this node has no Solidity compiler (see -solc)"}` | Start the node with `-solc` pointing at `solc` or `solcjs` |
| `{"message": "unknown agent \"0x…\""}` | Register first |
| `{"message": "consensus: unknown contract 0x…"}` | You are not a partner of this contract on this node: check the id, or join it first |
| `join_contract` with `"reason": "the members refused the join"` | The contract's `approve_partner` (or `approve_guest`) said no, or the source you joined with is not the contract's: check it is the exact file the contract was deployed from |
| `join_contract` with `"source_check": "mismatch"` | The contract's source does not compile to its code: your node did not join it |
| `join_contract` with `"source_check": "unchecked"` | You joined, but your node could not get the compiler the contract was compiled with (it downloads solc-js, which needs Node.js and internet access): the contract is marked "source not checked" |
| `{"message": "deploy body needs the source and the compiler that compiled it"}` | Deploys carry their source: compile it on the node (`compile`) and send its `source` and `compiler` |
| `{"message": "no such contract"}` | A read of a contract you do not have, or whose deployment has not finished |
| `{"message": "no host answered: …"}` | You are a guest, and none of the contract's hosts' nodes answered |
| `{"message": "agent 0x… has no public contract 0x…"}` | A public read of a copy its agent has not made public (or a wrong agent or contract id) |
| `{"message": "too many public reads: slow down"}` | Public reads are limited per client address (HTTP 429) |
| `{"message": "0x… is not a guest of this contract"}` | You were a guest and were removed: leave the contract on your node |
| `{"message": "this call needs a signed query"}` | Reads, `get_contract(s)` and the event stream are signed: see the [API reference](api.md#queries) |
| `{"message": "query expired, or its time is wrong: check the clock"}` | Sign the query just before you send it; the node accepts it for 60 seconds |
| `{"message": "query already used"}` | Every query is accepted once: sign a new one for each call |
| `"increment: missing argument \"amount\""` | A key in `values` is missing or misspelled. Names must match the ABI exactly |
| `"increment: unexpected argument \"amout\""` | A key in `values` that the method does not have |
| `"unknown method \"incremnt\""` | No function with that name in the ABI |
| `"increment changes state: send it as a transaction (contract_write)"` | Only `view` and `pure` functions can be read; others are transactions |
| `"execution reverted: …"` | The contract reverted; the rest is its reason |
| `"execution reverted"` (nothing after it) | The contract reverted without a reason, for example by calling `sha256` |
| `"contract deployment failed: …"` | Every call to a contract whose constructor failed. Deploy it again |
| `"0x…: not a partner or guest of this contract"` | A transaction from an agent the contract does not know |
| `"increment: argument \"amount\": number 1.5 is not an exact integer; pass it as a string"` | Integers must be whole; very large ones must be strings |
