// The contract page: details, an invite, and a form for each ABI function.
import {
  $, el, status, busy, errorText, connect, functionForm, isRead, encodeInvite, pretty, short,
  loadIdentity, sourceHashOf, sourcePaths, showSource, writeFunctions, permissionItems, registerPermission,
  functionLabel, contractName, signatureOf,
} from './app.mjs';
import { signBatch } from './js/index.js';

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
    ['Root contract', c.contract],
    ['Who may join', (c.membership ?? []).includes('approve_partner') ? 'whom the contract approves' : 'anyone a member invites'],
    ['Source files', c.source ? sourcePaths(c.source).join(', ') : undefined],
    ['Deployment error', c.deploy_error],
    ['Source', c.source_check === 'verified' ? 'checked: it compiles to the deployed code' : undefined],
  ];
  // Joined, but the node could not check the source against the code.
  if (c.source_check === 'unchecked') {
    $('#source-warning').textContent = `The code of this contract was not checked against its source: ${c.source_check_reason || 'no reason given'}. What the source says may not be what the contract does.`;
    $('#source-warning').classList.remove('hidden');
  }
  $('#details').replaceChildren(...rows.filter(([, v]) => v !== undefined && v !== '').flatMap(([k, v]) => [
    el('dt', {}, k), el('dd', { class: /id|root|address|Created/.test(k) ? 'mono' : '' }, v),
  ]));

  // A guest's node has no copy to welcome others into.
  $('#invite-card').classList.toggle('hidden', guest);
  const invite = encodeInvite({ contract: c.id, node: location.origin, agent: ibc.agent });
  $('#invite').textContent = invite;
  $('#copy-source').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(JSON.stringify(c.source ?? {}, sourcePaths(c.source), 2));
      $('#copy-source').textContent = 'Copied';
    } catch {
      $('#code').replaceChildren(el('pre', { class: 'result' }, JSON.stringify(c.source ?? {}, sourcePaths(c.source), 2)));
      $('#code').classList.remove('hidden');
      getSelection().selectAllChildren($('#code'));
    }
  });
  $('#copy-invite').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(invite);
      $('#copy-invite').textContent = 'Copied';
    } catch {
      getSelection().selectAllChildren($('#invite'));
    }
  });

  // A partner may let anyone read its copy; a guest keeps none.
  $('#public-card').classList.toggle('hidden', guest);
  let isPublic = c.public === true;
  const showPublic = () => {
    $('#public-state').textContent = isPublic ? 'Anyone may read this contract.' : 'Only you, and the apps you allowed, read this contract.';
    $('#public-link').textContent = `${location.origin}/ibc/app/${ibc.agent}/${c.id}/<function>?action=public_read&values={…}`;
    $('#public-link-row').classList.toggle('hidden', !isPublic);
    $('#public-toggle').textContent = isPublic ? 'Make it private' : 'Make it public';
  };
  showPublic();
  $('#public-toggle').addEventListener('click', () => busy($('#public-toggle'), $('#public-status'), async () => {
    status($('#public-status'), 'Approve with your passkey…');
    isPublic = await ibc.setPublic(c.id, !isPublic);
    showPublic();
    status($('#public-status'), '');
  }));

  // The forms of the chosen contract: the root contract, or another of the
  // source's at an address in the agreement's world.
  const showFunctions = () => {
    const name = $('#target-contract').value || c.contract;
    const at = name === c.contract ? undefined : $('#target-address').value.trim();
    const fns = (c.abis?.[name] ?? []).filter((x) => x.type === 'function' && x.name);
    const reads = fns.filter(isRead);
    const writes = fns.filter((f) => !isRead(f));
    const run = (op) => (f) => (values) => {
      if (name !== c.contract && !/^0x[0-9a-fA-F]{40}$/.test(at)) throw new Error('give the contract\'s address');
      return ibc[op](c.id, signatureOf(f), values, { at });
    };
    $('#reads').replaceChildren(...reads.map((f) => functionForm(f, 'Read', run('read')(f))));
    $('#writes').replaceChildren(...writes.map((f) => functionForm(f, 'Send', run('write')(f))));
    if (!reads.length) $('#reads').append(el('p', { class: 'hint' }, 'No read functions.'));
    if (!writes.length) $('#writes').append(el('p', { class: 'hint' }, 'No transaction functions.'));
  };
  const deployable = Object.keys(c.abis ?? {}).filter((name) => (c.abis[name] ?? []).some((x) => x.type === 'function'));
  $('#target-contract').replaceChildren(...[c.contract, ...deployable.filter((n) => n !== c.contract)].map((name) =>
    el('option', { value: name }, name === c.contract ? `${contractName(name)} (root)` : contractName(name))));
  $('#target-address').value = c.evm_address ?? '';
  $('#target-address').disabled = true;
  $('#target-contract').addEventListener('change', () => {
    const root = $('#target-contract').value === c.contract;
    $('#target-address').disabled = root;
    $('#target-address').value = root ? (c.evm_address ?? '') : '';
    showFunctions();
  });
  $('#target-address').addEventListener('change', showFunctions);
  $('#target-card').classList.toggle('hidden', deployable.length < 2);
  showFunctions();
  if (c.source) {
    showSource($('#code'), c.source);
    $('#code-toggle').classList.remove('hidden');
    $('#show-code').addEventListener('click', () => {
      const shown = $('#code').classList.toggle('hidden');
      $('#show-code').textContent = shown ? 'Show the code' : 'Hide the code';
    });
  }

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
  showApps(c).catch((err) => status($('#apps-status'), errorText(err), 'bad'));
}

/** The apps the user connected, and what each may do with this contract's code without asking. */
async function showApps(c) {
  if (!c.source) return;
  const sourceHash = sourceHashOf(c.source);
  const writes = writeFunctions(c.abis);
  const apps = (await ibc.apps()).filter((a) => !a.permissions[`0x${'0'.repeat(64)}`]); // not this page's own keys
  $('#app-perms').replaceChildren(...apps.map((a) => {
    const now = a.permissions[sourceHash] ?? { read: false, functions: [] };
    const read = el('input', { type: 'checkbox', checked: now.read });
    const boxes = writes.map((f) => el('input', { type: 'checkbox', value: f.signature, checked: now.functions.includes(f.signature) }));
    const save = el('button', { class: 'secondary' }, 'Save');
    const box = el('div', { class: 'app-perm', 'data-app': a.app },
      el('h3', {}, new URL(a.app).host),
      el('label', { class: 'check' }, read, 'Read this contract'),
      writes.length ? el('p', { class: 'hint' }, 'Call these functions (your node signs them for you):') : null,
      ...writes.map((f, i) => el('label', { class: 'check' }, boxes[i], functionLabel(f, c.abis))),
      save);
    save.addEventListener('click', () => busy(save, $('#apps-status'), async () => {
      status($('#apps-status'), 'Approve with your passkey…');
      const items = permissionItems(ibc, {
        app: a.app, appKey: a.keys[0], sourceHash, read: read.checked, functions: boxes.filter((b) => b.checked).map((b) => b.value),
      });
      await registerPermission(ibc, items, await signBatch(ibc.root, items));
      status($('#apps-status'), `Saved for ${new URL(a.app).host}.`, 'ok');
    }));
    return box;
  }));
  $('#apps-card').classList.toggle('hidden', apps.length === 0);
}
