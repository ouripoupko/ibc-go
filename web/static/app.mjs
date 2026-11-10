// Shared code of the web interface's pages: the agent's identity in this
// browser (the root passkey, and this page's own key), small DOM helpers, and
// forms built from a contract's ABI.
//
// Everything shown comes from the node or from contracts, so it is always
// inserted as text (textContent), never as HTML.
import { Ibc, IbcError } from './js/index.js';
import {
  addressOf, choosePasskey, createPasskey, fromHex, newP256Key, nonce, passkeySigner, signBatch, sign, toHex,
} from './js/index.js';

export { IbcError };

const ZERO_HASH = `0x${'0'.repeat(64)}`;

/** Can this browser use passkeys on this page? (https, or http://localhost) */
export function passkeysAvailable() {
  return Boolean(window.PublicKeyCredential) && window.isSecureContext;
}

// The identity this browser keeps for this node:
//   { agent, root: { credentialId, publicKey }, pageKey: { privateKey, publicKey } }
// The root passkey stays in the phone or computer; only its id and public key
// are kept here. The page key is this page's own key (an app key the user
// approved for every contract, to read); its private key cannot be extracted.

export const loadIdentity = () => store('get', 'current');
export const saveIdentity = (identity) => store('put', 'current', identity);
export const forgetIdentity = () => store('delete', 'current');

async function store(op, id, value) {
  try {
    const db = await new Promise((resolve, reject) => {
      const open = indexedDB.open('ibc', 3);
      open.onupgradeneeded = () => {
        for (const old of ['sessions', 'identity']) {
          if (open.result.objectStoreNames.contains(old)) open.result.deleteObjectStore(old);
        }
        open.result.createObjectStore('identity');
      };
      open.onsuccess = () => resolve(open.result);
      open.onerror = () => reject(open.error);
    });
    return await new Promise((resolve, reject) => {
      const tx = db.transaction('identity', op === 'get' ? 'readonly' : 'readwrite').objectStore('identity');
      const req = op === 'put' ? tx.put(value, id) : tx[op](id);
      req.onsuccess = () => resolve(req.result ?? null);
      req.onerror = () => reject(req.error);
    }).finally(() => db.close());
  } catch {
    return null;
  }
}

/** The root passkey as a signer, showing a "Sign request" box while it asks. */
export const rootSigner = (identity) => explained(passkeySigner({
  credentialId: identity.root.credentialId, publicKey: fromHex(identity.root.publicKey),
}));

/** Connects to this node as the kept identity: the page key reads, the root signs. */
export async function connect(identity) {
  const ibc = await Ibc.connect(location.origin, { agent: identity.agent, root: rootSigner(identity), appKey: identity.pageKey });
  showKeys(ibc);
  return ibc;
}

/** The consent for this page's key: read every contract, call nothing silently. */
function pageConsent(agent, pageKey) {
  return {
    agent, app: location.origin, appKey: toHex(pageKey.publicKey), codeHash: ZERO_HASH, read: true, functions: [], nonce: nonce(),
  };
}

/**
 * Creates a new identity: the root passkey, and its registration on this node
 * together with this page's key. Two approvals: making the passkey, and one
 * signature by it over both.
 */
export async function createIdentity(name) {
  const root = await passkeyStep('Create passkey: identity',
    () => createPasskey({ userName: `${name} · ibc (${location.host})` }));
  const agent = addressOf(root.publicKey);
  const identity = { agent, root: { credentialId: root.credentialId, publicKey: toHex(root.publicKey) }, pageKey: await newP256Key() };
  const ibc = await Ibc.connect(location.origin, { agent, root: rootSigner(identity), appKey: identity.pageKey });
  const registration = ibc.registration(root.credentialId);
  const consent = pageConsent(agent, identity.pageKey);
  const [regEnv, consentEnv] = await signBatch(rootSigner(identity), [
    { type: 'Registration', message: registration },
    { type: 'Consent', message: consent },
  ]);
  // Register first, then let the page key read.
  if (await registerWith(ibc, registration, regEnv) !== true) throw new IbcError('the node refused the registration');
  await ibc.registerApp({ consent, signature: consentEnv });
  await saveIdentity(identity);
  ibc.close();
  return { identity, ibc: await connect(identity) };
}

/**
 * Registers a kept identity again (when the node no longer knows it): its
 * registration and this page's key, in one approval.
 */
export async function reregister(identity) {
  const ibc = await Ibc.connect(location.origin, { agent: identity.agent, root: rootSigner(identity), appKey: identity.pageKey });
  const registration = ibc.registration(identity.root.credentialId);
  const consent = pageConsent(identity.agent, identity.pageKey);
  const [regEnv, consentEnv] = await signBatch(rootSigner(identity), [
    { type: 'Registration', message: registration },
    { type: 'Consent', message: consent },
  ]);
  if (await registerWith(ibc, registration, regEnv) !== true) throw new IbcError('the node refused the registration');
  await ibc.registerApp({ consent, signature: consentEnv });
  ibc.close();
  return connect(identity);
}

/** Registers, waiting for the node's answer on a stream the root signs once. */
async function registerWith(ibc, registration, signature) {
  // The page key cannot open the stream before the page is approved, so the
  // registration's answer is read back instead.
  await ibc.request('PUT', 'register_agent', { body: { registration, signature } });
  for (let i = 0; i < 100; i++) {
    if (await ibc.exists()) return true;
    await new Promise((r) => { setTimeout(r, 100); });
  }
  return false;
}

/**
 * Signs in on this browser with an identity registered on this node, from
 * its root passkey (synced to this device, or on a phone nearby). Two
 * approvals: choosing the passkey, and approving this page's key with it.
 */
export async function signIn() {
  const credentialId = await passkeyStep('Choose your identity passkey', () => choosePasskey());
  let info;
  try {
    info = await Ibc.agentInfo(location.origin, { credential: credentialId });
  } catch {
    throw new IbcError('This passkey is not an ibc identity on this node.');
  }
  if (info.rootScheme !== 'webauthn' || addressOf(fromHex(info.rootKey)) !== info.agent) {
    throw new IbcError('This identity has no passkey root on this node.');
  }
  const identity = { agent: info.agent, root: { credentialId, publicKey: info.rootKey }, pageKey: await newP256Key() };
  const consent = pageConsent(info.agent, identity.pageKey);
  const { envelope } = await sign(rootSigner(identity), 'Consent', consent);
  const ibc = await Ibc.connect(location.origin, { agent: info.agent, appKey: identity.pageKey });
  await ibc.registerApp({ consent, signature: envelope });
  await saveIdentity(identity);
  return { identity, ibc: await connect(identity) };
}

/** Shows the sign-out button in the header (#keys). */
function showKeys(ibc) {
  const holder = document.querySelector('#keys');
  if (!holder) return;
  holder.replaceChildren(el('button', {
    class: 'link', title: 'Forget your identity in this browser (your passkey stays on your devices)',
    onclick: async () => { ibc.close(); await forgetIdentity(); location.href = './'; },
  }, 'Sign out'));
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

// Explaining signatures. Every time a passkey is asked to sign, a message box
// says in a few words what is signed. It has no close button: it closes when
// the passkey answers, whether the user approved or cancelled.

/** Wraps a signer so that each signature request is shown first. */
export function explained(signer) {
  return {
    ...signer,
    async signDigest(digest, about) {
      const close = openSigningBox(explain(about?.type, about?.message ?? {}, about?.messages));
      try {
        return await signer.signDigest(digest, about);
      } finally {
        close();
      }
    },
  };
}

/** Shows the box while a passkey is created or chosen. */
async function passkeyStep(text, fn) {
  const close = openSigningBox(text);
  try {
    return await fn();
  } finally {
    close();
  }
}

let signingBox;
const openBoxes = [];

/** Shows the message box; returns a function that closes it. */
function openSigningBox(text) {
  if (!signingBox) {
    signingBox = el('dialog', { class: 'signing', 'aria-labelledby': 'signing-title' });
    // No way to dismiss it: Escape is ignored, and if the browser closes it
    // anyway while the passkey prompt is still open, it comes back.
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
function explain(type, m, batch) {
  const body = parseBody(m.body ?? '');
  const self = String(body.member).toLowerCase() === String(m.agent).toLowerCase();
  if (type === 'Batch') {
    if (batch?.some((b) => b.type === 'Registration')) return words('create identity on', location.host);
    const tx = batch?.find((b) => b.type === 'Transaction');
    if (tx) return explain('Transaction', tx.message);
    const consent = batch?.find((b) => b.type === 'Consent');
    if (consent) return explain('Consent', consent.message);
    const query = batch?.find((b) => b.type === 'Query');
    if (query) return explain('Query', query.message);
    const device = batch?.find((b) => b.type === 'Device');
    if (device) return explain('Device', device.message);
    return words(`${batch?.length ?? 0} requests`);
  }
  switch (`${type}:${m.kind ?? m.action ?? ''}`) {
    case 'Registration:': return words('register on', location.host);
    case 'Consent:': return words('allow', new URL(m.app).host);
    case 'Device:': return words('connect', new URL(m.app).host);
    case 'Grant:': return words('let your node call', m.functions.join(' '));
    case 'Query:contract_read': return words('read', body.method, values(body.values));
    case 'Query:revoke_app': return words('disconnect app');
    case 'Transaction:deploy': return words('deploy', body.name, values(Object.hasOwn(body, 'constructor') ? body.constructor : {}));
    case 'Transaction:call': return words(body.method, values(body.values));
    case 'Transaction:join': return words(body.guest ? 'join as a guest' : 'join', short(m.contract));
    case 'Transaction:leave': return self ? words('leave', short(m.contract)) : words('remove', short(body.member));
    case 'Transaction:rotate_key': return words('move to', body.node);
    default: return words(type ?? '', m.kind ?? m.action ?? '');
  }
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

/** A readable message for an error from a passkey, the node or the network. */
export function errorText(err) {
  if (err?.name === 'NotAllowedError') return 'The passkey request was cancelled or timed out.';
  if (err?.name === 'InvalidStateError') return 'This device already has this passkey.';
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
  const form = el('form', { class: 'fn', 'data-fn': fn.name },
    el('h3', {}, fn.name),
    el('div', { class: 'sig' }, signature(fn)),
    inputs.node, button, result);
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    button.disabled = true;
    result.className = 'result';
    result.textContent = isRead(fn) ? 'Reading…' : 'Sending… (approve with your passkey)';
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
