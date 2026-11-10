// A minimal ibc client for browsers and Node.js 20+. No dependencies but the
// vendored keccak in ./vendor.
//
// An agent has two keys (see docs/keys.md): its root key, on the user's
// device, which signs everything the user approves; and its node's key,
// which signs calls silently, only those the user allowed, under the user's
// grant. Apps get a key of their own, which the node knows from the user's
// consent: with it they read, and ask the node for silent calls.
//
// Two clients:
//   - Ibc: talks to the node with the agent's keys: the root signs
//     transactions; an app key (or the root) signs queries. The node's own
//     web page uses it, and so can tools.
//   - IbcApp: for an app on another site. It reads and writes silently where
//     the user allowed it, and otherwise opens the user's node, where the user
//     approves with the root passkey.
//
//   import { IbcApp } from './ibc.mjs';
//   const app = await IbcApp.open('https://node.example');
//   const chat = await app.deploy({ source, constructor: { initialTopic: 'lunch' } });  // the user approves
//   await app.write(chat, 'post', { text: 'hi' });      // silent, if the user allowed it
//   await app.write(chat, 'setTopic', { newTopic: 'x' }); // the user approves on the node

import {
  newP256Key, nonce, now, p256Signer, sign, toHex,
} from './keys.mjs';

export class IbcError extends Error {}

/** Does an error mean the user must approve on the node's page? */
export const needsApproval = (err) => /needs your approval|nor an app it connected/.test(err?.message ?? '');

export class Ibc {
  /**
   * Connects to a node as an agent.
   * @param {string} url the node, e.g. 'https://node.example'
   * @param {{agent: string, root?: object, appKey?: {privateKey, publicKey}}} keys
   *   root: a signer for the root key (keys.mjs); appKey: a WebCrypto key the node
   *   knows from the user's consent, which signs queries without asking
   */
  static async connect(url, { agent, root, appKey }) {
    const ibc = new Ibc(url, agent);
    ibc.root = root;
    ibc.appKey = appKey;
    ibc.nodeKey = (await ibc.request('GET', 'node_info', { agent: '' })).nodeKey;
    return ibc;
  }

  /** What a node knows of a registered agent: { agent, rootKey, rootScheme, credential }. */
  static async agentInfo(url, { agent = '', credential = '' }) {
    const base = url.replace(/\/+$/, '');
    const path = agent ? `/${agent}` : '';
    const res = await fetch(`${base}/ibc/app${path}?action=agent_info${credential ? `&credential=${credential}` : ''}`);
    const reply = await res.json();
    if (reply?.message) throw new IbcError(reply.message);
    return reply;
  }

  constructor(url, agent) {
    this.url = url.replace(/\/+$/, '');
    this.agent = agent;
    this.pending = new Map(); // request id -> { resolve, timer }
    this.inFlight = new Set(); // per request being sent: the events that arrive meanwhile
    this.listeners = new Set();
    this.stream = null;
  }

  /** One request to /ibc/app. Returns the decoded JSON reply. auth is a signed query for the URL. */
  async request(verb, action, { agent = this.agent, contract = '', method = '', body, auth } = {}) {
    const path = [agent, contract, method].filter(Boolean).map(encodeURIComponent).join('/');
    const query = `action=${action}` + (auth ? `&auth=${encodeURIComponent(JSON.stringify(auth))}` : '');
    const res = await fetch(`${this.url}/ibc/app${path ? '/' + path : ''}?${query}`, {
      method: verb,
      headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const reply = await res.json();
    if (reply && typeof reply === 'object' && !Array.isArray(reply) && 'message' in reply) {
      throw new IbcError(reply.message);
    }
    return reply;
  }

  /** Signs a transaction with the root key. body is an object, sent as JSON text. */
  async sign(kind, contract, body) {
    if (!this.root) throw new IbcError('no root key to sign with');
    const tx = { contract, kind, agent: this.agent, nonce: nonce(), body: JSON.stringify(body) };
    const { envelope } = await sign(this.root, 'Transaction', tx);
    return { tx, signature: envelope };
  }

  /**
   * Signs a query to the node, with the app key (silently) or else the root.
   * A query is accepted once, within a minute of signing.
   */
  async query(action, { contract = '', body, signer } = {}) {
    const query = {
      agent: this.agent, action, target: this.agent, contract,
      body: body === undefined ? '' : JSON.stringify(body),
      time: now(), nonce: nonce(),
    };
    const s = signer ?? (this.appKey ? p256Signer(this.appKey) : this.root);
    if (!s) throw new IbcError('no key to sign a query with');
    const { envelope } = await sign(s, 'Query', query);
    return { query, signature: envelope };
  }

  /** Opens the event stream (once). Resolves when the node has subscribed us. */
  connect() {
    if (this.stream) return this.stream;
    const controller = new AbortController();
    const stream = new Promise((resolve, reject) => {
      this.query('stream').then((auth) => fetch(`${this.url}/stream?auth=${encodeURIComponent(JSON.stringify(auth))}`, {
        signal: controller.signal,
      })).then(async (res) => {
        if (!res.ok) throw new IbcError(`stream: ${(await res.json()).message}`);
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buffer = '';
        let connected = false;
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break; // the node closed it: the next call opens a new one
          buffer += decoder.decode(value, { stream: true });
          let end;
          while ((end = buffer.indexOf('\n\n')) >= 0) {
            const chunk = buffer.slice(0, end);
            buffer = buffer.slice(end + 2);
            if (!connected) { connected = true; resolve(); }
            const data = chunk.startsWith('data: ') ? chunk.slice(6) : '';
            if (data) this.#dispatch(JSON.parse(data));
          }
        }
      }).catch((err) => { if (err.name !== 'AbortError') reject(err); })
        .finally(() => { if (this.stream === stream) this.stream = null; });
    });
    this.stream = stream;
    this.abort = () => controller.abort();
    return stream;
  }

  #dispatch(event) {
    for (const listener of this.listeners) listener(event);
    const waiter = this.pending.get(event.request);
    if (waiter) {
      this.pending.delete(event.request);
      clearTimeout(waiter.timer);
      waiter.resolve(event);
    } else {
      for (const seen of this.inFlight) seen.push(event);
    }
  }

  /** Calls fn(event) for every event on the stream. */
  onEvent(fn) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  /** Sends a request whose result arrives on the event stream, and waits for that event. */
  async submit(verb, action, options, timeoutMs = 60000) {
    await this.connect();
    // The request's id comes back in the reply, but its outcome can arrive on
    // the stream before the reply does: collect events while the request is
    // in flight, and look for the outcome among them.
    const seen = [];
    this.inFlight.add(seen);
    let id;
    try {
      id = await this.request(verb, action, options);
    } finally {
      this.inFlight.delete(seen);
    }
    if (id === null) throw new IbcError(`${action}: nothing to do`);
    const early = seen.find((event) => event.request === id);
    if (early) return early;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new IbcError(`no result for request ${id}`));
      }, timeoutMs);
      this.pending.set(id, { resolve, timer });
    });
  }

  /**
   * The registration of this agent on the node: it authorizes the node's key
   * to act for the agent. credential is the root passkey's id, if the root is one.
   */
  registration(credential = '0x') {
    return { agent: this.agent, node: this.url, nodeKey: this.nodeKey, credential, nonce: nonce() };
  }

  /** Registers this agent. signature: the root's envelope, if signed already (in a batch). */
  async register(registration = this.registration(), signature = null) {
    if (!signature) ({ envelope: signature } = await sign(this.root, 'Registration', registration));
    const event = await this.submit('PUT', 'register_agent', { body: { registration, signature } });
    return event.reply;
  }

  /** Is this agent registered on the node? */
  exists() {
    return this.request('GET', 'is_exist_agent');
  }

  /** Compiles Solidity source on the node: { contract, code, abi, codeHash, compiler }. */
  compile(source, contract = '') {
    return this.request('POST', 'compile', { agent: '', body: { source, contract } });
  }

  /** The deploy transaction's body, for a compiled contract. */
  deployBody({ name, code, abi, constructor = {}, source, compiler, ...rest }) {
    return {
      name, contract: `${name}.sol`, default_app: '', profile: '', ...rest,
      code: code.trim(), abi: typeof abi === 'string' ? JSON.parse(abi) : abi, constructor,
      node: this.url, nodeKey: this.nodeKey, ...(source ? { source, compiler } : {}),
    };
  }

  /**
   * Deploys a contract and returns its id once it is live. tx: the deploy
   * transaction, if signed already (in a batch); otherwise the root signs it.
   */
  async deploy(contract, tx = null) {
    const body = tx ?? await this.sign('deploy', '', this.deployBody(contract));
    const event = await this.submit('PUT', 'deploy_contract', { body });
    if (event.reply !== true) throw new IbcError(`deploy failed: ${event.reply}`);
    return event.contract;
  }

  /**
   * A transaction signed by the root key: changes the contract's state.
   * Resolves with the method's return value; rejects with the revert reason.
   */
  async write(contract, method, values = {}) {
    return this.#result(await this.submit('POST', 'contract_write',
      { contract, method, body: await this.sign('call', contract, { method, values }) }));
  }

  /** A call the node signs silently, if the user allowed this app this function. */
  async writeSilently(contract, method, values = {}) {
    const body = await this.query('contract_write', { contract, body: { method, values } });
    return this.#result(await this.submit('POST', 'contract_write', { contract, method, body }));
  }

  #result(event) {
    if (typeof event.reply === 'string' && /^execution reverted|^the node's grant/.test(event.reply)) {
      throw new IbcError(event.reply);
    }
    return event.reply;
  }

  /** A read of a view or pure function: returns its result, at once. */
  async read(contract, method, values = {}, signer) {
    const body = await this.query('contract_read', { contract, body: { method, values }, signer });
    return this.request('POST', 'contract_read', { contract, method, body });
  }

  /** This agent's contracts on the node (an app sees those it may read). */
  async contracts() {
    return this.request('GET', 'get_contracts', { auth: await this.query('get_contracts') });
  }

  /** One of this agent's contracts: its summary. */
  async contract(contract) {
    return this.request('GET', 'get_contract', { contract, auth: await this.query('get_contract', { contract }) });
  }

  /** The apps this agent connected, and what each may do. */
  async apps() {
    return this.request('GET', 'get_apps', { auth: await this.query('get_apps') });
  }

  /** Records a consent (and grant) the root signed. */
  registerApp(consent, grant = null) {
    return this.request('PUT', 'register_app', { body: grant ? { consent, grant } : { consent } });
  }

  /** Records a device the root signed: another browser of an app the agent approved. */
  registerDevice(device) {
    return this.request('PUT', 'register_app', { body: { device } });
  }

  /** Disconnects an app (by its site), with all its browsers. The root signs. */
  async revokeApp(app) {
    const body = await this.query('revoke_app', { body: { app }, signer: this.root });
    return this.request('PUT', 'revoke_app', { body });
  }

  /**
   * Asks a member of a contract to let this agent join. Resolves with true once
   * this node holds the contract, or false if the contract refused. A guest
   * takes no part in consensus: this node sends its transactions and reads to
   * the member's node, its host.
   */
  async join(contract, memberUrl, memberAgent, { guest = false, profile = '' } = {}) {
    const body = { node: this.url, nodeKey: this.nodeKey, profile };
    if (guest) Object.assign(body, { guest: true, host: memberAgent });
    const stx = await this.sign('join', contract, body);
    const event = await this.submit('PUT', 'join_contract', {
      body: { ...stx, partner: { address: memberUrl, agent: memberAgent } },
    });
    return event.reply;
  }

  /** Leaves a contract, or asks to remove another member. Resolves with the decision. */
  async leave(contract, member = this.agent) {
    const event = await this.submit('PUT', 'leave_contract', { body: await this.sign('leave', contract, { member }) });
    return event.reply;
  }

  /** Closes the event stream. */
  close() {
    if (this.abort) this.abort();
    for (const { timer } of this.pending.values()) clearTimeout(timer);
    this.pending.clear();
    this.stream = null;
  }
}

// Apps on other sites.

/**
 * Opens the user's node's approval page with a request, and resolves with its
 * answer. The user approves there with the root passkey. A browser opens the
 * page only on a user's click, so call this from a click handler.
 */
export function approve(nodeUrl, request) {
  const node = new URL(nodeUrl).origin;
  // A new window for each request, so that an old approval page is never reused.
  const popup = window.open(`${node}/approve.html`, `ibc-approve-${nonce()}`, 'width=520,height=720');
  if (!popup) return Promise.reject(new IbcError('the approval window was blocked'));
  return new Promise((resolve, reject) => {
    const onMessage = (e) => {
      if (e.origin !== node || e.source !== popup) return;
      if (e.data?.type === 'ibc-ready') popup.postMessage(request, node);
      else if (e.data?.type === 'ibc-done') done(null, e.data.result);
      else if (e.data?.type === 'ibc-denied') done(new IbcError(e.data.reason || 'the user did not approve'));
    };
    const timer = setInterval(() => { if (popup.closed) done(new IbcError('the approval window was closed')); }, 500);
    function done(err, result) {
      clearInterval(timer);
      window.removeEventListener('message', onMessage);
      if (err) reject(err);
      else resolve(result);
    }
    window.addEventListener('message', onMessage);
  });
}

/** Keeps an app's key and the agent it acts for in the app's own storage (IndexedDB). */
async function appStore(op, value) {
  const db = await new Promise((resolve, reject) => {
    const open = indexedDB.open('ibc-app', 1);
    open.onupgradeneeded = () => open.result.createObjectStore('app');
    open.onsuccess = () => resolve(open.result);
    open.onerror = () => reject(open.error);
  });
  try {
    return await new Promise((resolve, reject) => {
      const store = db.transaction('app', op === 'get' ? 'readonly' : 'readwrite').objectStore('app');
      const req = op === 'get' ? store.get('current') : store.put(value, 'current');
      req.onsuccess = () => resolve(req.result ?? null);
      req.onerror = () => reject(req.error);
    });
  } finally {
    db.close();
  }
}

/**
 * The client of an app on another site. The app has a key of its own, which
 * this browser keeps and cannot export; the user's node learns it when the
 * user first approves the app.
 */
export class IbcApp {
  /** Opens the app's connection to the user's node, making the app's key the first time. */
  static async open(nodeUrl) {
    let kept = await appStore('get');
    if (!kept || kept.node !== new URL(nodeUrl).origin) {
      kept = { node: new URL(nodeUrl).origin, key: await newP256Key(), agent: null };
      await appStore('put', kept);
    }
    const app = new IbcApp(kept);
    if (kept.agent) await app.#connect(kept.agent);
    return app;
  }

  constructor(kept) {
    this.kept = kept;
    this.node = kept.node;
    this.ibc = null;
  }

  /** The agent the app acts for, once the user approved it. */
  get agent() {
    return this.kept.agent;
  }

  async #connect(agent) {
    if (this.kept.agent !== agent) {
      this.kept.agent = agent;
      await appStore('put', this.kept);
    }
    this.ibc ??= await Ibc.connect(this.node, { agent, appKey: this.kept.key });
  }

  async #approve(request) {
    const result = await approve(this.node, { ...request, appKey: toHex(this.kept.key.publicKey) });
    if (result?.agent) await this.#connect(result.agent);
    return result;
  }

  /**
   * Deploys a contract from Solidity source: the user's node compiles it, and
   * the user approves on the node's page, choosing the app's permissions the
   * first time this code is used. Resolves with the contract's id.
   */
  async deploy({ source, contract = '', name, constructor = {}, protocol }) {
    return (await this.#approve({ type: 'deploy', source, contract, name, constructor, protocol })).contract;
  }

  /** Asks the user for permissions on an existing contract (one the agent joined). */
  async permit(contract) {
    return this.#approve({ type: 'permit', contract });
  }

  /** Asks the user to join a contract, through a member's node; as a guest with { guest: true }. */
  async join(contract, memberNode, memberAgent, { guest = false } = {}) {
    return (await this.#approve({ type: 'join', contract, memberNode, memberAgent, guest })).joined;
  }

  /** Reads silently if the user allowed it, and otherwise asks the user. */
  async read(contract, method, values = {}) {
    if (this.ibc) {
      try {
        return await this.ibc.read(contract, method, values);
      } catch (err) {
        if (!needsApproval(err)) throw err;
      }
    }
    return (await this.#approve({ type: 'read', contract, method, values })).value;
  }

  /** Writes silently if the user allowed this function, and otherwise asks the user. */
  async write(contract, method, values = {}) {
    if (this.ibc) {
      try {
        return await this.ibc.writeSilently(contract, method, values);
      } catch (err) {
        if (!needsApproval(err)) throw err;
      }
    }
    const { reply } = await this.#approve({ type: 'call', contract, method, values });
    if (typeof reply === 'string' && reply.startsWith('execution reverted')) throw new IbcError(reply);
    return reply;
  }

  /** Calls fn(event) for every event of the contracts the app may read. */
  onEvent(fn) {
    if (!this.ibc) throw new IbcError('not approved yet');
    this.ibc.connect();
    return this.ibc.onEvent(fn);
  }
}
