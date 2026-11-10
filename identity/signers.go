package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// KeySigner signs digests into envelopes. In the browser, the root key is a
// passkey and app keys are WebCrypto keys; these signers are for the node
// (its own key), the command line and tests.
type KeySigner interface {
	SignDigest(digest ethcommon.Hash) (Envelope, error)
}

// Secp256k1Signer signs with a secp256k1 key: a root key file, or the node's
// key, under a grant if Cert is set.
type Secp256k1Signer struct {
	Key  *ecdsa.PrivateKey
	Cert *Certificate
}

func (s Secp256k1Signer) SignDigest(digest ethcommon.Hash) (Envelope, error) {
	sig, err := SignHash(digest, s.Key)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Scheme: SchemeSecp256k1, Key: PublicKey(s.Key), Sig: sig, Certificate: s.Cert}, nil
}

// P256Signer signs with a P-256 key, as WebCrypto does.
type P256Signer struct {
	Key *ecdsa.PrivateKey
}

func (s P256Signer) SignDigest(digest ethcommon.Hash) (Envelope, error) {
	h := sha256.Sum256(digest[:])
	r, sv, err := ecdsa.Sign(rand.Reader, s.Key, h[:])
	if err != nil {
		return Envelope{}, err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return Envelope{Scheme: SchemeP256, Key: PublicKey(s.Key), Sig: sig}, nil
}

// WebAuthnSigner simulates a passkey, for tests: it makes the assertion a
// browser and an authenticator would make for RPID.
type WebAuthnSigner struct {
	Key   *ecdsa.PrivateKey // P-256
	RPID  string
	Flags byte // authenticator flags; 0 means user present and verified (0x05)
}

func (s WebAuthnSigner) SignDigest(digest ethcommon.Hash) (Envelope, error) {
	flags := s.Flags
	if flags == 0 {
		flags = 0x05
	}
	rp := sha256.Sum256([]byte(s.RPID))
	ad := append(rp[:], flags, 0, 0, 0, 0)
	cd, err := json.Marshal(map[string]any{
		"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(digest[:]),
		"origin": "https://" + s.RPID, "crossOrigin": false,
	})
	if err != nil {
		return Envelope{}, err
	}
	cdHash := sha256.Sum256(cd)
	h := sha256.Sum256(append(slices.Clone(ad), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, s.Key, h[:])
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Scheme: SchemeWebAuthn, Key: PublicKey(s.Key), Sig: sig,
		AuthenticatorData: ad, ClientDataJSON: string(cd)}, nil
}

// SignBatch signs several digests with one signature, and returns one
// envelope for each, in the same order.
func SignBatch(s KeySigner, digests ...ethcommon.Hash) ([]Envelope, error) {
	if len(digests) == 0 || len(digests) > maxBatch {
		return nil, errors.New("a batch signs 1 to 16 messages")
	}
	h, err := BatchHash(digests)
	if err != nil {
		return nil, err
	}
	env, err := s.SignDigest(h)
	if err != nil {
		return nil, err
	}
	env.Batch = slices.Clone(digests)
	out := make([]Envelope, len(digests))
	for i := range out {
		out[i] = env
	}
	return out, nil
}

// CertifyGrant signs a grant with the agent's root key, making the
// certificate a node signs under.
func CertifyGrant(g Grant, root KeySigner) (*Certificate, error) {
	h, err := g.Hash()
	if err != nil {
		return nil, err
	}
	env, err := root.SignDigest(h)
	if err != nil {
		return nil, err
	}
	return &Certificate{Grant: g, Signature: env}, nil
}

// NewP256Key makes a P-256 key.
func NewP256Key() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// RootAddress returns the agent address of a secp256k1 root key.
func RootAddress(key *ecdsa.PrivateKey) ethcommon.Address {
	return crypto.PubkeyToAddress(key.PublicKey)
}
