# Proposal: an agreement is a shared Ethereum world

Status: implemented. The reference for the result is the [API](api.md); this note keeps the
reasoning. Where the implementation differs from the proposal below:

- **The deploy carries the ABIs** (`abis`, every contract's of the source) and is checked on join
  with the code, so any node, a guest's too, can encode and decode without compiling. Execution
  still never reads them (open question 3).
- **A selector means one function in a source** (same signature, outputs and kind, in every
  contract that has it; and one custom error). The compiler and the join's check enforce it, so a
  grant's signature names one function everywhere (open question 2: grants stay by signature,
  across the agreement), and the node decodes any result from the call's selector alone.
- **No identification by code** (§5): with the rule above, the node does not need to know which
  contract is at an address to encode, decode or show a call; the page shows the function and the
  contracts of the source that have it.
- **Membership defaults stay** (open question 1): the deploy lists the membership functions the root
  contract defines (`membership`), checked on join; for the others, execution applies the defaults
  (joins accepted, guests and removals refused) without calling anything.

## In one paragraph

Every individual runs their own ledger. Individuals act together through **agreements**: a group
of partners who replicate a shared state and order their transactions by consensus. This proposal
makes that shared state behave like a small Ethereum chain: a world of EVM contracts at addresses,
any number of them, calling each other freely. A transaction is what it is in Ethereum, an address
and calldata, signed by an agent. The ABI (the description of a contract's functions) leaves
consensus: it becomes knowledge that each user's own node holds, derived from the source, used to
encode calls, to show the user what they sign, and to decode results.

## Why

Today a transaction is `{method, values}` in JSON, and every partner encodes it with the ABI
recorded at deploy ([common/state.go](../common/state.go) `execute`). This shape comes from the
Python ibc, where contracts were Python classes called by method name. On the EVM it has two
costs:

- **One contract per agreement.** A call can only reach the contract that was deployed, through
  its one ABI. An app whose contract is too big for the EVM's size limit, or that is naturally
  several objects, needs a forwarding layer (stubs, routing, a Diamond).
- **The ABI is something partners must agree on.** Every feature that touches functions (several
  contracts, combined ABIs, grants) must then define how partners agree on ABIs and check them.

Ethereum keeps the ABI out of consensus: a transaction is an address and bytes, and wallets use the
ABI to encode calls and to show them. Doing the same in ibc removes both costs.

## The model

| Concept | Meaning |
|---|---|
| Agent | An individual: a root key (passkey), a node, a personal ledger |
| Agreement | A group of partners and a shared EVM world (its own state, as each ibc contract has today). Its id is the digest of its deploy transaction, as today |
| Root contract | The contract the deploy creates: the agreement's entry point, and the one that governs membership |
| Contract | Any contract in the agreement's world, at an EVM address: the root, or one created by another contract (`new`) |
| Source | The Solidity files the agreement starts from. Partners read it, join by its hash, and check that it compiles to the deployed code |
| Transaction | A signed message into the agreement: a deploy, a call (`to` + `data`), a join, a leave, a key rotation |
| ABI | Knowledge on the user's side, compiled from the source: not part of consensus |

What partners agree on: the source (by its hash, checked against the code when they join) and the
ordered transactions. Everything else follows from executing them.

## Changes

### 1. Calls: `{to, data}`

The body of a `call` transaction becomes:

```json
{"to": "0x…", "data": "0x…"}
```

- `to`: a contract address in the agreement's world; omitted, the root contract.
- `data`: the calldata, as in Ethereum: the function's 4-byte selector and its ABI-encoded
  arguments.

Execution calls `to` with `data`, with `msg.sender` the agent, and needs no ABI. The `Transaction`
type and its EIP-712 signature are unchanged: only the body's content changes.

Calldata is produced before signing, by the user's node (the approval page) or by the client. The
user signs bytes they were shown decoded, as an Ethereum wallet does.

### 2. Deploy: code and encoded arguments

The deploy body keeps the source, the compiler, the root contract's name in the source
(`contract`), the node and the labels. Instead of `abi` and a JSON `constructor`, it carries:

- `code`: the root contract's creation bytecode, as compiled;
- `args`: the constructor's ABI-encoded arguments (hex), appended to `code` at creation.

They stay separate so that the source check compares `code` with the compiler's output exactly, as
today.

### 3. Membership decisions call the root contract

Joins, guests' joins and removals are decided by the root contract's `approve_partner(address)`,
`approve_guest(address)` and `approve_removal(address)`, called by their fixed selectors with the
subject as argument. A call that returns `true` approves; anything else (`false`, a revert, a
contract without the function) refuses.

This changes one default: today a contract *without* `approve_partner` accepts every join, which
ibc can only know by reading the ABI. With no ABI in execution, an agreement that admits everyone
says so in one line: `function approve_partner(address) external pure returns (bool) { return true; }`.
See [open question 1](#open-questions).

### 4. Grants and consents name function signatures

A grant (the user lets their node sign some calls silently) and a consent (what an app may do
without asking) list function **signatures**, such as `pay(address,uint256)`, instead of names. The
node checks a call against a grant by comparing the first 4 bytes of `data` with each signature's
selector (the first 4 bytes of its keccak256). No ABI is needed, and the check is the same on every
node. Grants and consents stay per source hash: they apply to every contract in any agreement of
that source.

### 5. The user's node holds the ABIs

Each node compiles the source of the agreements its agents are in. It already does: the deployer's
node compiles before the deploy, and a joining node compiles for the source check. From the
compiler's output it keeps, per contract in the source, the ABI and the runtime code (with the
positions of `immutable` values, which differ per instance).

With it, the node can **identify** a contract: compare the code at an address (in its own replica
of the state) with each contract of the source, masking the immutables. This is local, done by the
user's own node, for the user. It is not part of consensus.

The node uses it to:

- **encode** calls and reads given a function name and JSON values (for the approval page, for
  apps, and for the JSON API below);
- **show** a call on the approval page as `Currency.pay(to: 0xB0b…, amount: 10)`, naming the
  contract it identified at `to`; an address it cannot identify is shown as such, with the raw
  data, and a warning;
- **decode** return values and revert reasons for the event stream and read replies.

### 6. Reads and public reads

Reads stay node-local (they change no state and are not ordered). `contract_read` takes either
raw `{to, data}` or `{to, method, values}`, which the node encodes with the identified contract's
ABI and whose reply it decodes. `public_read` works the same, unsigned, with `msg.sender` the zero
address, as today. A read never commits state, so running any function this way is harmless;
contracts keep data private by checking `msg.sender`, as today.

### 7. Results and events

Execution returns the call's raw return data. The event for a write carries it as `return` (hex),
and, when the node identifies the contract, `reply` decoded, as today. Contract logs stay
discarded for now, as today.

### 8. JS client and approval page

- `write`, `read` and the app API (`IbcApp`) take an optional `at`: the address of a contract in
  the agreement (default: the root). Names and JSON values stay the interface for apps; the node
  encodes.
- The approval page shows the identified contract and the decoded call, and signs the calldata.
- Permissions on the approval page list function signatures, grouped by contract of the source.

### 9. Guests

Unchanged in shape: a guest's node forwards the signed transaction to a host, which orders it. The
host executes `{to, data}` like any call.

## What disappears

- ABI encoding inside execution ([common/abi_codec.go](../common/abi_codec.go) leaves the
  consensus path; it remains for the node's JSON API).
- The deploy's `abi` and JSON `constructor` fields.
- Grants by method name.
- The "accept when there is no `approve_partner`" default.
- Any need for facades, Diamonds, combined ABIs or service declarations to build an app from
  several contracts.

## What stays

Personal ledgers; agents, root and node keys; agreements with partners and guests; Cordial Miners
consensus and history replay; the deploy, join, leave and key rotation flow; the source, its hash,
multi-file sources and the source check on join; consents and grants per source hash; public
reads; the precompiles.

## Example: Communities

```solidity
contract Community {
    Membership public membership;
    Currency public currency;
    Collaborations public collaborations;

    constructor() {
        membership = new Membership(msg.sender);
        currency = new Currency(membership);
        collaborations = new Collaborations(membership);
        membership.setCurrency(currency);
    }

    function approve_partner(address who) external view returns (bool) {
        return membership.isMember(who);
    }
}
```

Each part is a plain contract with its own storage and its own checks (`Currency.pay` requires
`membership.isMember(msg.sender)`; `Currency.openAccount` accepts only `msg.sender ==
address(membership)`). Admitting a member opens their account in the same transaction. The app
reads `currency()` once and then calls `pay` at that address: the user's node shows
`Currency.pay(…)`, the user approves, and every partner executes the same calldata. One deploy, one
join and one approval per member, as today.

## Open questions

1. **Membership default.** This proposal refuses a join when the root contract has no
   `approve_partner`. The alternative, accepting, needs the ABI at decision time. Refusing is
   explicit and keeps the ABI out of execution.
2. **Grant scope.** A signature in a grant matches that function on any contract of the
   agreement. Should a grant be able to name a contract of the source too (`Currency:
   pay(address,uint256)`)? Simpler without; more precise with.
3. **Nodes that cannot compile.** A node that could not get the compiler (`source_check:
   unchecked`) cannot identify contracts, so it shows calls raw and returns undecoded results.
   Alternatively the deploy could carry the compiler's output (ABIs and runtime code), checked on
   join like the code, so every node can decode. Simpler wire format without.
4. **Contracts created from data.** A contract can create another from bytes given at runtime
   (`create` in assembly), whose code is not in the source. The node cannot identify it, and shows
   it as unknown. Should the source check warn about such code?

## Where the changes land

| Change | Files |
|---|---|
| Call body, deploy body | [identity/identity.go](../identity/identity.go) |
| Execution of calls and deploys, membership decisions | [common/state.go](../common/state.go), [execution/contract_execution.go](../execution/contract_execution.go), [consensus/router.go](../consensus/router.go) |
| Grants by signature | [identity/envelope.go](../identity/envelope.go), [identity/consent.go](../identity/consent.go), [dispatcher/apps.go](../dispatcher/apps.go) |
| ABIs, identifying contracts, encoding and decoding | [compiler/](../compiler), [dispatcher/dispatcher.go](../dispatcher/dispatcher.go), [common/abi_codec.go](../common/abi_codec.go) |
| Reads, public reads, events | [dispatcher/dispatcher.go](../dispatcher/dispatcher.go), [execution/queries.go](../execution/queries.go) |
| Client and pages | [js/src/ibc.ts](../js/src/ibc.ts), [web/static/approve.mjs](../web/static/approve.mjs), [web/static/contract.mjs](../web/static/contract.mjs), [web/static/index.mjs](../web/static/index.mjs) |
| Docs | [docs/api.md](api.md), [docs/keys.md](keys.md), [docs/usage.md](usage.md), [docs/architecture.md](architecture.md), [docs/consensus.md](consensus.md) |
| Tests and examples | every package's tests, [examples/](../examples), [testdata/](../testdata) |
