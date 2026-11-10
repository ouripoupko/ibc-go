/** A hex string, with 0x. */
export type Hex = string;
/** An agent id or node key: a checksummed address. */
export type Address = string;
export declare const DOMAIN: {
    readonly name: "ibc";
    readonly version: "1";
};
export declare const TYPES: {
    readonly Transaction: readonly [{
        readonly name: "contract";
        readonly type: "string";
    }, {
        readonly name: "kind";
        readonly type: "string";
    }, {
        readonly name: "agent";
        readonly type: "address";
    }, {
        readonly name: "nonce";
        readonly type: "string";
    }, {
        readonly name: "body";
        readonly type: "string";
    }];
    readonly Registration: readonly [{
        readonly name: "agent";
        readonly type: "address";
    }, {
        readonly name: "node";
        readonly type: "string";
    }, {
        readonly name: "nodeKey";
        readonly type: "address";
    }, {
        readonly name: "credential";
        readonly type: "bytes";
    }, {
        readonly name: "nonce";
        readonly type: "string";
    }];
    readonly Query: readonly [{
        readonly name: "agent";
        readonly type: "address";
    }, {
        readonly name: "action";
        readonly type: "string";
    }, {
        readonly name: "target";
        readonly type: "string";
    }, {
        readonly name: "contract";
        readonly type: "string";
    }, {
        readonly name: "body";
        readonly type: "string";
    }, {
        readonly name: "time";
        readonly type: "uint64";
    }, {
        readonly name: "nonce";
        readonly type: "string";
    }];
    readonly Grant: readonly [{
        readonly name: "agent";
        readonly type: "address";
    }, {
        readonly name: "nodeKey";
        readonly type: "address";
    }, {
        readonly name: "codeHash";
        readonly type: "bytes32";
    }, {
        readonly name: "functions";
        readonly type: "string[]";
    }, {
        readonly name: "nonce";
        readonly type: "string";
    }];
    readonly Consent: readonly [{
        readonly name: "agent";
        readonly type: "address";
    }, {
        readonly name: "app";
        readonly type: "string";
    }, {
        readonly name: "appKey";
        readonly type: "bytes";
    }, {
        readonly name: "codeHash";
        readonly type: "bytes32";
    }, {
        readonly name: "read";
        readonly type: "bool";
    }, {
        readonly name: "functions";
        readonly type: "string[]";
    }, {
        readonly name: "nonce";
        readonly type: "string";
    }];
    readonly Device: readonly [{
        readonly name: "agent";
        readonly type: "address";
    }, {
        readonly name: "app";
        readonly type: "string";
    }, {
        readonly name: "appKey";
        readonly type: "bytes";
    }, {
        readonly name: "nonce";
        readonly type: "string";
    }];
    readonly Publish: readonly [{
        readonly name: "agent";
        readonly type: "address";
    }, {
        readonly name: "contract";
        readonly type: "string";
    }, {
        readonly name: "public";
        readonly type: "bool";
    }, {
        readonly name: "time";
        readonly type: "uint64";
    }, {
        readonly name: "nonce";
        readonly type: "string";
    }];
    readonly Batch: readonly [{
        readonly name: "items";
        readonly type: "bytes32[]";
    }];
};
/** The name of a signed message type. */
export type MessageType = keyof typeof TYPES;
/** The messages, as the node signs and checks them. */
export interface Transaction {
    contract: string;
    kind: string;
    agent: Address;
    nonce: string;
    body: string;
}
export interface Registration {
    agent: Address;
    node: string;
    nodeKey: Address;
    credential: Hex;
    nonce: string;
}
export interface Query {
    agent: Address;
    action: string;
    target: string;
    contract: string;
    body: string;
    time: number;
    nonce: string;
}
export interface Grant {
    agent: Address;
    nodeKey: Address;
    codeHash: Hex;
    functions: string[];
    nonce: string;
}
export interface Consent {
    agent: Address;
    app: string;
    appKey: Hex;
    codeHash: Hex;
    read: boolean;
    functions: string[];
    nonce: string;
}
export interface Device {
    agent: Address;
    app: string;
    appKey: Hex;
    nonce: string;
}
/** Lets anyone read the agent's copy of a contract (public_read), or stops it. */
export interface Publish {
    agent: Address;
    contract: string;
    public: boolean;
    /** Unix milliseconds: the node takes only a newer choice than it has. */
    time: number;
    nonce: string;
}
export interface Batch {
    items: Hex[];
}
/** The message of each type. */
export interface Messages {
    Transaction: Transaction;
    Registration: Registration;
    Query: Query;
    Grant: Grant;
    Consent: Consent;
    Device: Device;
    Publish: Publish;
    Batch: Batch;
}
/** One message to sign, with its type. */
export type Item = {
    [T in MessageType]: {
        type: T;
        message: Messages[T];
    };
}[MessageType];
/** A signature, with the public key and what the node needs to check it. */
export interface Envelope {
    scheme: 'secp256k1-eip712' | 'p256' | 'webauthn';
    key: Hex;
    sig: Hex;
    authenticatorData?: Hex;
    clientDataJSON?: string;
    /** For one signature over several messages: their digests. */
    batch?: Hex[];
}
/** What a signer is asked to sign, for pages that show it to the user. */
export type About = Item | {
    type: 'Batch';
    messages: Item[];
};
/** Anything that signs a digest: a passkey, a WebCrypto key. */
export interface Signer {
    scheme: Envelope['scheme'];
    publicKey: Uint8Array;
    signDigest(digest: Uint8Array, about: About): Promise<Envelope>;
}
/** A WebCrypto P-256 key pair; the private key cannot be extracted. */
export interface P256Key {
    privateKey: CryptoKey;
    publicKey: Uint8Array;
}
export declare const ZERO_ADDRESS: Address;
export declare function toHex(bytes: Uint8Array): Hex;
export declare function fromHex(hex: Hex): Uint8Array;
export declare const keccak256: (bytes: Uint8Array) => Uint8Array;
/** A random nonce, as hex without 0x. */
export declare function nonce(): string;
/** The time in unix seconds, for queries. */
export declare const now: () => number;
/** An address in its checksummed form (EIP-55). */
export declare function checksum(address: string): Address;
/**
 * The agent address of a public key (65 bytes, uncompressed): the last 20
 * bytes of the keccak256 of its coordinates, for P-256 and secp256k1 keys.
 */
export declare function addressOf(publicKey: Uint8Array): Address;
/** The EIP-712 digest of a message of one of TYPES. A transaction's is its id. */
export declare function digestOf<T extends MessageType>(type: T, message: Messages[T]): Uint8Array;
/** A P-256 key made with WebCrypto (an app's key); the private key cannot be extracted. */
export declare function newP256Key(): Promise<P256Key>;
/** A signer for a WebCrypto P-256 key: ECDSA with SHA-256 over the digest. */
export declare function p256Signer({ privateKey, publicKey }: P256Key): Signer;
/** A passkey: its credential id, and its public key (65 bytes, uncompressed). */
export interface Passkey {
    credentialId: Hex;
    publicKey: Uint8Array;
}
/**
 * Makes a passkey on this site (rpId: its domain). Returns its credential id
 * and public key. The phone, computer or security key asks the user.
 */
export declare function createPasskey({ rpId, userName, displayName }: {
    rpId?: string;
    userName: string;
    displayName?: string;
}): Promise<Passkey>;
/** Asks the user to choose one of this site's passkeys; returns its credential id. */
export declare function choosePasskey({ rpId }?: {
    rpId?: string;
}): Promise<Hex>;
/** A signer for a passkey: the digest is the WebAuthn challenge. */
export declare function passkeySigner({ credentialId, publicKey, rpId }: Passkey & {
    rpId?: string;
}): Signer;
/** Signs a message of one of TYPES. Returns its digest and envelope. */
export declare function sign<T extends MessageType>(signer: Signer, type: T, message: Messages[T]): Promise<{
    digest: Uint8Array;
    envelope: Envelope;
}>;
/**
 * Signs several messages with one signature: one envelope for each, in
 * order. With a passkey, the user approves once.
 */
export declare function signBatch(signer: Signer, items: Item[]): Promise<Envelope[]>;
/** A grant, certified by the root: lets the agent's node sign these functions of this code silently. */
export declare function certifyGrant(root: Signer, { agent, nodeKey, codeHash, functions }: Omit<Grant, 'nonce'>): Promise<{
    grant: Grant;
    signature: Envelope;
}>;
