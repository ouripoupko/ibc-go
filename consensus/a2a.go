package consensus

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/execution"
	"github.com/ouripoupko/ibc-go/identity"
)

// envelopeWindow is how far a node message's time may be from this node's
// clock. Node clocks may drift, so it is wider than a client query's window.
var envelopeWindow = 5 * time.Minute

// envelope is the body of an a2a_consent request: a consensus message,
// signed by the sending node's key for one agent's copy of one contract.
type envelope struct {
	Msg       json.RawMessage   `json:"msg"`
	Node      ethcommon.Address `json:"node"` // the sending node's key
	Time      uint64            `json:"time"` // unix seconds when sent
	Signature hexutil.Bytes     `json:"signature"`
}

// envelopeHash is what a node signs: the message bound to its recipient
// (agent and contract) and to the time it was sent.
func envelopeHash(agent, contract string, t uint64, msg []byte) ethcommon.Hash {
	return crypto.Keccak256Hash([]byte("ibc a2a v1\n"+agent+"\n"+contract+"\n"+strconv.FormatUint(t, 10)+"\n"), msg)
}

// open verifies an envelope sent to agent's copy of contract, and returns
// the sending node's key.
func (e envelope) open(agent, contract string) (ethcommon.Address, error) {
	if len(e.Msg) == 0 {
		return ethcommon.Address{}, errors.New("a2a_consent without a message")
	}
	if len(e.Signature) == 0 {
		return ethcommon.Address{}, errors.New("a2a_consent message without a node signature")
	}
	sent := time.Unix(int64(e.Time), 0)
	if d := time.Since(sent); d > envelopeWindow || d < -envelopeWindow {
		return ethcommon.Address{}, errors.New("a2a_consent message expired, or the clocks differ")
	}
	signer, err := identity.Recover(envelopeHash(agent, contract, e.Time, e.Msg), e.Signature)
	if err != nil {
		return ethcommon.Address{}, fmt.Errorf("a2a_consent signature: %w", err)
	}
	if signer != e.Node {
		return ethcommon.Address{}, errors.New("a2a_consent signature does not match the node")
	}
	return signer, nil
}

// signingSender signs every a2a_consent request with the node's key when it
// sends it, not when it is queued, so that the outbox's retries carry a
// fresh time.
type signingSender struct {
	inner common.Sender
	key   *ecdsa.PrivateKey
}

func (s *signingSender) Send(ctx context.Context, req common.Request) ([]byte, error) {
	if req.Params["action"] != execution.ActionConsent {
		return s.inner.Send(ctx, req)
	}
	agent, contract, err := recipient(req.URL)
	if err != nil {
		return nil, err
	}
	var msg any
	if m, ok := req.JSON.(map[string]any); ok {
		msg = m["msg"]
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	t := uint64(time.Now().Unix())
	sig, err := identity.SignHash(envelopeHash(agent, contract, t, raw), s.key)
	if err != nil {
		return nil, err
	}
	req.JSON = envelope{Msg: raw, Node: identity.Address(s.key), Time: t, Signature: sig}
	return s.inner.Send(ctx, req)
}

// recipient returns the agent and contract of an /ibc/app/<agent>/<contract> URL.
func recipient(rawURL string) (string, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}
	_, rest, ok := strings.Cut(u.Path, "/ibc/app/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) < 2 {
		return "", "", fmt.Errorf("a2a_consent to %s: no agent and contract", rawURL)
	}
	agent := parts[0]
	if ethcommon.IsHexAddress(agent) {
		agent = ethcommon.HexToAddress(agent).Hex()
	}
	return agent, parts[1], nil
}
