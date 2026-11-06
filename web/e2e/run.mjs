// End-to-end test of the web interface in headless Chrome, with a stand-in
// for MetaMask (window.ethereum) that signs with an ethers.js wallet. Two
// nodes, two users: register, deploy, write, read, invite, join, write.
// Run it with web/e2e/run.sh, which builds and starts the nodes.
import puppeteer from 'puppeteer-core';
import { Wallet } from 'ethers';

const [A, B] = process.argv.slice(2);
const BUILD = new URL('../../examples/build', import.meta.url).pathname;
const CHROME = process.env.CHROME ?? '/usr/bin/google-chrome';
const SHOTS = process.env.SHOTS ?? '';  // a directory (ending in /) to save screenshots in
const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, args: ['--no-sandbox'] });
let prompts = 0;

async function pageFor(wallet, width = 1100) {
  const page = await browser.newPage();
  await page.setViewport({ width, height: 900 });
  page.on('pageerror', (e) => console.log('PAGE ERROR', e.message));
  page.on('console', (m) => { if (m.type() === 'error') console.log('CONSOLE', m.text()); });
  await page.exposeFunction('__sign', async (json) => {
    prompts++;
    const data = JSON.parse(json);
    delete data.types.EIP712Domain;
    return wallet.signTypedData(data.domain, data.types, data.message);
  });
  await page.evaluateOnNewDocument((address) => {
    window.ethereum = {
      async request({ method, params }) {
        if (method === 'eth_accounts' || method === 'eth_requestAccounts') return [address];
        if (method === 'eth_signTypedData_v4') return window.__sign(params[1]);
        throw new Error(`unsupported ${method}`);
      },
      on() {},
    };
  }, wallet.address);
  return page;
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

// Forms on the contract page, by function name.
async function runFn(page, section, name, values) {
  const form = await page.evaluateHandle((sec, n) => [...document.querySelectorAll(`${sec} form.fn`)]
    .find((f) => f.querySelector('h3').textContent === n), section, name);
  const inputs = await form.$$('input[type=text], textarea');
  for (let i = 0; i < values.length; i++) {
    await inputs[i].click({ clickCount: 3 });
    await inputs[i].type(String(values[i]));
  }
  await (await form.$('button')).click();
  await page.waitForFunction((f) => !f.querySelector('button').disabled && /result (ok|bad)/.test(f.querySelector('pre').className), { timeout: 30000 }, form);
  return form.evaluate((f) => ({ text: f.querySelector('pre').textContent, cls: f.querySelector('pre').className }));
}

try {
  const alice = Wallet.createRandom();
  const bob = Wallet.createRandom();

  // Alice on node A: register, deploy.
  const pa = await pageFor(alice);
  await pa.goto(`${A}/`);
  await pa.waitForSelector('#register-card:not(.hidden)');
  check('unregistered agent sees Register', true);
  await pa.click('#register');
  await waitText(pa, '#list-status', /no contracts/);
  check('registered; empty list', true, await text(pa, '#who'));
  if (SHOTS) await pa.screenshot({ path: `${SHOTS}index-empty.png`, fullPage: true });

  await (await pa.$('#d-bin-file')).uploadFile(`${BUILD}/Counter.bin`);
  await (await pa.$('#d-abi-file')).uploadFile(`${BUILD}/Counter.abi`);
  await pa.waitForFunction(() => document.querySelector('#d-bin').value.length > 10 && document.querySelector('#d-ctor input'));
  check('files loaded; constructor form shown', await text(pa, '#d-ctor').then((t) => /start/.test(t)), await pa.$eval('#d-name', (n) => n.value));
  await pa.type('#d-ctor input', '10');
  await pa.click('#d-submit');
  await waitText(pa, '#deploy-status', /Deployed/);
  check('deployed', true);
  if (SHOTS) await pa.screenshot({ path: `${SHOTS}index-deployed.png`, fullPage: true });

  // The contract page.
  await pa.click('#deploy-status a');
  await pa.waitForSelector('#contract-area:not(.hidden)');
  const forms = await pa.$$eval('form.fn h3', (hs) => hs.map((h) => h.textContent));
  check('ABI forms', forms.includes('increment') && forms.includes('count'), forms.join(' '));
  let r = await runFn(pa, '#writes', 'increment', [5]);
  check('increment(5) -> 15', r.text === '15' && r.cls.includes('ok'), r.text);
  r = await runFn(pa, '#writes', 'increment', [0]);
  check('increment(0) reverts, shown as error', r.cls.includes('bad') && /amount must be positive/.test(r.text), r.text);
  r = await runFn(pa, '#reads', 'count', []);
  check('count -> 15', r.text === '15', r.text);
  r = await runFn(pa, '#reads', 'contributions', [alice.address]);
  check('contributions(alice) -> 5 (unnamed parameter)', r.text === '5', r.text);
  const events = await text(pa, '#events');
  check('events listed', /contract_write/.test(events), events.slice(0, 80));
  const invite = await text(pa, '#invite');
  if (SHOTS) await pa.screenshot({ path: `${SHOTS}contract.png`, fullPage: true });

  // Bob on node B: register, join with the invite, read and write.
  const pb = await pageFor(bob, 390);
  await pb.goto(`${B}/#join=${invite}`);
  await pb.waitForSelector('#register-card:not(.hidden)');
  await pb.click('#register');
  await waitText(pb, '#list-status', /no contracts/);
  check('invite prefilled from the link', await pb.$eval('#j-contract', (n) => n.value) !== '');
  await pb.click('#j-submit');
  await waitText(pb, '#join-status', /Joined|refused/, 60000);
  check('bob joined', /Joined/.test(await text(pb, '#join-status')), await text(pb, '#join-status'));
  if (SHOTS) await pb.screenshot({ path: `${SHOTS}index-phone.png`, fullPage: true });
  await pb.click('#join-status a');
  await pb.waitForSelector('#contract-area:not(.hidden)');
  r = await runFn(pb, '#reads', 'count', []);
  check('bob reads count -> 15', r.text === '15', r.text);
  r = await runFn(pb, '#writes', 'increment', [1]);
  check('bob increment(1) -> 16', r.text === '16', r.text);
  r = await runFn(pb, '#reads', 'myContribution', []);
  check('bob myContribution -> 1 (msg.sender is bob)', r.text === '1', r.text);
  if (SHOTS) await pb.screenshot({ path: `${SHOTS}contract-phone.png`, fullPage: true });

  // Alice sees bob's write on her node.
  await pa.bringToFront();
  await pa.reload();
  await pa.waitForSelector('#contract-area:not(.hidden)');
  r = await runFn(pa, '#reads', 'count', []);
  check('alice reads count -> 16', r.text === '16', r.text);
  console.log('signatures asked:', prompts);
} catch (err) {
  console.log('FAIL', err.message);
  process.exitCode = 1;
} finally {
  await browser.close();
}
