// The contracts page: connect, register, list your contracts, deploy, join.
import {
  $, el, status, busy, errorText, connect, readNote, parseAbi, paramInputs, readFile, decodeInvite, short,
} from './app.mjs';

let ibc;
let ctorInputs = paramInputs([]);

$('#connect').addEventListener('click', () => start(true));
$('#register').addEventListener('click', () => busy($('#register'), $('#register-status'), async () => {
  status($('#register-status'), 'Sign the registration in your wallet…');
  if (await ibc.register() !== true) throw new Error('the node refused the registration');
  $('#register-card').classList.add('hidden');
  await showAgent();
}));
$('#refresh').addEventListener('click', () => busy($('#refresh'), $('#list-status'), loadContracts));

// Connect at once if the wallet already allows this page; otherwise wait for the button.
window.ethereum?.request({ method: 'eth_accounts' }).then((accounts) => {
  if (accounts?.length) start(false);
}).catch(() => {});
prefillInvite(location.hash.startsWith('#join=') ? location.hash.slice(6) : '');

async function start(fromButton) {
  const button = $('#connect');
  button.disabled = true;
  try {
    status($('#connect-status'), fromButton ? 'Approve the connection in your wallet…' : 'Connecting…');
    ibc = await connect();
    $('#who').textContent = `Agent ${ibc.agent} · node ${location.host}`;
    $('#connect-card').classList.add('hidden');
    if (await ibc.exists()) await showAgent();
    else $('#register-card').classList.remove('hidden');
  } catch (err) {
    status($('#connect-status'), errorText(err), 'bad');
  } finally {
    button.disabled = false;
  }
}

async function showAgent() {
  $('#agent-area').classList.remove('hidden');
  await busy($('#refresh'), $('#list-status'), loadContracts);
}

async function loadContracts() {
  status($('#list-status'), `Loading your contracts…${readNote(ibc)}`);
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

// Deploy.

$('#d-bin-file').addEventListener('change', async (e) => {
  const text = await readFile(e.target);
  if (text !== null) $('#d-bin').value = text.trim();
});
$('#d-abi-file').addEventListener('change', async (e) => {
  const text = await readFile(e.target);
  if (text !== null) {
    $('#d-abi').value = text.trim();
    showConstructor();
  }
  if (!$('#d-name').value && e.target.files?.[0]) $('#d-name').value = e.target.files[0].name.replace(/\.(abi|json)$/, '');
});
$('#d-abi').addEventListener('input', showConstructor);

function showConstructor() {
  const holder = $('#d-ctor');
  holder.replaceChildren();
  ctorInputs = paramInputs([]);
  let abi;
  try {
    abi = parseAbi($('#d-abi').value);
  } catch {
    return; // not valid yet
  }
  const ctor = abi.find((x) => x.type === 'constructor');
  if (!ctor?.inputs?.length) return;
  ctorInputs = paramInputs(ctor.inputs);
  holder.append(el('h3', { style: 'font-size: 14px; margin: 16px 0 0' }, 'Constructor arguments'), ctorInputs.node);
}

$('#deploy').addEventListener('submit', (event) => {
  event.preventDefault();
  busy($('#d-submit'), $('#deploy-status'), async () => {
    const code = $('#d-bin').value.trim().replace(/^0x/, '');
    if (!/^[0-9a-fA-F]+$/.test(code)) throw new Error('the bytecode must be hex, as in a .bin file');
    let abi;
    try {
      abi = parseAbi($('#d-abi').value);
    } catch (err) {
      throw new Error(`ABI: ${err.message}`);
    }
    const name = $('#d-name').value.trim();
    const constructor = ctorInputs.read();
    status($('#deploy-status'), 'Sign the deploy in your wallet, then wait for the node…');
    const id = await ibc.deploy({ name, code, abi, constructor, protocol: $('#d-protocol').value });
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
    status($('#join-status'), 'Sign the join in your wallet, then wait for the members to decide…');
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
