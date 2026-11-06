// The contract page: details, an invite, and a form for each ABI function.
import {
  $, el, status, busy, errorText, connect, parseAbi, functionForm, isRead, encodeInvite, pretty, short,
} from './app.mjs';

const id = new URLSearchParams(location.search).get('id') ?? '';
let ibc;

if (!id) {
  status($('#connect-status'), 'No contract id in the address. Open a contract from your list.', 'bad');
  $('#connect').classList.add('hidden');
}
$('#connect').addEventListener('click', () => start(true));
window.ethereum?.request({ method: 'eth_accounts' }).then((accounts) => {
  if (accounts?.length && id) start(false);
}).catch(() => {});

async function start(fromButton) {
  const button = $('#connect');
  button.disabled = true;
  try {
    status($('#connect-status'), fromButton ? 'Approve the connection in MetaMask…' : 'Connecting…');
    ibc = await connect();
    $('#who').textContent = `Agent ${ibc.agent} · node ${location.host}`;
    status($('#connect-status'), 'Sign the query in MetaMask to open the contract…');
    const c = await ibc.contract(id);
    if (!c || !c.id) throw new Error('You have no such contract on this node.');
    $('#connect-card').classList.add('hidden');
    show(c);
  } catch (err) {
    status($('#connect-status'), errorText(err), 'bad');
  } finally {
    button.disabled = false;
  }
}

function show(c) {
  document.title = `ibc · ${c.name || short(c.id)}`;
  $('#title-name').textContent = `· ${c.name || short(c.id)}`;
  $('#name').textContent = c.name || '(no name)';
  const rows = [
    ['Contract id', c.id],
    ['Protocol', c.protocol || 'cordial'],
    ['Created by', c.pid],
    ['Creator\'s node', c.address],
    ['Transactions', c.block_number ?? 0],
    ['State root', c.state_root],
    ['EVM address', c.evm_address],
    ['Source file', c.contract],
    ['Deployment error', c.deploy_error],
  ];
  $('#details').replaceChildren(...rows.filter(([, v]) => v !== undefined && v !== '').flatMap(([k, v]) => [
    el('dt', {}, k), el('dd', { class: /id|root|address|Created/.test(k) ? 'mono' : '' }, v),
  ]));

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
    if (!confirm('Leave this contract? The other members decide, by the contract\'s rules.')) return;
    busy($('#leave'), $('#leave-status'), async () => {
      status($('#leave-status'), 'Sign the leave in MetaMask, then wait for the members…');
      const reply = await ibc.leave(c.id);
      status($('#leave-status'), reply === true ? 'You left the contract.' : `Refused: ${pretty(reply)}`, reply === true ? 'ok' : 'bad');
    });
  });
  $('#contract-area').classList.remove('hidden');
}
