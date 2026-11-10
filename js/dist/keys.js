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
export const DOMAIN = { name: 'ibc', version: '1' };
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
};
export const ZERO_ADDRESS = '0x0000000000000000000000000000000000000000';
// Bytes and hex.
export function toHex(bytes) {
    return '0x' + Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}
export function fromHex(hex) {
    const h = hex.startsWith('0x') ? hex.slice(2) : hex;
    if (h.length % 2 || /[^0-9a-fA-F]/.test(h))
        throw new Error(`not hex: ${hex}`);
    return Uint8Array.from(h.match(/../g) ?? [], (b) => parseInt(b, 16));
}
/** Bytes as the Web Crypto and WebAuthn APIs take them. */
const buffer = (b) => b;
const utf8 = (s) => new TextEncoder().encode(s);
function concat(...parts) {
    const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
    let i = 0;
    for (const p of parts) {
        out.set(p, i);
        i += p.length;
    }
    return out;
}
export const keccak256 = (bytes) => keccak_256(bytes);
/** A random nonce, as hex without 0x. */
export function nonce() {
    const bytes = new Uint8Array(16);
    crypto.getRandomValues(bytes);
    return toHex(bytes).slice(2);
}
/** The time in unix seconds, for queries. */
export const now = () => Math.floor(Date.now() / 1000);
// Addresses.
/** An address in its checksummed form (EIP-55). */
export function checksum(address) {
    const lower = address.toLowerCase().replace(/^0x/, '');
    const hash = toHex(keccak256(utf8(lower))).slice(2);
    return '0x' + [...lower].map((c, i) => (parseInt(hash[i], 16) >= 8 ? c.toUpperCase() : c)).join('');
}
/**
 * The agent address of a public key (65 bytes, uncompressed): the last 20
 * bytes of the keccak256 of its coordinates, for P-256 and secp256k1 keys.
 */
export function addressOf(publicKey) {
    if (publicKey.length !== 65 || publicKey[0] !== 4)
        throw new Error('a public key is 65 bytes, uncompressed');
    return checksum(toHex(keccak256(publicKey.slice(1)).slice(12)));
}
// EIP-712 digests, for the flat types above.
function encodeValue(type, value) {
    switch (type) {
        case 'string': return keccak256(utf8(value));
        case 'bytes': return keccak256(fromHex(value || '0x'));
        case 'address': return concat(new Uint8Array(12), fromHex(value));
        case 'uint64': {
            const out = new Uint8Array(32);
            let v = BigInt(value);
            for (let i = 31; v > 0n; i--, v >>= 8n)
                out[i] = Number(v & 0xffn);
            return out;
        }
        case 'bytes32': return fromHex(value);
        case 'bool': return encodeValue('uint64', value ? 1 : 0);
        case 'bytes32[]': return keccak256(concat(...value.map((h) => fromHex(h))));
        case 'string[]': return keccak256(concat(...value.map((v) => keccak256(utf8(v)))));
        default: throw new Error(`unsupported EIP-712 type ${type}`);
    }
}
function hashStruct(name, fields, message) {
    const typeString = `${name}(${fields.map((f) => `${f.type} ${f.name}`).join(',')})`;
    return keccak256(concat(keccak256(utf8(typeString)), ...fields.map((f) => encodeValue(f.type, message[f.name]))));
}
const domainSeparator = hashStruct('EIP712Domain', [{ name: 'name', type: 'string' }, { name: 'version', type: 'string' }], DOMAIN);
/** The EIP-712 digest of a message of one of TYPES. A transaction's is its id. */
export function digestOf(type, message) {
    return keccak256(concat(Uint8Array.of(0x19, 0x01), domainSeparator, hashStruct(type, TYPES[type], message)));
}
// Signers.
/** A P-256 key made with WebCrypto (an app's key); the private key cannot be extracted. */
export async function newP256Key() {
    const pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
    return { privateKey: pair.privateKey, publicKey: new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey)) };
}
function envelope(scheme, publicKey, sig) {
    return { scheme, key: toHex(publicKey), sig: toHex(sig) };
}
/** A signer for a WebCrypto P-256 key: ECDSA with SHA-256 over the digest. */
export function p256Signer({ privateKey, publicKey }) {
    return {
        scheme: 'p256', publicKey,
        async signDigest(digest) {
            const sig = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, privateKey, buffer(digest));
            return envelope('p256', publicKey, new Uint8Array(sig));
        },
    };
}
/**
 * Makes a passkey on this site (rpId: its domain). Returns its credential id
 * and public key. The phone, computer or security key asks the user.
 */
export async function createPasskey({ rpId = location.hostname, userName, displayName = userName }) {
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
    });
    if (!cred)
        throw new Error('no passkey was created');
    const response = cred.response;
    if (response.getPublicKeyAlgorithm?.() !== -7)
        throw new Error('the passkey is not a P-256 key');
    const spki = response.getPublicKey();
    if (!spki)
        throw new Error('the passkey has no public key');
    const key = await crypto.subtle.importKey('spki', spki, { name: 'ECDSA', namedCurve: 'P-256' }, true, ['verify']);
    return {
        credentialId: toHex(new Uint8Array(cred.rawId)),
        publicKey: new Uint8Array(await crypto.subtle.exportKey('raw', key)),
    };
}
/** Asks the user to choose one of this site's passkeys; returns its credential id. */
export async function choosePasskey({ rpId = location.hostname } = {}) {
    const challenge = new Uint8Array(32);
    crypto.getRandomValues(challenge);
    const cred = await navigator.credentials.get({ publicKey: { challenge, rpId, userVerification: 'preferred' } });
    if (!cred)
        throw new Error('no passkey was chosen');
    return toHex(new Uint8Array(cred.rawId));
}
/** A signer for a passkey: the digest is the WebAuthn challenge. */
export function passkeySigner({ credentialId, publicKey, rpId = location.hostname }) {
    return {
        scheme: 'webauthn', publicKey,
        async signDigest(digest) {
            const cred = await navigator.credentials.get({
                publicKey: {
                    challenge: buffer(digest), rpId, userVerification: 'preferred',
                    allowCredentials: [{ type: 'public-key', id: buffer(fromHex(credentialId)) }],
                },
            });
            if (!cred)
                throw new Error('the passkey did not sign');
            const r = cred.response;
            return {
                ...envelope('webauthn', publicKey, new Uint8Array(r.signature)),
                authenticatorData: toHex(new Uint8Array(r.authenticatorData)),
                clientDataJSON: new TextDecoder().decode(r.clientDataJSON),
            };
        },
    };
}
/** Signs a message of one of TYPES. Returns its digest and envelope. */
export async function sign(signer, type, message) {
    const digest = digestOf(type, message);
    return { digest, envelope: await signer.signDigest(digest, { type, message }) };
}
/**
 * Signs several messages with one signature: one envelope for each, in
 * order. With a passkey, the user approves once.
 */
export async function signBatch(signer, items) {
    const digests = items.map(({ type, message }) => toHex(digestOf(type, message)));
    const env = await signer.signDigest(digestOf('Batch', { items: digests }), { type: 'Batch', messages: items });
    return items.map(() => ({ ...env, batch: digests }));
}
/** A grant, certified by the root: lets the agent's node sign these functions of this code silently. */
export async function certifyGrant(root, { agent, nodeKey, codeHash, functions }) {
    const grant = { agent, nodeKey, codeHash, functions, nonce: nonce() };
    const { envelope: signature } = await sign(root, 'Grant', grant);
    return { grant, signature };
}
