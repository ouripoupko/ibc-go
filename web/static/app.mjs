// Shared code of the web interface's pages: connecting the wallet, small DOM
// helpers, and forms built from a contract's ABI.
//
// Everything shown comes from the node or from contracts, so it is always
// inserted as text (textContent), never as HTML.
import { Ibc, IbcError } from './ibc.mjs';

export { IbcError };

// How long a read session lasts. The node accepts up to 7 days.
export const SESSION_HOURS = 8;

/**
 * Connects to this node as the agent of the browser wallet's account. Reads
 * are signed by a read session, kept in this browser (IndexedDB) so that it
 * outlives the page: the wallet is asked once, when the first read needs it.
 */
export async function connect() {
  if (!window.ethereum) {
    throw new Error('No browser wallet was found. Install one (see the list below), then reload this page.');
  }
  const ibc = await Ibc.connect(location.origin, explained(Ibc.wallet(window.ethereum)));
  window.ethereum.on?.('accountsChanged', () => location.reload());
  if (Ibc.sessionsAvailable()) {
    ibc.sessionHours = SESSION_HOURS;
    const id = `${location.origin} ${ibc.agent.toLowerCase()}`;
    ibc.useSession(await sessionStore('get', id));
    ibc.onSession((s) => {
      sessionStore(s ? 'put' : 'delete', id, s ? { session: s.session, signature: s.signature, key: s.key } : undefined);
      showSession(ibc);
    });
  }
  showSession(ibc);
  return ibc;
}

// The node's storage: the size of its data folder, from node_info (public).

/** Bytes in a short form: 1.2 GB. */
export function sizeText(bytes) {
  const units = ['bytes', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let n = bytes;
  while (n >= 1000 && i < units.length - 1) {
    n /= 1000;
    i++;
  }
  return i === 0 ? `${n} bytes` : `${n.toFixed(n < 10 ? 1 : 0)} ${units[i]}`;
}

async function showStorage() {
  const footer = document.querySelector('#storage');
  if (!footer) return;
  try {
    const info = await (await fetch(`${location.origin}/ibc/app?action=node_info`)).json();
    if (typeof info.storage === 'number') {
      footer.textContent = `Node storage: ${sizeText(info.storage)}`;
      footer.title = `${info.storage.toLocaleString()} bytes in the node's data folder`;
    }
  } catch {
    // no footer
  }
}
showStorage();

// Explaining signatures. Every time the wallet is asked to sign, a message
// box says in plain words what the signature does. It has no close button:
// it closes when the wallet answers, whether the user signed or declined.

/** Wraps a wallet signer so that each signature request is explained first. */
function explained(signer) {
  return {
    getAddress: () => signer.getAddress(),
    async signTypedData(domain, types, message) {
      const primaryType = Object.keys(types).find((t) => t !== 'EIP712Domain');
      const close = openSigningBox(explain(primaryType, message));
      try {
        return await signer.signTypedData(domain, types, message);
      } finally {
        close();
      }
    },
  };
}

let signingBox;
const openBoxes = [];

/** Shows the message box; returns a function that closes it. */
function openSigningBox(text) {
  if (!signingBox) {
    signingBox = el('dialog', { class: 'signing', 'aria-labelledby': 'signing-title' });
    // No way to dismiss it: Escape is ignored, and if the browser closes it
    // anyway while the wallet is still open, it comes back.
    signingBox.addEventListener('cancel', (e) => e.preventDefault());
    signingBox.addEventListener('close', () => { if (openBoxes.length) signingBox.showModal(); });
    document.body.append(signingBox);
  }
  const entry = { text };
  openBoxes.push(entry);
  renderBox();
  return () => {
    openBoxes.splice(openBoxes.indexOf(entry), 1);
    if (openBoxes.length) renderBox();
    else signingBox.close();
  };
}

function renderBox() {
  signingBox.replaceChildren(
    el('h2', { id: 'signing-title' }, openBoxes[openBoxes.length - 1].text),
    el('p', {}, 'Waiting…'));
  if (!signingBox.open) signingBox.showModal();
}

function parseBody(text) {
  try {
    return JSON.parse(text);
  } catch {
    return {};
  }
}

/** Argument values, space separated: "increment 7". */
const values = (v) => Object.values(v ?? {}).map((x) => (typeof x === 'string' ? x : JSON.stringify(x))).join(' ');
const words = (...parts) => `Sign request: ${parts.filter((p) => p !== '' && p !== undefined).join(' ')}`;

/** One line saying what is being signed, by its EIP-712 type. */
function explain(type, m) {
  const body = parseBody(m.body ?? '');
  const self = String(body.member).toLowerCase() === String(m.agent).toLowerCase();
  switch (`${type}:${m.kind ?? m.action ?? ''}`) {
    case 'Registration:': return words('register on', location.host);
    case 'Session:': return words('read session until',
      new Date(Number(m.expires) * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }));
    case 'Query:contract_read': return words('read', body.method, values(body.values));
    case 'Query:get_contracts': return words('list contracts');
    case 'Query:get_contract': return words('open contract', short(m.contract));
    case 'Query:stream': return words('open events');
    case 'Transaction:deploy': return words('deploy', body.name, values(Object.hasOwn(body, 'constructor') ? body.constructor : {}));
    case 'Transaction:call': return words(body.method, values(body.values));
    case 'Transaction:join': return words('join', short(m.contract));
    case 'Transaction:leave': return self ? words('leave', short(m.contract)) : words('remove', short(body.member));
    case 'Transaction:rotate_key': return words('move to', body.node);
    default: return words(type, m.kind ?? m.action ?? '');
  }
}

/** Reads or writes the kept sessions; any storage failure means "no session kept". */
async function sessionStore(op, id, value) {
  try {
    const db = await new Promise((resolve, reject) => {
      const open = indexedDB.open('ibc', 1);
      open.onupgradeneeded = () => open.result.createObjectStore('sessions');
      open.onsuccess = () => resolve(open.result);
      open.onerror = () => reject(open.error);
    });
    return await new Promise((resolve, reject) => {
      const store = db.transaction('sessions', op === 'get' ? 'readonly' : 'readwrite').objectStore('sessions');
      const req = op === 'put' ? store.put(value, id) : store[op](id);
      req.onsuccess = () => resolve(req.result ?? null);
      req.onerror = () => reject(req.error);
    }).finally(() => db.close());
  } catch {
    return null;
  }
}

/** Shows the read session in the header (#session), with a button to end it. */
function showSession(ibc) {
  const holder = document.querySelector('#session');
  if (!holder) return;
  if (!Ibc.sessionsAvailable()) {
    holder.textContent = 'Reads are signed one by one (this page is not secure: use https)';
    return;
  }
  if (!ibc.session) {
    holder.textContent = 'No reading session';
    return;
  }
  const until = new Date(ibc.session.session.expires * 1000).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' });
  holder.replaceChildren(`Reading session until ${until} · `,
    el('button', { class: 'link', title: 'Forget the session key: the next read asks your wallet again', onclick: () => ibc.endSession() }, 'End'));
}

/** A note for a status line: whether the wallet will ask before reading. */
export function readNote(ibc) {
  return ibc.sessionValid() || !ibc.sessionHours ? '' : ' (approve a reading session in your wallet)';
}

export const $ = (selector, root = document) => root.querySelector(selector);

/** Creates an element: el('td', { class: 'id' }, 'text', child, …). */
export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === null || value === false) continue;
    if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
    else if (key === 'class') node.className = value;
    else node.setAttribute(key, value === true ? '' : value);
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null) continue;
    node.append(child instanceof Node ? child : String(child));
  }
  return node;
}

/** Shows a status line: kind is '', 'ok' or 'bad'. */
export function status(node, text, kind = '') {
  node.textContent = text;
  node.className = `status ${kind}`.trim();
}

/** Runs fn while button is disabled; shows a thrown error in statusNode. */
export async function busy(button, statusNode, fn) {
  button.disabled = true;
  try {
    return await fn();
  } catch (err) {
    status(statusNode, errorText(err), 'bad');
  } finally {
    button.disabled = false;
  }
}

/** A readable message for an error from the wallet, the node or the network. */
export function errorText(err) {
  if (err?.code === 4001) return 'You declined the signature in your wallet.';
  if (err instanceof TypeError && /fetch/i.test(err.message)) return 'The node could not be reached.';
  return err?.message ?? String(err);
}

/** Pretty JSON for a result. */
export function pretty(value) {
  return typeof value === 'string' ? value : JSON.stringify(value, null, 2);
}

/** Errors come back from contracts as strings in place of a result. */
export function isErrorReply(value) {
  return typeof value === 'string' && /^(execution reverted|out of gas|contract deployment failed|unknown method|\w+: (missing|unexpected) argument|\w+: argument )/.test(value);
}

export function short(id) {
  return typeof id === 'string' && id.length > 14 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id;
}

/** Reads a file input as text. */
export function readFile(input) {
  const file = input.files?.[0];
  return file ? file.text() : Promise.resolve(null);
}

// Invites: a member shares the contract id, its node and its agent id, so
// another agent can ask to join from its own node.

export function encodeInvite(invite) {
  const bytes = new TextEncoder().encode(JSON.stringify(invite));
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

export function decodeInvite(code) {
  const b64 = code.trim().replace(/-/g, '+').replace(/_/g, '/');
  const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
  const invite = JSON.parse(new TextDecoder().decode(bytes));
  if (!invite?.contract || !invite?.node || !invite?.agent) throw new Error('not an ibc invite');
  return invite;
}

// ABI forms.

/** Parses ABI JSON text (an array, or an object with an "abi" array). */
export function parseAbi(text) {
  const abi = JSON.parse(text);
  const list = Array.isArray(abi) ? abi : abi?.abi;
  if (!Array.isArray(list)) throw new Error('the ABI must be a JSON array');
  return list;
}

/** The type as Solidity writes it, with tuple components spelled out. */
export function typeName(param) {
  if (!param.type.startsWith('tuple')) return param.type;
  const inner = (param.components ?? []).map((c) => `${typeName(c)} ${c.name}`.trim()).join(', ');
  return `(${inner})${param.type.slice(5)}`;
}

export function signature(fn) {
  const ins = (fn.inputs ?? []).map((p) => `${typeName(p)} ${p.name ?? ''}`.trim()).join(', ');
  const outs = (fn.outputs ?? []).map((p) => `${typeName(p)} ${p.name ?? ''}`.trim()).join(', ');
  return `${fn.name ?? 'constructor'}(${ins})${outs ? ` returns (${outs})` : ''}`;
}

export function isRead(fn) {
  return fn.stateMutability === 'view' || fn.stateMutability === 'pure' || fn.constant === true;
}

/**
 * Builds the inputs for a list of ABI parameters. Returns the element and a
 * read() that returns the values object the API expects, keyed by name (or
 * by position for unnamed parameters), or throws a readable error.
 */
export function paramInputs(params = []) {
  const fields = params.map((param, i) => {
    const key = param.name || String(i);
    const id = `p-${Math.random().toString(36).slice(2)}`;
    const label = el('label', { for: id }, param.name || `#${i}`, ' ', el('span', { class: 'type' }, typeName(param)));
    let input;
    let read;
    if (param.type === 'bool') {
      input = el('input', { type: 'checkbox', id });
      read = () => input.checked;
      return { key, node: el('label', { class: 'check' }, input, param.name || `#${i}`, ' ', el('span', { class: 'type' }, 'bool')), read };
    }
    if (param.type.endsWith(']') || param.type.startsWith('tuple')) {
      input = el('textarea', { id, placeholder: placeholderFor(param), spellcheck: 'false' });
      read = () => {
        try {
          return JSON.parse(input.value);
        } catch {
          throw new Error(`${param.name || `#${i}`}: enter JSON, for example ${placeholderFor(param)}`);
        }
      };
    } else {
      input = el('input', { type: 'text', id, placeholder: placeholderFor(param), spellcheck: 'false', autocomplete: 'off' });
      read = () => {
        const v = input.value.trim();
        if (v === '' && !param.type.startsWith('string')) throw new Error(`${param.name || `#${i}`} is empty`);
        return param.type === 'string' ? input.value : v; // integers go as strings: exact at any size
      };
    }
    return { key, node: el('div', {}, label, input), read };
  });
  return {
    node: el('div', {}, fields.map((f) => f.node)),
    read() {
      return Object.fromEntries(fields.map((f) => [f.key, f.read()]));
    },
  };
}

function placeholderFor(param) {
  const t = param.type;
  if (t.endsWith(']')) {
    const base = { ...param, type: t.slice(0, t.lastIndexOf('[')) };
    return `[${placeholderFor(base)}, …]`;
  }
  if (t.startsWith('tuple')) {
    const parts = (param.components ?? []).map((c) => `"${c.name}": ${placeholderFor(c)}`);
    return `{${parts.join(', ')}}`;
  }
  if (t === 'address') return '"0x…"';
  if (t.startsWith('uint') || t.startsWith('int')) return '42';
  if (t.startsWith('bytes')) return '"0x…"';
  if (t === 'string') return '"text"';
  return t;
}

/**
 * A form for one function. run(values) performs the call and returns its
 * result; the form shows the result, or the error.
 */
export function functionForm(fn, actionLabel, run) {
  const inputs = paramInputs(fn.inputs);
  const result = el('pre', { class: 'result hidden' });
  const button = el('button', { type: 'submit', class: isRead(fn) ? 'secondary' : '' }, actionLabel);
  const form = el('form', { class: 'fn' },
    el('h3', {}, fn.name),
    el('div', { class: 'sig' }, signature(fn)),
    inputs.node, button, result);
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    button.disabled = true;
    result.className = 'result';
    result.textContent = isRead(fn) ? 'Reading…' : 'Sending… (sign the transaction in your wallet)';
    try {
      const value = await run(inputs.read());
      result.textContent = pretty(value);
      result.className = `result ${isErrorReply(value) ? 'bad' : 'ok'}`;
    } catch (err) {
      result.textContent = errorText(err);
      result.className = 'result bad';
    } finally {
      button.disabled = false;
    }
  });
  return form;
}
