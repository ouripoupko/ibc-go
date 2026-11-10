# API reference

The complete HTTP interface of an ibc node. For a guided introduction with examples, see the
[usage guide](usage.md).

- [Conventions](#conventions)
- [Signatures](#signatures)
- [Values](#values)
- [The node](#the-node): `node_info`
- [Agents](#agents): `register_agent`, `is_exist_agent`, `agent_info`
- [Apps](#apps): `register_app`, `get_apps`, `revoke_app`
- [Contracts](#contracts): `compile`, `deploy_contract`, `get_contracts`, `get_contract`
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

- **Contract errors** (a revert, a wrong argument) come back as a string in place of the result:
  `"execution reverted: amount must be positive"`.
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
    { name: 'codeHash',   type: 'bytes32' },  // keccak256 of the contract's creation code
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
    { name: 'codeHash',   type: 'bytes32' },  // the contract code; zero: every contract (read only)
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
a grant the root signed, for a `call` to a function the grant names. Execution then also checks the
grant's `codeHash` against the contract's code, and its `nodeKey` against the agent's node in this
contract; otherwise the call is recorded with the result `"the node's grant does not apply: it is for
other code"` (or `for another node`) and changes nothing. Errors include: `signature does not match
the agent`; `the grant does not let the node call setTopic: it needs the root key`; `a node signs only
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
the new contract's id. The bodies, by kind:

| Kind | Body |
|---|---|
| `deploy` | `{"name", "contract", "protocol", "default_app", "profile", "code", "abi", "constructor", "node", "nodeKey", "source", "compiler"}`: see [deploy_contract](#deploy_contract) |
| `call` | `{"method": "increment", "values": {"amount": 5}}` |
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
| `contract_read`, `contract_write` | `{"method": "count", "values": {…}}` |
| `revoke_app` | `{"app": "https://wishes.example.com"}` |
| `get_contracts`, `get_contract`, `get_apps`, `stream` | `""` |

## Values

Contract arguments are sent in `values`, an object keyed by the parameter names in the contract's
ABI. Every parameter must be given, and no others. A parameter without a name is keyed by its
position: `"0"`, `"1"`, and so on.

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
decides, per contract code, what the app may do without asking: read, and have the node call some
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
                          "codeHash": "0x…", "read": true, "functions": ["post"], "nonce": "…"},
              "signature": <the root key's envelope>},
  "grant":   {"grant": {"agent": "0x86D6…", "nodeKey": "<this node's key>", "codeHash": "0x…",
                        "functions": ["post"], "nonce": "…"},
              "signature": <the root key's envelope>}
}
```

- The root key signs both. `grant` is needed when `functions` is not empty, and must name the same
  agent, code and functions, and this node's key.
- A consent replaces the app's permission for that code. A `codeHash` of zero means every contract,
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

The agent's connected apps: `[{"agent", "app", "keys": ["0x04…"], "permissions": {"<codeHash>":
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
{"source": "pragma solidity ^0.8.24; contract Chat { … }", "contract": "Chat"}
```

`contract` names the contract to compile when the source has several; it may be omitted otherwise.

**Reply:** `{"contract": "Chat", "code": "6080…", "abi": [...], "codeHash": "0x…", "compiler":
{"version": "0.8.38+commit.1835d247", "settings": {"optimizer": {"enabled": true, "runs": 200}}}}`.
**Errors:** `this node has no Solidity compiler (see -solc)`; `the source does not compile: …`; `the
source has 2 contracts (A, B): say which one`.

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
| `code` | yes | The contract's bytecode, as hex, with or without `0x`: from [compile](#compile), or `.bin` |
| `abi` | yes | The contract's ABI (`.abi`), as a JSON array |
| `constructor` | if the constructor has parameters | The constructor's arguments, as [values](#values) |
| `node` | yes | This node's URL |
| `nodeKey` | yes | This node's key, from `node_info` |
| `protocol` | no | The consensus protocol: `cordial` (the default; `BFT` means the same) or `immediate`, which orders the creator's own transactions locally, with no partners, for testing |
| `source`, `compiler` | no | The Solidity source and how the node compiled `code` from it (the `compiler` of [compile](#compile)). Kept with the contract, so members can read and check the code |
| `name`, `contract`, `default_app`, `profile` | no | Labels kept with the contract and returned by `get_contract` |

**Reply:** the transaction id, which is the new contract's id. Use it as `<contract>` in every later
call.

**Event:** `deploy_contract`, with `contract` set to the contract's id. `reply` is `true` if the
contract was deployed, or the error as a string, for example
`"execution reverted: a poll needs at least two options"` or
`"constructor: missing argument \"start\""`. A contract whose deployment failed stays failed: every
later call to it is answered with `"contract deployment failed: <reason>"`.

**Errors:** `unknown agent "0x…"` (register first); `deploy names node key 0x…, this node's key is
0x…`; `consensus: unknown protocol "…"`.

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

### `contract_write`

A transaction: runs a contract function and keeps its changes. All partners apply it, in the order
the consensus protocol gives.

```
POST /ibc/app/<agent>/<contract>/<method>?action=contract_write
{"tx": {"contract": "<contract>", "kind": "call", "agent": "<agent>", "nonce": "…",
        "body": "{\"method\":\"<method>\",\"values\":{…}}"}, "signature": <the root key's envelope>}
```

The contract and method in the URL must match the transaction's.

**A silent call, by an app.** The body may instead be a [query](#queries) signed by an app's key, with
action `contract_write` and body `{"method", "values"}`. If the agent lets the app call this function
silently (its [consent](#register_app)), the node signs the call with its own key, under the agent's
grant, and sends it to consensus; the reply and the event are the same. Otherwise the answer is
`{"message": "calling <method> needs your approval on your node's page"}`: send the user to the
approval page.

**Reply:** the transaction id.

**Event:** `contract_write`, with `reply` set to the function's result, or the error as a string. If
the function fails, nothing it did is kept, and `state_root` is unchanged.

| Error reply | Meaning |
|---|---|
| `"execution reverted: <reason>"` | The function reverted with this reason (`require`, `revert`, or a custom error, shown as `Name(arg, …)`) |
| `"execution reverted"` | The function reverted without a reason |
| `"unknown method \"<method>\""` | The ABI has no function with this name |
| `"<method>: missing argument \"<name>\""` | A parameter is missing from `values` |
| `"<method>: unexpected argument \"<name>\""` | `values` has a key the function does not have |
| `"<method>: argument \"<name>\": <problem>"` | A value of the wrong type or out of range |
| `"out of gas"` | The call used more than 100 million gas |
| `"contract deployment failed: <reason>"` | The contract's constructor failed |
| `"get_partners: not implemented"` | Also `get_parameters`: built-in methods that are not defined yet |

**Errors** (request not accepted): `consensus: unknown contract 0x…` (the agent is not a partner
of this contract on this node); `transaction signed by 0x…, sent as 0x…`; `signature does not
match the agent`; `0x… is not a member`.

### `contract_read`

Runs a `view` or `pure` function of a contract: one that does not change the state. Nothing goes
through consensus, nothing is added to the ledger, and no event is sent. A function that may change
the state is a transaction: send it with [`contract_write`](#contract_write); reading it answers
`"<method> changes state: send it as a transaction (contract_write)"`. The body is a signed
[query](#queries):

```
POST /ibc/app/<agent>/<contract>/<method>?action=contract_read
{"query": {"agent": "<agent>", "action": "contract_read", "target": "<agent>", "contract": "<contract>",
           "body": "{\"method\":\"getValue\",\"values\":{\"who\":\"0x86D6…\"}}",
           "time": 1789000000, "nonce": "…"},
 "signature": { "scheme": "p256", "key": "0x04…", "sig": "0x…" }}
```

The query is signed by the agent's root key, or by an app the agent lets read this contract's code
(otherwise: `reading this contract needs your approval on your node's page`). The agent reads its own
copy, and the function sees `msg.sender` set to **the agent**. The `<method>` in the URL is optional;
if given, it must match the query's. The function sees the latest state, and `block.timestamp` set to the
time of the request.

**Reply:** the function's result, or the error as a string (the same strings as for
`contract_write`). `{"message": "no such contract"}` if the agent has no such contract, or its
deployment has not finished yet.

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
GET /ibc/app/<agent>/<contract>/<method>?action=public_read&values=<the arguments as URL-encoded JSON>
```

`values` is the same object as in a `contract_read` body, e.g. `{"limit":5}`, and may be left out
for a function without arguments.

**Reply:** the function's result, or the error as a string, as for `contract_read`, e.g.
`"execution reverted: sign in to read the messages"`. `{"message": "agent 0x… has no public
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

### `join_contract`

Asks a partner of a contract to let the agent join. Send it to **your own** node, which passes the
signed join to the partner's node.

```
PUT /ibc/app/<agent>?action=join_contract
{
  "tx": {"contract": "<contract>", "kind": "join", "agent": "<agent>", "nonce": "…",
         "body": "{\"node\":\"<your node's URL>\",\"nodeKey\":\"<your node's key>\",\"profile\":\"\"}"},
  "signature": "0x…",
  "partner": {"address": "<the partner's node URL>", "agent": "<the partner's agent id>"}
}
```

**Reply:** the transaction id, or `null` if the agent already has the contract on this node.

**Event:** `join_contract` on the joining agent's stream, with `request` set to the transaction id.
`reply` is `true` once this node holds the contract and takes part in it, or `false` if the
contract refused. The decision is the contract's `approve_partner(address)` function, called as the
joining agent; a contract without it accepts everyone. The partners' own streams see a `join`
event with the decision.

**Errors:** `join_contract needs partner.address and partner.agent`; the partner's node's errors,
for example `consensus: unknown contract 0x…`.

### Guests

To join as a **guest**, add `"guest": true` and `"host": "<the partner's agent id>"` to the join's
body; the host must be the partner the join is sent to:

```
"body": "{\"node\":\"<your node's URL>\",\"nodeKey\":\"<your node's key>\",\"profile\":\"\",\"guest\":true,\"host\":\"<the partner's agent id>\"}"
```

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
`approve_removal(address)` function, called as you; a contract without it refuses.

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
| `welcome` | `config`: the contract's new configuration; `sender`: the welcoming member | `null`; the node then fetches the ledger from the sender (`get_ledger`) and joins |
| `join_result` | `txId`, `approved: false`: a refused join | `null` |
| `read` | `guest`, `method`, `values`: a guest's read, sent to a host by the guest's node | `{"value": …}`, or `{"error": "…"}` for the contract's error |
| `guest_welcome` | `config`: the members (the hosts); `sender`; `record`: the contract's deploy record; `txId`: the guest's join | `null`; the guest's node adds the contract, as a guest |
| `event` | `reply`: an [event](#event) of the contract; `config`: the current members | `null`; the guest's node passes the event to the guest's stream |

## Objects

### Event

| Field | Meaning |
|---|---|
| `request` | The id of the request this event answers |
| `contract` | The contract's id, or `null` for `register_agent` |
| `action` | `register_agent`, `deploy_contract`, `contract_write`, `join` (a member's view of a join), `leave`, `rotate_key`, or `join_contract` (the joining agent's view) |
| `reply` | The outcome: the function's result for `contract_write`, `true`/`false` for the others, or an error string |
| `state_root` | The contract's state root after the transaction, as a hex hash. All zeros if the contract was never deployed |
| `agent` | The subscribed agent the event belongs to |

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
| `code`, `abi` | The bytecode and ABI given at deployment |
| `source`, `compiler` | The Solidity source and how the node compiled it, if it did |
| `role` | `partner`, or `guest`: the agent is a [guest](#guests), and this node has no copy of the contract (no state fields) |
| `hosts` | For a guest: the contract's members, `[{agent, nodeKey, node}]`, to whom its node sends its transactions and reads |
| `public` | Whether the agent lets anyone read its copy ([public contracts](#public-contracts)) |
| `evm_address` | The contract's address in its own EVM state |
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
| `method` | For `contract_write`, the function |
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
