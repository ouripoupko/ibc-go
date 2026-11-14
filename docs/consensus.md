# Consensus design

This is the design of ibc's consensus protocol, Cordial Miners, as implemented in
[`consensus/cordial`](../consensus/cordial). It is based on the paper by Keidar, Naor, Poupko and
Shapiro, *Cordial Miners: Fast and Efficient Consensus for Every Eventuality* (DISC 2023), and
follows the engineering choices of the
[CordialMiners prototype](https://github.com/ouripoupko/CordialMiners). Every place where it
differs from the prototype is listed in [§12](#12-deviations-from-the-prototype), together with the
one prototype rule kept despite an open question.

Terms follow the paper: miner, block, blocklace, depth (round), cordial, supermajority, approve,
ratify, super-ratify, wave, leader, final, τ.

- [1. Goals](#1-goals)
- [2. Contracts, protocols and miners](#2-contracts-protocols-and-miners)
- [3. Keys and signatures](#3-keys-and-signatures)
- [4. Transactions](#4-transactions)
- [5. Blocks](#5-blocks)
- [6. The blocklace](#6-the-blocklace)
- [7. Rounds, leaders and ordering](#7-rounds-leaders-and-ordering)
- [8. Dissemination](#8-dissemination)
- [9. From τ to execution](#9-from-τ-to-execution)
- [10. Membership changes](#10-membership-changes)
- [11. Guests](#11-guests)
- [12. Deviations from the prototype](#12-deviations-from-the-prototype)
- [13. Decisions](#13-decisions)
- [14. Not in this version](#14-not-in-this-version)

## 1. Goals

- **Safety.** All correct partners of a contract execute the same sequence of transactions, and so
  reach the same state roots.
- **Liveness.** Every transaction submitted by a correct partner is eventually executed, while a
  supermajority of partners is correct, online and connected.
- **Fault tolerance.** A contract with *n* partners tolerates *f* = ⌊(n−1)/3⌋ Byzantine partners.
- **Self-sovereignty.** Only an agent can create its own transactions. Nodes never hold an agent's
  personal key.

Where safety and liveness compete, this design gives up liveness. In particular, a contract stops
making progress while fewer than a supermajority of its partners are online, and a membership
change freezes the contract until the change is final (§10).

A set of miners is a **supermajority** if it has more than (n+f)/2 members:

| n | 1 | 2 | 3 | 4 | 5 | 7 | 10 |
|---|---|---|---|---|---|---|---|
| f | 0 | 0 | 0 | 1 | 1 | 2 | 3 |
| supermajority | 1 | 2 | 2 | 3 | 4 | 5 | 7 |

There is no limit on the number of partners, and no optimization for large contracts:
dissemination is all-to-all.

## 2. Contracts, protocols and miners

- **Each contract chooses its consensus protocol** when it is deployed, with the deploy's
  `protocol` field, and keeps it for its lifetime:
  - `cordial` (the default, also used for an empty field and for the Python value `BFT`) is this
    protocol;
  - `immediate` orders the creator's transactions locally, with no partners. It exists for testing.
- **Each contract has its own blocklace.** Its miners are exactly the contract's partners (agents).
  A contract may also have **guests**, who are not miners (§11).
  Contracts never wait for each other.
- **An agent's miner runs on the agent's node.** A node runs one miner per (agent, contract). Two
  partners hosted on the same node are two miners.

## 3. Keys and signatures

Agents' keys are described in [Identity and keys](keys.md); in short:

| Key | Held by | Signs |
|---|---|---|
| An agent's **root key** | The agent: a passkey (P-256), or a secp256k1 key file | The agent's registration and transactions, and grants for its node |
| The **node key** | The node; one per node, created on first start (secp256k1) | Blocks; and calls an agent allows, under the agent's grant |

- **The agent id is the root key's address** (`0x…`, 40 hex digits). It is also the agent's
  `msg.sender` in contracts.
- **Nodes never hold personal keys.** A node signs blocks with its own key, and calls only under an
  agent's grant, which names the code and the functions.
- **An agent authorizes its node by registering.** The registration names the node and its node key
  and is signed by the agent. Contracts learn a member's node key from the member's deploy or join
  transaction, which is signed by the member's root key.
- **A node cannot forge an agent's transactions.** It can delay or drop them, and it can misbehave
  in consensus. A misbehaving node is replaced with a `rotate_key` transaction (§10).

## 4. Transactions

A transaction is signed by the agent's root key, or, for a call the agent allows silently, by its
node under the agent's grant, as its EIP-712 digest, in an envelope that carries the key and the
grant. Any partner checks it
from the block alone, without a clock ([Identity and keys](keys.md#6-the-signature-envelope)).

```
domain:  { name: "ibc", version: "1" }
Transaction {
  contract: string    the contract id; empty for a deploy
  kind:     string    deploy | call | join | leave | rotate_key
  agent:    address   the signer
  nonce:    string    random, so equal requests are still different transactions
  body:     string    JSON, by kind:
              deploy:     { name, contract, protocol, default_app, profile,
                            code, abi, constructor, node, nodeKey }
              call:       { method, values }
              join:       { node, nodeKey, profile }
              leave:      { member }
              rotate_key: { nodeKey }
}
```

- The **transaction id** is the EIP-712 digest. A deploy's id is the new contract's id.
- A transaction is **valid** if the signature recovers `agent`, the body parses, and it belongs
  to the contract. That depends only on the transaction, so every correct miner agrees.
- **Each transaction is executed at most once.** τ's output skips transaction ids it has already
  output (§9).
- A transaction's **time** is the time of the block that carries it; that is `block.timestamp`.

## 5. Blocks

```
Block {
  contract    the contract id
  config      the index of the membership configuration (§10)
  creator     the agent's address
  depth       the block's round
  time        the creator's clock, unix microseconds
  pointers    hashes of earlier blocks
  payload     signed transactions
  stateRoot   the creator's state root after the last output it executed (cross-check)
  signature   by the node key the configuration holds for the creator
}
```

Blocks are encoded with RLP. A block's hash is the keccak256 of its encoding without the signature.

### Validity

A block is added to the blocklace only if every check passes. Each check depends only on the
block, the blocks it points to and its configuration, so all correct miners agree.

1. **Configuration.** `config` is a configuration the miner knows, `creator` is one of its members,
   and the signature recovers that member's node key.
2. **Pointers.** All pointed blocks are present (otherwise the block waits in a buffer, §8), and
   they are blocks of the same configuration. The exception is the configuration's anchor (§10).
3. **The first round of a configuration** (depth 0 for configuration 0; the anchor's depth + 1
   otherwise): the block points to nothing (configuration 0) or to exactly the anchor.
4. **Later rounds:**
   - **Cordial.** The pointers include blocks of depth − 1 from a supermajority of the members.
   - **One pointer per creator.** No two pointers refer to blocks of the same creator, and every
     pointed block has a smaller depth.
   - **Self-chain** (from the prototype). At most one pointer refers to the creator's own block. A
     correct miner always points to its previous block; a block with none starts a new chain, and
     two chains of one creator are an equivocation.
5. **Time.** `time` is at least the time of every pointed block.
6. **Payload.** Every transaction is valid (§4) and belongs to the contract. `call`, `leave` and
   `rotate_key` transactions are by members; a `deploy` appears only in the creator's first block
   of configuration 0.

## 6. The blocklace

Each miner keeps the blocklace of its contract in the node's Pebble database: blocks by hash,
configurations, the miner's own blocks (written **before** they are sent, so that a restart cannot
make a second block at the same depth), the mempool, and the progress of τ. Indexes by depth and
creator are rebuilt in memory on start.

### Observation summaries

Approval must depend only on the approving block (§11). To compute it exactly and fast, every
block gets an **observation summary** when it is added:

> summary(b): for each creator *c*, the maximal blocks of *c* that *b* observes. One block
> normally; two or more if *b* observes an equivocation by *c*.

summary(b) merges the summaries of the blocks *b* points to, plus *b* itself. With it:

- *b* **observes** *x* if a block in summary(b)[creator(x)] is *x* or descends from *x* along the
  creator's chain;
- *b* **approves** *x* if *b* observes *x* and **every** block in summary(b)[creator(x)] is *x* or
  descends from it. Otherwise *b* sees a block of that creator that does not observe *x*: an
  equivocation.

These are the paper's definitions (Defs. 16–18), and they depend on *b* alone.

## 7. Rounds, leaders and ordering

These are the rules of the eventual-synchrony variant, as the prototype implements them.

- **Waves** have *w* = 3 rounds, counted from the configuration's first round.
- **Leaders.** The leader of the wave starting at depth *d* is the member at position
  ((d − first round) / w) mod n, members sorted by address.
- **Rounds.** Round *r* is cordial when it has blocks from a supermajority. The completed round is
  the last round such that it and all rounds before it in the configuration are cordial.
- **Creating blocks.** A miner's next block is always **one round above its last** (its first block
  is in the configuration's first round). It may create it once its last round is cordial, counting
  only members it does not know to have equivocated. It points to each such member's latest block
  below the new depth (walking the member's chain down, as the prototype does), its own previous
  block included.
- **Idle rule, with catch-up.** A miner creates a block only if it has transactions in its
  mempool, the blocklace has transactions that τ has not output yet (blocks of known equivocators
  do not count), a membership change is pending, it has no block yet in the current configuration,
  or another member already has a block in the round it would create. Contracts with nothing to
  order produce no blocks.
- **Ratification.** *b* **ratifies** *x* if *b*'s closure has blocks approving *x* from a
  supermajority. A set of blocks **super-ratifies** *x* if it has blocks from a supermajority, each
  of which ratifies *x*.
- **Finality** (the prototype's window, an [open issue](#open-issue-the-finality-window)). A leader
  block of depth *r* is final if blocks of depth *r*+1 to *r*+3 super-ratify it.
- **τ.** For the last final leader, τ walks back through each leader's previous ratified leader,
  until it reaches output it has produced or the configuration's anchor. It then outputs, from the
  oldest leader on, each leader's blocks not yet output, in the prototype's order: depth-first
  along the pointers, in the order the block lists them. Like the prototype, τ does not filter out
  equivocating blocks; transactions are signed by users and output at most once, so an
  equivocating node can neither forge nor duplicate anything.

## 8. Dissemination

Messages between nodes travel in `a2a_consent` requests:

- **Push.** A miner sends each block it creates to every other member.
- **Pull.** A block whose pointers are not all known waits in a buffer, and the miner asks the
  block's creator for the missing blocks by hash; unanswered requests are repeated to every member.
- **Catch-up.** A miner that has work to do but has neither received nor made a block for a second
  asks every member for its blocks from the round of the last final leader on (`get_from`), and
  sends its own latest block again. This recovers from lost messages: a block nobody points to
  cannot be pulled by hash, and a member that missed a block may be idle without knowing a round
  waits for it.
- **During a join,** blocks of the new configuration that reach the newcomer's node before its
  sync finishes are kept and given to its miner afterwards.
- **Relay.** A client may send a transaction to any member's node; joins are relayed this way.
- Senders need no authentication: blocks and transactions carry their own signatures.

## 9. From τ to execution

τ's output is a sequence of blocks, each with transactions. In that order, each transaction not
output before becomes a record with the next ledger index, and goes to
`execution.Navigator.Submit`. Execution then works as before: the record is written to the ledger
first, and applied by the contract's goroutine.

- τ's progress (output blocks, output transaction ids, the next index) is saved after the records
  are submitted. After a crash, the step is repeated, and the navigator ignores indexes it already
  has.
- `msg.sender` is the transaction's agent, and `block.timestamp` is the carrying block's time.

## 10. Membership changes

A contract's **configuration** is its members and each member's node key. The deploy creates
configuration 0, with the creator alone. `join`, `leave` and `rotate_key` transactions change it.

### The rule

1. **A change is an ordinary transaction,** ordered by the current members.
2. **Freeze.** While a block of the configuration holds a change that τ has not output, correct
   miners put no other transactions in their blocks; incoming ones wait in the mempool. The idle
   rule keeps them making (empty) blocks until the change is output.
3. **Decide.** When τ outputs the change, execution applies it at its place in the order:
   - a `join` names the hash of the contract's source as the joining agent read it
     (`sourceHash`); if that is not the hash of the source in the contract's deploy, it is
     refused. Otherwise `join` calls the contract's `approve_partner(address)` as the joining
     agent; a contract without that function accepts every join (as the Python default did);
   - `leave` of oneself is always accepted; removing another member calls the contract's
     `approve_removal(address)` as the requesting member, and a contract without that function
     refuses;
   - `rotate_key` by a member is always accepted. It names the agent's new node (URL and key):
     rotating a key means moving to another node, or replacing a compromised one.
4. **Switch at the leader.** Let *L* be the leader whose part of τ's output holds the change. If
   the change is accepted, configuration *k* ends at *L*: τ outputs nothing after *L*'s part.
   Configuration *k*+1 starts with *L* as its **anchor**: its first round is *L*'s depth + 1, and
   its first blocks point to *L* alone. Blocks of configuration *k* that *L* does not observe are
   left out; the freeze keeps correct miners' blocks there empty, and any transaction left out is
   returned to its node's mempool. A rejected change unfreezes configuration *k* with no switch.
5. **New nodes** (a newcomer's, or a member's new node after `rotate_key`) receive a welcome from
   each member, with the new configuration and where the change left the contract: its ledger
   index, the state root after it, and the hash of the record ids up to it. A member's old node,
   after a rotation, stops taking part.
6. **The new node joins on what the members agree on, not on one member's word.** It waits for f+1
   welcomes (of the members before the change) that agree, so at least one is from a correct
   member. It then fetches the ledger, first from the member the agent joined through, and keeps
   the first history that checks: its first record is the deploy whose id is the contract id,
   every record is signed by its agent, its record ids hash to the welcomes' hash, and it replays
   to their state root. If a member gives a fake history, the node takes a correct member's
   instead: a corrupt member cannot leave the joiner out by lying, and so cannot fill the
   configuration with members who never take part.
7. **The source check.** The new node then compiles the deploy's source with the compiler the
   deploy names (`compiler`: version and settings), and compares the result with the deployed
   code. If they differ (`mismatch`), the contract is not what its source says, and the node does
   not join: it keeps nothing, and the agent hears why. Its absence then costs the contract's
   members some liveness, but only members who did not check, since a correct member's node
   checks the same way. If it cannot compile (`unchecked`: it could not get that compiler version),
   it joins, and records the warning with the agent's copy. This check is local to the node, not
   part of consensus: nodes without the same compiler would decide differently.

### Why it is safe

The argument is that every correct miner switches at the same point, with the same state. The
paper's proofs cover each configuration on its own, since membership is fixed within it.

1. **Every correct miner outputs the same sequence within configuration *k*,** by τ's safety
   (Theorem 8) for a fixed set of miners.
2. **The change has the same place, and the same leader *L*, at every correct miner.** τ's output
   is built from leaders' parts, and a final leader's part is the same everywhere: every later
   leader's chain of ratified leaders passes through it (leader safety, Prop. 3), so the part
   below it, and so its own, are fixed. (The finality window kept from the prototype puts this
   step under the [open issue](#open-issue-the-finality-window).)
3. **The decision is the same at every correct miner,** since execution is deterministic and runs
   the same records in the same order.
4. **So every correct miner ends configuration *k* at the same *L*, with the same state.** A miner
   that learns of *L*'s finality late keeps making blocks of configuration *k* until then; since
   τ never outputs anything after *L* in configuration *k*, those blocks change nothing. That is
   why the switch is tied to *L* itself rather than to a later round: a rule like "switch at the
   next wave" would need every miner to know of *L*'s finality before reaching that wave, which
   no miner can be sure of.
5. **Configuration *k*+1 starts from the same point everywhere,** because its first blocks point to
   *L* alone and τ stops at the anchor. Within it, the paper's argument applies again.
6. **Nothing is lost or executed twice.** Transactions in left-out blocks return to mempools and
   are proposed again; output skips transaction ids already output.
7. **Liveness is what is paid.** The freeze holds other transactions back until the change is
   output, and the new configuration needs a supermajority of its members, including newcomers,
   before it progresses.

### Consequences

- **One change at a time.** Changes made during a freeze wait for the next configuration.
- **A disappearing member can stop a small contract for good.** Removing it needs a supermajority
  of the current members, which in a two-member contract is both.
- **Key rotation takes effect at the next switch.** Until then a stolen node key can act as one
  Byzantine miner.

## 11. Guests

A **guest** of a contract uses it like a partner, but takes no part in its consensus. The guest's
node keeps no copy of the contract: it sends the guest's transactions and reads to a **host**, one of
the contract's members (its full partners), and receives the contract's events from it. The guest
still talks only to its own node, and the contract does not get slower: guests add no miners, and
their joins do not freeze it.

The price is trust: a guest trusts its hosts for what it reads, and for passing its transactions on.
The hosts cannot forge the guest's transactions, since every partner checks their signatures.

### Joining

1. The guest signs a `join` whose body says `"guest": true` and names its host: `"host"`, the member
   whose node it sends the join to. Its node sends it there, as for any join.
2. The members order it like any transaction, but it is not a configuration change: no freeze, no
   switch. Execution calls the contract's `approve_guest(address)` as the joining agent. **A contract
   without that function refuses every guest**: guests are something a contract opts in to.
3. If approved, every member records the guest in the contract's partners, with the role `guest`,
   its node and node key, and its host. The members' nodes send the guest's node a **welcome**: the
   contract's members (the hosts) and the contract's deploy record. The guest's node checks that the
   deploy is signed by the creator and that its id is the contract's id, so it knows the contract's
   code, ABI and source for certain, and compiles the source against the code as a partner's node
   does (§10). It then adds the contract to the agent's list of contracts, with the role `guest`;
   on a mismatch, it does not. As for a partner, the guest's join names its source hash, and is
   refused if that is not the contract's.

### Using the contract

- **Transactions.** The guest's node sends each transaction to a host: the one it joined through
  first, then the others. The host accepts a guest's transaction only from the guest's node (the
  node key the guest joined with). Members order it with their own, and execution runs it with the
  guest as `msg.sender`. Execution refuses a call from an agent that is neither a partner nor a guest,
  so a member cannot slip in a stranger's transaction.
- **Silent calls** work as for partners: the guest's node signs under the guest's grant, and
  partners check that the grant's node is the guest's node in this contract.
- **Reads.** The guest's node sends the read to a host, signed by its node key. The host runs it on
  its copy, as the guest, and answers. Only `view` and `pure` functions, as for partners.
- **Events.** The guest's host sends the guest's node every reply of the contract, with the current
  members, so the guest's list of hosts stays up to date. If the host is no longer a member, the
  first member sends them.
- **Leaving.** A guest may always leave. Its node forgets the contract at once. Removing a guest
  calls the contract's `approve_removal`, as for a member.

### Limits

- A guest cannot rotate its key: it leaves and joins again from its new node.
- A guest cannot become a partner while it is a guest: it leaves first.
- A removed guest's node learns of it at its next transaction or read, which the host refuses.
- A guest's node does not check what its host answers to a read; a dishonest host can lie.
  Comparing the answers of several hosts is future work.
- Only Cordial Miners contracts take guests.

## 12. Deviations from the prototype

| Prototype | This design | Why |
|---|---|---|
| `approves` uses the miner's own list of equivocators, and `leaf_of_creator` follows a creator's chain below a fork | Approval from the approving block's observation summary (§6) | **Safety.** Approval must depend on the block alone. Otherwise two correct miners can compute different approvals for the same block and disagree on finality. |
| No signatures, hashes not verified | Signed transactions (EIP-712) and blocks | Without signatures any node can forge any miner's blocks. |
| Own blocks only in memory | Saved before they are sent | A restarted miner must not make a second, different block at the same depth. |
| Whole-blocklace scans | Indexes by depth and creator; summaries | Performance. |
| Fixed miners, every block points to its creator's previous block | Configurations (§10); a missing self-pointer starts a new chain | ibc's partners change. |
| A miner jumps to the completed round | It never skips a round | **Liveness**, found by the simulator. Miners learn of equivocators at different times. With 7 miners and 2 equivocators, a miner that counted an equivocator's block skipped a round; miners that knew of the equivocation then saw that round without a supermajority of eligible blocks, forever. |
| A miner makes a block only for its own messages or for unordered blocks | Also when another member has started the round (catch-up rule) | **Liveness**, found by the simulator. A miner that had already made its block for round *r* got a new transaction; the others were idle, so round *r* never completed and the transaction waited forever. |
| Rounds counted with every creator | The round a correct miner builds on counts only members not known to equivocate | **Liveness.** Otherwise a round can look complete thanks to an equivocator's block, while correct miners, who do not point to equivocators, cannot build on it. |
| — | Catch-up pull and re-push when stalled (§8) | **Liveness** with lost messages, found by the simulator dropping 30% of blocks. |

Kept from the prototype: advancing without a timeout, round-robin leaders, the finality window,
choosing pointers by walking members' chains below the current round, the idle rule, buffering
until pointers resolve, and outputting all of a leader's unoutput blocks.

### Open issue: the finality window

The prototype counts ratifiers of a leader at depth *r* in rounds *r*+1 to *r*+3. The paper's
Def. 24 stops at *r*+2 (*r* + *w* − 1). The prototype's window is kept by decision: it lets leaders
become final more often on real, irregular networks.

The concern: the paper's proof that every later leader ratifies a final leader (Prop. 3, Fig. 2)
needs the ratifying blocks to be below the next leader's round, which is *r*+3. A ratifier at
*r*+3 may be unseen by the next leader. No run producing disagreement has been found, but the proof
does not cover this window.

| Variant | w | Ratifiers up to | Next leader | Covered by the proof |
|---|---|---|---|---|
| Paper | 3 | *r*+2 | *r*+3 | Yes |
| **Prototype (used)** | 3 | *r*+3 | *r*+3 | No |
| Alternative | 4 | *r*+3 | *r*+4 | Yes |

The window and the wavelength are parameters of the implementation, and the simulator tests run
all three variants with equivocating miners.

## 13. Decisions

| # | Decision |
|---|---|
| D1 | The agent id is the address of the agent's root key |
| D2 | No custodial mode: nodes never hold personal keys; users sign with passkeys ([keys](keys.md)) |
| — | One node key per node, authorized by each hosted agent's registration |
| — | secp256k1 keys; EIP-712 signatures |
| D3 | One blocklace; membership changes freeze the contract until output, and switch at the leader that output them (§10) |
| D4 | τ does not filter equivocating blocks (as the prototype) |
| D5 | The prototype's finality window, tracked as an open issue (§12) |
| D6 | No shortcut for single-member contracts |
| D7 | Push own blocks; pull missing ones by hash |
| D8 | Blocks carry the creator's state root |
| D9 | A member may always leave; removing another needs the contract's `approve_removal` |
| D10 | Guests: a contract may take guests (`approve_guest`), who use it through a host and are not miners (§11) |
| D11 | Joins name the contract's source hash; a new node joins on f+1 members' agreed point, from any member whose history checks, and only if the source compiles to the code (§10) |
| — | Each contract chooses its protocol; one blocklace per contract |
| — | No limit on partners, no optimizations for large contracts |

## 14. Not in this version

- The personal two-pointer ledger of the ssDA paper (same-contract and same-agent pointers).
- Recovery of a lost root key through trusted friends, and a native
  app for the root key.
- A timeout in eventual synchrony, and the asynchronous variant with a shared coin.
- Contract events (logs).
