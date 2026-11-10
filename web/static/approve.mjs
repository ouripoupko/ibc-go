// The approval page: an app on another site asks the user, here on the
// user's own node, to approve something with the root passkey.
//
// Protocol (see docs/keys.md): this page posts { type: 'ibc-ready' } to its
// opener; the app answers with a request, which always carries its key
// (appKey, a P-256 public key, hex):
//   { type: 'deploy', source, contract, name, constructor, protocol, appKey }
//   { type: 'permit', contract, appKey }   permissions on an existing contract
//   { type: 'call', contract, method, values, appKey }
//   { type: 'read', contract, method, values, appKey }
//   { type: 'join', contract, memberNode, memberAgent, appKey }
// and this page replies { type: 'ibc-done', result } or { type: 'ibc-denied', reason }.
// The app's site (its origin) is what the user sees and what the node records.
import {
  $, el, status, errorText, loadIdentity, connect, rootSigner, signIn, parseAbi, isRead, pretty, short,
} from './app.mjs';
import { fromHex, keccak256, nonce, now, signBatch, toHex } from './keys.mjs';

const opener = window.opener;
let origin;
let request;

if (!opener) {
  status($('#status'), 'An app opens this page to ask for your approval; there is nothing to approve here.', 'bad');
} else {
  window.addEventListener('message', onMessage);
  opener.postMessage({ type: 'ibc-ready' }, '*');
}

const reply = (message) => opener.postMessage(message, origin);
const deny = (reason) => {
  reply({ type: 'ibc-denied', reason });
  window.close();
};
const codeHashOf = (code) => toHex(keccak256(fromHex(code)));

function valid(r) {
  const known = ['deploy', 'permit', 'call', 'read', 'join'];
  return r && known.includes(r.type) && /^0x04[0-9a-fA-F]{128}$/.test(r.appKey ?? '')
    && (r.type === 'deploy' ? typeof r.source === 'string' : typeof r.contract === 'string');
}

async function onMessage(e) {
  if (e.source !== opener || request || !valid(e.data)) return;
  request = e.data;
  origin = e.origin;
  $('#deny').addEventListener('click', () => deny('the user denied'));
  let identity = await loadIdentity();
  if (!identity) {
    $('#title').textContent = `${new URL(origin).host} asks for your approval`;
    $('#sign-in-box').classList.remove('hidden');
    $('#sign-in').addEventListener('click', async () => {
      try {
        ({ identity } = await signIn());
        $('#sign-in-box').classList.add('hidden');
        await show(identity);
      } catch (err) {
        status($('#status'), errorText(err), 'bad');
      }
    });
    return;
  }
  await show(identity);
}

async function show(identity) {
  const host = new URL(origin).host;
  $('#who').textContent = `Agent ${identity.agent}`;
  try {
    const ibc = await connect(identity);
    const handler = { deploy: showDeploy, permit: showPermit, call: showCall, read: showRead, join: showJoin }[request.type];
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
    const g = at('Grant');
    await ibc.registerApp({ consent: items[at('Consent')].message, signature: envs[at('Consent')] },
      g >= 0 ? { grant: items[g].message, signature: envs[g] } : null);
  } else if (at('Device') >= 0) {
    await ibc.registerDevice({ device: items[at('Device')].message, signature: envs[at('Device')] });
  }
}

/** Shows the permissions form for the contract's functions; returns a reader of the choices. */
function permissionsForm(abi, current = null) {
  const writes = parseAbi(typeof abi === 'string' ? abi : JSON.stringify(abi))
    .filter((f) => f.type === 'function' && f.name && !isRead(f));
  const boxes = writes.map((f) => el('input', { type: 'checkbox', value: f.name, checked: current?.functions?.includes(f.name) }));
  if (current) $('#perm-read').checked = current.read;
  $('#perm-writes').replaceChildren(...writes.map((f, i) => el('label', { class: 'check' }, boxes[i], f.name)));
  if (!writes.length) $('#perm-writes-hint').classList.add('hidden');
  $('#permissions').classList.remove('hidden');
  return () => ({ read: $('#perm-read').checked, functions: boxes.filter((b) => b.checked).map((b) => b.value) });
}

/** The messages that record the app's permissions: the consent, and the grant if it names functions. */
function permissionMessages(ibc, identity, codeHash, choice) {
  const items = [{
    type: 'Consent',
    message: {
      agent: identity.agent, app: origin, appKey: request.appKey, codeHash, read: choice.read, functions: choice.functions, nonce: nonce(),
    },
  }];
  if (choice.functions.length) {
    items.push({
      type: 'Grant',
      message: { agent: identity.agent, nodeKey: ibc.nodeKey, codeHash, functions: choice.functions, nonce: nonce() },
    });
  }
  return items;
}

async function showDeploy(ibc, identity, host) {
  $('#title').textContent = `${host} wants to create a contract`;
  status($('#status'), 'Compiling the code on your node…');
  const compiled = await ibc.compile(request.source, request.contract ?? '');
  status($('#status'), '');
  const name = request.name || compiled.contract;
  $('#what').textContent = `A new ${compiled.contract} contract, which you will own.`;
  details([['Contract', name], ['Arguments', Object.entries(request.constructor ?? {}).map(([k, v]) => `${k}: ${pretty(v)}`).join(', ')]]);
  showCode(request.source);
  const app = await siteOf(ibc);
  const choose = app.site?.permissions?.[compiled.codeHash] ? null : permissionsForm(compiled.abi);
  return async () => {
    const items = choose ? permissionMessages(ibc, identity, compiled.codeHash, choose()) : deviceItems(identity, app);
    const tx = {
      contract: '', kind: 'deploy', agent: identity.agent, nonce: nonce(),
      body: JSON.stringify(ibc.deployBody({
        name, code: compiled.code, abi: compiled.abi, constructor: request.constructor ?? {},
        protocol: request.protocol, source: request.source, compiler: compiled.compiler,
      })),
    };
    const envs = await signBatch(rootSigner(identity), [...items, { type: 'Transaction', message: tx }]);
    await registerSigned(ibc, items, envs);
    const contract = await ibc.deploy(null, { tx, signature: envs[envs.length - 1] });
    return { contract, codeHash: compiled.codeHash };
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
  return async () => {
    const items = permissionMessages(ibc, identity, codeHash, choose());
    const envs = await signBatch(rootSigner(identity), items);
    await registerSigned(ibc, items, envs);
    return { contract: c.id };
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
  $('#title').textContent = `${host} asks you to join a contract`;
  $('#what').textContent = 'You become a member: your node takes part in it, and receives its history.';
  details([['Contract', short(request.contract)], ['Through', `${request.memberNode} (${short(request.memberAgent)})`]]);
  const app = await siteOf(ibc);
  return async () => {
    const tx = {
      contract: request.contract, kind: 'join', agent: identity.agent, nonce: nonce(),
      body: JSON.stringify({ node: ibc.url, nodeKey: ibc.nodeKey, profile: '' }),
    };
    const signature = await signWithDevice(ibc, identity, app, { type: 'Transaction', message: tx });
    const event = await ibc.submit('PUT', 'join_contract', {
      body: { tx, signature, partner: { address: request.memberNode, agent: request.memberAgent } },
    });
    return { joined: event.reply };
  };
}
