package execution

import (
	"errors"
	"strconv"
	"sync"

	ethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/ouripoupko/ibc-go/common"
)

// Public contracts: an agent may let anyone read its copy of a contract
// (public_read), with unsigned reads of its view and pure functions, which
// run with msg.sender the zero address. It is the agent's choice for its own
// copy, kept in its contract document, which partners never see.

// Contract document fields of a public contract.
const (
	KeyPublic     = "public"      // true: anyone may read this copy
	KeyPublicTime = "public_time" // the time of the agent's last choice, unix milliseconds (decimal text)
)

// The deploy's source, and the joining node's check of it.
const (
	KeySource = "source"
	// KeySourceCheck is this node's check of the deploy's source against
	// its code (SourceVerified, SourceUnchecked), made when the agent joined;
	// KeySourceCheckReason says why a check could not be made.
	KeySourceCheck       = "source_check"
	KeySourceCheckReason = "source_check_reason"
)

// The results of checking a deploy's source against its code.
const (
	SourceVerified  = "verified"  // the source compiles to the code
	SourceUnchecked = "unchecked" // the node could not compile it (KeySourceCheckReason says why)
	SourceMismatch  = "mismatch"  // the source does not compile to the code: the node did not join
)

// SetSourceCheck records the node's check of the contract's source.
func (n *Navigator) SetSourceCheck(agent, contract, check, reason string) error {
	fields := map[string]any{KeySourceCheck: check}
	if reason != "" {
		fields[KeySourceCheckReason] = reason
	}
	return n.contracts(agent).Document(contract).Update(fields)
}

// ErrStale means a publish is not newer than the agent's last choice.
var ErrStale = errors.New("an older choice than the one this node has")

var publicMu sync.Mutex // orders SetPublic's check and write

// SetPublic records the agent's choice for its copy of a contract, if its
// time is after the last choice's. A guest's contract has no copy to show.
func (n *Navigator) SetPublic(agent, contract string, public bool, time uint64) error {
	publicMu.Lock()
	defer publicMu.Unlock()
	doc := n.contracts(agent).Document(contract)
	exists, err := doc.Exists()
	if err != nil {
		return err
	}
	if !exists {
		return ErrUnknownContract
	}
	fields, err := doc.Fields()
	if err != nil {
		return err
	}
	if role, _ := fields[KeyRole].(string); role == RoleGuest {
		return errors.New("a guest has no copy of the contract to make public")
	}
	if last, _ := fields[KeyPublicTime].(string); last != "" {
		if t, err := strconv.ParseUint(last, 10, 64); err == nil && time <= t {
			return ErrStale
		}
	}
	return doc.Update(map[string]any{KeyPublic: public, KeyPublicTime: strconv.FormatUint(time, 10)})
}

// IsPublic reports whether the agent lets anyone read its copy of the contract.
func (n *Navigator) IsPublic(agent, contract string) (bool, error) {
	doc := n.contracts(agent).Document(contract)
	exists, err := doc.Exists()
	if err != nil || !exists {
		return false, err
	}
	fields, err := doc.Fields()
	if err != nil {
		return false, err
	}
	if role, _ := fields[KeyRole].(string); role == RoleGuest {
		return false, nil
	}
	public, _ := fields[KeyPublic].(bool)
	return public, nil
}

// PublicRead runs an anonymous read of the agent's copy of a contract: as
// Read, with msg.sender the zero address and a smaller gas limit. The caller
// checks IsPublic first.
func (n *Navigator) PublicRead(agent string, record Record) (common.Result, error) {
	return n.read(agent, ethcommon.Address{}.Hex(), record, common.PublicGasLimit)
}
