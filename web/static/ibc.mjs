// A minimal ibc client for browsers and Node.js 18+. No dependencies.
//
// Every transaction, and every read, is signed by the user's key with
// EIP-712 (in a browser wallet, each signature asks the user). Reads can
// instead be signed by a read session: a key the client makes, which the
// user authorizes once (see startSession). Pass a signer:
//   - in a browser, Ibc.wallet(window.ethereum) signs with the browser wallet
//     (MetaMask, Rabby, Coinbase Wallet, …);
//   - anything with getAddress() and signTypedData(domain, types, message),
//     such as an ethers.js Wallet, works too.
//
//   import { Ibc } from './ibc.mjs';
//   const ibc = await Ibc.connect('http://localhost:2611', Ibc.wallet(window.ethereum));
//   await ibc.register();
//   const contract = await ibc.deploy({ name: 'counter', code, abi, constructor: { start: 10 } });
//   await ibc.write(contract, 'increment', { amount: 5 });   // -> 15
//   await ibc.read(contract, 'count');                       // -> 15
//   ibc.close();

export class IbcError extends Error {}

// The EIP-712 domain and types; they must match the node's (package identity).
export const DOMAIN = { name: 'ibc', version: '1' };
export const TYPES = {
  Transaction: [
    { name: 'contract', type: 'string' },
    { name: 'kind', type: 'string' },
    { name: 'agent', type: 'address' },
    { name: 'nonce', type: 'string' },
    { name: 'body', type: 'string' },
  ],
  Registration: [
    { name: 'agent', type: 'address' },
    { name: 'node', type: 'string' },
    { name: 'nodeKey', type: 'address' },
    { name: 'nonce', type: 'string' },
  ],
  Query: [
    { name: 'agent', type: 'address' },
    { name: 'action', type: 'string' },
    { name: 'target', type: 'string' },
    { name: 'contract', type: 'string' },
    { name: 'body', type: 'string' },
    { name: 'time', type: 'uint64' },
    { name: 'nonce', type: 'string' },
  ],
  Session: [
    { name: 'agent', type: 'address' },
    { name: 'nodeKey', type: 'address' },
    { name: 'key', type: 'bytes' },
    { name: 'scope', type: 'string' },
    { name: 'expires', type: 'uint64' },
    { name: 'nonce', type: 'string' },
  ],
};

const hex = (bytes) => '0x' + Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
const now = () => Math.floor(Date.now() / 1000);

function nonce() {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

export class Ibc {
  /**
   * Connects to a node as the signer's agent.
   * @param {string} url the node, e.g. 'http://localhost:2611'
   * @param {{getAddress(): Promise<string>, signTypedData(d: object, t: object, m: object): Promise<string>}} signer
   */
  static async connect(url, signer) {
    const ibc = new Ibc(url, signer, await signer.getAddress());
    ibc.nodeKey = (await ibc.request('GET', 'node_info', { agent: '' })).nodeKey;
    return ibc;
  }

  /**
   * A signer that signs with a browser wallet: any EIP-1193 provider that
   * supports eth_requestAccounts and eth_signTypedData_v4, such as MetaMask.
   */
  static wallet(ethereum) {
    let account;
    return {
      async getAddress() {
        [account] = await ethereum.request({ method: 'eth_requestAccounts' });
        return account;
      },
      async signTypedData(domain, types, message) {
        const primaryType = Object.keys(types).find((t) => t !== 'EIP712Domain');
        const data = {
          types: { EIP712Domain: [{ name: 'name', type: 'string' }, { name: 'version', type: 'string' }], ...types },
          primaryType, domain, message,
        };
        return ethereum.request({ method: 'eth_signTypedData_v4', params: [account, JSON.stringify(data)] });
      },
    };
  }

  /** The earlier name of Ibc.wallet. */
  static metamask(ethereum) {
    return Ibc.wallet(ethereum);
  }

  constructor(url, signer, agent) {
    this.url = url.replace(/\/+$/, '');
    this.signer = signer;
    this.agent = agent; // the agent id: the signer's address
    this.pending = new Map(); // request id -> { resolve, timer }
    this.inFlight = new Set(); // per request being sent: the events that arrive meanwhile
    this.listeners = new Set();
    this.stream = null;
    this.session = null; // the read session: { session, signature, key }
    this.sessionHours = 0; // > 0: start a read session of this length when one is needed
    this.sessionListeners = new Set();
    this.sessionStarting = null;
  }

  // Read sessions. A session key signs this agent's queries (reads, contract
  // details, the event stream) on this node, never transactions. The key is
  // made with WebCrypto and cannot be extracted; the user signs the session
  // once in the wallet. Sessions need WebCrypto, which browsers give only to
  // secure pages (https, or http://localhost).

  /** Can this environment make session keys? */
  static sessionsAvailable() {
    return Boolean(globalThis.crypto?.subtle);
  }

  /** Starts a read session lasting hours: one signature in the wallet. */
  async startSession(hours = 8) {
    const pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
    const raw = new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey));
    const session = {
      agent: this.agent, nodeKey: this.nodeKey, key: hex(raw), scope: 'read',
      expires: now() + Math.round(hours * 3600), nonce: nonce(),
    };
    const signature = await this.signer.signTypedData(DOMAIN, { Session: TYPES.Session }, session);
    this.useSession({ session, signature, key: pair.privateKey });
    return this.session;
  }

  /** Uses a session kept from before, if it is this agent's, for this node, and still valid. */
  useSession(s) {
    const ok = s && this.sessionValid(s)
      && s.session.agent.toLowerCase() === this.agent.toLowerCase()
      && s.session.nodeKey.toLowerCase() === String(this.nodeKey).toLowerCase();
    this.session = ok ? s : null;
    for (const fn of this.sessionListeners) fn(this.session);
    return this.session;
  }

  /** Forgets the session; reads are signed in the wallet again. */
  endSession() {
    this.useSession(null);
  }

  /** Is the session valid for at least another minute? */
  sessionValid(s = this.session) {
    return Boolean(s) && s.session.expires - 60 > now();
  }

  /** Calls fn(session or null) whenever the session changes. */
  onSession(fn) {
    this.sessionListeners.add(fn);
    return () => this.sessionListeners.delete(fn);
  }

  async #readSession() {
    if (this.sessionValid()) return this.session;
    if (this.session) this.endSession(); // expired
    if (!this.sessionHours || !Ibc.sessionsAvailable()) return null;
    this.sessionStarting ??= this.startSession(this.sessionHours).finally(() => { this.sessionStarting = null; });
    return this.sessionStarting;
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

  /** Signs a transaction of this agent. body is an object; it is sent as JSON text. */
  async sign(kind, contract, body) {
    const tx = { contract, kind, agent: this.agent, nonce: nonce(), body: JSON.stringify(body) };
    const types = { Transaction: TYPES.Transaction };
    return { tx, signature: await this.signer.signTypedData(DOMAIN, types, tx) };
  }

  /**
   * Signs a query: a request to see something on target's copy of contract.
   * body, if given, is an object sent as JSON text. A query is accepted once,
   * within a minute of signing.
   */
  async query(action, { target = this.agent, contract = '', body } = {}) {
    const query = {
      agent: this.agent, action, target, contract,
      body: body === undefined ? '' : JSON.stringify(body),
      time: now(), nonce: nonce(),
    };
    const s = await this.#readSession();
    if (s) {
      // The session key signs the fields one per line (identity.Query.SessionMessage).
      const message = ['ibc query v1', query.agent.toLowerCase(), query.action, query.target, query.contract,
        query.body, String(query.time), query.nonce].join('\n');
      const signature = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, s.key, new TextEncoder().encode(message));
      return { query, signature: hex(new Uint8Array(signature)), session: { session: s.session, signature: s.signature } };
    }
    return { query, signature: await this.signer.signTypedData(DOMAIN, { Query: TYPES.Query }, query) };
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
  async #submit(verb, action, options, timeoutMs = 60000) {
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

  /** Registers this agent on the node, authorizing the node's key to act for it. */
  async register() {
    const registration = { agent: this.agent, node: this.url, nodeKey: this.nodeKey, nonce: nonce() };
    const signature = await this.signer.signTypedData(DOMAIN, { Registration: TYPES.Registration }, registration);
    const event = await this.#submit('PUT', 'register_agent', { body: { registration, signature } });
    return event.reply;
  }

  /** Is this agent registered on the node? */
  exists() {
    return this.request('GET', 'is_exist_agent');
  }

  /**
   * Deploys a contract and returns its id once it is live.
   * @param {{name: string, code: string, abi: string|object[], constructor?: object}} contract
   */
  async deploy({ name, code, abi, constructor = {}, ...rest }) {
    const body = {
      name, contract: `${name}.sol`, default_app: '', profile: '', ...rest,
      code: code.trim(), abi: typeof abi === 'string' ? JSON.parse(abi) : abi, constructor,
      node: this.url, nodeKey: this.nodeKey,
    };
    const event = await this.#submit('PUT', 'deploy_contract', { body: await this.sign('deploy', '', body) });
    if (event.reply !== true) throw new IbcError(`deploy failed: ${event.reply}`);
    return event.contract;
  }

  /**
   * A transaction: changes the contract's state. Resolves with the method's
   * return value; rejects with the revert reason if it fails.
   */
  async write(contract, method, values = {}) {
    const body = await this.sign('call', contract, { method, values });
    const event = await this.#submit('POST', 'contract_write', { contract, method, body });
    if (typeof event.reply === 'string' && event.reply.startsWith('execution reverted')) {
      throw new IbcError(event.reply);
    }
    return event.reply;
  }

  /**
   * A read-only call, signed by this agent: returns the method's result at once
   * and changes nothing. The contract sees this agent as msg.sender. owner is
   * whose copy of the contract to read, on this node (default: this agent's).
   */
  async read(contract, method, values = {}, owner = this.agent) {
    const body = await this.query('contract_read', { target: owner, contract, body: { method, values } });
    return this.request('POST', 'contract_read', { agent: owner, contract, method, body });
  }

  /** This agent's contracts on the node. */
  async contracts() {
    return this.request('GET', 'get_contracts', { auth: await this.query('get_contracts') });
  }

  /** One of this agent's contracts: its summary. */
  async contract(contract) {
    return this.request('GET', 'get_contract', { contract, auth: await this.query('get_contract', { contract }) });
  }

  /**
   * Asks a member of a contract to let this agent join. Resolves with true once
   * this node holds the contract, or false if the contract refused.
   */
  async join(contract, memberUrl, memberAgent, profile = '') {
    const stx = await this.sign('join', contract, { node: this.url, nodeKey: this.nodeKey, profile });
    const event = await this.#submit('PUT', 'join_contract', {
      body: { ...stx, partner: { address: memberUrl, agent: memberAgent } },
    });
    return event.reply;
  }

  /** Leaves a contract, or asks to remove another member. Resolves with the decision. */
  async leave(contract, member = this.agent) {
    const event = await this.#submit('PUT', 'leave_contract', { body: await this.sign('leave', contract, { member }) });
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
