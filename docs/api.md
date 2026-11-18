# API reference

The complete HTTP interface of an ibc node. For a guided introduction with examples, see the
[usage guide](usage.md).

- [Conventions](#conventions)
- [Signatures](#signatures)
- [Values](#values)
- [The node](#the-node): `node_info`
- [Agents](#agents): `register_agent`, `is_exist_agent`, `agent_info`
- [Apps](#apps): `register_app`, `get_apps`, `revoke_app`
- [Contracts](#contracts): `compile`, `encode`, `deploy_contract`, `get_contracts`, `get_contract`
- [Calling a contract](#calling-a-contract): `contract_write`, `contract_read`
- [Public contracts](#public-contracts): `set_public`, `public_read`
- [Membership](#membership): `join_contract`, `leave_contract`, `rotate_key`
- [The event stream](#the-event-stream): `/stream`
- [Node to node](#node-to-node): `a2a_consent`
- [Objects](#objects): event, contract summary, record
- [Limits](#limits)

## Conventions

### Requests

Every call except the event stream goes to one URL family:

```
<VERB> /ibc/app/<agent>/<contract>/<method>?action=<action>
Content-Type: application/json

<JSON body>
```

- `<agent>` is an agent id: the address of the agent's key, such as
  `0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851`, in any letter case. Signed transactions must name the
  agent that signed.
- `<contract>` and `<method>` are given only by the calls that need them. Leave out trailing parts
  you do not need, as in `/ibc/app/0x86D6…?action=register_agent`.
- The verb and the action together select the call. The same action with another verb is
  unknown.
- A body is read only if `Content-Type` is `application/json`.

### Replies

The reply is always JSON. The HTTP status is `200` for every call that the node could parse, even
when the call failed; check the reply itself.

- **Errors** in the request (a bad signature, an unknown contract, an unknown action) come back as
  an object with a `message`:

  ```json
  {"message": "consensus: unknown contract 0x67bb…"}
  ```

- **Contract errors** (a revert, a wrong argument, an unknown function) are marked, since a function
  may return a string too. A read answers `{"error": "execution reverted: amount must be
  positive"}`. A transaction's event has `error` set to the text, which is also its `reply`.
- Status `400` means the body is not valid JSON, and `404` means the path has more than three parts
  after `/ibc/app/`.

### Asynchronous calls

`register_agent`, `deploy_contract`, `contract_write`, `join_contract`, `leave_contract` and
`rotate_key` reply at once with a **request id**: the id of the signed transaction (or
registration), a `0x`-prefixed hex hash. Their outcome arrives later as an
[event](#the-event-stream) whose `request` field holds that id. Open the stream before you send
the request.

### CORS

Every reply carries `Access-Control-Allow-Origin: *`. `OPTIONS` requests (browser preflight) are
answered with status `204`, allowing the methods `GET`, `POST`, `PUT` and `DELETE` and the
`Content-Type` header.

### Who may do what

Everything done in an agent's name is signed by the agent's root key, or, for the calls the agent
lets an app make silently, by the agent's node under the agent's grant. The node and every partner
check it. Reads are signed too, as [queries](#queries).

| Call | Who may make it |
|---|---|
| `register_agent`, `register_app`, `revoke_app`, `set_public` | The agent, with its root key |
| `deploy_contract`, `contract_write`, `join_contract`, `leave_contract`, `rotate_key` | The agent, with its root key |
| `contract_write` with a query | An app the agent connected, for a function the agent lets it call silently: the node signs |
| `contract_read`, `get_contract`, `get_contracts`, the event stream | The agent's root key, or an app the agent connected and lets read |
| `get_apps` | The agent (its root key, or its node's own page) |
| `node_info`, `is_exist_agent`, `agent_info`, `compile` | Anyone, unsigned |
| `public_read` | Anyone, unsigned: only a contract the agent made [public](#public-contracts), only its read functions |
| Node to node (`a2a_consent`) | Nodes, signed with their node key; see [Node to node](#node-to-node) |

An agent reads only its own copy of a contract; a [guest](#guests) has none, and its node reads a
host's copy for it, as the guest. An agent may also let anyone read its copy: a
[public contract](#public-contracts). The node's operator can still see everything stored on the node.

## Signatures

An agent has two keys (the design is in [Identity and keys](keys.md)):

| Key | Signs |
|---|---|
| **Root key:** a passkey, or a key file; its address is the agent id | Registrations, transactions, the agent's permissions for apps (consents and grants), and which of its contracts are public |
| **Node key:** the node's own key | Blocks; and calls the agent lets an app make silently, under the agent's grant |

Apps have keys of their own, which sign their [queries](#queries) to the node.

Every message is signed as its [EIP-712](https://eips.ethereum.org/EIPS/eip-712) digest, with this
domain and these types:

```js
const domain = { name: 'ibc', version: '1' };
const types = {
  Transaction: [
    { name: 'contract',   type: 'string' },   // the contract id; "" for a deploy
    { name: 'kind',       type: 'string' },   // deploy | call | join | leave | rotate_key
    { name: 'agent',      type: 'address' },  // the agent
    { name: 'nonce',      type: 'string' },   // random, to make each transaction unique
    { name: 'body',       type: 'string' },   // JSON text, by kind (below)
  ],
  Registration: [
    { name: 'agent',      type: 'address' },
    { name: 'node',       type: 'string' },   // the node's URL
    { name: 'nodeKey',    type: 'address' },  // the node's key, from node_info
    { name: 'credential', type: 'bytes' },    // the root passkey's credential id, or empty
    { name: 'nonce',      type: 'string' },
  ],
  Query: [
    { name: 'agent',      type: 'address' },  // the agent the query acts for
    { name: 'action',     type: 'string' },   // contract_read | get_contract | get_contracts | stream | ...
    { name: 'target',     type: 'string' },   // the URL's <agent>: the same agent
    { name: 'contract',   type: 'string' },   // the URL's <contract>, or ""
    { name: 'body',       type: 'string' },   // JSON text, by action (below), or ""
    { name: 'time',       type: 'uint64' },   // unix seconds when signed
    { name: 'nonce',      type: 'string' },
  ],
  Grant: [                                     // lets the agent's node sign calls silently
    { name: 'agent',      type: 'address' },
    { name: 'nodeKey',    type: 'address' },  // the node that may sign
    { name: 'sourceHash', type: 'bytes32' },  // the contract's source hash (below)
    { name: 'functions',  type: 'string[]' }, // the functions it may call
    { name: 'nonce',      type: 'string' },
  ],
  Device: [                                    // connects another browser of an approved app (kept by the node)
    { name: 'agent',      type: 'address' },
    { name: 'app',        type: 'string' },   // the app's origin
    { name: 'appKey',     type: 'bytes' },    // the new browser's P-256 public key
    { name: 'nonce',      type: 'string' },
  ],
  Consent: [                                   // what the agent lets an app do (kept by the node)
    { name: 'agent',      type: 'address' },
    { name: 'app',        type: 'string' },   // the app's origin, e.g. https://wishes.example.com
    { name: 'appKey',     type: 'bytes' },    // the app's P-256 public key, uncompressed
    { name: 'sourceHash', type: 'bytes32' },  // the contract's source; zero: every contract (read only)
    { name: 'read',       type: 'bool' },     // may read without asking
    { name: 'functions',  type: 'string[]' }, // may have the node call these without asking
    { name: 'nonce',      type: 'string' },
  ],
  Publish: [                                   // lets anyone read the agent's copy (kept by the node)
    { name: 'agent',      type: 'address' },
    { name: 'contract',   type: 'string' },   // the contract id
    { name: 'public',     type: 'bool' },     // true: public; false: private again
    { name: 'time',       type: 'uint64' },   // unix milliseconds: orders the agent's choices
    { name: 'nonce',      type: 'string' },
  ],
  Batch: [                                     // one signature over several messages
    { name: 'items',      type: 'bytes32[]' }, // their digests
  ],
};
```

### The envelope

A signature is sent as an envelope that carries everything needed to check it:

```json
{
  "scheme": "webauthn",
  "key": "0x04…",
  "sig": "0x3045…",
  "authenticatorData": "0x49960de5…",
  "clientDataJSON": "{\"type\":\"webauthn.get\",\"challenge\":\"…\",\"origin\":\"https://ibc.example.com\"}"
}
```

| Field | Meaning |
|---|---|
| `scheme` | `secp256k1-eip712`: a 65-byte secp256k1 signature of the digest (key files, node keys). `p256`: ECDSA P-256 with SHA-256 over the digest, 64 bytes (`r`, `s`) (app keys). `webauthn`: a passkey assertion whose challenge is the digest; `sig` is DER |
| `key` | The signing public key, uncompressed, 65 bytes. Optional for `secp256k1-eip712` |
| `authenticatorData`, `clientDataJSON` | `webauthn` only, as the passkey returned them |
| `certificate` | Only on a call the node signed: `{"grant": {…}, "signature": <the root's envelope>}` |
| `batch` | For a batch signature: the digests of all the messages signed at once. The key signed the `Batch` digest, and this message's digest must be in the list |

A transaction is accepted if the agent's root key signed it, or if the agent's node signed it under
a grant the root signed, for a `call` whose calldata starts with the selector of a function the
grant names. Execution then also checks the
grant's `sourceHash` against the hash of the contract's source, and its `nodeKey` against the agent's
node in this contract; otherwise the call is recorded with the result `"the node's grant does not
apply: it is for another source"` (or `for another node`) and changes nothing. Errors include: `signature does not match
the agent`; `the grant does not let the node call function 0x…: it needs the root key`; `a node signs only
calls: this transaction needs the root key`; `grant: not signed by the agent's root key`; `signature:
not by the node the grant names`; `webauthn: the user did not approve`; `webauthn: the signature is
for another message`; `signature: the message is not in its batch`.

### Transactions

A signed transaction is sent as:

```json
{
  "tx": {"contract": "0x67bb…", "kind": "call", "agent": "0x86D6…", "nonce": "9f2c…",
         "body": "{\"method\":\"increment\",\"values\":{\"amount\":5}}"},
  "signature": { "scheme": "webauthn", "key": "0x04…", "sig": "0x…", "…": "…" }
}
```

The **transaction id** is the EIP-712 digest of the transaction, whoever signs it. A deploy's id is
the new contract's id.

A contract's **source** is its Solidity files, by path: `{"Main.sol": "…", "lib/Util.sol": "…"}`.
The files import each other by these paths (`import "./lib/Util.sol";` in `Main.sol`), and the
contract compiles from them alone: the node fetches nothing, so a contract that uses a library
(OpenZeppelin, …) includes its files. Paths are relative, of ASCII letters, digits, `.`, `_`, `-`
and `/`, with no segment starting with `.`, and end in `.sol`; a source has at most 256 files.

Its **source hash** is keccak256 over the files sorted by path, each as keccak256(path) ‖
keccak256(content) (UTF-8), the same for a single file: the same files in any order give the
same hash, and a change to any path or content changes it. Joins, grants and consents name
contracts by it (`identity.SourceHash`, `sourceHashOf` in the client, `ibc source-hash [-C DIR]
FILE|DIR...`; `ibc source` prints the files as JSON). The bodies, by kind:

| Kind | Body |
|---|---|
| `deploy` | `{"name", "protocol", "default_app", "profile", "node", "nodeKey", "source", "compiler", "contract", "code", "args", "abis", "membership"}`: see [deploy_contract](#deploy_contract) |
| `call` | `{"to": "0x…", "data": "0x7cf5dab0…"}`: calldata, a function's selector and its ABI-encoded arguments, for the contract at `to` in the agreement (no `to`: the root contract). See [calling a contract](#calling-a-contract) |
| `join` | `{"node": "<your node's URL>", "nodeKey": "<your node's key>", "profile": …}` |
| `leave` | `{"member": "<agent id>"}`: yourself to leave, or another member to remove |
| `rotate_key` | `{"node": "<your new node's URL>", "nodeKey": "<its key>"}` |

### Queries

A query is a signed request to the agent's own node. It is never ordered or stored, and its EIP-712
type is not a transaction's, so a signed query can never be used as a transaction. A signed query is:

```json
{
  "query": {"agent": "0x86D6…", "action": "contract_read", "target": "0x86D6…", "contract": "0x67bb…",
            "body": "{\"method\":\"count\",\"values\":{}}", "time": 1789000000, "nonce": "4e1a…"},
  "signature": { "scheme": "p256", "key": "0x04…", "sig": "0x…" }
}
```

- It is signed by the agent's root key, or by an app's key the agent connected
  ([`register_app`](#register_app)); the node decides which from the key, and applies the app's
  permissions.
- `agent` and `target` are the agent whose copy is asked: an agent reads only its own copy.
- `contract_read` and a silent `contract_write` send it as the request body. The other calls send
  its JSON in the `auth` URL parameter, URL-encoded, because a `GET` (and a browser's `EventSource`)
  has no body.
- `action`, `target` and `contract` must be those of the call it is sent with.
- `time` must be within 60 seconds of the node's clock, and the node accepts each query only once.
  Sign a new query for every call, including when you reconnect the event stream.
- Errors: `this call needs a signed query`; `query: …` (a signature error, above); `query for agent
  0x… sent to 0x…`; `query for get_contract sent to contract_read` (and the same for contract);
  `query expired, or its time is wrong: check the clock`; `query already used`; `this key is not
  agent 0x…'s, nor an app it connected`; `… needs your approval on your node's page` (the app may not
  do this silently: send the user to the [approval page](keys.md#8-the-approval-page)).

| Action | Body |
|---|---|
| `contract_read`, `contract_write` | `{"method": "count", "values": {…}, "to": "0x…"}`, or `{"data": "0x…", "to": "0x…"}`: see [calling a contract](#calling-a-contract) |
| `revoke_app` | `{"app": "https://wishes.example.com"}` |
| `get_contracts`, `get_contract`, `get_apps`, `stream` | `""` |

## Values

Contract arguments are sent in `values`, an object keyed by the parameter names in the function's
ABI. Every parameter must be given, and no others. A parameter without a name is keyed by its
position: `"0"`, `"1"`, and so on. The node encodes them (in reads, silent calls, public reads and
[`encode`](#encode)); a transaction carries the encoded calldata.

| ABI type | Sent as | Returned as |
|---|---|---|
| `uint<N>`, `int<N>` | A JSON number, or a string: decimal (`"12"`) or hex (`"0x0c"`). A number must be a whole number of at most 2^53; send anything larger as a string | A JSON number, exact |
| `bool` | `true` / `false` | `true` / `false` |
| `string` | A JSON string | A JSON string |
| `address` | A hex string, `"0x"` and 40 digits | A checksummed hex string |
| `bytes`, `bytes<N>` | A hex string, `"0x…"`. `bytes<N>` needs exactly N bytes | A hex string, `"0x…"` |
| `T[]`, `T[N]` | A JSON array. `T[N]` needs exactly N elements | A JSON array |
| tuple / struct | An object keyed by the component names | An object keyed by the component names |

A function's result is `null` if it returns nothing, the value itself if it returns one value, and
an array if it returns several.

> **JavaScript.** `JSON.parse` keeps integers exact only up to 2^53. Results larger than that are
> correct in the reply, but lose precision when parsed as JavaScript numbers.

## The node

### `node_info`

```
GET /ibc/app?action=node_info
```

**Reply:** `{"apiVersion": 1, "nodeKey": "0x57E3…", "storage": 1234567}`.

| Field | Meaning |
|---|---|
| `apiVersion` | The version of this API and of the approval page's messages. The JavaScript client refuses a node of another version |
| `nodeKey` | The address of the node's key. Registrations, deploys and joins name it |
| `storage` | The size of the node's data folder (the database and the node key), in bytes. Measured at most every 30 seconds. Missing if the node keeps its data in memory |

Logs are not counted: the node writes them to standard error, not to the data folder.

## Agents

### `register_agent`

Registers an agent on the node. The agent's root key authorizes the node's key to sign consensus
blocks for it, and to sign the calls the agent allows apps under the agent's grants. Registering
again updates the registration.

```
PUT /ibc/app/<agent>?action=register_agent
{
  "registration": {"agent": "0x86D6…", "node": "https://ibc.example.com",
                   "nodeKey": "0x57E3…", "credential": "0x…", "nonce": "…"},
  "signature": { "scheme": "webauthn", "key": "0x04…", "sig": "0x…", "…": "…" }
}
```

| Field | Meaning |
|---|---|
| `agent` | The agent; its root key signs, and it must be the agent in the URL |
| `node` | The node's URL, at which partners' nodes reach this agent |
| `nodeKey` | This node's key, from `node_info` |
| `credential` | The root passkey's credential id (`"0x"` for a root key that is not a passkey), so the agent can be found from its passkey on a new device |

The node keeps the root key, its scheme and the credential id, which `agent_info` returns.

**Reply:** a request id. **Event:** `register_agent` with `reply: true` and `contract: null`.

**Errors:** `register_agent: a registration is signed by the root key`;
`signature does not match the agent`; `registration of 0x… sent as 0x…`; `registration authorizes
node key 0x…, this node's key is 0x…`.

### `is_exist_agent`

```
GET /ibc/app/<agent>?action=is_exist_agent
```

**Reply:** `true` if the agent is registered on this node, otherwise `false`.

### `agent_info`

What the node knows of a registered agent's identity, by agent id or by its root passkey:

```
GET /ibc/app/<agent>?action=agent_info
GET /ibc/app?action=agent_info&credential=<root passkey credential id, hex>
```

**Reply:** `{"agent": "0x86D6…", "rootKey": "0x04…", "rootScheme": "webauthn", "credential": "0x…"}`,
or `{"message": "no such agent on this node"}` (`no agent with this passkey on this node`). The web
interface uses the credential form to sign in on a new device.

## Apps

An app on another site acts for an agent through the agent's node, with a key of its own. The agent
decides, per contract source, what the app may do without asking: read, and have the node call some
functions. The [approval page](keys.md#8-the-approval-page) does this with the agent's root key;
these are the calls behind it.

### `register_app`

Records what the agent lets an app do, for contracts of one code; or connects a browser of an app:
a new app (its `connect`), or another browser of an app the agent approved before. The node keeps apps by their site (origin), with the keys of their
browsers.

```
PUT /ibc/app/<agent>?action=register_app
{
  "consent": {"consent": {"agent": "0x86D6…", "app": "https://wishes.example.com", "appKey": "0x04…",
                          "sourceHash": "0x…", "read": true, "functions": ["post(string)"], "nonce": "…"},
              "signature": <the root key's envelope>},
  "grant":   {"grant": {"agent": "0x86D6…", "nodeKey": "<this node's key>", "sourceHash": "0x…",
                        "functions": ["post(string)"], "nonce": "…"},
              "signature": <the root key's envelope>}
}
```

- The root key signs both. `grant` is needed when `functions` is not empty, and must name the same
  agent, code and functions, and this node's key.
- Functions are named by their signature, as Solidity computes selectors: the name and the
  parameters' canonical types, without spaces (`post(string)`, `vote((uint8,bytes32)[])`). In a
  source, a signature means one function (see [compile](#compile)), so it names the same function
  in every contract of the agreement that has it.
- A consent replaces the app's permission for that source. A `sourceHash` of zero means every contract,
  and may only allow reading: the node's own page uses it.
- A consent also connects its `appKey` as one of the app's browsers.
- To connect a browser without changing permissions, send a device instead:
  `{"device": {"device": {"agent", "app", "appKey", "nonce"}, "signature": <the root key's envelope>}}`.
  For an app the agent approved before, the browser gets the app's permissions. For a new app, the
  node records the app with no permissions: it may sign the agent's queries, and is asked for
  everything (`… needs your approval on your node's page`). A key belongs to one app only.

**Reply:** `true`. **Errors:** `register_app: signature does not match the agent`; `register_app:
the grant does not match the consent, or names another node`; `a consent for every contract only
lets the app read`; `agent 0x… is not registered here`; `this key belongs to https://…, not
https://…`.

### `get_apps`

```
GET /ibc/app/<agent>?action=get_apps&auth=<signed query>
```

The agent's connected apps: `[{"agent", "app", "keys": ["0x04…"], "permissions": {"<sourceHash>":
{"read", "functions", "grant"}}}]`. Signed by the root key, or by the node's own page's key.

### `revoke_app`

Disconnects an app: the node forgets all its browsers and permissions at once.

```
PUT /ibc/app/<agent>?action=revoke_app
<a query signed by the root key, action "revoke_app", body {"app": "https://wishes.example.com"}>
```

**Reply:** `true`.

## Contracts

### `compile`

Compiles Solidity source on the node, with its compiler (`-solc`) and fixed settings. Apps send
source; the node produces the bytecode the user signs.

```
POST /ibc/app?action=compile
{"source": {"Chat.sol": "pragma solidity ^0.8.24; import \"./lib/Names.sol\"; contract Chat { … }",
            "lib/Names.sol": "pragma solidity ^0.8.24; library Names { … }"},
 "contract": "Chat"}
```

`source` is the contract's [source](#transactions): its files, by path, all compiled together.
`contract` names the contract to deploy, `Name`, or `path:Name` if two files have a contract of
that name; it may be omitted if the source has only one deployable contract (a library or base
contract imported from another file counts as one, so a source of several files usually names it).
An import of a file not in `source` does not compile.

**Reply:** what a deploy carries:

```json
{"contract": "Chat.sol:Chat", "code": "6080…", "abi": [...],
 "abis": {"Chat.sol:Chat": [...], "lib/Names.sol:Names": [...]}, "membership": ["approve_guest"],
 "sourceHash": "0x…",
 "compiler": {"version": "0.8.38+commit.1835d247.Emscripten.clang", "settings": {"optimizer": {"enabled": true, "runs": 200}}}}
```

`contract` is the contract's name in the source, `path:Name`; `code` its creation bytecode and
`abi` its ABI; `abis` the ABIs of all the source's contracts, by name; `membership` the
[membership functions](#membership) the contract defines.

In a source, a selector (the first 4 bytes of the keccak256 of a function's signature) must mean
one function: the same signature, outputs, and kind (a read, or a transaction) in every contract
that has it; and one custom error. So calldata alone says which function it calls, whatever
contract runs it. A membership function must take an address and return `bool`.

**Errors:** `solc 0.8.38: …` (the node could not get its compiler); `the source does not compile:
…`; `the source has 2 deployable contracts (Chat.sol:Chat, lib/Names.sol:Names): say which one`;
`2 files have a contract "Vault" (…): name it as path:Name`; `source file path "…": …`;
`get() means different functions in A.sol:A and A.sol:B (outputs or view): make them the same, or
rename one`; `approve_guest(address) must return bool: it decides a membership change`.

The node compiles with the Solidity version this release of ibc pins (0.8.38): with the `-solc`
compiler if it is that version, or else with the solc-js build of it, which the node downloads from
soliditylang.org into its data directory the first time, checks against the release list's hash,
and runs with Node.js.

### `encode`

ABI-encodes values for one function or constructor, for clients that sign transactions: a
function's calldata, or a constructor's arguments (`args` in a deploy). No signature: it is a
computation.

```
POST /ibc/app?action=encode
{"abi": {"type": "function", "name": "increment", "inputs": [{"name": "amount", "type": "uint256"}], …},
 "values": {"amount": 5}}
```

`abi` is one item of an ABI, as `get_contract` or `compile` returns it. **Reply:**
`{"data": "0x7cf5dab0…05"}`. **Errors:** as for [values](#values): `increment: missing argument
"amount"`, `increment: unexpected argument "x"`, `increment: argument "amount": …`.

### `deploy_contract`

Deploys a contract. The deploying agent is its first partner, and the contract's constructor runs
with the agent's address as `msg.sender`.

```
PUT /ibc/app/<agent>?action=deploy_contract
{"tx": {"contract": "", "kind": "deploy", "agent": "0x86D6…", "nonce": "…", "body": "<JSON>"}, "signature": <the root key's envelope>}
```

The body:

| Field | Required | Meaning |
|---|---|---|
| `contract`, `code`, `abis`, `membership` | yes (`membership` may be empty) | From [compile](#compile): the root contract's name, its creation bytecode (hex), the ABIs of the source's contracts, and the membership functions it defines |
| `args` | if the constructor has parameters | The constructor's arguments, ABI-encoded (hex): from [encode](#encode) |
| `node` | yes | This node's URL |
| `nodeKey` | yes | This node's key, from `node_info` |
| `protocol` | no | The consensus protocol: `cordial` (the default; `BFT` means the same) or `immediate`, which orders the creator's own transactions locally, with no partners, for testing |
| `source`, `compiler` | yes | The Solidity [source](#transactions) (its files, by path) and how the node compiled it (the `compiler` of [compile](#compile)). Joins name the source's hash, and a joiner's node compiles the source again with that compiler and checks `code`, `abis` and `membership` against it |
| `name`, `default_app`, `profile` | no | Labels kept with the contract and returned by `get_contract` |

An agreement's state is a small EVM world: the root contract, and the contracts it creates. Its
partners execute calldata, and never read `abis`: users' nodes use them to encode calls and decode
results.

**Reply:** the transaction id, which is the new contract's id. Use it as `<contract>` in every later
call.

**Event:** `deploy_contract`, with `contract` set to the contract's id. `reply` is `true` if the
contract was deployed, or the error as a string, for example
`"execution reverted: a poll needs at least two options"`, or `"execution reverted"` for
constructor arguments that do not decode. A contract whose deployment failed stays failed: every
later call to it is answered with `"contract deployment failed: <reason>"`.

**Errors:** `unknown agent "0x…"` (register first); `deploy names node key 0x…, this node's key is
0x…`; `consensus: unknown protocol "…"`; `deploy body needs the source and the compiler that compiled
it`; `deploy body needs the abis of the source's contracts, … among them`; `deploy body:
membership: "…" is not a membership function`.

### `get_contracts`

```
GET /ibc/app/<agent>?action=get_contracts&auth=<signed query>
```

The [query](#queries) is signed by the agent's root key, or by an app the agent connected.

**Reply:** an array of [contract summaries](#contract-summary), one for each contract the agent
has on this node, sorted by contract id. An app sees the contracts it may read.

### `get_contract`

```
GET /ibc/app/<agent>/<contract>?action=get_contract&auth=<signed query>
```

The [query](#queries) is signed by the agent's root key, or by an app the agent lets read this
contract.

**Reply:** the [contract summary](#contract-summary), or `{}` if the agent has no such contract.

## Calling a contract

An agreement's state holds its root contract and the contracts it creates, at EVM addresses. A call
runs on one of them: the root contract, or the one at `to`. Contracts call each other as on
Ethereum, and every contract sees `msg.sender` as the agent who signed (or the contract that called
it).

Partners execute calldata. The node speaks names for its users and their apps: a read, a silent
call or a public read names a function, by name (`"count"`) or, if the source has several of that
name, by signature (`"pay(address,uint256)"`), with its [values](#values); the node finds it in the
deploy's `abis`, encodes the call, and decodes the result. They may give `data`, calldata, instead.

### `contract_write`

A transaction: runs a contract function and keeps its changes. All partners apply it, in the order
the consensus protocol gives.

```
POST /ibc/app/<agent>/<contract>?action=contract_write
{"tx": {"contract": "<contract>", "kind": "call", "agent": "<agent>", "nonce": "…",
        "body": "{\"to\":\"0x…\",\"data\":\"0x7cf5dab0…\"}"}, "signature": <the root key's envelope>}
```

The contract in the URL must match the transaction's. The client makes `data` before signing: it
finds the function in the contract's `abis` (from [`get_contract`](#get_contract)) and encodes it
with [`encode`](#encode).

**A silent call, by an app.** The body may instead be a [query](#queries) signed by an app's key, with
action `contract_write` and body `{"method", "values", "to"}`. If the agent lets the app call this
function silently (its [consent](#register_app)), the node encodes the call, signs it with its own
key, under the agent's grant, and sends it to consensus; the reply and the event are the same.
Otherwise the answer is `{"message": "calling <signature> needs your approval on your node's page"}`:
send the user to the approval page.

**Reply:** the transaction id.

**Event:** `contract_write`, with `reply` set to the function's result, which the node decodes with
the source's ABIs (data it cannot decode stays hex). `selector` is the call's function selector. If
the function fails, `error` is set to the reason, `reply` is the same text, nothing the function did
is kept, and `state_root` is unchanged. Check `error`, not the type of `reply`: a function may return
a string.

| `error` | Meaning |
|---|---|
| `"execution reverted: <reason>"` | The function reverted with this reason (`require`, `revert`, or a custom error of the source, shown as `Name(arg, …)`; `revert` has its data, hex) |
| `"execution reverted"` | The function reverted without a reason, or no function of the contract has the call's selector |
| `"out of gas"` | The call used more than 100 million gas |
| `"contract deployment failed: <reason>"` | The contract's constructor failed |

A function, or values, that the node cannot encode are refused before anything is signed: `unknown
method "incremnt"`, `increment: missing argument "amount"` (from `encode`, or as the `message` of a
silent call).

**Errors** (request not accepted): `consensus: unknown contract 0x…` (the agent is not a partner
of this contract on this node); `transaction signed by 0x…, sent as 0x…`; `signature does not
match the agent`; `0x… is not a member`.

### `contract_read`

Runs a `view` or `pure` function of a contract: one that does not change the state. Nothing goes
through consensus, nothing is added to the ledger, and no event is sent. A function that may change
the state is a transaction: send it with [`contract_write`](#contract_write); reading it answers
`{"message": "<method> changes state: send it as a transaction (contract_write)"}`. The body is a
signed [query](#queries), whose body names the function and its values (or gives `data`), and the
contract it runs on (`to`; default: the root contract):

```
POST /ibc/app/<agent>/<contract>?action=contract_read
{"query": {"agent": "<agent>", "action": "contract_read", "target": "<agent>", "contract": "<contract>",
           "body": "{\"method\":\"getValue\",\"values\":{\"who\":\"0x86D6…\"},\"to\":\"0x…\"}",
           "time": 1789000000, "nonce": "…"},
 "signature": { "scheme": "p256", "key": "0x04…", "sig": "0x…" }}
```

The query is signed by the agent's root key, or by an app the agent lets read this contract's code
(otherwise: `reading this contract needs your approval on your node's page`). The agent reads its own
copy, and the function sees `msg.sender` set to **the agent**. A `<method>` after the contract in
the URL is optional; if given, it must be the query's function. The function sees the latest state, and `block.timestamp` set to the
time of the request.

**Reply:** the function's result, decoded, or `{"error": "<reason>"}` if the contract failed it (the
same reasons as for `contract_write`). `{"message": "no such contract"}` if the agent has no such
contract, or its deployment has not finished yet; `{"message": "<method>: missing argument …"}` and
the like for values the node cannot encode.

For a contract the agent is a [guest](#guests) of, the node sends the read to a host, which runs it
on its copy with `msg.sender` set to the agent. The reply is the same; if no host answers:
`no host answered: …`.

## Public contracts

An agent may let anyone read its copy of a contract, without signing: to publish information. It
is the agent's choice, for its own copy, on its own node; partners never see it, and each one
decides for its own copy. A public read acts for no one: the function runs with `msg.sender` set to
the zero address (`address(0)`), only `view` and `pure` functions run, and nothing is recorded.

The contract's code decides what an unknown reader sees. A function that must stay private refuses
one:

```solidity
function latest(uint256 limit) external view returns (Message[] memory) {
    require(msg.sender != address(0), "sign in to read the messages");
    …
}
```

Public is off until the agent turns it on: when deploying or joining (an app may propose it, and the
user decides on the [approval page](keys.md#8-the-approval-page)), or later on the contract's page.
A [guest](#guests) has no copy, so has none to make public.

### `set_public`

Makes the agent's copy of a contract public, or private again. The root key signs a `Publish`
message ([its type](#signatures)), which stays on the node:

```
PUT /ibc/app/<agent>/<contract>?action=set_public
{"publish": {"agent": "<agent>", "contract": "<contract>", "public": true, "time": 1789000000000, "nonce": "…"},
 "signature": <the root key's envelope>}
```

`time` is in unix milliseconds, and orders the agent's choices: the node takes only a newer one than
it has, so an old message cannot be sent again. It may not be more than a minute ahead of the node's
clock.

**Reply:** the new setting, `true` or `false`. **Errors:** `set_public: signature does not match the
agent`; `publish of 0x… sent as 0x…`; `agent 0x… has no contract 0x…`; `a guest has no copy of the
contract to make public`; `an older choice than the one this node has`; `set_public: its time is in
the future: check the clock`.

### `public_read`

Anyone's read of a public contract: no key, no signature. The URL names the agent whose copy is
read, since a node keeps a copy per agent.

```
GET /ibc/app/<agent>/<contract>/<method>?action=public_read&values=<the arguments as URL-encoded JSON>&to=<address>
```

`values` is the same object as in a `contract_read` body, e.g. `{"limit":5}`, and may be left out
for a function without arguments. `to` is the contract it runs on (default: the root contract).
`data=<calldata>` may replace the method and values. Only `view` and `pure` functions:
`{"message": "<method> changes state: a public read runs only view and pure functions"}`.

**Reply:** the function's result, or `{"error": …}` as for `contract_read`, e.g.
`{"error": "execution reverted: sign in to read the messages"}`. `{"message": "agent 0x… has no public
contract 0x…"}` if the agent's copy is not public, or the agent has no such contract: the answer is
the same, so a stranger learns nothing of private contracts.

Anyone may send one, so the node limits them: 10 a second per client address (20 at once), and 16
running at once on the node. Beyond that: HTTP 429, `{"message": "too many public reads: slow
down"}` or `{"message": "the node is busy with public reads: try again"}`. Behind a proxy on the same
machine ([Caddy](deploy.md#7-add-https-with-caddy)), the client address is the last one the proxy
adds to `X-Forwarded-For`. A public read has a smaller gas limit, see [Limits](#limits).

## Membership

A contract's partners change through `join`, `leave` and `rotate_key` transactions, which are
ordered like every other transaction. While one is being decided, the contract holds back other
transactions (see [consensus design](consensus.md#10-membership-changes)). A contract may also take
[guests](#guests), who are not partners in consensus.

The root contract decides the changes whose **membership function** it defines; the deploy lists
them (`membership`), and a joining node checks the list against the source. Each takes the subject's
address and returns `bool`: `true` approves, anything else (`false`, a revert) refuses. The others
get the defaults:

| Function | Decides | Without it |
|---|---|---|
| `approve_partner(address)` | a join, called as the joining agent | approved |
| `approve_guest(address)` | a guest's join, called as the joining agent | refused |
| `approve_removal(address)` | removing another member, called as the requester | refused |

### `join_contract`

Asks a partner of a contract to let the agent join. Send it to **your own** node, which passes the
signed join to the partner's node. The join names the hash of the contract's source, as the agent
read it.

```
PUT /ibc/app/<agent>?action=join_contract
{
  "tx": {"contract": "<contract>", "kind": "join", "agent": "<agent>", "nonce": "…",
         "body": "{\"node\":\"<your node's URL>\",\"nodeKey\":\"<your node's key>\",\"profile\":\"\",\"sourceHash\":\"0x…\"}"},
  "signature": "0x…",
  "partner": {"address": "<the partner's node URL>", "agent": "<the partner's agent id>"},
  "publish": {"publish": {…}, "signature": <the root key's envelope>}
}
```

`publish` is optional: a signed [`Publish`](#set_public) for this contract, which the node applies
once it holds the agent's copy.

**Reply:** the transaction id, or `null` if the agent already has the contract on this node.

**What happens** ([consensus design](consensus.md#10-membership-changes)):

1. The partners order the join. They refuse it if its `sourceHash` is not the hash of the contract's
   source; otherwise the root contract's `approve_partner(address)` function decides, called as the
   joining agent (an agreement whose root contract has none accepts everyone). The partners' own streams see a `join`
   event with the decision.
2. Each partner's node welcomes the joining node with where the join left the contract: its ledger
   index, the state root after it, and the hash of the record ids up to it. Once members who
   include at least one correct one agree (f+1 of them), the joining node fetches the history from
   the partner the join went to, or, if that history does not check, from another member. A
   history checks if its first record is the deploy whose id is the contract id, every record is
   signed by its agent, its record ids are the members', and it replays to their state root.
3. The joining node compiles the deploy's source with the deploy's compiler and compares the
   result with the deployed code. On a mismatch it does not join.

**Event:** `join_contract` on the joining agent's stream, with `request` set to the transaction id:

| Field | |
|---|---|
| `reply` | `true` once this node holds the contract and takes part in it; `false` if the partners refused, or this node did not join |
| `source_check` | `verified` (the source compiles to the code), `unchecked` (this node could not compile it, for example could not download that compiler; it joined), or `mismatch` (it did not join) |
| `reason` | Why the source was not checked, or why the node did not join: `the members refused the join`, `the source, compiled with solc 0.8.38+commit…, is not the deployed code`, `no member sent a history that the members' state confirms` |
| `source_hash` | The source hash the join named |

The [contract summary](#contract-summary) keeps `source_check` (and `source_check_reason`).

**Errors:** `join_contract needs partner.address and partner.agent`; the partner's node's errors,
for example `consensus: unknown contract 0x…`.

### Guests

To join as a **guest**, add `"guest": true` and `"host": "<the partner's agent id>"` to the join's
body; the host must be the partner the join is sent to:

```
"body": "{\"node\":\"<your node's URL>\",\"nodeKey\":\"<your node's key>\",\"profile\":\"\",\"sourceHash\":\"0x…\",\"guest\":true,\"host\":\"<the partner's agent id>\"}"
```

The join names the source the same way, and the guest's node checks the deploy the host sends (its
id, and its source against its code) before it keeps it. A guest has no copy, so takes no `publish`.

A guest takes no part in consensus, and its node keeps no copy of the contract (see
[consensus design](consensus.md#11-guests)). The contract decides with its `approve_guest(address)`
function, called as the joining agent; **a contract without it refuses every guest**. The event is
`join_contract`, as for a partner.

Then the guest uses the contract through its own node, with the same calls as a partner:
`contract_write` (signed by the root key, or silently by the node under a grant), `contract_read`,
the event stream, and `leave_contract`. Its node sends them to a host: the partner it joined through
first, then the others. In `get_contracts` and `get_contract`, the contract has `"role": "guest"`
and its `hosts`. A guest's `leave_contract` is always approved, and its node forgets the contract
at once. A guest cannot `rotate_key`: it leaves and joins again.

### `leave_contract`

Leaves a contract, or asks to remove another member.

```
PUT /ibc/app/<agent>?action=leave_contract
{"tx": {"contract": "<contract>", "kind": "leave", "agent": "<agent>", "nonce": "…",
        "body": "{\"member\":\"<agent id>\"}"}, "signature": "0x…"}
```

**Reply:** the transaction id. **Event:** `leave`, with `reply` set to the decision. Leaving
yourself is always approved. Removing another member is decided by the contract's
`approve_removal(address)` function, called as you; an agreement whose root contract has none refuses.

### `rotate_key`

Moves an agent's part in a contract to another node, or replaces a compromised node key. Send it to
your current node.

```
PUT /ibc/app/<agent>?action=rotate_key
{"tx": {"contract": "<contract>", "kind": "rotate_key", "agent": "<agent>", "nonce": "…",
        "body": "{\"node\":\"<new node URL>\",\"nodeKey\":\"<new node key>\"}"}, "signature": "0x…"}
```

**Reply:** the transaction id. **Event:** `rotate_key` with `reply: true`. The new node receives the
contract's history from the partners and takes over; the old node stops taking part. Register the
agent on the new node first.

## The event stream

```
GET /stream?auth=<signed query>[&auth=<signed query>…]
```

A [Server-Sent Events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events)
stream of the outcomes of asynchronous calls.

- Each `auth` is a [query](#queries) with action `stream`, for the agent whose events it subscribes
  to, signed by the agent's root key or by an app the agent connected. An app hears only about the
  contracts it has a permission for.
- The query's `contract` chooses the events: empty for every event of the agent, or a contract
  id for that contract's events only.
- Repeat `auth` to subscribe to several agents or contracts. An event is delivered once if it
  matches any of them.
- A missing, expired or wrong query is refused with status `401` and a `{"message": …}` body.

The stream starts with an empty message (`data: `), which tells you the subscription is active.
After that every event is one message, and an empty message is sent every 10 seconds to keep the
connection open:

```
data: 

data: {"request":"0x9a00…","contract":"0x67bb…","action":"contract_write","reply":15,"state_root":"0x…","agent":"0x86D6…"}

data: 
```

The node does not keep events for a client that is not connected: events sent while the stream is
down are lost. The stream ends when the node shuts down, or when a client reads too slowly. A
client should open it again with a new query (a query is accepted once), and read again what it
shows. The JavaScript client does this by itself, and reports the stream's state
(`onStatus`).

In a browser, `EventSource` handles the stream:

```js
const auth = await ibc.query('stream');   // a signed query, with the client package @ouripoupko/ibc
const events = new EventSource(`http://localhost:2611/stream?auth=${encodeURIComponent(JSON.stringify(auth))}`);
events.onmessage = (e) => {
  if (!e.data) return;                 // keep-alive
  const event = JSON.parse(e.data);    // see "Event" below
};
```

If a client falls more than 256 events behind, the node closes its stream, and the events in
between are lost. `EventSource` reconnects by itself with the same URL, which the node refuses,
since each query is accepted once: on an error, close it and open a new stream with a new query. When the node shuts down, it closes
every stream.

## Node to node

These calls are sent by nodes to each other, not by apps. They are listed so that a node can be
implemented or debugged against them.

### `a2a_consent`

The consensus protocol's messages, addressed to an agent's miner of a contract. Every message is
signed by the sending node's key:

```
PUT /ibc/app/<agent>/<contract>?action=a2a_consent
{"msg": {"type": "<type>", …}, "node": "<sending node's key>", "time": 1789000000, "signature": "0x…"}
```

- The signature is over `keccak256("ibc a2a v1\n" + agent + "\n" + contract + "\n" + time + "\n" + msg)`,
  where `agent` is the checksummed address in the URL, `time` is decimal, and `msg` is the exact
  JSON text of `msg` in the body. It is a 65-byte signature like an agent's, by `node`.
- `time` is when the message was sent, and must be within 5 minutes of the receiving node's clock.
  A node signs when it sends, so a message it retries carries a new time.
- `blocks`, `tx` and `join_result` are accepted from any node: blocks and transactions carry their
  own signatures. But a guest's `tx` (one from an agent that is not a member, other than a join),
  and a guest's `read`, are accepted only from the guest's node. `get_blocks`, `get_from` and
  `get_ledger` are answered only for a node whose key belongs to a member of the contract's current
  configuration. A `welcome` or `guest_welcome` must be signed by the welcoming member's node key, as
  the configuration in it records it. An `event` is accepted only from a node of one of the guest's
  hosts.

| `type` | Fields | Reply |
|---|---|---|
| `blocks` | `blocks`: hex RLP encodings of signed blocks | `null` |
| `get_blocks` | `hashes`: block hashes | `{"blocks": [...]}` |
| `get_from` | `depth`: a round | `{"blocks": [...]}`: the blocks from that round on |
| `get_ledger` | none | `{"records": [...]}`: every [record](#record) of the contract, in order. A newcomer's node replays them to build the contract's state |
| `tx` | `tx`: a signed transaction to put in the miner's mempool (a relayed join, for example) | the transaction id |
| `welcome` | `config`: the contract's new configuration; `sender`: the welcoming member; `index`, `stateRoot`, `history`: the ledger index of the change, the state root after it, and the keccak256 of the record ids up to it | `null`; once f+1 members agree, the node fetches the ledger (`get_ledger`), checks it, and joins (see [`join_contract`](#join_contract)) |
| `join_result` | `txId`, `approved: false`: a refused join | `null` |
| `read` | `guest`, `to`, `data`: a guest's read (its calldata), sent to a host by the guest's node | `{"value": "0x…"}` (the return data), or `{"error": "…", "revert": "0x…"}` for the contract's error; the guest's node decodes |
| `guest_welcome` | `config`: the members (the hosts); `sender`; `record`: the contract's deploy record; `txId`: the guest's join | `null`; the guest's node checks the deploy and its source, and adds the contract, as a guest |
| `event` | `reply`: an [event](#event) of the contract; `config`: the current members | `null`; the guest's node passes the event to the guest's stream |

## Objects

### Event

| Field | Meaning |
|---|---|
| `request` | The id of the request this event answers |
| `contract` | The contract's id, or `null` for `register_agent` |
| `action` | `register_agent`, `deploy_contract`, `contract_write`, `join` (a member's view of a join), `leave`, `rotate_key`, or `join_contract` (the joining agent's view) |
| `reply` | The outcome: the function's result for `contract_write` (decoded by the node), `true`/`false` for the others, or an error string |
| `error` | Set, to the same text as `reply`, when the transaction failed (a revert, out of gas) |
| `selector`, `revert` | `contract_write` only: the call's function selector, and its revert data if it reverted, hex: what the node decoded `reply` or `error` from |
| `state_root` | The contract's state root after the transaction, as a hex hash. All zeros if the contract was never deployed |
| `agent` | The subscribed agent the event belongs to |
| `source_check`, `reason`, `source_hash` | `join_contract` only: see [`join_contract`](#join_contract) |

The state root summarizes the contract's entire state. Partners that processed the same
transactions have the same state root.

### Contract summary

Returned by `get_contracts` and `get_contract`. Fields are present only when they have a value.

| Field | Meaning |
|---|---|
| `id` | The contract's id |
| `name`, `contract`, `protocol`, `default_app`, `profile` | The labels given at deployment |
| `pid` | The creating agent |
| `address` | The creating agent's node URL |
| `code`, `abis`, `membership` | The root contract's creation bytecode, the ABIs of the source's contracts, and the membership functions it defines, as deployed |
| `source`, `compiler` | The Solidity source (its files, by path) and how the node compiled it |
| `role` | `partner`, or `guest`: the agent is a [guest](#guests), and this node has no copy of the contract (no state fields) |
| `hosts` | For a guest: the contract's members, `[{agent, nodeKey, node}]`, to whom its node sends its transactions and reads |
| `public` | Whether the agent lets anyone read its copy ([public contracts](#public-contracts)) |
| `source_check`, `source_check_reason` | For a contract the agent joined: this node's check of the source against the code, `verified` or `unchecked` (with the reason), see [`join_contract`](#join_contract) |
| `evm_address` | The root contract's address in the agreement's EVM world |
| `state_root` | The state root after the last successful transaction |
| `block_number` | How many transactions the contract has processed |
| `deploy_error` | Why the deployment failed, if it did |

### Record

A transaction as the ledger keeps it.

| Field | Meaning |
|---|---|
| `type` | `TX` |
| `action` | `deploy_contract`, `contract_write`, `join`, `leave` or `rotate_key` |
| `agent` | The signing agent |
| `contract` | The contract's id |
| `to`, `data` | For `contract_write`, the call: its target (empty: the root contract) and calldata |
| `message` | The transaction's body, decoded |
| `timestamp` | The time of the consensus block that carried it: `YYYYMMDDHHMMSSffffff`, UTC |
| `hash_code` | The transaction id |
| `index` | Its position in the contract's ledger |
| `tx` | The signed transaction itself, so anyone can check the agent's signature |

## Limits

| What | Limit |
|---|---|
| Request body | 16 MiB |
| Gas per call | 100,000,000 |
| Gas per public read | 10,000,000 |
| Public reads | 10 a second per client address (bursts of 20); 16 at once per node |
| Events a stream client may fall behind | 256 |
| Event stream keep-alive | every 10 seconds |
| Requests to other nodes | 30 seconds |
