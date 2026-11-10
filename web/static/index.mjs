// The contracts page: your identity, your contracts, deploy, join, and the
// apps you connected.
import {
  $, el, status, busy, errorText, connect, paramInputs, readFile, decodeInvite, short,
  loadIdentity, createIdentity, signIn, reregister, passkeysAvailable,
} from './app.mjs';

let ibc;
let ctorInputs = paramInputs([]);
let compiled = null; // the node's compilation of the source in the deploy form

$('#create').addEventListener('click', () => begin($('#create'), async (name) => {
  status($('#connect-status'), 'Creating your identity: approve each passkey request…');
  return createIdentity(name);
}));
$('#sign-in').addEventListener('click', () => begin($('#sign-in'), async () => {
  status($('#connect-status'), 'Signing in: choose your passkey…');
  return signIn();
}));
$('#register').addEventListener('click', () => busy($('#register'), $('#register-status'), async () => {
  status($('#register-status'), 'Approve the registration with your passkey…');
  ibc = await reregister(await loadIdentity());
  $('#register-card').classList.add('hidden');
  await showAgent();
}));
$('#refresh').addEventListener('click', () => busy($('#refresh'), $('#list-status'), loadContracts));
$('#apps-refresh').addEventListener('click', () => busy($('#apps-refresh'), $('#apps-status'), loadApps));
prefillInvite(location.hash.startsWith('#join=') ? location.hash.slice(6) : '');
start();

// Use the identity this browser keeps, or offer to create one or sign in.
async function start() {
  try {
    const identity = await loadIdentity();
    if (!identity) {
      $('#connect-card').classList.remove('hidden');
      if (!passkeysAvailable()) {
        $('#no-passkeys').classList.remove('hidden');
        $('#create').disabled = $('#sign-in').disabled = true;
      }
      return;
    }
    ibc = await connect(identity);
    await enter();
  } catch (err) {
    $('#connect-card').classList.remove('hidden');
    status($('#connect-status'), errorText(err), 'bad');
  }
}

/** Creates an identity or signs in, from a button. */
async function begin(button, fn) {
  const name = $('#name').value.trim() || 'me';
  $('#create').disabled = $('#sign-in').disabled = true;
  try {
    ({ ibc } = await fn(name));
    $('#connect-card').classList.add('hidden');
    await enter();
  } catch (err) {
    status($('#connect-status'), errorText(err), 'bad');
  } finally {
    $('#create').disabled = $('#sign-in').disabled = false;
  }
}

async function enter() {
  $('#who').textContent = `Agent ${ibc.agent} · node ${location.host}`;
  if (await ibc.exists()) await showAgent();
  else $('#register-card').classList.remove('hidden');
}

async function showAgent() {
  $('#agent-area').classList.remove('hidden');
  await busy($('#refresh'), $('#list-status'), loadContracts);
  await busy($('#apps-refresh'), $('#apps-status'), loadApps);
}

async function loadContracts() {
  status($('#list-status'), 'Loading your contracts…');
  const list = await ibc.contracts();
  const body = $('#contracts tbody');
  body.replaceChildren();
  for (const c of list) addRow(c);
  $('#contracts').classList.toggle('hidden', list.length === 0);
  status($('#list-status'), list.length ? '' : 'You have no contracts on this node yet. Deploy one, or join one.');
}

function addRow(c) {
  const href = `contract.html?id=${encodeURIComponent(c.id)}`;
  const row = el('tr', { class: 'link', onclick: () => { location.href = href; } },
    el('td', {}, el('a', { href }, c.name || '(no name)'), c.deploy_error ? el('div', { class: 'status bad' }, 'deployment failed') : null),
    el('td', { class: 'id mono', title: c.id }, short(c.id)),
    el('td', { class: 'optional' }, c.protocol || 'cordial'),
    el('td', { class: 'optional' }, c.block_number ?? 0));
  $('#contracts tbody').append(row);
  $('#contracts').classList.remove('hidden');
  if (/no contracts/.test($('#list-status').textContent)) status($('#list-status'), '');
}

// Deploy: the node compiles the source, and the root signs the deployment.

$('#d-source-file').addEventListener('change', async (e) => {
  const text = await readFile(e.target);
  if (text !== null) $('#d-source').value = text;
  if (!$('#d-name').value && e.target.files?.[0]) $('#d-name').value = e.target.files[0].name.replace(/\.sol$/, '');
  compiled = null;
  $('#d-compiled').classList.add('hidden');
});
$('#d-source').addEventListener('input', () => {
  compiled = null;
  $('#d-compiled').classList.add('hidden');
});

$('#d-compile').addEventListener('click', () => busy($('#d-compile'), $('#compile-status'), async () => {
  status($('#compile-status'), 'Compiling on the node…');
  compiled = await ibc.compile($('#d-source').value, $('#d-contract').value.trim());
  status($('#compile-status'), `Compiled ${compiled.contract} with solc ${compiled.compiler.version}.`, 'ok');
  if (!$('#d-name').value) $('#d-name').value = compiled.contract;
  const holder = $('#d-ctor');
  holder.replaceChildren();
  ctorInputs = paramInputs([]);
  const ctor = compiled.abi.find((x) => x.type === 'constructor');
  if (ctor?.inputs?.length) {
    ctorInputs = paramInputs(ctor.inputs);
    holder.append(el('h3', { style: 'font-size: 14px; margin: 16px 0 0' }, 'Constructor arguments'), ctorInputs.node);
  }
  $('#d-compiled').classList.remove('hidden');
}));

$('#deploy').addEventListener('submit', (event) => {
  event.preventDefault();
  busy($('#d-submit'), $('#deploy-status'), async () => {
    if (!compiled) throw new Error('compile the source first');
    const name = $('#d-name').value.trim() || compiled.contract;
    const constructor = ctorInputs.read();
    status($('#deploy-status'), 'Approve the deploy with your passkey, then wait for the node…');
    const id = await ibc.deploy({
      name, code: compiled.code, abi: compiled.abi, constructor, protocol: $('#d-protocol').value,
      source: $('#d-source').value, compiler: compiled.compiler,
    });
    addRow({ id, name, protocol: $('#d-protocol').value, block_number: 1 });
    const done = $('#deploy-status');
    done.replaceChildren('Deployed: ', el('a', { href: `contract.html?id=${encodeURIComponent(id)}` }, `open ${name}`));
    done.className = 'status ok';
  });
});

// Join.

$('#j-invite').addEventListener('input', (e) => prefillInvite(e.target.value));

function prefillInvite(code) {
  if (!code.trim()) return;
  try {
    const invite = decodeInvite(code);
    $('#j-invite').value = code.trim();
    $('#j-contract').value = invite.contract;
    $('#j-node').value = invite.node;
    $('#j-agent').value = invite.agent;
    status($('#join-status'), '');
  } catch {
    status($('#join-status'), 'This is not an ibc invite code.', 'bad');
  }
}

$('#join').addEventListener('submit', (event) => {
  event.preventDefault();
  busy($('#j-submit'), $('#join-status'), async () => {
    const contract = $('#j-contract').value.trim();
    status($('#join-status'), 'Approve the join with your passkey, then wait for the members to decide…');
    const joined = await ibc.join(contract, $('#j-node').value.trim(), $('#j-agent').value.trim());
    if (joined !== true) {
      status($('#join-status'), 'The contract refused the join.', 'bad');
      return;
    }
    const done = $('#join-status');
    done.replaceChildren('Joined: ', el('a', { href: `contract.html?id=${encodeURIComponent(contract)}` }, 'open the contract'));
    done.className = 'status ok';
    addRow({ id: contract, name: '(new: refresh for details)', protocol: '', block_number: '' });
  });
});

// Connected apps: what each may do without asking, and a button to disconnect it.

async function loadApps() {
  const apps = (await ibc.apps()).filter((a) => !a.permissions[`0x${'0'.repeat(64)}`]); // not this page's own keys
  const list = $('#apps');
  list.replaceChildren(...apps.map((a) => {
    const kinds = Object.values(a.permissions);
    const silent = [...new Set(kinds.flatMap((p) => p.functions))];
    const what = [kinds.some((p) => p.read) ? 'reads' : null, silent.length ? `calls ${silent.join(', ')}` : null]
      .filter(Boolean).join('; ') || 'asks for everything';
    const button = el('button', { class: 'danger' }, 'Disconnect');
    button.addEventListener('click', () => busy(button, $('#apps-status'), async () => {
      status($('#apps-status'), 'Approve with your passkey…');
      await ibc.revokeApp(a.app);
      await loadApps();
    }));
    return el('li', { class: 'row', style: 'justify-content: space-between' },
      el('span', {}, el('strong', {}, a.app),
        ` · ${a.keys.length} browser${a.keys.length === 1 ? '' : 's'} · ${kinds.length} contract type(s) · without asking: ${what}`), button);
  }));
  status($('#apps-status'), apps.length ? '' : 'No apps yet.');
}
