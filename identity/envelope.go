package identity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// Signature schemes: how a key signs a message's EIP-712 digest.
const (
	// SchemeSecp256k1 is an Ethereum key: a 65-byte secp256k1 signature of
	// the digest (as wallets and key files make it). The public key is
	// recovered from the signature.
	SchemeSecp256k1 = "secp256k1-eip712"
	// SchemeP256 is a P-256 key, as WebCrypto keeps it: ECDSA with SHA-256
	// over the digest, the signature as r and s (64 bytes).
	SchemeP256 = "p256"
	// SchemeWebAuthn is a passkey: the digest is the WebAuthn challenge, and
	// the signature (DER) covers the authenticator data and the hash of the
	// client data, as WebAuthn defines.
	SchemeWebAuthn = "webauthn"
)

// maxBatch bounds the number of messages one batch signature covers.
const maxBatch = 16

// Envelope is a signature with everything needed to check it: the scheme,
// the public key, and, for the agent's node signing silently, the grant
// that allows it. A partner can check a transaction from its envelope alone.
type Envelope struct {
	Scheme string        `json:"scheme"`
	Key    hexutil.Bytes `json:"key,omitempty"` // uncompressed public key, 65 bytes (optional for secp256k1)
	Sig    hexutil.Bytes `json:"sig"`
	// WebAuthn only.
	AuthenticatorData hexutil.Bytes `json:"authenticatorData,omitempty"`
	ClientDataJSON    string        `json:"clientDataJSON,omitempty"`
	// When the agent's node signs a call silently: the root's grant.
	Certificate *Certificate `json:"certificate,omitempty"`
	// For a batch signature: the digests of all the messages signed at
	// once; the key signed the digest of the Batch.
	Batch []ethcommon.Hash `json:"batch,omitempty"`
}

// Grant lets the agent's node sign calls for the agent, silently: calls to
// the given functions of contracts made from the given source. The agent's
// root key signs it; partners check every node-signed call against it.
type Grant struct {
	Agent      ethcommon.Address `json:"agent"`
	NodeKey    ethcommon.Address `json:"nodeKey"`    // the node that may sign
	SourceHash ethcommon.Hash    `json:"sourceHash"` // SourceHash of the contract's Solidity source
	Functions  []string          `json:"functions"`  // the functions it may call
	Nonce      string            `json:"nonce"`
}

// Certificate is a grant with the root key's signature.
type Certificate struct {
	Grant     Grant    `json:"grant"`
	Signature Envelope `json:"signature"`
}

// Signer is who signed, as Verify found it.
type Signer struct {
	Agent ethcommon.Address // the key's address, or, for a node signing under a grant, the agent
	Key   []byte            // the public key that signed
	Grant *Grant            // the grant a node signed under, or nil
}

// Purposes: what a signature is for.
type Purpose int

const (
	// ForRoot: registrations, consents and grants: the agent's root key only.
	ForRoot Purpose = iota
	// ForTransaction: the root key, or the agent's node under a grant.
	ForTransaction
	// ForQuery: any key; the node decides whose it is (the root, or an app).
	ForQuery
)

// Verify checks that env is a signature of digest by its key, and, if it
// carries a grant, that the agent's root signed the grant for this key (only
// for transactions). It is the only place where signatures are checked, and
// it uses no clock: every partner reaches the same answer.
func Verify(digest ethcommon.Hash, env Envelope, purpose Purpose) (Signer, error) {
	signed := digest
	if len(env.Batch) > 0 {
		if len(env.Batch) > maxBatch {
			return Signer{}, fmt.Errorf("a batch signs at most %d messages", maxBatch)
		}
		if !slices.Contains(env.Batch, digest) {
			return Signer{}, errors.New("signature: the message is not in its batch")
		}
		var err error
		if signed, err = BatchHash(env.Batch); err != nil {
			return Signer{}, err
		}
	}
	key, err := checkScheme(signed, env)
	if err != nil {
		return Signer{}, err
	}
	addr, err := AddressOf(key)
	if err != nil {
		return Signer{}, err
	}
	if env.Certificate == nil {
		return Signer{Agent: addr, Key: key}, nil
	}

	if purpose != ForTransaction {
		return Signer{}, errors.New("a grant only allows a node to sign transactions")
	}
	g := env.Certificate.Grant
	if err := g.check(); err != nil {
		return Signer{}, err
	}
	if addr != g.NodeKey {
		return Signer{}, errors.New("signature: not by the node the grant names")
	}
	if env.Certificate.Signature.Certificate != nil {
		return Signer{}, errors.New("grant: signed under another grant")
	}
	gh, err := g.Hash()
	if err != nil {
		return Signer{}, err
	}
	issuer, err := Verify(gh, env.Certificate.Signature, ForRoot)
	if err != nil {
		return Signer{}, fmt.Errorf("grant: %w", err)
	}
	if issuer.Agent != g.Agent {
		return Signer{}, errors.New("grant: not signed by the agent's root key")
	}
	return Signer{Agent: g.Agent, Key: key, Grant: &g}, nil
}

// VerifyGrant checks a grant on its own: its fields, and that the agent's
// root key signed it.
func VerifyGrant(c *Certificate) error {
	if err := c.Grant.check(); err != nil {
		return err
	}
	h, err := c.Grant.Hash()
	if err != nil {
		return err
	}
	if c.Signature.Certificate != nil {
		return errors.New("grant: signed under another grant")
	}
	issuer, err := Verify(h, c.Signature, ForRoot)
	if err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	if issuer.Agent != c.Grant.Agent {
		return errors.New("grant: not signed by the agent's root key")
	}
	return nil
}

func (g Grant) check() error {
	if g.Agent == (ethcommon.Address{}) || g.NodeKey == (ethcommon.Address{}) || g.SourceHash == (ethcommon.Hash{}) {
		return errors.New("a grant names an agent, a node and a source hash")
	}
	if len(g.Functions) == 0 {
		return errors.New("a grant names at least one function")
	}
	return nil
}

// Allows reports whether the grant lets the node call method.
func (g Grant) Allows(method string) bool {
	return slices.Contains(g.Functions, method)
}

// checkScheme verifies the signature of signed by the envelope's key, and
// returns that key.
func checkScheme(signed ethcommon.Hash, env Envelope) ([]byte, error) {
	switch env.Scheme {
	case SchemeSecp256k1:
		if len(env.Sig) != 65 {
			return nil, errors.New("a secp256k1 signature is 65 bytes")
		}
		pub, err := recoverPub(signed, env.Sig)
		if err != nil {
			return nil, err
		}
		key := crypto.FromECDSAPub(pub)
		if len(env.Key) > 0 && !bytes.Equal(env.Key, key) {
			return nil, ErrBadSignature
		}
		return key, nil

	case SchemeP256:
		pub, err := p256Key(env.Key)
		if err != nil {
			return nil, err
		}
		if len(env.Sig) != 64 {
			return nil, errors.New("a p256 signature is 64 bytes (r and s)")
		}
		h := sha256.Sum256(signed[:])
		r, s := new(big.Int).SetBytes(env.Sig[:32]), new(big.Int).SetBytes(env.Sig[32:])
		if !ecdsa.Verify(pub, h[:], r, s) {
			return nil, ErrBadSignature
		}
		return env.Key, nil

	case SchemeWebAuthn:
		pub, err := p256Key(env.Key)
		if err != nil {
			return nil, err
		}
		if err := checkWebAuthn(signed, env, pub); err != nil {
			return nil, err
		}
		return env.Key, nil
	}
	return nil, fmt.Errorf("unknown signature scheme %q", env.Scheme)
}

// checkWebAuthn verifies a passkey's assertion: the client data's type and
// challenge, the user-presence flag, and the signature.
func checkWebAuthn(signed ethcommon.Hash, env Envelope, pub *ecdsa.PublicKey) error {
	ad := env.AuthenticatorData
	if len(ad) < 37 {
		return errors.New("webauthn: authenticator data too short")
	}
	const userPresent = 0x01
	if ad[32]&userPresent == 0 {
		return errors.New("webauthn: the user did not approve")
	}
	var cd struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal([]byte(env.ClientDataJSON), &cd); err != nil {
		return fmt.Errorf("webauthn client data: %w", err)
	}
	if cd.Type != "webauthn.get" {
		return errors.New("webauthn: not an assertion")
	}
	if cd.Challenge != base64.RawURLEncoding.EncodeToString(signed[:]) {
		return errors.New("webauthn: the signature is for another message")
	}
	cdHash := sha256.Sum256([]byte(env.ClientDataJSON))
	h := sha256.Sum256(append(slices.Clone([]byte(ad)), cdHash[:]...))
	if !ecdsa.VerifyASN1(pub, h[:], env.Sig) {
		return ErrBadSignature
	}
	return nil
}

func p256Key(key []byte) (*ecdsa.PublicKey, error) {
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), key)
	if err != nil {
		return nil, fmt.Errorf("p256 key: %w", err)
	}
	return pub, nil
}

func recoverPub(hash ethcommon.Hash, sig []byte) (*ecdsa.PublicKey, error) {
	s := slices.Clone(sig)
	if s[64] >= 27 {
		s[64] -= 27
	}
	if s[64] > 1 {
		return nil, errors.New("invalid signature recovery id")
	}
	return crypto.SigToPub(hash[:], s)
}

// AddressOf returns the agent address of a public key: the last 20 bytes of
// the keccak256 of its coordinates, Ethereum's formula, for secp256k1 and
// P-256 keys alike.
func AddressOf(key []byte) (ethcommon.Address, error) {
	if len(key) != 65 || key[0] != 4 {
		return ethcommon.Address{}, errors.New("a public key is 65 bytes, uncompressed")
	}
	return ethcommon.BytesToAddress(crypto.Keccak256(key[1:])[12:]), nil
}

// PublicKey returns a key's public key, uncompressed (65 bytes).
func PublicKey(key *ecdsa.PrivateKey) []byte {
	return elliptic.Marshal(key.Curve, key.X, key.Y) //nolint:staticcheck // the uncompressed form, for both curves
}

// TypedData returns the grant as EIP-712 typed data.
func (g Grant) TypedData() apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": domainType,
			"Grant": {
				{Name: "agent", Type: "address"},
				{Name: "nodeKey", Type: "address"},
				{Name: "sourceHash", Type: "bytes32"},
				{Name: "functions", Type: "string[]"},
				{Name: "nonce", Type: "string"},
			},
		},
		PrimaryType: "Grant",
		Domain:      domain(),
		Message: apitypes.TypedDataMessage{
			"agent":      g.Agent.Hex(),
			"nodeKey":    g.NodeKey.Hex(),
			"sourceHash": g.SourceHash.Hex(),
			"functions":  stringList(g.Functions),
			"nonce":      g.Nonce,
		},
	}
}

// Hash returns the grant's EIP-712 digest.
func (g Grant) Hash() (ethcommon.Hash, error) {
	return typedHash(g.TypedData())
}

func stringList(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// BatchHash is what one signature over several messages signs: the EIP-712
// digest of Batch { bytes32[] items }.
func BatchHash(items []ethcommon.Hash) (ethcommon.Hash, error) {
	list := make([]any, len(items))
	for i, h := range items {
		list[i] = h.Hex()
	}
	return typedHash(apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": domainType,
			"Batch":        {{Name: "items", Type: "bytes32[]"}},
		},
		PrimaryType: "Batch",
		Domain:      domain(),
		Message:     apitypes.TypedDataMessage{"items": list},
	})
}
