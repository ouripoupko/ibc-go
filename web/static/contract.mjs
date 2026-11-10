// The contract page: details, an invite, and a form for each ABI function.
import {
  $, el, status, busy, errorText, connect, parseAbi, functionForm, isRead, encodeInvite, pretty, short,
  loadIdentity,
} from './app.mjs';

const id = new URLSearchParams(location.search).get('id') ?? '';
let ibc;

if (id) start();
else status($('#connect-status'), 'No contract id in the address. Open a contract from your list.', 'bad');

async function start() {
  try {
    const identity = await loadIdentity();
    if (!identity) {
      $('#connect-status').replaceChildren('Sign in first, on ', el('a', { href: './' }, 'the main page'), '.');
      return;
    }
    status($('#connect-status'), 'Connecting…');
    ibc = await connect(identity);
    $('#who').textContent = `Agent ${ibc.agent} · node ${location.host}`;
    status($('#connect-status'), 'Opening the contract…');
    const c = await ibc.contract(id);
    if (!c || !c.id) throw new Error('You have no such contract on this node.');
    $('#connect-card').classList.add('hidden');
    show(c);
  } catch (err) {
    status($('#connect-status'), errorText(err), 'bad');
  }
}

function show(c) {
  document.title = `ibc · ${c.name || short(c.id)}`;
  $('#title-name').textContent = `· ${c.name || short(c.id)}`;
  $('#name').textContent = c.name || '(no name)';
  const guest = c.role === 'guest';
  const rows = [
    ['Contract id', c.id],
    ['Protocol', c.protocol || 'cordial'],
    ['Your role', guest ? 'guest: your node sends your transactions and reads to a host' : 'partner'],
    ['Hosts', guest ? (c.hosts ?? []).map((h) => h.node).join(', ') : undefined],
    ['Created by', c.pid],
    ['Creator\'s node', c.address],
    ['Transactions', guest ? undefined : (c.block_number ?? 0)],
    ['State root', c.state_root],
    ['EVM address', c.evm_address],
    ['Source file', c.contract],
    ['Deployment error', c.deploy_error],
  ];
  $('#details').replaceChildren(...rows.filter(([, v]) => v !== undefined && v !== '').flatMap(([k, v]) => [
    el('dt', {}, k), el('dd', { class: /id|root|address|Created/.test(k) ? 'mono' : '' }, v),
  ]));

  // A guest's node has no copy to welcome others into.
  $('#invite-card').classList.toggle('hidden', guest);
  const invite = encodeInvite({ contract: c.id, node: location.origin, agent: ibc.agent });
  $('#invite').textContent = invite;
  $('#copy-invite').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(invite);
      $('#copy-invite').textContent = 'Copied';
    } catch {
      getSelection().selectAllChildren($('#invite'));
    }
  });

  let abi = [];
  try {
    abi = typeof c.abi === 'string' ? parseAbi(c.abi) : (c.abi ?? []);
  } catch {
    // shown below as "no functions"
  }
  const fns = abi.filter((x) => x.type === 'function' && x.name);
  const reads = fns.filter(isRead);
  const writes = fns.filter((f) => !isRead(f));
  $('#reads').replaceChildren(...reads.map((f) => functionForm(f, 'Read', (values) => ibc.read(c.id, f.name, values))));
  $('#writes').replaceChildren(...writes.map((f) => functionForm(f, 'Send', (values) => ibc.write(c.id, f.name, values))));
  if (c.source) {
    $('#code').textContent = c.source;
    $('#code-toggle').classList.remove('hidden');
    $('#show-code').addEventListener('click', () => {
      const shown = $('#code').classList.toggle('hidden');
      $('#show-code').textContent = shown ? 'Show the code' : 'Hide the code';
    });
  }
  if (!reads.length) $('#reads').append(el('p', { class: 'hint' }, 'No read functions in the ABI.'));
  if (!writes.length) $('#writes').append(el('p', { class: 'hint' }, 'No transaction functions in the ABI.'));

  ibc.onEvent((event) => {
    if (event.contract !== c.id) return;
    const list = $('#events');
    if (list.firstElementChild?.classList.contains('hint')) list.replaceChildren();
    list.prepend(el('li', {}, el('strong', {}, event.action), ' ', el('span', { class: 'mono' }, short(event.request)),
      ' → ', el('span', { class: 'mono' }, pretty(event.reply))));
  });

  $('#leave').addEventListener('click', () => {
    if (!confirm(guest ? 'Leave this contract?' : 'Leave this contract? The other members decide, by the contract\'s rules.')) return;
    busy($('#leave'), $('#leave-status'), async () => {
      status($('#leave-status'), 'Approve the leave with your passkey, then wait for the members…');
      const reply = await ibc.leave(c.id);
      status($('#leave-status'), reply === true ? 'You left the contract.' : `Refused: ${pretty(reply)}`, reply === true ? 'ok' : 'bad');
    });
  });
  $('#contract-area').classList.remove('hidden');
}
