// The approval page: an app on another site asks the user, here on the
// user's own node, to approve something with the root passkey.
//
// Protocol (see docs/keys.md): this page posts { type: 'ibc-ready', apiVersion }
// to its opener; the app answers with a request, which always carries the API
// version it speaks (apiVersion) and its key (appKey, a P-256 public key, hex):
//   { type: 'connect', appKey }            who the user is: connects the app, with no permissions
//   { type: 'deploy', source, contract, name, constructor, protocol, public, appKey }
//   { type: 'permit', contract, public, appKey }   permissions on an existing contract
//   { type: 'call', contract, method, values, appKey }
//   { type: 'read', contract, method, values, appKey }
//   { type: 'join', contract, memberNode, memberAgent, guest, public, appKey }
// public (a boolean) proposes whether anyone may read the user's copy of the
// contract (public_read); the user decides here. Unless proposed, it is off
// (deploy, join) or unchanged (permit).
// and this page replies { type: 'ibc-done', result } or { type: 'ibc-denied', reason }.
// The app's site (its origin) is what the user sees and what the node records.
// A user with no identity on this node yet can create one here, or sign in.
import {
  $, el, status, errorText, loadIdentity, connect, rootSigner, signIn, createIdentity, isKnown, reregister,
  pretty, short, codeHashOf, writeFunctions, permissionItems, registerPermission,
} from './app.mjs';
import { API_VERSION, digestOf, nonce, now, signBatch, toHex } from './js/index.js';

const opener = window.opener;
let origin;
let request;

if (!opener) {
  status($('#status'), 'An app opens this page to ask for your approval; there is nothing to approve here.', 'bad');
} else {
  window.addEventListener('message', onMessage);
  opener.postMessage({ type: 'ibc-ready', apiVersion: API_VERSION }, '*');
}

const reply = (message) => opener.postMessage(message, origin);
const deny = (reason) => {
  reply({ type: 'ibc-denied', reason });
  window.close();
};

function valid(r) {
  const known = ['connect', 'deploy', 'permit', 'call', 'read', 'join'];
  if (!r || r.apiVersion !== API_VERSION || !known.includes(r.type) || !/^0x04[0-9a-fA-F]{128}$/.test(r.appKey ?? '')) return false;
  if (r.type === 'connect') return true;
  return r.type === 'deploy' ? typeof r.source === 'string' : typeof r.contract === 'string';
}

async function onMessage(e) {
  if (e.source !== opener || request || !valid(e.data)) return;
  request = e.data;
  origin = e.origin;
  $('#deny').addEventListener('click', () => deny('the user denied'));
  let identity = await loadIdentity();
  if (!identity) {
    // This browser has no identity on this node: sign in with the passkey, or create one.
    $('#title').textContent = `${new URL(origin).host} asks for your approval`;
    $('#sign-in-box').classList.remove('hidden');
    const begin = (fn) => async () => {
      $('#sign-in').disabled = $('#create-identity').disabled = true;
      try {
        ({ identity } = await fn());
        $('#sign-in-box').classList.add('hidden');
        status($('#status'), '');
        await show(identity);
      } catch (err) {
        status($('#status'), errorText(err), 'bad');
        $('#sign-in').disabled = $('#create-identity').disabled = false;
      }
    };
    $('#sign-in').addEventListener('click', begin(signIn));
    $('#create-identity').addEventListener('click', begin(() => createIdentity($('#new-name').value.trim() || 'me')));
    return;
  }
  await show(identity);
}

async function show(identity) {
  const host = new URL(origin).host;
  $('#who').textContent = `Agent ${identity.agent}`;
  try {
    let ibc = await connect(identity);
    if (!(await isKnown(ibc))) ibc = await renew(identity);
    const handler = {
      connect: showConnect, deploy: showDeploy, permit: showPermit, call: showCall, read: showRead, join: showJoin,
    }[request.type];
    const approveFn = await handler(ibc, identity, host);
    $('#buttons').classList.remove('hidden');
    $('#approve').addEventListener('click', async () => {
      $('#approve').disabled = $('#deny').disabled = true;
      try {
        status($('#status'), 'Approve with your passkey…');
        const result = await approveFn();
        reply({ type: 'ibc-done', result: { ...result, agent: identity.agent } });
        status($('#status'), 'Done. You can close this window.', 'ok');
        setTimeout(() => window.close(), 800);
      } catch (err) {
        status($('#status'), errorText(err), 'bad');
        $('#approve').disabled = $('#deny').disabled = false;
      }
    });
  } catch (err) {
    status($('#status'), errorText(err), 'bad');
  }
}

/**
 * The node does not know this browser's identity (any more): the user
 * registers again, with one approval, and the request continues.
 */
function renew(identity) {
  $('#renew-box').classList.remove('hidden');
  return new Promise((resolve) => {
    $('#renew').addEventListener('click', async () => {
      $('#renew').disabled = true;
      try {
        status($('#status'), 'Approve with your passkey…');
        const ibc = await reregister(identity);
        $('#renew-box').classList.add('hidden');
        status($('#status'), '');
        resolve(ibc);
      } catch (err) {
        status($('#status'), errorText(err), 'bad');
        $('#renew').disabled = false;
      }
    });
  });
}

/**
 * A second approval in the same window, after the request's first one (the
 * app's permissions for a contract joined just now). Resolves true if the user
 * approves, false if they skip it.
 */
function askAgain(message) {
  // Fresh buttons, without the first approval's handlers.
  const approve = $('#approve').cloneNode(true);
  const skip = $('#deny').cloneNode(true);
  $('#approve').replaceWith(approve);
  $('#deny').replaceWith(skip);
  skip.textContent = 'Skip';
  approve.disabled = skip.disabled = false;
  status($('#status'), message, 'ok');
  return new Promise((resolve) => {
    approve.addEventListener('click', () => { approve.disabled = skip.disabled = true; resolve(true); });
    skip.addEventListener('click', () => { approve.disabled = skip.disabled = true; resolve(false); });
  });
}

function details(rows) {
  $('#details').replaceChildren(...rows.filter(([, v]) => v !== undefined && v !== '').flatMap(([k, v]) => [
    el('dt', {}, k), el('dd', {}, v),
  ]));
  $('#details').classList.remove('hidden');
}

function showCode(source) {
  if (!source) return;
  $('#code').textContent = source;
  $('#code-toggle').classList.remove('hidden');
  $('#show-code').addEventListener('click', () => {
    const hidden = $('#code').classList.toggle('hidden');
    $('#show-code').textContent = hidden ? 'Show the code' : 'Hide the code';
  });
}

/**
 * What the node knows of the app (by its site): its record, and whether this
 * browser's key is one of its devices.
 */
async function siteOf(ibc) {
  const site = (await ibc.apps()).find((a) => a.app === origin) ?? null;
  const known = Boolean(site?.keys?.includes(request.appKey.toLowerCase()));
  if (site && !known) {
    $('#what').append(el('span', { class: 'hint' }, ` This is a new browser for ${new URL(origin).host}: approving also connects it, with the permissions you gave it.`));
  }
  return { site, known };
}

/** The device message that connects this browser of an app the user approved before. */
function deviceItems(identity, { site, known }) {
  return site && !known
    ? [{ type: 'Device', message: { agent: identity.agent, app: origin, appKey: request.appKey, nonce: nonce() } }]
    : [];
}

/** Registers what the root signed: a consent (and grant), or a device. */
async function registerSigned(ibc, items, envs) {
  const at = (type) => items.findIndex((i) => i.type === type);
  if (at('Consent') >= 0) {
    await registerPermission(ibc, items, envs);
  } else if (at('Device') >= 0) {
    await ibc.registerDevice({ device: items[at('Device')].message, signature: envs[at('Device')] });
  }
}

/**
 * Shows the choice of letting anyone read the user's copy; returns a reader of
 * it. current: what the node has (permit), else off.
 */
function publicForm(current = false) {
  $('#public').checked = typeof request.public === 'boolean' ? request.public : current;
  $('#public-box').classList.remove('hidden');
  return () => $('#public').checked;
}

/** The message that makes the user's copy of a contract public, or not. */
const publishItem = (ibc, contract, isPublic) => ({ type: 'Publish', message: ibc.publishing(contract, isPublic) });

/** Records a publish the root signed (in a batch). */
const registerPublish = (ibc, item, signature) => ibc.setPublic(item.message.contract, item.message.public,
  { publish: item.message, signature });

/** Shows the permissions form for the contract's functions; returns a reader of the choices. */
function permissionsForm(abi, current = null) {
  const writes = writeFunctions(abi);
  const boxes = writes.map((f) => el('input', { type: 'checkbox', value: f.name, checked: current?.functions?.includes(f.name) }));
  if (current) $('#perm-read').checked = current.read;
  $('#perm-writes').replaceChildren(...writes.map((f, i) => el('label', { class: 'check' }, boxes[i], f.name)));
  if (!writes.length) $('#perm-writes-hint').classList.add('hidden');
  $('#permissions').classList.remove('hidden');
  return () => ({ read: $('#perm-read').checked, functions: boxes.filter((b) => b.checked).map((b) => b.value) });
}

/** The messages that record the app's permissions: the consent, and the grant if it names functions. */
function permissionMessages(ibc, identity, codeHash, choice) {
  return permissionItems(ibc, { app: origin, appKey: request.appKey, codeHash, ...choice });
}

async function showConnect(ibc, identity, host) {
  $('#title').textContent = `${host} wants to know who you are`;
  $('#what').textContent = 'It learns your agent id. It asks you here before it does anything in your name.';
  const app = await siteOf(ibc);
  return async () => {
    if (app.known) return {}; // this browser of the app is connected already
    const items = [{ type: 'Device', message: { agent: identity.agent, app: origin, appKey: request.appKey, nonce: nonce() } }];
    await registerSigned(ibc, items, await signBatch(rootSigner(identity), items));
    return {};
  };
}

async function showDeploy(ibc, identity, host) {
  // Every object inherits a constructor: only the request's own counts.
  const ctor = Object.hasOwn(request, 'constructor') ? request.constructor : {};
  $('#title').textContent = `${host} wants to create a contract`;
  status($('#status'), 'Compiling the code on your node…');
  const compiled = await ibc.compile(request.source, request.contract ?? '');
  status($('#status'), '');
  const name = request.name || compiled.contract;
  $('#what').textContent = `A new ${compiled.contract} contract, which you will own.`;
  details([['Contract', name], ['Arguments', Object.entries(ctor).map(([k, v]) => `${k}: ${pretty(v)}`).join(', ')]]);
  showCode(request.source);
  const app = await siteOf(ibc);
  const choose = app.site?.permissions?.[compiled.codeHash] ? null : permissionsForm(compiled.abi);
  const isPublic = publicForm();
  return async () => {
    const items = choose ? permissionMessages(ibc, identity, compiled.codeHash, choose()) : deviceItems(identity, app);
    const tx = {
      contract: '', kind: 'deploy', agent: identity.agent, nonce: nonce(),
      body: JSON.stringify(ibc.deployBody({
        name, code: compiled.code, abi: compiled.abi, constructor: ctor,
        protocol: request.protocol, source: request.source, compiler: compiled.compiler,
      })),
    };
    // The contract's id is its deploy transaction's digest: the publish names it before it exists.
    const publish = isPublic() ? [publishItem(ibc, toHex(digestOf('Transaction', tx)), true)] : [];
    const envs = await signBatch(rootSigner(identity), [...items, ...publish, { type: 'Transaction', message: tx }]);
    await registerSigned(ibc, items, envs);
    const contract = await ibc.deploy(null, { tx, signature: envs[envs.length - 1] });
    if (publish.length) await registerPublish(ibc, publish[0], envs[items.length]);
    return { contract, codeHash: compiled.codeHash, public: publish.length > 0 };
  };
}

async function showPermit(ibc, identity, host) {
  const c = await ibc.contract(request.contract);
  if (!c?.id) throw new Error('You have no such contract on this node.');
  const codeHash = codeHashOf(c.code);
  $('#title').textContent = `${host} wants to use ${c.name || short(c.id)}`;
  $('#what').textContent = 'Choose what it may do without asking you. It asks you for everything else.';
  details([['Contract', c.name], ['Id', short(c.id)]]);
  showCode(c.source);
  const app = await siteOf(ibc);
  const choose = permissionsForm(c.abi, app.site?.permissions?.[codeHash]);
  const isPublic = c.role === 'guest' ? null : publicForm(c.public === true);
  return async () => {
    const items = permissionMessages(ibc, identity, codeHash, choose());
    const publish = isPublic && isPublic() !== (c.public === true) ? [publishItem(ibc, c.id, isPublic())] : [];
    const envs = await signBatch(rootSigner(identity), [...items, ...publish]);
    await registerSigned(ibc, items, envs);
    if (publish.length) await registerPublish(ibc, publish[0], envs[items.length]);
    return { contract: c.id, public: isPublic ? isPublic() : false };
  };
}

/**
 * Signs, with the root, a message of the request together with the device
 * message if this browser is new, and registers the device.
 */
async function signWithDevice(ibc, identity, app, item) {
  const items = [...deviceItems(identity, app), item];
  const envs = await signBatch(rootSigner(identity), items);
  await registerSigned(ibc, items, envs);
  return envs[envs.length - 1];
}

async function showCall(ibc, identity, host) {
  const c = await ibc.contract(request.contract);
  $('#title').textContent = `${host} wants to: ${request.method}`;
  $('#what').textContent = 'A transaction in your name, applied by all the contract\'s members.';
  details([['Contract', c?.name || short(request.contract)], ['Arguments', Object.entries(request.values ?? {})
    .map(([k, v]) => `${k}: ${pretty(v)}`).join(', ')]]);
  const app = await siteOf(ibc);
  return async () => {
    const tx = {
      contract: request.contract, kind: 'call', agent: identity.agent, nonce: nonce(),
      body: JSON.stringify({ method: request.method, values: request.values ?? {} }),
    };
    const signature = await signWithDevice(ibc, identity, app, { type: 'Transaction', message: tx });
    const event = await ibc.submit('POST', 'contract_write', { contract: request.contract, method: request.method, body: { tx, signature } });
    return { reply: event.reply };
  };
}

async function showRead(ibc, identity, host) {
  const c = await ibc.contract(request.contract);
  $('#title').textContent = `${host} wants to read: ${request.method}`;
  $('#what').textContent = 'It reads, and changes nothing.';
  details([['Contract', c?.name || short(request.contract)]]);
  const app = await siteOf(ibc);
  return async () => {
    const query = {
      agent: identity.agent, action: 'contract_read', target: identity.agent, contract: request.contract,
      body: JSON.stringify({ method: request.method, values: request.values ?? {} }), time: now(), nonce: nonce(),
    };
    const signature = await signWithDevice(ibc, identity, app, { type: 'Query', message: query });
    return { value: await ibc.request('POST', 'contract_read', { contract: request.contract, method: request.method, body: { query, signature } }) };
  };
}

async function showJoin(ibc, identity, host) {
  const guest = request.guest === true;
  $('#title').textContent = `${host} asks you to join a contract${guest ? ' as a guest' : ''}`;
  $('#what').textContent = guest
    ? 'You become a guest: your node sends your transactions and reads to the member\'s node, which you trust with them, and keeps no copy.'
    : 'You become a member: your node takes part in it, and receives its history.';
  details([['Contract', short(request.contract)], ['Through', `${request.memberNode} (${short(request.memberAgent)})`]]);
  const app = await siteOf(ibc);
  const isPublic = guest ? () => false : publicForm(); // a guest keeps no copy to show
  return async () => {
    const tx = {
      contract: request.contract, kind: 'join', agent: identity.agent, nonce: nonce(),
      body: JSON.stringify({ node: ibc.url, nodeKey: ibc.nodeKey, profile: '',
        ...(guest ? { guest: true, host: request.memberAgent } : {}) }),
    };
    const items = [...deviceItems(identity, app), ...(isPublic() ? [publishItem(ibc, request.contract, true)] : []),
      { type: 'Transaction', message: tx }];
    const envs = await signBatch(rootSigner(identity), items);
    await registerSigned(ibc, items, envs);
    const event = await ibc.submit('PUT', 'join_contract', {
      body: { tx, signature: envs[envs.length - 1], partner: { address: request.memberNode, agent: request.memberAgent } },
    });
    // Once joined, this node holds the user's copy: the publish applies to it.
    const at = items.findIndex((i) => i.type === 'Publish');
    if (at >= 0 && event.reply === true) await registerPublish(ibc, items[at], envs[at]);
    const result = { joined: event.reply, public: at >= 0 && event.reply === true };
    if (event.reply === true) result.codeHash = await joinedPermissions(ibc, identity, host);
    return result;
  };
}

/**
 * After a join: the contract's code is known now. The first time the app uses
 * this code, the user chooses what it may do without asking, as for a deploy:
 * a second approval, which the user may skip. Returns the code hash.
 */
async function joinedPermissions(ibc, identity, host) {
  const c = await ibc.contract(request.contract);
  if (!c?.code) return undefined;
  const codeHash = codeHashOf(c.code);
  const site = (await ibc.apps()).find((a) => a.app === origin);
  if (site?.permissions?.[codeHash]) return codeHash; // chosen before, for this code
  $('#public-box').classList.add('hidden');
  $('#title').textContent = `Joined ${c.name || short(c.id)}`;
  $('#what').textContent = `Choose what ${host} may do in it without asking you. It asks you for everything else.`;
  showCode(c.source);
  const choose = permissionsForm(c.abi);
  if (await askAgain('Joined. Now choose the app\'s permissions, and approve (or skip).')) {
    status($('#status'), 'Approve with your passkey…');
    const items = permissionMessages(ibc, identity, codeHash, choose());
    await registerSigned(ibc, items, await signBatch(rootSigner(identity), items));
  }
  return codeHash;
}
