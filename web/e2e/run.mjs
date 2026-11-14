// End-to-end test of the web interface in headless Chrome, with Chrome's
// virtual passkey authenticator in place of a phone. Two nodes, two users:
// create an identity, deploy from source, write, read, invite, join, join as a guest, sign in
// on a new device; and an app on another site that deploys a chat, posts
// silently where the user allowed it, and asks the user for the rest.
// Run it with web/e2e/run.sh, which builds and starts the nodes.
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import puppeteer from 'puppeteer-core';

const [A, B] = process.argv.slice(2);
const CONTRACTS = new URL('../../examples/contracts/', import.meta.url);
const CLIENT = new URL('../../js/dist/', import.meta.url);
const CHROME = process.env.CHROME ?? '/usr/bin/google-chrome';
const SHOTS = process.env.SHOTS ?? ''; // a directory (ending in /) to save screenshots in
const APP_PORT = 2799;
const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, args: ['--no-sandbox'] });
const authenticators = new Map(); // page -> { cdp, id }

// An app on another site: the test app page, the chat's source, and the client package's modules.
const app = http.createServer(async (req, res) => {
  const path = new URL(req.url, 'http://x').pathname;
  try {
    const file = path === '/app.html' ? new URL('./app.html', import.meta.url)
      : path === '/Chat.sol' ? new URL('Chat.sol', CONTRACTS)
        : path.startsWith('/js/') ? new URL(`.${path.slice(3)}`, CLIENT) : null;
    if (!file) throw new Error('not found');
    const type = path.endsWith('.html') ? 'text/html' : path.endsWith('.sol') ? 'text/plain' : 'text/javascript';
    const body = await readFile(file);
    res.writeHead(200, { 'Content-Type': type }).end(body);
  } catch {
    res.writeHead(404).end();
  }
}).listen(APP_PORT, 'localhost');

/** Gives a page a virtual authenticator: a phone that approves every request. */
async function addAuthenticator(page) {
  const cdp = await page.createCDPSession();
  await cdp.send('WebAuthn.enable', { enableUI: false });
  const { authenticatorId: id } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true,
      isUserVerified: true, automaticPresenceSimulation: true,
    },
  });
  authenticators.set(page, { cdp, id });
}

/** Copies a passkey from one page's authenticator to another's: a passkey synced to another device. */
async function syncPasskey(from, to, credentialIdHex = null) {
  const src = authenticators.get(from);
  const dst = authenticators.get(to);
  const { credentials } = await src.cdp.send('WebAuthn.getCredentials', { authenticatorId: src.id });
  // One passkey by its id, or all of them.
  const want = credentialIdHex && Buffer.from(credentialIdHex.replace(/^0x/, ''), 'hex').toString('base64');
  const creds = want ? credentials.filter((c) => c.credentialId === want) : credentials;
  if (!creds.length) throw new Error('passkey not found');
  for (const credential of creds) await dst.cdp.send('WebAuthn.addCredential', { authenticatorId: dst.id, credential });
}

// The boxes the pages show while a passkey is asked: each passkey request.
const boxes = [];

async function pageFor(context, width = 1100) {
  const page = await context.newPage();
  await watch(page, width);
  await addAuthenticator(page);
  return page;
}

async function watch(page, width = 1100) {
  await page.setViewport({ width, height: 900 });
  page.on('pageerror', (e) => console.log('PAGE ERROR', e.message));
  page.on('console', (m) => { if (m.type() === 'error' && !/favicon|404/.test(m.text())) console.log('CONSOLE', m.text()); });
  await page.exposeFunction('__box', (t) => { boxes.push(t); });
  await page.evaluateOnNewDocument(() => {
    new MutationObserver((records) => {
      for (const r of records) {
        if (r.target.matches?.('dialog.signing') && r.target.open) window.__box(r.target.querySelector('h2')?.textContent ?? '');
      }
    }).observe(document, { subtree: true, attributes: true, attributeFilter: ['open'] });
  });
}

const text = (page, sel) => page.$eval(sel, (n) => n.textContent);
async function waitText(page, sel, re, timeout = 30000) {
  await page.waitForFunction((s, r) => new RegExp(r).test(document.querySelector(s)?.textContent ?? ''), { timeout }, sel, re.source);
  return text(page, sel);
}
function check(what, ok, detail = '') {
  console.log(ok ? 'ok  ' : 'FAIL', what, detail);
  if (!ok) process.exitCode = 1;
}
const since = (n) => boxes.slice(n);

// Forms on the contract page, by function name.
async function runFn(page, section, name, values) {
  const form = await page.evaluateHandle((sec, n) => [...document.querySelectorAll(`${sec} form.fn`)]
    .find((f) => f.dataset.fn === n), section, name);
  const inputs = await form.$$('input[type=text], textarea');
  for (let i = 0; i < values.length; i++) {
    await inputs[i].click({ clickCount: 3 });
    await inputs[i].type(String(values[i]));
  }
  await (await form.$('button')).click();
  await page.waitForFunction((f) => !f.querySelector('button').disabled && /result (ok|bad)/.test(f.querySelector('pre').className), { timeout: 60000 }, form);
  return form.evaluate((f) => ({ text: f.querySelector('pre').textContent, cls: f.querySelector('pre').className }));
}

async function createIdentity(page, url, name) {
  await page.goto(url);
  await page.waitForSelector('#connect-card:not(.hidden)');
  await page.type('#name', name);
  await page.click('#create');
  await waitText(page, '#list-status', /no contracts/);
  return page.evaluate(() => import('./app.mjs').then((m) => m.loadIdentity()));
}

// Approval windows already handled, so that each request waits for its own.
const seenPopups = new Set();
const newPopup = () => browser.waitForTarget((t) => t.url().includes('/approve.html') && !seenPopups.has(t), { timeout: 30000 })
  .then((t) => { seenPopups.add(t); return t.page(); });

/** Clicks a button in the test app; if the app opens the node's approval page, approves there. */
async function appClick(appPage, button, { approve = null } = {}) {
  await appPage.bringToFront(); // a background page does not poll
  const before = (await text(appPage, '#out')).split('\n').length;
  const popupPage = approve ? newPopup() : null;
  await appPage.click(`#${button}`);
  if (approve) {
    const popup = await popupPage;
    await watch(popup, 520);
    await addAuthenticator(popup);
    if (approve.from) await syncPasskey(approve.from, popup, approve.credential);
    // A browser where the user is not signed in to the node yet: sign in first.
    await popup.waitForFunction(() => !document.querySelector('#buttons').classList.contains('hidden')
      || !document.querySelector('#sign-in-box').classList.contains('hidden')
      || !document.querySelector('#renew-box').classList.contains('hidden'), { timeout: 90000 });
    // The node does not know this browser's identity any more: register again first.
    if (await popup.$eval('#renew-box', (b) => !b.classList.contains('hidden'))) {
      await popup.click('#renew');
      await popup.waitForSelector('#buttons:not(.hidden)', { timeout: 90000 });
      approve.renewed = true;
    }
    if (await popup.$eval('#sign-in-box', (b) => !b.classList.contains('hidden'))) {
      if (approve.create) {
        await popup.type('#new-name', approve.create);
        await popup.click('#create-identity');
      } else {
        await popup.click('#sign-in');
      }
      await popup.waitForSelector('#buttons:not(.hidden)', { timeout: 90000 });
      approve.signedIn = true;
      // Keep a passkey made in the popup on the app's page, as the user's phone would.
      if (approve.create) await syncPasskey(popup, appPage);
    }
    approve.seen = {
      title: await text(popup, '#title'), what: await text(popup, '#what'),
      permissions: await popup.$eval('#permissions', (p) => !p.classList.contains('hidden')),
      public: await popup.$eval('#public-box', (b) => !b.classList.contains('hidden') && b.querySelector('#public').checked),
    };
    for (const fn of approve.check ?? []) await popup.click(`#perm-writes input[value="${fn}"]`);
    if (SHOTS && approve.shot) await popup.screenshot({ path: `${SHOTS}${approve.shot}.png`, fullPage: true });
    await popup.click('#approve');
  }
  await appPage.waitForFunction((n) => document.querySelector('#out').textContent.split('\n').length > n, { timeout: 90000 }, before);
  return (await text(appPage, '#out')).split('\n')[before - 1];
}

try {
  const ctx = browser.defaultBrowserContext();

  // Alice on node A: create an identity, deploy from source.
  const pa = await pageFor(ctx);
  let n = boxes.length;
  const alice = await createIdentity(pa, `${A}/`, 'alice');
  check('identity created and registered', /^0x[0-9a-fA-F]{40}$/.test(alice.agent), alice.agent);
  check('two passkey requests to create it', since(n).join(' | ') === 'Create passkey: identity | Sign request: create identity on localhost:2731',
    since(n).join(' | '));
  if (SHOTS) await pa.screenshot({ path: `${SHOTS}index-empty.png`, fullPage: true });

  await (await pa.$('#d-source-file')).uploadFile(new URL('Counter.sol', CONTRACTS).pathname);
  await pa.click('#d-compile');
  await waitText(pa, '#compile-status', /Compiled|error|not/, 90000);
  check('the node compiled the source', /Compiled Counter/.test(await text(pa, '#compile-status')), await text(pa, '#compile-status'));
  await pa.type('#d-ctor input', '10');
  n = boxes.length;
  await pa.click('#d-submit');
  await waitText(pa, '#deploy-status', /Deployed/);
  check('deployed with one passkey request', since(n).join() === 'Sign request: deploy Counter 10', since(n).join(' | '));

  // The contract page.
  await pa.click('#deploy-status a');
  await pa.waitForSelector('#contract-area:not(.hidden)');
  const counter = new URL(pa.url()).searchParams.get('id');
  check('the code is behind a link', await pa.$eval('#code', (c) => c.classList.contains('hidden') && c.textContent.includes('contract Counter')));
  n = boxes.length;
  let r = await runFn(pa, '#writes', 'increment', [5]);
  check('increment(5) -> 15, one approval', r.text === '15' && since(n).join() === 'Sign request: increment 5', `${r.text} ${since(n)}`);
  r = await runFn(pa, '#writes', 'increment', [0]);
  check('increment(0) reverts, shown as error', r.cls.includes('bad') && /amount must be positive/.test(r.text), r.text);
  n = boxes.length;
  r = await runFn(pa, '#reads', 'count', []);
  check('count -> 15, read without asking', r.text === '15' && since(n).length === 0, `${r.text} ${since(n)}`);
  const invite = await text(pa, '#invite');
  if (SHOTS) await pa.screenshot({ path: `${SHOTS}contract.png`, fullPage: true });

  // Alice makes her copy public: anyone reads it, unsigned; then private again.
  const anyone = (agent, contract, method) => fetch(`${A}/ibc/app/${agent}/${contract}/${method}?action=public_read`)
    .then((res) => res.json());
  check('private by default', /no public contract/.test((await anyone(alice.agent, counter, 'count')).message ?? ''));
  n = boxes.length;
  await pa.click('#public-toggle');
  await waitText(pa, '#public-state', /Anyone may read/);
  check('made public, one approval', /^Sign request: make public/.test(since(n).join()), since(n).join(' | '));
  check('anyone reads count -> 15', await anyone(alice.agent, counter, 'count') === 15);
  await pa.click('#public-toggle');
  await waitText(pa, '#public-state', /Only you/);
  check('private again', /no public contract/.test((await anyone(alice.agent, counter, 'count')).message ?? ''));

  // Bob on node B: create an identity, join with the invite, read and write.
  const pb = await pageFor(ctx, 390);
  await createIdentity(pb, `${B}/#join=${invite}`, 'bob');
  check('invite prefilled from the link', await pb.$eval('#j-contract', (e) => e.value) === counter);
  // Bob reads what he joins: the source, which Alice sent with the invite.
  const setSource = (page, source) => page.$eval('#j-source', (e, v) => { e.value = v; }, source);
  await setSource(pb, await readFile(new URL('Counter.sol', CONTRACTS), 'utf8'));
  await pb.click('#j-submit');
  await waitText(pb, '#join-status', /Joined|refused|Not joined/, 90000);
  check('bob joined, the source checked', /^Joined: open/.test(await text(pb, '#join-status')), await text(pb, '#join-status'));
  await pb.click('#join-status a');
  await pb.waitForSelector('#contract-area:not(.hidden)');
  r = await runFn(pb, '#reads', 'count', []);
  check('bob reads count -> 15', r.text === '15', r.text);
  r = await runFn(pb, '#writes', 'increment', [1]);
  check('bob increment(1) -> 16', r.text === '16', r.text);
  r = await runFn(pb, '#reads', 'myContribution', []);
  check('bob myContribution -> 1 (msg.sender is bob)', r.text === '1', r.text);

  // Alice signs in on a new device, where her passkey is synced.
  const other = await browser.createBrowserContext();
  const pc = await pageFor(other);
  await syncPasskey(pa, pc, alice.root.credentialId);
  await pc.goto(`${A}/`);
  await pc.waitForSelector('#connect-card:not(.hidden)');
  n = boxes.length;
  await pc.click('#sign-in');
  await pc.waitForFunction(() => document.querySelectorAll('#contracts tbody tr').length === 1, { timeout: 30000 });
  check('signed in on a new device, two requests', (await text(pc, '#who')).includes(alice.agent) && since(n).length === 2,
    since(n).join(' | '));

  // An app on another site, for Alice.
  const pApp = await pageFor(ctx);
  await pApp.goto(`http://localhost:${APP_PORT}/app.html?node=${encodeURIComponent(A)}`);
  await pApp.waitForFunction(() => window.appReady === true);
  const approveAs = { from: pa, credential: alice.root.credentialId };

  let a = { ...approveAs, check: ['post'], shot: 'approve-deploy' };
  let line = await appClick(pApp, 'deploy', { approve: a });
  check('the app deployed a chat, approved on the node', /^deploy "0x/.test(line) && /wants to create a contract/.test(a.seen.title) && a.seen.permissions,
    `${line} · ${a.seen.title}`);
  n = boxes.length;
  line = await appClick(pApp, 'post');
  check('the app posts silently: no popup, no passkey', line === 'post 0' && since(n).length === 0, `${line} ${since(n)}`);
  line = await appClick(pApp, 'topic');
  check('the app reads silently', line === 'topic "lunch"', line);
  a = { ...approveAs };
  line = await appClick(pApp, 'setTopic', { approve: a });
  check('setTopic asks the user on the node', line === 'setTopic null' && /wants to: setTopic/.test(a.seen.title), `${line} · ${a.seen.title}`);
  line = await appClick(pApp, 'topic');
  check('topic -> dinner', line === 'topic "dinner"', line);
  a = { ...approveAs };
  line = await appClick(pApp, 'deploy', { approve: a });
  check('the same code again: approve the deploy, no permissions to choose', /^deploy "0x/.test(line) && !a.seen.permissions, line);
  const chat = JSON.parse(line.slice('deploy '.length));

  // The app proposes a public chat: the box is ticked on the node, and anyone reads the topic.
  a = { ...approveAs };
  line = await appClick(pApp, 'deployPublic', { approve: a });
  check('the app proposes a public chat', /^deployPublic "0x/.test(line) && a.seen.public, `${line} · public ${a.seen.public}`);
  line = await appClick(pApp, 'publicTopic');
  check('anyone reads its topic, unsigned', line === 'publicTopic "lunch"', line);
  check('a chat deployed without it stays private', /no public contract/.test((await anyone(alice.agent, chat, 'topic')).message ?? ''));

  // Alice uses the app on another computer. Its first request opens the node: she signs in there
  // (a new browser for the node too), and approving connects this browser of the app, with the
  // permissions she gave it: nothing to choose again. Then it posts silently.
  const laptop = await browser.createBrowserContext();
  const pApp2 = await pageFor(laptop);
  await pApp2.goto(`http://localhost:${APP_PORT}/app.html?node=${encodeURIComponent(A)}&chat=${chat}`);
  await pApp2.waitForFunction(() => window.appReady === true);
  a = { ...approveAs };
  line = await appClick(pApp2, 'post', { approve: a });
  check('another computer: its first post connects it, no permissions to choose',
    line === 'post 0' && a.signedIn && !a.seen.permissions && /new browser/.test(a.seen.what), `${line} · ${a.seen.what}`);
  n = boxes.length;
  line = await appClick(pApp2, 'post');
  check('then it posts silently', line === 'post 1' && since(n).length === 0, `${line} ${since(n)}`);

  // Bob joins that chat as a guest, from node B: his node keeps no copy, and sends his
  // transactions and reads to Alice's node.
  await pb.bringToFront();
  const chatInvite = await pb.evaluate((contract, node, agent) => import('./app.mjs')
    .then((m) => m.encodeInvite({ contract, node, agent })), chat, A, alice.agent);
  await pb.goto(`${B}/#join=${chatInvite}`);
  await pb.waitForFunction((c) => document.querySelector('#j-contract').value === c, { timeout: 30000 }, chat);
  await pb.click('#j-guest');
  await setSource(pb, await readFile(new URL('Chat.sol', CONTRACTS), 'utf8'));
  n = boxes.length;
  await pb.click('#j-submit');
  await waitText(pb, '#join-status', /Joined|refused|Not joined/, 90000);
  check('bob joined the chat as a guest, one approval', /Joined/.test(await text(pb, '#join-status'))
    && since(n).length === 1 && /join as a guest/.test(since(n)[0]), `${await text(pb, '#join-status')} ${since(n)}`);
  await pb.click('#join-status a');
  await pb.waitForSelector('#contract-area:not(.hidden)');
  check('the page shows a guest, with no invite', /guest/.test(await text(pb, '#details'))
    && await pb.$eval('#invite-card', (c) => c.classList.contains('hidden')), await text(pb, '#details'));
  r = await runFn(pb, '#reads', 'count', []);
  check('the guest reads count -> 2, through the host', r.text === '2', r.text);
  r = await runFn(pb, '#writes', 'post', ['hi from a guest']);
  check('the guest posts -> 2', r.text === '2', r.text);
  r = await runFn(pb, '#reads', 'count', []);
  check('count -> 3', r.text === '3', r.text);

  // On the chat's page on her node, Alice changes what the app may do without asking: no more
  // silent posts; then she allows them again.
  await pa.bringToFront();
  await pa.goto(`${A}/contract.html?id=${chat}`);
  await pa.waitForSelector('#apps-card:not(.hidden)', { timeout: 30000 });
  const perm = (fn) => `.app-perm[data-app="http://localhost:${APP_PORT}"] input[value="${fn}"]`;
  check('the contract page shows the app\'s permissions', await pa.$eval(perm('post'), (b) => b.checked)
    && !(await pa.$eval(perm('setTopic'), (b) => b.checked)));
  const savePerm = async () => {
    await pa.$eval('#apps-status', (e) => { e.textContent = ''; });
    await pa.click(`.app-perm[data-app="http://localhost:${APP_PORT}"] button`);
    await waitText(pa, '#apps-status', /Saved|rror/);
  };
  n = boxes.length;
  await pa.click(perm('post'));
  await savePerm();
  check('unticked post, one approval', since(n).join() === 'Sign request: allow localhost:2799', since(n).join(' | '));
  let postPopup = newPopup();
  await pApp.bringToFront();
  await pApp.click('#post');
  check('now the app\'s post asks the user', Boolean(await postPopup));
  await (await postPopup).close();
  await pApp.bringToFront(); // a background page does not poll
  await waitText(pApp, '#out', /post error/);
  await pa.bringToFront();
  await pa.click(perm('post'));
  await savePerm();
  line = await appClick(pApp, 'post');
  check('ticked again: the app posts silently', /^post \d+$/.test(line), line);

  // Alice disconnects the app on her node: its silent post now needs her again.
  await pa.bringToFront();
  await pa.goto(`${A}/`);
  await pa.waitForFunction(() => document.querySelectorAll('#apps li').length === 1, { timeout: 30000 });
  check('the node lists the app, with its two browsers', /localhost:2799 · 2 browsers.*calls post/.test(await text(pa, '#apps')), await text(pa, '#apps'));
  n = boxes.length;
  await pa.click('#apps li button');
  await pa.waitForFunction(() => document.querySelectorAll('#apps li').length === 0, { timeout: 30000 });
  check('disconnected with one approval', since(n).join() === 'Sign request: disconnect app', since(n).join(' | '));
  const popupPage = newPopup();
  await pApp.bringToFront();
  await pApp.click('#post');
  const popup = await popupPage;
  check('a disconnected app must ask the user again', Boolean(popup));
  await popup.close();

  // Carol, new to ibc, opens the app in her own browser. The app asks who she is: on her node's
  // page she creates her identity, and approves the app, which learns her agent id.
  const carolBrowser = await browser.createBrowserContext();
  const pApp3 = await pageFor(carolBrowser);
  await pApp3.goto(`http://localhost:${APP_PORT}/app.html?node=${encodeURIComponent(A)}`);
  await pApp3.waitForFunction(() => window.appReady === true);
  a = { create: 'carol' };
  line = await appClick(pApp3, 'connect', { approve: a });
  const carol = JSON.parse(line.slice('connect '.length));
  const registered = await (await fetch(`${A}/ibc/app/${carol}?action=is_exist_agent`)).json();
  check('connect: a new user creates an identity on the node, and the app learns it',
    /^0x[0-9a-fA-F]{40}$/.test(carol) && a.signedIn && registered === true && /wants to know who you are/.test(a.seen.title),
    `${line} · ${a.seen.title} · registered ${registered}`);

  // Through the app, Carol joins Alice's chat. She reads its source on her node, and approves at
  // once the join and what the app may do in it without asking, as for a deploy. The window
  // closes; the outcome reaches the app as an event. Then the app posts silently.
  await pApp3.goto(`http://localhost:${APP_PORT}/app.html?node=${encodeURIComponent(A)}&chat=${chat}`
    + `&member=${alice.agent}&memberNode=${encodeURIComponent(A)}`);
  await pApp3.waitForFunction(() => window.appReady === true);
  a = { from: pApp3, check: ['post'] };
  line = await appClick(pApp3, 'join', { approve: a });
  check('carol joins through the app, with its permissions, in one approval', line === 'join "true verified"'
    && a.seen.permissions && /asks you to join/.test(a.seen.title), `${line} · ${a.seen.title}`);
  n = boxes.length;
  line = await appClick(pApp3, 'post');
  check('carol\'s app posts silently in the joined chat', /^post \d+$/.test(line) && since(n).length === 0, `${line} ${since(n)}`);

  // Alice's node forgets her page's key (as when the node's own site is disconnected, or its
  // data reset). The node's page offers to register again, and so does the approval page.
  const forgetPage = () => pa.evaluate(async () => {
    const m = await import('./app.mjs');
    const ibc = await m.connect(await m.loadIdentity());
    await ibc.revokeApp(location.origin);
  });
  await pa.bringToFront();
  await forgetPage();
  await pa.goto(`${A}/`);
  await pa.waitForSelector('#register-card:not(.hidden)', { timeout: 30000 });
  check('the node page offers to register again', true);
  await pa.click('#register');
  await pa.waitForSelector('#agent-area:not(.hidden)', { timeout: 30000 });
  await pa.waitForFunction(() => /0x/.test(document.querySelector('#contracts')?.textContent ?? ''), { timeout: 30000 });
  check('registered again: the page reads again', true);
  await forgetPage();
  await pApp.bringToFront();
  a = { ...approveAs };
  line = await appClick(pApp, 'connect', { approve: a });
  check('the approval page registers again, then the request continues',
    a.renewed === true && line === `connect "${alice.agent}"`, `${line} · renewed ${a.renewed}`);

  check('message boxes closed', await pa.$eval('body', () => !document.querySelector('dialog.signing')?.open));
  console.log('passkey requests:', boxes.length);
} catch (err) {
  console.log('FAIL', err.message);
  process.exitCode = 1;
} finally {
  await browser.close();
  app.close();
}
