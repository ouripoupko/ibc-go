package identity

import (
	"errors"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// Consent is what the agent allows an app, for one contract source, on the
// agent's own node: to read silently, and to have the node sign some of its
// functions silently. The root key signs it on the node's page; it stays on
// the node (partners never see it: they see the grant).
type Consent struct {
	Agent      ethcommon.Address `json:"agent"`
	App        string            `json:"app"`        // the app's origin, e.g. https://wishes.example.com
	AppKey     hexutil.Bytes     `json:"appKey"`     // the app's key in the browser, P-256, uncompressed
	SourceHash ethcommon.Hash    `json:"sourceHash"` // the contract's source (SourceHash); zero: every contract (the node's own page)
	Read       bool              `json:"read"`       // may read without asking
	Functions  []string          `json:"functions"`  // may have the node call these without asking
	Nonce      string            `json:"nonce"`
}

// SignedConsent is a consent with the root key's signature.
type SignedConsent struct {
	Consent   Consent  `json:"consent"`
	Signature Envelope `json:"signature"`
}

// TypedData returns the consent as EIP-712 typed data.
func (c Consent) TypedData() apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": domainType,
			"Consent": {
				{Name: "agent", Type: "address"},
				{Name: "app", Type: "string"},
				{Name: "appKey", Type: "bytes"},
				{Name: "sourceHash", Type: "bytes32"},
				{Name: "read", Type: "bool"},
				{Name: "functions", Type: "string[]"},
				{Name: "nonce", Type: "string"},
			},
		},
		PrimaryType: "Consent",
		Domain:      domain(),
		Message: apitypes.TypedDataMessage{
			"agent":      c.Agent.Hex(),
			"app":        c.App,
			"appKey":     hexutil.Encode(c.AppKey),
			"sourceHash": c.SourceHash.Hex(),
			"read":       c.Read,
			"functions":  stringList(c.Functions),
			"nonce":      c.Nonce,
		},
	}
}

// Hash returns the consent's EIP-712 digest.
func (c Consent) Hash() (ethcommon.Hash, error) {
	return typedHash(c.TypedData())
}

// Verify checks that the agent's root key signed the consent.
func (s SignedConsent) Verify() error {
	if s.Signature.Certificate != nil {
		return errors.New("a consent is signed by the root key")
	}
	if _, err := AddressOf(s.Consent.AppKey); err != nil {
		return errors.New("consent: the app key must be a public key, 65 bytes uncompressed")
	}
	h, err := s.Consent.Hash()
	if err != nil {
		return err
	}
	signer, err := Verify(h, s.Signature, ForRoot)
	if err != nil {
		return err
	}
	if signer.Agent != s.Consent.Agent {
		return ErrBadSignature
	}
	return nil
}

// SignConsent signs a consent with the root key (the command line and tests).
func SignConsent(c Consent, root KeySigner) (SignedConsent, error) {
	h, err := c.Hash()
	if err != nil {
		return SignedConsent{}, err
	}
	env, err := root.SignDigest(h)
	if err != nil {
		return SignedConsent{}, err
	}
	return SignedConsent{Consent: c, Signature: env}, nil
}

// Device connects another browser of an app the agent already approved: the
// app's permissions apply to this key too. The root key signs it; it stays on
// the node.
type Device struct {
	Agent  ethcommon.Address `json:"agent"`
	App    string            `json:"app"`    // the app's origin
	AppKey hexutil.Bytes     `json:"appKey"` // the new browser's key, P-256, uncompressed
	Nonce  string            `json:"nonce"`
}

// SignedDevice is a device with the root key's signature.
type SignedDevice struct {
	Device    Device   `json:"device"`
	Signature Envelope `json:"signature"`
}

// TypedData returns the device as EIP-712 typed data.
func (d Device) TypedData() apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": domainType,
			"Device": {
				{Name: "agent", Type: "address"},
				{Name: "app", Type: "string"},
				{Name: "appKey", Type: "bytes"},
				{Name: "nonce", Type: "string"},
			},
		},
		PrimaryType: "Device",
		Domain:      domain(),
		Message: apitypes.TypedDataMessage{
			"agent":  d.Agent.Hex(),
			"app":    d.App,
			"appKey": hexutil.Encode(d.AppKey),
			"nonce":  d.Nonce,
		},
	}
}

// Hash returns the device's EIP-712 digest.
func (d Device) Hash() (ethcommon.Hash, error) {
	return typedHash(d.TypedData())
}

// Verify checks that the agent's root key signed the device.
func (s SignedDevice) Verify() error {
	if s.Signature.Certificate != nil {
		return errors.New("a device is signed by the root key")
	}
	if _, err := AddressOf(s.Device.AppKey); err != nil {
		return errors.New("device: the app key must be a public key, 65 bytes uncompressed")
	}
	h, err := s.Device.Hash()
	if err != nil {
		return err
	}
	signer, err := Verify(h, s.Signature, ForRoot)
	if err != nil {
		return err
	}
	if signer.Agent != s.Device.Agent {
		return ErrBadSignature
	}
	return nil
}

// SignDevice signs a device with the root key (the command line and tests).
func SignDevice(d Device, root KeySigner) (SignedDevice, error) {
	h, err := d.Hash()
	if err != nil {
		return SignedDevice{}, err
	}
	env, err := root.SignDigest(h)
	if err != nil {
		return SignedDevice{}, err
	}
	return SignedDevice{Device: d, Signature: env}, nil
}
