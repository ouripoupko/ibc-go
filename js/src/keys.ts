// Keys and signatures for ibc clients, in browsers and Node.js 20+.
//
// An agent has two keys: its root key on the user's device (a passkey), which
// signs registrations, consents, grants and transactions; and its node's key,
// which signs calls silently under a grant from the root. Apps get a key of
// their own in the browser, which the node knows from the user's consent.
// Every message is signed as its EIP-712 digest, inside an envelope that
// carries the public key. See docs/keys.md; the Go side is package identity.
//
// A signer is any object with signDigest(digest, about) -> envelope, where
// `about` says what is signed, for pages that show it. This module makes two:
//   - passkeySigner: a passkey (WebAuthn): asks the user, in the browser;
//   - p256Signer: a WebCrypto P-256 key: signs silently (app keys, tools).

import { keccak_256 } from './vendor/noble-hashes/sha3.js';

/** A hex string, with 0x. */
export type Hex = string;
/** An agent id or node key: a checksummed address. */
export type Address = string;

export const DOMAIN = { name: 'ibc', version: '1' } as const;

interface Field {
  name: string;
  type: 'string' | 'address' | 'bytes' | 'uint64' | 'bytes32' | 'bool' | 'string[]' | 'bytes32[]';
}

// The EIP-712 types; they must match the node's (package identity).
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
    { name: 'credential', type: 'bytes' },
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
  Grant: [
    { name: 'agent', type: 'address' },
    { name: 'nodeKey', type: 'address' },
    { name: 'codeHash', type: 'bytes32' },
    { name: 'functions', type: 'string[]' },
    { name: 'nonce', type: 'string' },
  ],
  Consent: [
    { name: 'agent', type: 'address' },
    { name: 'app', type: 'string' },
    { name: 'appKey', type: 'bytes' },
    { name: 'codeHash', type: 'bytes32' },
    { name: 'read', type: 'bool' },
    { name: 'functions', type: 'string[]' },
    { name: 'nonce', type: 'string' },
  ],
  Device: [
    { name: 'agent', type: 'address' },
    { name: 'app', type: 'string' },
    { name: 'appKey', type: 'bytes' },
    { name: 'nonce', type: 'string' },
  ],
  Publish: [
    { name: 'agent', type: 'address' },
    { name: 'contract', type: 'string' },
    { name: 'public', type: 'bool' },
    { name: 'time', type: 'uint64' },
    { name: 'nonce', type: 'string' },
  ],
  Batch: [{ name: 'items', type: 'bytes32[]' }],
} as const satisfies Record<string, readonly Field[]>;

/** The name of a signed message type. */
export type MessageType = keyof typeof TYPES;

/** The messages, as the node signs and checks them. */
export interface Transaction { contract: string; kind: string; agent: Address; nonce: string; body: string }
export interface Registration { agent: Address; node: string; nodeKey: Address; credential: Hex; nonce: string }
export interface Query {
  agent: Address; action: string; target: string; contract: string; body: string; time: number; nonce: string;
}
export interface Grant { agent: Address; nodeKey: Address; codeHash: Hex; functions: string[]; nonce: string }
export interface Consent {
  agent: Address; app: string; appKey: Hex; codeHash: Hex; read: boolean; functions: string[]; nonce: string;
}
export interface Device { agent: Address; app: string; appKey: Hex; nonce: string }
/** Lets anyone read the agent's copy of a contract (public_read), or stops it. */
export interface Publish {
  agent: Address; contract: string; public: boolean;
  /** Unix milliseconds: the node takes only a newer choice than it has. */
  time: number; nonce: string;
}
export interface Batch { items: Hex[] }

/** The message of each type. */
export interface Messages {
  Transaction: Transaction; Registration: Registration; Query: Query; Grant: Grant;
  Consent: Consent; Device: Device; Publish: Publish; Batch: Batch;
}

/** One message to sign, with its type. */
export type Item = { [T in MessageType]: { type: T; message: Messages[T] } }[MessageType];

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
export type About = Item | { type: 'Batch'; messages: Item[] };

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

export const ZERO_ADDRESS: Address = '0x0000000000000000000000000000000000000000';

// Bytes and hex.

export function toHex(bytes: Uint8Array): Hex {
  return '0x' + Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

export function fromHex(hex: Hex): Uint8Array {
  const h = hex.startsWith('0x') ? hex.slice(2) : hex;
  if (h.length % 2 || /[^0-9a-fA-F]/.test(h)) throw new Error(`not hex: ${hex}`);
  return Uint8Array.from(h.match(/../g) ?? [], (b) => parseInt(b, 16));
}

/** Bytes as the Web Crypto and WebAuthn APIs take them. */
const buffer = (b: Uint8Array): Uint8Array<ArrayBuffer> => b as Uint8Array<ArrayBuffer>;

const utf8 = (s: string): Uint8Array => new TextEncoder().encode(s);

function concat(...parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let i = 0;
  for (const p of parts) {
    out.set(p, i);
    i += p.length;
  }
  return out;
}

export const keccak256 = (bytes: Uint8Array): Uint8Array => keccak_256(bytes);

/** A random nonce, as hex without 0x. */
export function nonce(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return toHex(bytes).slice(2);
}

/** The time in unix seconds, for queries. */
export const now = (): number => Math.floor(Date.now() / 1000);

// Addresses.

/** An address in its checksummed form (EIP-55). */
export function checksum(address: string): Address {
  const lower = address.toLowerCase().replace(/^0x/, '');
  const hash = toHex(keccak256(utf8(lower))).slice(2);
  return '0x' + [...lower].map((c, i) => (parseInt(hash[i], 16) >= 8 ? c.toUpperCase() : c)).join('');
}

/**
 * The agent address of a public key (65 bytes, uncompressed): the last 20
 * bytes of the keccak256 of its coordinates, for P-256 and secp256k1 keys.
 */
export function addressOf(publicKey: Uint8Array): Address {
  if (publicKey.length !== 65 || publicKey[0] !== 4) throw new Error('a public key is 65 bytes, uncompressed');
  return checksum(toHex(keccak256(publicKey.slice(1)).slice(12)));
}

// EIP-712 digests, for the flat types above.

function encodeValue(type: Field['type'], value: unknown): Uint8Array {
  switch (type) {
    case 'string': return keccak256(utf8(value as string));
    case 'bytes': return keccak256(fromHex((value as string) || '0x'));
    case 'address': return concat(new Uint8Array(12), fromHex(value as string));
    case 'uint64': {
      const out = new Uint8Array(32);
      let v = BigInt(value as number | string | bigint);
      for (let i = 31; v > 0n; i--, v >>= 8n) out[i] = Number(v & 0xffn);
      return out;
    }
    case 'bytes32': return fromHex(value as string);
    case 'bool': return encodeValue('uint64', value ? 1 : 0);
    case 'bytes32[]': return keccak256(concat(...(value as string[]).map((h) => fromHex(h))));
    case 'string[]': return keccak256(concat(...(value as string[]).map((v) => keccak256(utf8(v)))));
    default: throw new Error(`unsupported EIP-712 type ${type as string}`);
  }
}

function hashStruct(name: string, fields: readonly Field[], message: Record<string, unknown>): Uint8Array {
  const typeString = `${name}(${fields.map((f) => `${f.type} ${f.name}`).join(',')})`;
  return keccak256(concat(keccak256(utf8(typeString)), ...fields.map((f) => encodeValue(f.type, message[f.name]))));
}

const domainSeparator = hashStruct('EIP712Domain',
  [{ name: 'name', type: 'string' }, { name: 'version', type: 'string' }], DOMAIN);

/** The EIP-712 digest of a message of one of TYPES. A transaction's is its id. */
export function digestOf<T extends MessageType>(type: T, message: Messages[T]): Uint8Array {
  return keccak256(concat(Uint8Array.of(0x19, 0x01), domainSeparator,
    hashStruct(type, TYPES[type], message as unknown as Record<string, unknown>)));
}

// Signers.

/** A P-256 key made with WebCrypto (an app's key); the private key cannot be extracted. */
export async function newP256Key(): Promise<P256Key> {
  const pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
  return { privateKey: pair.privateKey, publicKey: new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey)) };
}

function envelope(scheme: Envelope['scheme'], publicKey: Uint8Array, sig: Uint8Array): Envelope {
  return { scheme, key: toHex(publicKey), sig: toHex(sig) };
}

/** A signer for a WebCrypto P-256 key: ECDSA with SHA-256 over the digest. */
export function p256Signer({ privateKey, publicKey }: P256Key): Signer {
  return {
    scheme: 'p256', publicKey,
    async signDigest(digest) {
      const sig = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, privateKey, buffer(digest));
      return envelope('p256', publicKey, new Uint8Array(sig));
    },
  };
}

/** A passkey: its credential id, and its public key (65 bytes, uncompressed). */
export interface Passkey {
  credentialId: Hex;
  publicKey: Uint8Array;
}

/**
 * Makes a passkey on this site (rpId: its domain). Returns its credential id
 * and public key. The phone, computer or security key asks the user.
 */
export async function createPasskey({ rpId = location.hostname, userName, displayName = userName }:
  { rpId?: string; userName: string; displayName?: string }): Promise<Passkey> {
  const userId = new Uint8Array(16);
  crypto.getRandomValues(userId);
  const challenge = new Uint8Array(32);
  crypto.getRandomValues(challenge);
  const cred = await navigator.credentials.create({
    publicKey: {
      challenge,
      rp: { id: rpId, name: 'ibc' },
      user: { id: userId, name: userName, displayName },
      pubKeyCredParams: [{ type: 'public-key', alg: -7 }], // ES256: P-256
      authenticatorSelection: { residentKey: 'required', userVerification: 'preferred' },
      attestation: 'none',
    },
  }) as PublicKeyCredential | null;
  if (!cred) throw new Error('no passkey was created');
  const response = cred.response as AuthenticatorAttestationResponse;
  if (response.getPublicKeyAlgorithm?.() !== -7) throw new Error('the passkey is not a P-256 key');
  const spki = response.getPublicKey();
  if (!spki) throw new Error('the passkey has no public key');
  const key = await crypto.subtle.importKey('spki', spki, { name: 'ECDSA', namedCurve: 'P-256' }, true, ['verify']);
  return {
    credentialId: toHex(new Uint8Array(cred.rawId)),
    publicKey: new Uint8Array(await crypto.subtle.exportKey('raw', key)),
  };
}

/** Asks the user to choose one of this site's passkeys; returns its credential id. */
export async function choosePasskey({ rpId = location.hostname }: { rpId?: string } = {}): Promise<Hex> {
  const challenge = new Uint8Array(32);
  crypto.getRandomValues(challenge);
  const cred = await navigator.credentials.get({ publicKey: { challenge, rpId, userVerification: 'preferred' } }) as
    PublicKeyCredential | null;
  if (!cred) throw new Error('no passkey was chosen');
  return toHex(new Uint8Array(cred.rawId));
}

/** A signer for a passkey: the digest is the WebAuthn challenge. */
export function passkeySigner({ credentialId, publicKey, rpId = location.hostname }: Passkey & { rpId?: string }): Signer {
  return {
    scheme: 'webauthn', publicKey,
    async signDigest(digest) {
      const cred = await navigator.credentials.get({
        publicKey: {
          challenge: buffer(digest), rpId, userVerification: 'preferred',
          allowCredentials: [{ type: 'public-key', id: buffer(fromHex(credentialId)) }],
        },
      }) as PublicKeyCredential | null;
      if (!cred) throw new Error('the passkey did not sign');
      const r = cred.response as AuthenticatorAssertionResponse;
      return {
        ...envelope('webauthn', publicKey, new Uint8Array(r.signature)),
        authenticatorData: toHex(new Uint8Array(r.authenticatorData)),
        clientDataJSON: new TextDecoder().decode(r.clientDataJSON),
      };
    },
  };
}

/** Signs a message of one of TYPES. Returns its digest and envelope. */
export async function sign<T extends MessageType>(signer: Signer, type: T, message: Messages[T]):
  Promise<{ digest: Uint8Array; envelope: Envelope }> {
  const digest = digestOf(type, message);
  return { digest, envelope: await signer.signDigest(digest, { type, message } as Item) };
}

/**
 * Signs several messages with one signature: one envelope for each, in
 * order. With a passkey, the user approves once.
 */
export async function signBatch(signer: Signer, items: Item[]): Promise<Envelope[]> {
  const digests = items.map(({ type, message }) => toHex(digestOf(type, message as never)));
  const env = await signer.signDigest(digestOf('Batch', { items: digests }), { type: 'Batch', messages: items });
  return items.map(() => ({ ...env, batch: digests }));
}

/** A grant, certified by the root: lets the agent's node sign these functions of this code silently. */
export async function certifyGrant(root: Signer, { agent, nodeKey, codeHash, functions }:
  Omit<Grant, 'nonce'>): Promise<{ grant: Grant; signature: Envelope }> {
  const grant: Grant = { agent, nodeKey, codeHash, functions, nonce: nonce() };
  const { envelope: signature } = await sign(root, 'Grant', grant);
  return { grant, signature };
}
