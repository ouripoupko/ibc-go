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
const STATIC = new URL('../static/', import.meta.url);
const CHROME = process.env.CHROME ?? '/usr/bin/google-chrome';
const SHOTS = process.env.SHOTS ?? ''; // a directory (ending in /) to save screenshots in
const APP_PORT = 2799;
const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, args: ['--no-sandbox'] });
const authenticators = new Map(); // page -> { cdp, id }

// An app on another site: the test app page, the chat's source, and the client modules.
const app = http.createServer(async (req, res) => {
  const path = new URL(req.url, 'http://x').pathname;
  try {
    const file = path === '/app.html' ? new URL('./app.html', import.meta.url)
      : path === '/Chat.sol' ? new URL('Chat.sol', CONTRACTS) : new URL(`.${path}`, STATIC);
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
async function syncPasskey(from, to, credentialIdHex) {
  const src = authenticators.get(from);
  const dst = authenticators.get(to);
  const { credentials } = await src.cdp.send('WebAuthn.getCredentials', { authenticatorId: src.id });
  const want = Buffer.from(credentialIdHex.replace(/^0x/, ''), 'hex').toString('base64');
  const cred = credentials.find((c) => c.credentialId === want);
  if (!cred) throw new Error('passkey not found');
  await dst.cdp.send('WebAuthn.addCredential', { authenticatorId: dst.id, credential: cred });
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
  const before = (await text(appPage, '#out')).split('\n').length;
  const popupPage = approve ? newPopup() : null;
  await appPage.click(`#${button}`);
  if (approve) {
    const popup = await popupPage;
    await watch(popup, 520);
    await addAuthenticator(popup);
    await syncPasskey(approve.from, popup, approve.credential);
    // A browser where the user is not signed in to the node yet: sign in first.
    await popup.waitForFunction(() => !document.querySelector('#buttons').classList.contains('hidden')
      || !document.querySelector('#sign-in-box').classList.contains('hidden'), { timeout: 90000 });
    if (await popup.$eval('#sign-in-box', (b) => !b.classList.contains('hidden'))) {
      await popup.click('#sign-in');
      await popup.waitForSelector('#buttons:not(.hidden)', { timeout: 90000 });
      approve.signedIn = true;
    }
    approve.seen = {
      title: await text(popup, '#title'), what: await text(popup, '#what'),
      permissions: await popup.$eval('#permissions', (p) => !p.classList.contains('hidden')),
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

  // Bob on node B: create an identity, join with the invite, read and write.
  const pb = await pageFor(ctx, 390);
  await createIdentity(pb, `${B}/#join=${invite}`, 'bob');
  check('invite prefilled from the link', await pb.$eval('#j-contract', (e) => e.value) === counter);
  await pb.click('#j-submit');
  await waitText(pb, '#join-status', /Joined|refused/, 60000);
  check('bob joined', /Joined/.test(await text(pb, '#join-status')), await text(pb, '#join-status'));
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
  n = boxes.length;
  await pb.click('#j-submit');
  await waitText(pb, '#join-status', /Joined|refused/, 60000);
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

  check('message boxes closed', await pa.$eval('body', () => !document.querySelector('dialog.signing')?.open));
  console.log('passkey requests:', boxes.length);
} catch (err) {
  console.log('FAIL', err.message);
  process.exitCode = 1;
} finally {
  await browser.close();
  app.close();
}
