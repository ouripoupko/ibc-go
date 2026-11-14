// The ibc client, for browsers and Node.js 20+.
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
import { newP256Key, nonce, now, p256Signer, sign, sourceHashOf, toHex, } from './keys.js';
/**
 * The version of the node's API that this client speaks: its HTTP calls and
 * its approval page's messages. A node answers node_info, and its approval
 * page says it is ready, with its apiVersion; the client refuses a node of
 * another version.
 */
export const API_VERSION = 1;
export class IbcError extends Error {
    name = 'IbcError';
}
/** Does an error mean the user must approve on the node's page? */
export const needsApproval = (err) => /needs your approval|nor an app it connected/.test(err?.message ?? '');
/** The node refused the stream (a bad query, or an app the user has not approved): reconnecting would not help. */
class StreamRefused extends IbcError {
}
/** The constructor's arguments given in o: its own `constructor` property, or none. */
function ctorOf(o) {
    return Object.hasOwn(o, 'constructor') ? o.constructor : {};
}
export class Ibc {
    url;
    agent;
    root;
    appKey;
    /** The node's key, which the agent's registration authorizes. */
    nodeKey = '';
    #pending = new Map();
    #inFlight = new Set(); // per request being sent: the events that arrive meanwhile
    #listeners = new Set();
    // The event stream.
    #status = 'closed';
    #statusListeners = new Set();
    #running = false; // the stream should be open: reconnect when it breaks
    #controller = null; // the open attempt or connection
    #wake = null; // ends the wait before the next attempt
    #ready = null;
    /**
     * How long the stream may stay silent before the client takes it for broken
     * and reconnects. The node sends a keep-alive every 10 seconds.
     */
    streamTimeoutMs = 30000;
    /** The first delay before reconnecting; it doubles with each failure, up to 30 seconds. */
    reconnectDelayMs = 1000;
    /**
     * Connects to a node as an agent.
     * @param url the node, e.g. 'https://node.example'
     */
    static async connect(url, { agent, root, appKey }) {
        const ibc = new Ibc(url, agent);
        ibc.root = root;
        ibc.appKey = appKey;
        const info = await ibc.request('GET', 'node_info', { agent: '' });
        if (info.apiVersion !== API_VERSION) {
            throw new IbcError(`the node at ${ibc.url} speaks API version ${info.apiVersion ?? 0}; this client speaks ${API_VERSION}`);
        }
        ibc.nodeKey = info.nodeKey;
        return ibc;
    }
    /** What a node knows of a registered agent: { agent, rootKey, rootScheme, credential }. */
    static async agentInfo(url, { agent = '', credential = '' }) {
        const base = url.replace(/\/+$/, '');
        const path = agent ? `/${agent}` : '';
        const res = await fetch(`${base}/ibc/app${path}?action=agent_info${credential ? `&credential=${credential}` : ''}`);
        const reply = await res.json();
        if (reply?.message)
            throw new IbcError(reply.message);
        return reply;
    }
    /**
     * An anonymous read of a public contract: a view or pure function of the
     * agent's copy, if the agent lets anyone read it. No key and no signature;
     * the function runs with msg.sender the zero address.
     */
    static async publicRead(url, agent, contract, method, values = {}) {
        const base = url.replace(/\/+$/, '');
        const path = [agent, contract, method].map(encodeURIComponent).join('/');
        const res = await fetch(`${base}/ibc/app/${path}?action=public_read&values=${encodeURIComponent(JSON.stringify(values))}`);
        const reply = await res.json();
        if (reply && typeof reply === 'object' && !Array.isArray(reply) && 'message' in reply) {
            throw new IbcError(String(reply.message));
        }
        return reply;
    }
    constructor(url, agent) {
        this.url = url.replace(/\/+$/, '');
        this.agent = agent;
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
            throw new IbcError(String(reply.message));
        }
        return reply;
    }
    /** Signs a transaction with the root key. body is an object, sent as JSON text. */
    async sign(kind, contract, body) {
        if (!this.root)
            throw new IbcError('no root key to sign with');
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
        if (!s)
            throw new IbcError('no key to sign a query with');
        const { envelope } = await sign(s, 'Query', query);
        return { query, signature: envelope };
    }
    /** The event stream's state. */
    get streamStatus() {
        return this.#status;
    }
    /**
     * Calls fn(status, error) whenever the event stream's state changes; error
     * says why it is reconnecting or closed. Returns a function that stops it.
     */
    onStatus(fn) {
        this.#statusListeners.add(fn);
        return () => { this.#statusListeners.delete(fn); };
    }
    #setStatus(status, error) {
        if (status === this.#status && !error)
            return;
        this.#status = status;
        for (const fn of this.#statusListeners)
            fn(status, error);
    }
    /**
     * Opens the event stream, and keeps it open: if it breaks, the client opens
     * it again (see StreamStatus). Resolves once it is open; rejects if the node
     * refuses it, or if close() is called first. While the network is down, it
     * waits.
     */
    connect() {
        if (this.#status === 'open')
            return Promise.resolve();
        if (!this.#ready) {
            let resolve;
            let reject;
            const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
            promise.catch(() => { }); // a caller may not wait for it
            this.#ready = { promise, resolve, reject };
        }
        const { promise } = this.#ready;
        if (!this.#running)
            void this.#run();
        return promise;
    }
    /** Whether the stream can be opened again without asking the user (a passkey asks). */
    #silent() {
        return Boolean(this.appKey) || (this.root !== undefined && this.root.scheme !== 'webauthn');
    }
    /** Keeps the stream open until close(), or until it cannot be. */
    async #run() {
        this.#running = true;
        this.#setStatus('connecting');
        let failures = 0;
        while (this.#running) {
            let error;
            try {
                await this.#open(() => {
                    failures = 0;
                    this.#setStatus('open');
                    this.#ready?.resolve();
                    this.#ready = null;
                });
                error = new IbcError('the node closed the event stream');
            }
            catch (err) {
                error = err;
            }
            if (!this.#running)
                return;
            if (error instanceof StreamRefused) {
                this.#stop(error);
                return;
            }
            if (!this.#silent()) {
                this.#stop(new IbcError(`the event stream closed (${error.message}); call connect() to open it again`));
                return;
            }
            this.#setStatus('reconnecting', error);
            const delay = Math.min(this.reconnectDelayMs * 2 ** failures, 30000);
            failures++;
            await new Promise((resolve) => {
                const timer = setTimeout(resolve, delay);
                this.#wake = () => { clearTimeout(timer); resolve(); };
            });
            this.#wake = null;
        }
    }
    /** Ends the stream for good: the waiters of connect() are told why. */
    #stop(error) {
        this.#running = false;
        this.#setStatus('closed', error);
        this.#ready?.reject(error);
        this.#ready = null;
    }
    /** Opens the stream once and reads it until it ends; calls opened() when the node has subscribed us. */
    async #open(opened) {
        const controller = new AbortController();
        this.#controller = controller;
        let silentTimer;
        const quiet = () => {
            clearTimeout(silentTimer);
            silentTimer = setTimeout(() => controller.abort(new IbcError('the event stream went silent')), this.streamTimeoutMs);
        };
        try {
            const auth = await this.query('stream');
            quiet();
            const res = await fetch(`${this.url}/stream?auth=${encodeURIComponent(JSON.stringify(auth))}`, { signal: controller.signal });
            if (!res.ok || !res.body) {
                const message = await res.json().then((r) => r?.message, () => res.statusText);
                const err = `event stream: ${message || res.status}`;
                // 4xx: the node refuses this query; anything else may pass.
                throw res.status >= 400 && res.status < 500 ? new StreamRefused(err) : new IbcError(err);
            }
            const reader = res.body.getReader();
            const decoder = new TextDecoder();
            let buffer = '';
            let connected = false;
            for (;;) {
                const { value, done } = await reader.read();
                if (done)
                    return; // the node closed it
                quiet();
                buffer += decoder.decode(value, { stream: true });
                let end;
                while ((end = buffer.indexOf('\n\n')) >= 0) {
                    const chunk = buffer.slice(0, end);
                    buffer = buffer.slice(end + 2);
                    if (!connected) {
                        connected = true;
                        opened();
                    }
                    const data = chunk.startsWith('data: ') ? chunk.slice(6) : '';
                    if (data)
                        this.#dispatch(JSON.parse(data));
                }
            }
        }
        catch (err) {
            // An abort's reason says why: silence, or close().
            throw controller.signal.aborted && controller.signal.reason instanceof Error ? controller.signal.reason : err;
        }
        finally {
            clearTimeout(silentTimer);
            if (this.#controller === controller)
                this.#controller = null;
        }
    }
    #dispatch(event) {
        for (const listener of this.#listeners)
            listener(event);
        const waiter = this.#pending.get(event.request);
        if (waiter) {
            this.#pending.delete(event.request);
            clearTimeout(waiter.timer);
            waiter.resolve(event);
        }
        else {
            for (const seen of this.#inFlight)
                seen.push(event);
        }
    }
    /** Calls fn(event) for every event on the stream. Returns a function that stops it. */
    onEvent(fn) {
        this.#listeners.add(fn);
        return () => { this.#listeners.delete(fn); };
    }
    /** Sends a request whose result arrives on the event stream, and waits for that event. */
    async submit(verb, action, options, timeoutMs = 60000) {
        await this.connect();
        // The request's id comes back in the reply, but its outcome can arrive on
        // the stream before the reply does: collect events while the request is
        // in flight, and look for the outcome among them.
        const seen = [];
        this.#inFlight.add(seen);
        let id;
        try {
            id = await this.request(verb, action, options);
        }
        finally {
            this.#inFlight.delete(seen);
        }
        if (id === null)
            throw new IbcError(`${action}: nothing to do`);
        const early = seen.find((event) => event.request === id);
        if (early)
            return early;
        const want = id;
        return new Promise((resolve, reject) => {
            const timer = setTimeout(() => {
                this.#pending.delete(want);
                reject(new IbcError(`no result for request ${want}`));
            }, timeoutMs);
            this.#pending.set(want, { resolve, timer });
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
        if (!signature) {
            if (!this.root)
                throw new IbcError('no root key to sign with');
            ({ envelope: signature } = await sign(this.root, 'Registration', registration));
        }
        const event = await this.submit('PUT', 'register_agent', { body: { registration, signature } });
        return event.reply;
    }
    /** Is this agent registered on the node? */
    exists() {
        return this.request('GET', 'is_exist_agent');
    }
    /** Compiles Solidity source on the node. */
    compile(source, contract = '') {
        return this.request('POST', 'compile', { agent: '', body: { source, contract } });
    }
    /** The deploy transaction's body, for a compiled contract. */
    deployBody(input) {
        const { name, code, abi, source, compiler, ...rest } = input;
        delete rest.constructor;
        return {
            name, contract: `${name}.sol`, default_app: '', profile: '', ...rest,
            code: code.trim(), abi: typeof abi === 'string' ? JSON.parse(abi) : abi, constructor: ctorOf(input),
            node: this.url, nodeKey: this.nodeKey, source, compiler,
        };
    }
    /**
     * Deploys a contract and returns its id once it is live. tx: the deploy
     * transaction, if signed already (in a batch); otherwise the root signs it.
     */
    async deploy(contract, tx = null) {
        if (!tx && !contract)
            throw new IbcError('nothing to deploy');
        const body = tx ?? await this.sign('deploy', '', this.deployBody(contract));
        const event = await this.submit('PUT', 'deploy_contract', { body });
        if (event.reply !== true)
            throw new IbcError(`deploy failed: ${String(event.reply)}`);
        return event.contract;
    }
    /**
     * A transaction signed by the root key: changes the contract's state.
     * Resolves with the method's return value; rejects with the revert reason.
     */
    async write(contract, method, values = {}) {
        return this.#result(await this.submit('POST', 'contract_write', { contract, method, body: await this.sign('call', contract, { method, values }) }));
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
    /** One of this agent's contracts: its summary ({} if the agent has no such contract). */
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
    /** Records a device the root signed: a browser of an app (a new one, or another of one approved before). */
    registerDevice(device) {
        return this.request('PUT', 'register_app', { body: { device } });
    }
    /** The message that makes the agent's copy of a contract public (or not), for the root to sign. */
    publishing(contract, isPublic) {
        return { agent: this.agent, contract, public: isPublic, time: Date.now(), nonce: nonce() };
    }
    /**
     * Lets anyone read this agent's copy of a contract (public_read), or stops
     * it. The root signs; signature: its envelope, if signed already (in a batch).
     */
    async setPublic(contract, isPublic, signed = null) {
        if (!signed) {
            if (!this.root)
                throw new IbcError('no root key to sign with');
            const publish = this.publishing(contract, isPublic);
            signed = { publish, signature: (await sign(this.root, 'Publish', publish)).envelope };
        }
        return this.request('PUT', 'set_public', { contract: signed.publish.contract, body: signed });
    }
    /** Disconnects an app (by its site), with all its browsers. The root signs. */
    async revokeApp(app) {
        const body = await this.query('revoke_app', { body: { app }, signer: this.root });
        return this.request('PUT', 'revoke_app', { body });
    }
    /**
     * Asks a member of a contract to let this agent join, and resolves with the
     * join's id once the member has it. source is the contract's Solidity
     * source, as the agent read it: the members refuse the join if it is not
     * the contract's. The outcome arrives later on the event stream (onEvent):
     * a join_contract event for this id, with reply true or false and the
     * node's check of the source (source_check, reason). A guest takes no part
     * in consensus: this node sends its transactions and reads to the member's
     * node, its host.
     */
    async join(contract, memberUrl, memberAgent, { source, guest = false, profile = '' }) {
        const body = { node: this.url, nodeKey: this.nodeKey, profile, sourceHash: sourceHashOf(source) };
        if (guest)
            Object.assign(body, { guest: true, host: memberAgent });
        const stx = await this.sign('join', contract, body);
        await this.connect(); // so the outcome is heard
        return this.request('PUT', 'join_contract', { body: { ...stx, partner: { address: memberUrl, agent: memberAgent } } });
    }
    /** Leaves a contract, or asks to remove another member. Resolves with the decision. */
    async leave(contract, member = this.agent) {
        const event = await this.submit('PUT', 'leave_contract', { body: await this.sign('leave', contract, { member }) });
        return event.reply;
    }
    /** Closes the event stream. */
    close() {
        const wasRunning = this.#running;
        this.#running = false;
        this.#controller?.abort(new IbcError('closed'));
        this.#wake?.();
        for (const { timer } of this.#pending.values())
            clearTimeout(timer);
        this.#pending.clear();
        if (wasRunning || this.#ready)
            this.#stop(new IbcError('closed'));
    }
}
/**
 * Opens the user's node's approval page with a request, and resolves with its
 * answer. The user approves there with the root passkey. A browser opens the
 * page only on a user's click, so call this from a click handler.
 */
export function approve(nodeUrl, request) {
    const node = new URL(nodeUrl).origin;
    // A new window for each request, so that an old approval page is never reused.
    const popup = window.open(`${node}/approve.html`, `ibc-approve-${nonce()}`, 'width=520,height=720');
    if (!popup)
        return Promise.reject(new IbcError('the approval window was blocked'));
    return new Promise((resolve, reject) => {
        const onMessage = (e) => {
            if (e.origin !== node || e.source !== popup)
                return;
            if (e.data?.type === 'ibc-ready') {
                if (e.data.apiVersion !== API_VERSION) {
                    popup.close();
                    done(new IbcError(`the node at ${node} speaks API version ${e.data.apiVersion ?? 0}; this client speaks ${API_VERSION}`));
                }
                else {
                    popup.postMessage({ ...request, apiVersion: API_VERSION }, node);
                }
            }
            else if (e.data?.type === 'ibc-done')
                done(null, e.data.result);
            else if (e.data?.type === 'ibc-denied')
                done(new IbcError(e.data.reason || 'the user did not approve'));
        };
        const timer = setInterval(() => { if (popup.closed)
            done(new IbcError('the approval window was closed')); }, 500);
        function done(err, result) {
            clearInterval(timer);
            window.removeEventListener('message', onMessage);
            if (err)
                reject(err);
            else
                resolve(result);
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
    }
    finally {
        db.close();
    }
}
/**
 * The client of an app on another site. The app has a key of its own, which
 * this browser keeps and cannot export; the user's node learns it when the
 * user first approves the app.
 *
 *   const app = await IbcApp.open('https://node.example');
 *   const agent = await app.connect();                   // on a click: the user approves
 *   const chat = await app.deploy({ source, constructor: { initialTopic: 'lunch' } });
 *   await app.write(chat, 'post', { text: 'hi' });       // silent, if the user allowed it
 */
export class IbcApp {
    node;
    /** The client the app uses once the user approved it; null before. */
    ibc = null;
    #kept;
    /** Opens the app's connection to the user's node, making the app's key the first time. */
    static async open(nodeUrl) {
        let kept = await appStore('get');
        if (!kept || kept.node !== new URL(nodeUrl).origin) {
            kept = { node: new URL(nodeUrl).origin, key: await newP256Key(), agent: null };
            await appStore('put', kept);
        }
        const app = new IbcApp(kept);
        if (kept.agent)
            await app.#connect(kept.agent);
        return app;
    }
    constructor(kept) {
        this.#kept = kept;
        this.node = kept.node;
    }
    /** The agent the app acts for, once the user approved it; null before. */
    get agent() {
        return this.#kept.agent;
    }
    async #connect(agent) {
        if (this.#kept.agent !== agent) {
            this.#kept.agent = agent;
            await appStore('put', this.#kept);
        }
        this.ibc ??= await Ibc.connect(this.node, { agent, appKey: this.#kept.key });
    }
    async #approve(request) {
        const result = await approve(this.node, { ...request, appKey: toHex(this.#kept.key.publicKey) });
        if (result?.agent)
            await this.#connect(result.agent);
        return result;
    }
    /**
     * Asks the user who they are: the node's page signs the user in (or creates
     * their identity), and connects this app, with no permissions. Resolves
     * with the user's agent id. Other requests connect the app too; this one
     * only asks for that.
     */
    async connect() {
        const { agent } = await this.#approve({ type: 'connect' });
        return agent;
    }
    /**
     * Deploys a contract from Solidity source: the user's node compiles it, and
     * the user approves on the node's page, choosing the app's permissions the
     * first time this code is used. Resolves with the contract's id.
     * public: true proposes to let anyone read the user's copy (public_read);
     * the user decides, and it is off unless proposed.
     */
    async deploy(input) {
        const { source, contract = '', name, protocol } = input;
        return (await this.#approve({
            type: 'deploy', source, contract, name, constructor: ctorOf(input), protocol, public: input.public === true,
        })).contract;
    }
    /**
     * Asks the user for permissions on an existing contract (one the agent
     * joined); with { public }, proposes to let anyone read it, or to stop it.
     */
    async permit(contract, options = {}) {
        return this.#approve({ type: 'permit', contract, ...(typeof options.public === 'boolean' ? { public: options.public } : {}) });
    }
    /**
     * Asks the user to join a contract, through a member's node; as a guest
     * with { guest: true }. source is the contract's Solidity source, which the
     * user reads on the approval page and signs for, with the app's permissions
     * and, with public: true, a proposal to let anyone read the user's copy (not
     * for a guest, who keeps none): one approval. Resolves with the join's id
     * once it is sent; the outcome arrives later, as a join_contract event for
     * that id (onEvent), with reply true or false, and the user's node's check
     * of the source (source_check, reason).
     */
    async join(contract, memberNode, memberAgent, { source, guest = false, public: isPublic = false }) {
        return (await this.#approve({ type: 'join', contract, source, memberNode, memberAgent, guest, public: isPublic })).request;
    }
    /** Reads silently if the user allowed it, and otherwise asks the user. */
    async read(contract, method, values = {}) {
        if (this.ibc) {
            try {
                return await this.ibc.read(contract, method, values);
            }
            catch (err) {
                if (!needsApproval(err))
                    throw err;
            }
        }
        return (await this.#approve({ type: 'read', contract, method, values })).value;
    }
    /** Writes silently if the user allowed this function, and otherwise asks the user. */
    async write(contract, method, values = {}) {
        if (this.ibc) {
            try {
                return await this.ibc.writeSilently(contract, method, values);
            }
            catch (err) {
                if (!needsApproval(err))
                    throw err;
            }
        }
        const { reply } = await this.#approve({ type: 'call', contract, method, values });
        if (typeof reply === 'string' && reply.startsWith('execution reverted'))
            throw new IbcError(reply);
        return reply;
    }
    /**
     * Calls fn(event) for every event of the contracts the app may read, and
     * opens the event stream, which then stays open (it reconnects by itself).
     * Returns a function that stops it.
     */
    onEvent(fn) {
        if (!this.ibc)
            throw new IbcError('not approved yet');
        void this.ibc.connect();
        return this.ibc.onEvent(fn);
    }
    /**
     * Calls fn(status, error) whenever the event stream opens, breaks, reconnects
     * or closes (see StreamStatus). Returns a function that stops it.
     */
    onStatus(fn) {
        if (!this.ibc)
            throw new IbcError('not approved yet');
        return this.ibc.onStatus(fn);
    }
    /** The event stream's state. */
    get streamStatus() {
        return this.ibc?.streamStatus ?? 'closed';
    }
}
