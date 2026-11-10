import { type Address, type Envelope, type Hex, type P256Key, type Signer, type Transaction, type Query, type Registration, type Consent, type Grant, type Device, type Publish } from './keys.js';
/**
 * The version of the node's API that this client speaks: its HTTP calls and
 * its approval page's messages. A node answers node_info, and its approval
 * page says it is ready, with its apiVersion; the client refuses a node of
 * another version.
 */
export declare const API_VERSION = 1;
export declare class IbcError extends Error {
    name: string;
}
/** Does an error mean the user must approve on the node's page? */
export declare const needsApproval: (err: unknown) => boolean;
/** An event of the agent's stream: the outcome of a request. */
export interface IbcEvent {
    /** The id of the request this event answers. */
    request: string;
    /** The contract, or null for an agent's registration. */
    contract: string | null;
    action: string;
    /** The outcome: a function's result, true or false, or an error string. */
    reply: unknown;
    state_root?: Hex;
    agent?: Address;
}
/**
 * The state of the event stream:
 *   - connecting: opening it;
 *   - open: events arrive;
 *   - reconnecting: it broke (the network, the node restarting), and the
 *     client opens it again, after a delay that grows to 30 seconds;
 *   - closed: not open, and not reconnecting: before connect(), after close(),
 *     after the node refused it, or when reopening would need the user's
 *     passkey (call connect() again, from a click).
 * Events sent while the stream was not open are lost: after reconnecting,
 * read again what the app shows.
 */
export type StreamStatus = 'connecting' | 'open' | 'reconnecting' | 'closed';
/** A contract, as the node lists it. */
export interface ContractSummary {
    id: string;
    name?: string;
    contract?: string;
    protocol?: string;
    pid?: Address;
    address?: string;
    code?: Hex;
    abi?: unknown;
    source?: string;
    compiler?: unknown;
    role: 'partner' | 'guest';
    /** Whether the agent lets anyone read its copy (public_read). */
    public?: boolean;
    hosts?: {
        agent: Address;
        nodeKey: Address;
        node: string;
    }[];
    evm_address?: Address;
    state_root?: Hex;
    block_number?: number;
    deploy_error?: string;
    [field: string]: unknown;
}
/** An app the agent connected, as the node keeps it. */
export interface AppRecord {
    agent: Address;
    app: string;
    keys: Hex[];
    permissions: Record<Hex, {
        read: boolean;
        functions: string[];
        grant?: unknown;
    }>;
}
/** What the node compiled: the deploy needs it. */
export interface Compiled {
    contract: string;
    code: Hex;
    abi: unknown[];
    codeHash: Hex;
    compiler: {
        version: string;
        settings: unknown;
    };
}
/**
 * The constructor's arguments, by name. (Typed as object: every object
 * inherits a `constructor`, so only an own property counts; see ctorOf.)
 */
export type ConstructorArgs = object;
/** A contract to deploy: compiled code and its ABI, or what the node compiled. */
export interface DeployInput {
    name: string;
    code: Hex;
    abi: unknown;
    constructor?: ConstructorArgs;
    protocol?: string;
    source?: string;
    compiler?: unknown;
    [field: string]: unknown;
}
/** A transaction and the root's signature. */
export interface SignedTx {
    tx: Transaction;
    signature: Envelope;
}
/** A query and its signature. */
export interface SignedQuery {
    query: Query;
    signature: Envelope;
}
interface RequestOptions {
    agent?: string;
    contract?: string;
    method?: string;
    body?: unknown;
    auth?: SignedQuery;
}
export interface IbcKeys {
    /** The agent id. */
    agent: Address;
    /** A signer for the root key (see keys.ts). */
    root?: Signer;
    /** A WebCrypto key the node knows from the user's consent, which signs queries without asking. */
    appKey?: P256Key;
}
export declare class Ibc {
    #private;
    readonly url: string;
    readonly agent: Address;
    root?: Signer;
    appKey?: P256Key;
    /** The node's key, which the agent's registration authorizes. */
    nodeKey: Address;
    /**
     * How long the stream may stay silent before the client takes it for broken
     * and reconnects. The node sends a keep-alive every 10 seconds.
     */
    streamTimeoutMs: number;
    /** The first delay before reconnecting; it doubles with each failure, up to 30 seconds. */
    reconnectDelayMs: number;
    /**
     * Connects to a node as an agent.
     * @param url the node, e.g. 'https://node.example'
     */
    static connect(url: string, { agent, root, appKey }: IbcKeys): Promise<Ibc>;
    /** What a node knows of a registered agent: { agent, rootKey, rootScheme, credential }. */
    static agentInfo(url: string, { agent, credential }: {
        agent?: string;
        credential?: string;
    }): Promise<{
        agent: Address;
        rootKey: Hex;
        rootScheme: string;
        credential: Hex;
    }>;
    /**
     * An anonymous read of a public contract: a view or pure function of the
     * agent's copy, if the agent lets anyone read it. No key and no signature;
     * the function runs with msg.sender the zero address.
     */
    static publicRead(url: string, agent: Address, contract: string, method: string, values?: Record<string, unknown>): Promise<unknown>;
    constructor(url: string, agent: Address);
    /** One request to /ibc/app. Returns the decoded JSON reply. auth is a signed query for the URL. */
    request(verb: string, action: string, { agent, contract, method, body, auth }?: RequestOptions): Promise<any>;
    /** Signs a transaction with the root key. body is an object, sent as JSON text. */
    sign(kind: string, contract: string, body: unknown): Promise<SignedTx>;
    /**
     * Signs a query to the node, with the app key (silently) or else the root.
     * A query is accepted once, within a minute of signing.
     */
    query(action: string, { contract, body, signer }?: {
        contract?: string;
        body?: unknown;
        signer?: Signer;
    }): Promise<SignedQuery>;
    /** The event stream's state. */
    get streamStatus(): StreamStatus;
    /**
     * Calls fn(status, error) whenever the event stream's state changes; error
     * says why it is reconnecting or closed. Returns a function that stops it.
     */
    onStatus(fn: (status: StreamStatus, error?: Error) => void): () => void;
    /**
     * Opens the event stream, and keeps it open: if it breaks, the client opens
     * it again (see StreamStatus). Resolves once it is open; rejects if the node
     * refuses it, or if close() is called first. While the network is down, it
     * waits.
     */
    connect(): Promise<void>;
    /** Calls fn(event) for every event on the stream. Returns a function that stops it. */
    onEvent(fn: (event: IbcEvent) => void): () => void;
    /** Sends a request whose result arrives on the event stream, and waits for that event. */
    submit(verb: string, action: string, options: RequestOptions, timeoutMs?: number): Promise<IbcEvent>;
    /**
     * The registration of this agent on the node: it authorizes the node's key
     * to act for the agent. credential is the root passkey's id, if the root is one.
     */
    registration(credential?: Hex): Registration;
    /** Registers this agent. signature: the root's envelope, if signed already (in a batch). */
    register(registration?: Registration, signature?: Envelope | null): Promise<unknown>;
    /** Is this agent registered on the node? */
    exists(): Promise<boolean>;
    /** Compiles Solidity source on the node. */
    compile(source: string, contract?: string): Promise<Compiled>;
    /** The deploy transaction's body, for a compiled contract. */
    deployBody(input: DeployInput): Record<string, unknown>;
    /**
     * Deploys a contract and returns its id once it is live. tx: the deploy
     * transaction, if signed already (in a batch); otherwise the root signs it.
     */
    deploy(contract: DeployInput | null, tx?: SignedTx | null): Promise<string>;
    /**
     * A transaction signed by the root key: changes the contract's state.
     * Resolves with the method's return value; rejects with the revert reason.
     */
    write(contract: string, method: string, values?: Record<string, unknown>): Promise<unknown>;
    /** A call the node signs silently, if the user allowed this app this function. */
    writeSilently(contract: string, method: string, values?: Record<string, unknown>): Promise<unknown>;
    /** A read of a view or pure function: returns its result, at once. */
    read(contract: string, method: string, values?: Record<string, unknown>, signer?: Signer): Promise<unknown>;
    /** This agent's contracts on the node (an app sees those it may read). */
    contracts(): Promise<ContractSummary[]>;
    /** One of this agent's contracts: its summary ({} if the agent has no such contract). */
    contract(contract: string): Promise<ContractSummary>;
    /** The apps this agent connected, and what each may do. */
    apps(): Promise<AppRecord[]>;
    /** Records a consent (and grant) the root signed. */
    registerApp(consent: {
        consent: Consent;
        signature: Envelope;
    }, grant?: {
        grant: Grant;
        signature: Envelope;
    } | null): Promise<boolean>;
    /** Records a device the root signed: a browser of an app (a new one, or another of one approved before). */
    registerDevice(device: {
        device: Device;
        signature: Envelope;
    }): Promise<boolean>;
    /** The message that makes the agent's copy of a contract public (or not), for the root to sign. */
    publishing(contract: string, isPublic: boolean): Publish;
    /**
     * Lets anyone read this agent's copy of a contract (public_read), or stops
     * it. The root signs; signature: its envelope, if signed already (in a batch).
     */
    setPublic(contract: string, isPublic: boolean, signed?: {
        publish: Publish;
        signature: Envelope;
    } | null): Promise<boolean>;
    /** Disconnects an app (by its site), with all its browsers. The root signs. */
    revokeApp(app: string): Promise<boolean>;
    /**
     * Asks a member of a contract to let this agent join. Resolves with true once
     * this node holds the contract, or false if the contract refused. A guest
     * takes no part in consensus: this node sends its transactions and reads to
     * the member's node, its host.
     */
    join(contract: string, memberUrl: string, memberAgent: Address, { guest, profile }?: {
        guest?: boolean;
        profile?: string;
    }): Promise<unknown>;
    /** Leaves a contract, or asks to remove another member. Resolves with the decision. */
    leave(contract: string, member?: Address): Promise<unknown>;
    /** Closes the event stream. */
    close(): void;
}
/** A request to the node's approval page. */
export type ApprovalRequest = {
    type: 'connect';
} | {
    type: 'deploy';
    source: string;
    contract?: string;
    name?: string;
    constructor?: ConstructorArgs;
    protocol?: string;
    public?: boolean;
} | {
    type: 'permit';
    contract: string;
    public?: boolean;
} | {
    type: 'call';
    contract: string;
    method: string;
    values?: Record<string, unknown>;
} | {
    type: 'read';
    contract: string;
    method: string;
    values?: Record<string, unknown>;
} | {
    type: 'join';
    contract: string;
    memberNode: string;
    memberAgent: Address;
    guest?: boolean;
    public?: boolean;
};
/** What the approval page answers: the agent, and the request's result. */
export interface ApprovalResult {
    agent: Address;
    contract?: string;
    codeHash?: Hex;
    reply?: unknown;
    value?: unknown;
    joined?: unknown;
    /** Whether the user made the contract public (deploy, join, permit). */
    public?: boolean;
    [field: string]: unknown;
}
/**
 * Opens the user's node's approval page with a request, and resolves with its
 * answer. The user approves there with the root passkey. A browser opens the
 * page only on a user's click, so call this from a click handler.
 */
export declare function approve(nodeUrl: string, request: ApprovalRequest & {
    appKey: Hex;
}): Promise<ApprovalResult>;
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
export declare class IbcApp {
    #private;
    readonly node: string;
    /** The client the app uses once the user approved it; null before. */
    ibc: Ibc | null;
    /** Opens the app's connection to the user's node, making the app's key the first time. */
    static open(nodeUrl: string): Promise<IbcApp>;
    private constructor();
    /** The agent the app acts for, once the user approved it; null before. */
    get agent(): Address | null;
    /**
     * Asks the user who they are: the node's page signs the user in (or creates
     * their identity), and connects this app, with no permissions. Resolves
     * with the user's agent id. Other requests connect the app too; this one
     * only asks for that.
     */
    connect(): Promise<Address>;
    /**
     * Deploys a contract from Solidity source: the user's node compiles it, and
     * the user approves on the node's page, choosing the app's permissions the
     * first time this code is used. Resolves with the contract's id.
     * public: true proposes to let anyone read the user's copy (public_read);
     * the user decides, and it is off unless proposed.
     */
    deploy(input: {
        source: string;
        contract?: string;
        name?: string;
        constructor?: ConstructorArgs;
        protocol?: string;
        public?: boolean;
    }): Promise<string>;
    /**
     * Asks the user for permissions on an existing contract (one the agent
     * joined); with { public }, proposes to let anyone read it, or to stop it.
     */
    permit(contract: string, options?: {
        public?: boolean;
    }): Promise<ApprovalResult>;
    /**
     * Asks the user to join a contract, through a member's node; as a guest
     * with { guest: true }. public: true proposes to let anyone read the user's
     * copy (not for a guest, who keeps none).
     */
    join(contract: string, memberNode: string, memberAgent: Address, { guest, public: isPublic }?: {
        guest?: boolean;
        public?: boolean;
    }): Promise<unknown>;
    /** Reads silently if the user allowed it, and otherwise asks the user. */
    read(contract: string, method: string, values?: Record<string, unknown>): Promise<unknown>;
    /** Writes silently if the user allowed this function, and otherwise asks the user. */
    write(contract: string, method: string, values?: Record<string, unknown>): Promise<unknown>;
    /**
     * Calls fn(event) for every event of the contracts the app may read, and
     * opens the event stream, which then stays open (it reconnects by itself).
     * Returns a function that stops it.
     */
    onEvent(fn: (event: IbcEvent) => void): () => void;
    /**
     * Calls fn(status, error) whenever the event stream opens, breaks, reconnects
     * or closes (see StreamStatus). Returns a function that stops it.
     */
    onStatus(fn: (status: StreamStatus, error?: Error) => void): () => void;
    /** The event stream's state. */
    get streamStatus(): StreamStatus;
}
export {};
