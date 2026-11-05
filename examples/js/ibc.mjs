// A minimal ibc client for browsers and Node.js 18+. No dependencies.
//
//   import { Ibc } from './ibc.mjs';
//   const ibc = new Ibc('http://localhost:2611', 'alice');
//   await ibc.register();
//   const contract = await ibc.deploy({ name: 'counter', code, abi, constructor: { start: 10 } });
//   await ibc.write(contract, 'increment', { amount: 5 });   // -> 15
//   await ibc.read(contract, 'count');                       // -> 15
//   ibc.close();

export class IbcError extends Error {}

export class Ibc {
  /**
   * @param {string} url   the node, e.g. 'http://localhost:2611'
   * @param {string} agent your agent id
   */
  constructor(url, agent) {
    this.url = url.replace(/\/+$/, '');
    this.agent = agent;
    this.pending = new Map(); // request id -> { resolve, timer }
    this.listeners = new Set();
    this.stream = null;
  }

  /** One request to /ibc/app. Returns the decoded JSON reply. */
  async request(verb, action, { contract = '', method = '', body } = {}) {
    const path = [this.agent, contract, method].filter(Boolean).map(encodeURIComponent).join('/');
    const res = await fetch(`${this.url}/ibc/app/${path}?action=${action}`, {
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

  /** Opens the event stream (once). Resolves when the node has subscribed us. */
  connect() {
    if (this.stream) return this.stream;
    const controller = new AbortController();
    this.stream = new Promise((resolve, reject) => {
      fetch(`${this.url}/stream?agent=${encodeURIComponent(this.agent)}&contract=`, {
        signal: controller.signal,
      }).then(async (res) => {
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buffer = '';
        let connected = false;
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
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
      }).catch((err) => { if (err.name !== 'AbortError') reject(err); });
    });
    this.abort = () => controller.abort();
    return this.stream;
  }

  #dispatch(event) {
    for (const listener of this.listeners) listener(event);
    const waiter = this.pending.get(event.request);
    if (waiter) {
      this.pending.delete(event.request);
      clearTimeout(waiter.timer);
      waiter.resolve(event);
    }
  }

  /** Calls fn(event) for every event on the stream. */
  onEvent(fn) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  /** Sends a request whose result arrives on the event stream, and waits for that event. */
  async #submit(verb, action, options, timeoutMs = 30000) {
    await this.connect();
    const id = await this.request(verb, action, options);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new IbcError(`no result for request ${id}`));
      }, timeoutMs);
      this.pending.set(id, { resolve, timer });
    });
  }

  /** Registers this agent on the node. */
  async register() {
    const event = await this.#submit('PUT', 'register_agent', { body: { address: this.url } });
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
      name, contract: `${name}.sol`, protocol: 'BFT', default_app: '', profile: '',
      pid: this.agent, address: this.url, ...rest,
      code: code.trim(), abi, constructor,
    };
    const event = await this.#submit('PUT', 'deploy_contract', { body });
    if (event.reply !== true) throw new IbcError(`deploy failed: ${event.reply}`);
    return event.contract;
  }

  /**
   * A transaction: changes the contract's state. Resolves with the method's
   * return value; rejects with the revert reason if it fails.
   */
  async write(contract, method, values = {}) {
    const event = await this.#submit('POST', 'contract_write', { contract, method, body: { values } });
    if (typeof event.reply === 'string' && event.reply.startsWith('execution reverted')) {
      throw new IbcError(event.reply);
    }
    return event.reply;
  }

  /** A read-only call: returns the method's result at once and changes nothing. */
  read(contract, method, values = {}) {
    return this.request('POST', 'contract_read', { contract, method, body: { values } });
  }

  /** This agent's contracts on the node. */
  contracts() {
    return this.request('GET', 'get_contracts');
  }

  /** Asks a partner's node to let this agent join a contract. */
  join(contract, partnerUrl, partnerAgent, profile = '') {
    return this.request('PUT', 'join_contract', {
      body: { contract, address: partnerUrl, agent: partnerAgent, profile },
    });
  }

  /** Closes the event stream. */
  close() {
    if (this.abort) this.abort();
    for (const { timer } of this.pending.values()) clearTimeout(timer);
    this.pending.clear();
    this.stream = null;
  }
}
