# Identity and keys

How an agent is identified, who signs what, and how apps act for a user. The API reference has the
[wire formats](api.md#signatures); this page is the design.

- [1. Two keys](#1-two-keys)
- [2. Addresses](#2-addresses)
- [3. What partners check](#3-what-partners-check)
- [4. Apps, permissions and grants](#4-apps-permissions-and-grants)
- [5. Deploying: the node compiles](#5-deploying-the-node-compiles)
- [6. The signature envelope](#6-the-signature-envelope)
- [7. Passkeys in the web interface](#7-passkeys-in-the-web-interface)
- [8. The approval page](#8-the-approval-page)
- [9. Keys on the command line](#9-keys-on-the-command-line)
- [10. What to know](#10-what-to-know)

## 1. Two keys

An agent has two keys:

| Key | Where | Signs |
|---|---|---|
| **Root key** | The user's device: a passkey, made on the node's web page (or a key file, from the command line) | Everything the user approves: the registration, deploys, joins, leaves, every transaction the user approves, and the user's permissions for apps |
| **Node key** | The user's node (one key per node, shared by the agents it serves) | Consensus blocks; and calls the user lets apps make silently, under the user's **grant** |

The node never holds the root key. The root key is the agent's identity: its address is the agent id
and `msg.sender` in contracts.

## 2. Addresses

An agent's address is the last 20 bytes of the keccak256 of its root public key's coordinates:
Ethereum's formula, applied to P-256 keys (passkeys) and secp256k1 keys (key files) alike.

## 3. What partners check

A transaction reaches partners in one of two forms, and they accept nothing else:

- **Signed by the root key.** Any kind of transaction. The signature proves the user approved it.
- **Signed by the agent's node, under a grant.** Only calls, and only to functions the grant names.
  The grant is signed by the agent's root key and names:

  | Field | Meaning |
  |---|---|
  | `agent` | The agent |
  | `nodeKey` | The node that may sign |
  | `codeHash` | The contract code it is for: the keccak256 of the creation bytecode |
  | `functions` | The functions the node may call silently |

  Every partner checks the grant's signature (`identity.Verify`), that the node signed, that the
  function is listed, and, in execution, that the contract's code is the grant's and that the node
  is still the agent's node in this contract. A grant therefore stops working when the agent moves to
  another node.

Execution also checks that the agent is a partner or a [guest](consensus.md#11-guests) of the
contract. A guest's node is recorded when the guest joins, so a guest's grant works the same way:
its node signs, and the hosts' nodes only pass the transaction on.

Partners cannot tell whether the user watched each silent call; they can tell that the user allowed
these functions, for this code, to this node. A node, even a corrupt one, can sign nothing else in
the user's name.

## 4. Apps, permissions and grants

An app on any site acts for the user through the user's node, never with the user's keys:

- **The app's key.** The app makes a key in the browser, which cannot be extracted, and keeps it.
  The node learns it when the user first approves the app. It identifies the app to the node; it
  signs nothing that reaches partners.
- **Connecting.** An app learns who the user is by sending the user to the node's approval page
  with a `connect` request. The page knows the user from this browser, or the user signs in with
  the passkey, or creates an identity there. The root signs a `Device` message ("connect
  wishes.example.com"), and the node records the app with this browser's key and no permissions.
  The app learns the agent id. The app's first other request does the same if it skipped this.
- **Apps are known by their site.** The node keeps permissions per app site (its origin, which the
  browser vouches for), and each browser's key as one of the app's **devices**. When the user uses
  the app on another computer, its first request that needs the node connects that browser: the
  root signs a `Device` message ("connect wishes.example.com") in the same approval,
  and the app's permissions apply to it. Nothing to choose again.
- **Permissions, per contract code.** The first time an app uses a contract code, the user decides,
  on the node's page:
  - **read without asking:** yes by default;
  - **functions the node may call without asking:** none by default; the user ticks each one.

  The root key signs this choice (a **consent**, kept by the node) and, if functions are ticked, the
  **grant** partners need. The node keeps both, per app and per code, so further contracts of the
  same code need no new permissions.
- **Silent reads:** the app signs its request with its key; the node answers if allowed.
- **Silent calls:** the app asks the node; if allowed, the node signs the call with its own key, under
  the grant, and sends it to consensus.
- **Everything else** (other functions, reads when not allowed, deploys, joins): the app sends the
  user to the node's [approval page](#8-the-approval-page), where the root key signs.
- **Disconnecting** an app (on the node's page, with the root key) makes the node forget all its
  browsers and permissions at once.

Only the agent itself reads its copy of a contract: queries are signed by the root key or by an app
the agent connected, and contracts see the agent as `msg.sender`.

## 5. Deploying: the node compiles

Apps send Solidity **source**, not bytecode. The user's node compiles it (with a fixed compiler and
settings), shows it to the user on request, and packages the deploy transaction: bytecode, ABI,
source, and the compiler's version and settings. The user signs that with the root key. Partners
receive the bytecode; anyone can compile the source again and compare. An app cannot slip in code
the user was not shown.

## 6. The signature envelope

Every message is signed as its [EIP-712](https://eips.ethereum.org/EIPS/eip-712) digest (domain
`{name: "ibc", version: "1"}`); a transaction's id is that digest. The envelope:

| Field | Meaning |
|---|---|
| `scheme` | `secp256k1-eip712` (key files, node keys), `p256` (app keys), `webauthn` (passkeys) |
| `key` | The signing public key, uncompressed (optional for secp256k1) |
| `sig` | The signature |
| `authenticatorData`, `clientDataJSON` | WebAuthn only |
| `certificate` | A node's grant: `{grant, signature}`, the root's signature. Only for node-signed calls |
| `batch` | For one signature over several messages: their digests |

**Batch.** One root signature can cover several messages: it signs the EIP-712 digest of
`Batch { bytes32[] items }`, and each message's envelope carries the list. The approval page uses it
so that one approval covers, for example, an app's permissions, its grant, and the deploy.

Inside consensus, no check uses a clock: every partner reaches the same answer from the block alone.

## 7. Passkeys in the web interface

The root passkey belongs to the node's address: the phone lets only that site use it. The node is the
user's representation on the internet.

- **Create an identity:** two approvals. The page makes the root passkey, then the root signs the
  registration and the permission for this page's own key, in one batch.
- **The page's key.** The node's own page is an app with permission to read every contract of the
  agent. It reads silently; every transaction it makes asks for the root passkey.
- **Sign in on a new device:** the passkey is synced there (iCloud Keychain, Google Password
  Manager) or on a phone nearby. The user chooses it; the page finds the agent by the passkey's
  credential id (`agent_info`), and the root approves a key for this browser's page. Two approvals.
- **Sign out** forgets the identity in this browser; the passkey stays on the user's devices.

Passkeys need a secure page: `https`, or `http://localhost`. They do not work at an IP address.

## 8. The approval page

An app opens `/approve.html` on the user's node in a new window (on a click), and exchanges messages
with it:

1. The page sends `{type: "ibc-ready"}`.
2. The app sends a request with its key (`appKey`):

   | `type` | Fields | The user approves |
   |---|---|---|
   | `connect` | none | Connecting the app: it learns the agent id, with no permissions |
   | `deploy` | `source`, `contract` (name, optional), `name`, `constructor`, `protocol` | The deploy, compiled by the node; and the app's permissions for this code, the first time |
   | `permit` | `contract` | The app's permissions for an existing contract's code |
   | `call` | `contract`, `method`, `values` | A transaction, signed by the root |
   | `read` | `contract`, `method`, `values` | A read, signed by the root |
   | `join` | `contract`, `memberNode`, `memberAgent` | Joining a contract |

3. The page answers `{type: "ibc-done", result}` (with the agent's id) or `{type: "ibc-denied",
   reason}`.

If this browser has no identity on the node, the page first offers to sign in with the passkey, or
to create an identity (a passkey, and the registration: two approvals). The page shows the app's
site (its origin, which the browser vouches for), what it asks, and the code behind a link. If the request comes from a new browser of an app the user approved before, the
approval also connects that browser (a `Device` message in the same signature). `IbcApp` in `ibc.mjs` does all of this for an app: it tries silently first, and
opens the page when the node says the user must approve.

## 9. Keys on the command line

```sh
ibc key new alice.key                                 # a root key (secp256k1); prints the agent id
ibc sign registration -key alice.key -node URL -node-key 0x57E3…
ibc sign tx -key alice.key -kind call -contract ID -body '{"method": "increment"}'
ibc sign query -key alice.key -action get_contracts
```

The root key file signs everything; `ibc.sh` uses it.

## 10. What to know

- **The root passkey belongs to the node's address.** If the node moves, the root cannot be used at
  the new address. A native app for the root key will lift this.
- **Losing every device that holds the root** loses the identity; recovery through trusted friends is
  future work.
- **The node is trusted with the silent calls you allow,** and only those: partners refuse anything
  else it signs.
- **Revoking a grant on other nodes** is not needed: only your node can use it, and your node forgets
  it when you disconnect the app. A grant ends for everyone when you move to another node.
- **The node's operator** can read everything stored on the node.
