# Architecture

How an ibc node works inside, and how to develop it. For running a node see the
[README](../README.md); for using it, the [usage guide](usage.md).

- [Overview](#overview)
- [Packages](#packages)
- [The life of a transaction](#the-life-of-a-transaction)
- [Storage](#storage)
- [Contracts on the EVM](#contracts-on-the-evm)
- [Development](#development)
- [Not implemented yet](#not-implemented-yet)

## Overview

This is a Go rewrite of the Python [ibc](https://github.com/ouripoupko/ibc). The Python node ran
three processes (a Flask app under gunicorn, a consensus service and an execution service),
connected through Redis, and kept its data in MongoDB. Contracts were Python classes run with
`exec`, and partners agreed with PBFT.

The Go node is one process. Its parts are goroutines connected by function calls, the data is in an
embedded [Pebble](https://github.com/cockroachdb/pebble) database, contracts run on the EVM from
[go-ethereum](https://github.com/ethereum/go-ethereum), and partners agree with
[Cordial Miners](consensus.md). Users sign every change with their own keys (passkeys; see
[Identity and keys](keys.md)); nodes never hold personal keys.

```
client (a passkey signs)
   │ signed transaction
   ▼
HTTP (dispatcher) ── reads ──────────────────► execution: read-only call on the latest state
   │
   ▼
consensus.Router ── the contract's protocol (Cordial Miners, or "immediate")
   │  the agent's miner of the contract: mempool → signed blocks ⇄ partners' nodes (a2a_consent)
   │  τ orders the blocklace; membership changes switch the configuration
   ▼ transactions in order, with their ledger index
execution.Navigator.Submit
   1. append the record to the contract's ledger (durable)
   2. wake the contract's goroutine, which applies records after the cursor on the EVM state
   │
   ├─ replies ──────► event stream (/stream)
   └─ decisions and progress ──► back to the miner
```

## Packages

| Package | Python counterpart | Role |
|---|---|---|
| [`cmd/ibc`](../cmd/ibc) | `ibc.py`, `*_service.py` | The program: the node, and key and signing tools |
| [`node`](../node) | `docker-start.sh` | Wires storage, execution, consensus and HTTP in one process; startup recovery; orderly shutdown |
| [`dispatcher`](../dispatcher) | `ibc.py`, `dispatcher/` | HTTP API, Server-Sent Events, the in-process event broker |
| [`web`](../web) | — | The web interface: static pages and the JavaScript client, embedded in the binary and served at `/`. They call the API from the browser; the root passkey signs, and the page's own key reads. `approve.html` is where apps send the user to approve |
| [`compiler`](../compiler) | — | Compiles Solidity source with the pinned version (`-solc` if it is that version, or else solc-js, downloaded and run with Node.js), and checks a joined contract's source against its code with the deploy's version |
| [`identity`](../identity) | — | Signed transactions and registrations (EIP-712), the node key |
| [`consensus`](../consensus) | `consensus/` | The `Protocol` interface and the `Router`, which runs each contract's protocol, carries miners' messages between nodes, and syncs joining nodes |
| [`consensus/cordial`](../consensus/cordial) | `consensus/pbft.py` | Cordial Miners: blocks, the blocklace, τ, the miner, its store, and the simulator tests |
| [`execution`](../execution) | `execution/` | `Navigator` (per-contract goroutines, recovery, queries), `ContractExecution`, records |
| [`common`](../common) | `common/` | `State` (the EVM), the ABI codec, documents, ledgers, the outbox |

## The life of a transaction

1. **The client** builds a transaction (deploy, call, join, leave or rotate_key) and has the user
   sign it with the root key (a passkey, or a key file); or, for a call an app may make silently,
   the node signs it under the user's grant.
2. **The dispatcher** checks that the agent in the URL is the signer and hands the transaction to
   the router.
3. **The router** verifies the signature and gives it to the agent's miner of the contract, which
   keeps it in its mempool. A deploy creates the contract and its miner; a join is relayed to a
   member's node, and the joiner's node then checks the history and the source before it keeps
   the contract ([`consensus/join.go`](../consensus/join.go)). For a contract the agent is a guest of, the router sends the transaction to a
   host's node instead, which puts it in its miner's mempool
   ([`consensus/guest.go`](../consensus/guest.go); see [guests](consensus.md#11-guests)).
4. **The miners** of the contract's partners put transactions in signed blocks, exchange them, and
   order the blocklace with τ (see the [consensus design](consensus.md)).
5. **`Navigator.Submit`** receives each ordered transaction as a record with its ledger index,
   and appends it to the contract's ledger before it returns. From that point a crash cannot lose
   it, and a repeated index is ignored.
6. **The contract's goroutine** applies, in order, every ledger record after the contract's
   *cursor*. Each contract has its own goroutine, so contracts never wait for each other.
7. **The outcome** goes to the event stream. Execution also tells the miner how far it got, and how
   membership changes were decided.

### The cursor

A contract's cursor is its `block_number`: the position of the last ledger record applied. Every
record in the ledger advances it exactly once, whatever happens: a successful write, a revert, a
call to a built-in stub, a join, or a record for a contract whose deployment failed. Only a local
storage failure leaves the cursor in place, and the record is applied again later.

Because of this, the node can always tell which records it still has to apply. At startup,
`Navigator.Recover` wakes every contract whose ledger is ahead of its cursor. Applying a record is
deterministic, so applying it again after a crash reaches the same state root.

A write is committed in this order: the EVM state's trie nodes first, then the contract document
with the new root and cursor, in one atomic write. A crash between the two leaves the document at
the old root, and the record is applied again.

### Reads

`contract_read` does not go through consensus or the contract's goroutine. It runs only `view` and
`pure` functions (it refuses others: they are transactions), on the contract's state at its latest
root. A read never deploys a
contract: a contract whose deploy record has not been applied yet is reported as unknown. A guest's
node has no state: it sends the read to a host's node, which runs it on its copy, as the guest.

A **public read** (`public_read`, [`dispatcher/public.go`](../dispatcher/public.go)) is the one
unsigned read: of an agent's copy that the agent made public, with a root-signed `Publish` the node
keeps in the contract document (`public`, and `public_time`, which orders the agent's choices). It
runs like `contract_read`, with `msg.sender` the zero address and a gas limit of 10,000,000, and the
dispatcher limits these reads per client address and in total. A contract that is not public, and a
contract the agent does not have, get the same answer.

### Signatures on every call

The dispatcher checks a signature on every call that changes or shows an agent's data
([`dispatcher/auth.go`](../dispatcher/auth.go)):

- **Writes** carry a signed transaction, checked again by every partner.
- **Reads, `get_contract(s)` and the event stream** carry a signed query
  ([`identity/query.go`](../identity/query.go)): bound to its action, agent and contract, within
  60 seconds of the node's clock, and accepted once. A read runs with `msg.sender` set to the
  signer, so the contract's own `view` functions decide what each reader may see; the other calls
  are for the agent itself only. The exception is a public read, unsigned, of a copy its agent made
  public (above).
- **Every signature** is checked by one function, `identity.Verify`
  ([`identity/envelope.go`](../identity/envelope.go)): the scheme (secp256k1, P-256 or WebAuthn),
  and, for a call the node signed, the grant the agent's root signed (its functions; execution also
  checks its source hash and node). Inside consensus it uses no clock. The dispatcher, the miners and a
  joining node's ledger replay all call it. See [Identity and keys](keys.md).
- **Apps** ([`dispatcher/apps.go`](../dispatcher/apps.go)): the node keeps, per agent and app key,
  what the user allowed per contract source: reading, and the functions the node may call silently,
  with the grant. The dispatcher answers an app's reads and signs its silent calls accordingly.
- **Node-to-node messages** are signed with the sending node's key when they are sent, not when
  they are queued, so the outbox's retries stay fresh ([`consensus/a2a.go`](../consensus/a2a.go)).
  Requests for blocks or the ledger are answered only for the nodes of the contract's members.

## Storage

One Pebble database holds everything, under separate key prefixes:

| Prefix | Contents |
|---|---|
| `ibc/doc/` | Documents, in the layout of the Python MongoDB: `agents/<agent>` → `contracts/<hash>` → `pda_partners/<pid>` |
| `ibc/ledger/` | One ledger per agent and contract, numbered from 1 |
| `ibc/evm/` | Each contract's EVM world state (trie nodes and code), separately for every agent and contract |
| `ibc/cm/` | Each miner's consensus state: configuration, blocks, mempool, output progress |
| `ibc/im/` | The order of each `immediate` contract |

The node's key is a separate file, `nodekey`, in the data directory.

Variable-length parts of a key are length-prefixed, so no id can reach into another id's keys,
whatever characters it contains.

Each document is one JSON value under one key, so updating a document is atomic. Numbers are read
back as `json.Number`, so large integers stay exact.

The `Document` and `Collection` interfaces in [`common/document.go`](../common/document.go) keep
the storage replaceable. Tests use in-memory implementations.

Pebble is a library compiled into the program, not a separate server. The data directory holds its
write-ahead log and data files, and only one process can open it at a time.

## Contracts on the EVM

### State

Every contract instance has its own world state: its own `state.StateDB` and trie. There is no
state shared between contracts. After every successful write, the state root is stored in the
contract document and reported in the event. Partners that processed the same records have the
same root, which is what a consensus protocol can compare.

### Identity

An agent id is the address of the agent's Ethereum key, and that address is `msg.sender` for the
agent's transactions. (`common.AgentAddress` also maps any other string to an address, as
`keccak256(id)[12:]`; that is used only by tests and old records.)

### Determinism

Every partner must compute the same result for the same records, so nothing depends on the node:

| Block field | Value |
|---|---|
| `block.timestamp` | The record's timestamp (`YYYYMMDDHHMMSSffffff`, UTC), in unix seconds |
| `block.number` | The contract's cursor |
| `coinbase`, `prevrandao`, `basefee`, `blobbasefee`, `gasprice`, `blockhash` | Zero |
| `block.gaslimit` | 100,000,000 for every call. Gas only bounds runaway code; nobody pays |
| `chainid` | `0x1bc` |
| Value transfers | Always zero |
| Precompiles | None |

Every fork up to and including **Osaka** is active from genesis. Amsterdam and later are still in
development in go-ethereum and stay off.

### Arguments and results

[`common/abi_codec.go`](../common/abi_codec.go) converts between JSON values and the Go types of
go-ethereum's `accounts/abi`. The rules are in the [API reference](api.md#values). Revert data is
decoded as `Error(string)`, `Panic(uint256)`, or a custom error from the contract's ABI.

### Differences from the Python contract model

- Contracts are EVM bytecode with an ABI, not Python classes.
- A failed call rolls back completely. In Python, writes made before an exception stayed.
- A read runs on a copy of the state. In Python, a read could change state.
- Dropped: `read()` between contracts, governance `parameters` and medians, `random`, `hashcode`,
  `elapsed_time`, and `Storage(name)`.
- Membership is decided by the contract's own `approve_partner(address)` and
  `approve_removal(address)` functions, if it defines them.
- The built-in methods `get_partners` and `get_parameters` are stubs that answer `not implemented`.

## Development

### Tests

```sh
go test ./...        # all tests
go test -race ./...  # with the race detector
go vet ./...
web/e2e/run.sh       # optional: the web interface in headless Chrome (needs Node.js and Chrome)
```

The browser test starts two nodes and drives the web interface as two users, with Chrome's virtual
passkey authenticator in place of a phone: create an identity, deploy, write, read, invite, join,
sign in on a new device, and an app on another site that deploys a chat (the user chooses its
permissions on the node's approval page), posts silently, asks for the rest, and is disconnected. It is not part of `go test`.

### The JavaScript client

The client is the npm package `@ouripoupko/ibc`, in [`js/`](../js): TypeScript in `js/src`,
compiled to ES modules and type declarations in `js/dist`. `js/dist` is committed: the node embeds
it (package `ibcjs`) and serves it at `/js/` to its own web pages, so `go build` needs no Node.js.

```sh
cd js
npm install          # TypeScript, once
npm run build        # after changing js/src: rebuilds js/dist
npm test             # checks that js/dist is the build of js/src, and runs the tests
```

The tests check that the client computes the same digests and addresses as Go
(`identity/testdata/vectors.json`; refresh it with `go test ./identity -run TestVectors -update`).
The client and the node check each other's API version (`API_VERSION` in `js/src/ibc.ts`,
`dispatcher.APIVersion`); change both when the API or the approval page's messages change
incompatibly. To publish: raise the version in `js/package.json`, then `npm publish --access
public` in `js/` (it runs the tests first).

The tests include:

- the EVM state: deployment, writes, reads, reverts, determinism between two partners, restarts,
  and membership decisions;
- the consensus simulator ([`consensus/cordial/sim_test.go`](../consensus/cordial/sim_test.go)):
  miners in one process with messages delivered in random orders, equivocating miners, lost
  blocks, joins, leaves, key rotations and restarts, checking that correct miners always agree and
  eventually output every transaction;
- storage and the ledger on Pebble;
- the navigator: ordering, redelivery, crash recovery, idle shutdown;
- end-to-end runs over real HTTP: the API walkthrough, three nodes sharing a contract, a join
  refused by a contract's policy, stream scoping, restarts, and shutdown with open streams;
- [`examples/examples_test.go`](../examples/examples_test.go), which runs the calls shown in the
  [usage guide](usage.md) against the example contracts, so the documented results stay true.

### Contracts used by the tests

[`testdata/Registry.sol`](../testdata/Registry.sol) is the contract of the unit tests. Its compiled
`Registry.bin` and `Registry.abi` are committed, so the tests do not need solc. To rebuild them:

```sh
npx -p solc@0.8.38 solcjs --bin --abi --optimize --base-path testdata -o out testdata/Registry.sol
mv out/Registry_sol_Registry.bin testdata/Registry.bin
mv out/Registry_sol_Registry.abi testdata/Registry.abi
```

The example contracts are rebuilt with `examples/scripts/compile.sh`.

### Upgrading go-ethereum

go-ethereum is pinned in `go.mod`. Its internal APIs (`vm.NewEVM`, `state.New`, `triedb`, gas
accounting) change between releases, so check the signatures in the module cache of the new
version, not in older documentation. A new version can also change results, so all partners must
upgrade together.

## Not implemented yet

- **The personal ledger** of the ssDA paper: one DAG per agent with same-contract and same-agent
  pointers, linking an agent's actions across contracts.
- **Recovering an identity** whose passkey is lost: future work, through trusted friends.
- **A native app for the root key.** The root passkey belongs to the node's web address for now.
- **Contract events.** Logs emitted by contracts are discarded.
- **Precompile addresses.** There are no precompiles, but a call to one of their addresses does not
  fail: `ecrecover` and `ripemd160` return zero, `sha256` reverts.
- **Built-in methods.** `get_partners` and `get_parameters` answer "not implemented".
- **Guests' trust in hosts.** A guest's node takes a host's answer to a read as it is, and is not
  told when the guest is removed (it learns at its next call). See
  [guests](consensus.md#11-guests).
- **Unverified refusals.** A `join_result` refusal is accepted from any node, so a stranger could
  tell a joining agent that it was refused. The welcome's configuration is trusted from the
  welcoming member's node; the ledger replayed from it is checked transaction by transaction.
- **Duplicates after recovery.** Records applied again after a crash publish their replies again;
  clients must tolerate duplicate events.
- **Execution rules version.** Records do not yet say which execution rules (EVM fork, gas limit,
  block context) interpret them.
- **Consensus variants.** No timeout in eventual synchrony, and no asynchronous variant with a
  shared coin. The finality window is an [open issue](consensus.md#open-issue-the-finality-window).
- **Packaging.** No Dockerfile or compose file yet.
