package dispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/vm"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/execution"
)

// Calls on the node's API: consensus executes calldata, and the node, for its
// users and their apps, encodes calls given by function name and JSON values,
// and decodes what they return, with the ABIs the deploy carries
// (common.ABISet).

// callRequest is a call as the API takes it, in a read, a silent write or a
// public read: a function by name ("pay") or signature ("pay(address,uint256)")
// with its values, or raw calldata; on the contract at To in the agreement's
// world (empty: the root contract).
type callRequest struct {
	To     string         `json:"to,omitempty"`
	Data   string         `json:"data,omitempty"`
	Method string         `json:"method,omitempty"`
	Values map[string]any `json:"values,omitempty"`
}

// call is a resolved call: its target, its calldata and, if the source has
// it, the function it calls.
type call struct {
	to   *ethcommon.Address
	data []byte
	fn   *common.Function
	abis *common.ABISet
}

// abiSets caches each contract's ABISet: a contract's ABIs never change.
type abiSets struct {
	mu   sync.Mutex
	sets map[string]*common.ABISet // by contract id
}

// abiSet returns the ABIs of agent's contract.
func (d *Dispatcher) abiSet(agent, contract string) (*common.ABISet, error) {
	d.abis.mu.Lock()
	set, ok := d.abis.sets[contract]
	d.abis.mu.Unlock()
	if ok {
		return set, nil
	}
	summary, ok, err := d.navigator.Contract(agent, contract)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w %q", execution.ErrUnknownContract, contract)
	}
	raw, _ := summary[common.KeyABIs].(map[string]any)
	abis := make(map[string]json.RawMessage, len(raw))
	for name, a := range raw {
		b, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		abis[name] = b
	}
	if set, err = common.NewABISet(abis); err != nil {
		return nil, fmt.Errorf("contract %s: %w", contract, err)
	}
	d.abis.mu.Lock()
	if d.abis.sets == nil {
		d.abis.sets = map[string]*common.ABISet{}
	}
	d.abis.sets[contract] = set
	d.abis.mu.Unlock()
	return set, nil
}

// resolve turns a call request on agent's contract into calldata.
func (d *Dispatcher) resolve(agent, contract string, req callRequest) (call, error) {
	var c call
	if req.To != "" {
		if !ethcommon.IsHexAddress(req.To) {
			return c, fmt.Errorf("invalid to %q: a contract address", req.To)
		}
		to := ethcommon.HexToAddress(req.To)
		c.to = &to
	}
	set, err := d.abiSet(agent, contract)
	if err != nil {
		return c, err
	}
	c.abis = set
	if req.Data != "" {
		if req.Method != "" {
			return c, errors.New("a call names a method with values, or gives data: not both")
		}
		if c.data, err = hexutil.Decode(req.Data); err != nil || len(c.data) < 4 {
			return c, errors.New("data must be calldata, hex: a function's selector and its arguments")
		}
		c.fn = set.BySelector(c.data)
		return c, nil
	}
	if req.Method == "" {
		return c, errors.New("a call needs a method (with values) or data")
	}
	if c.fn, err = set.Function(req.Method); err != nil {
		return c, err
	}
	values := req.Values
	if values == nil {
		values = map[string]any{}
	}
	c.data, err = c.fn.Encode(values)
	return c, err
}

// decode turns what a call returned (hex) into its value: decoded by the
// function's outputs if the source has the function, or else the hex.
func (c call) decode(value any) any {
	ret, ok := value.(string)
	if !ok || c.fn == nil {
		return value
	}
	raw, err := hexutil.Decode(ret)
	if err != nil {
		return value
	}
	v, err := c.fn.Decode(raw)
	if err != nil {
		return value
	}
	return v
}

// errorText says why a call failed, decoding a custom error of the source.
func errorText(set *common.ABISet, err error) string {
	var ce *common.ContractError
	if !errors.As(err, &ce) || len(ce.Data) == 0 {
		return err.Error()
	}
	return ce.Err.Error() + ": " + set.DecodeRevert(ce.Data)
}

// decodeReply decodes, in place, the outcome of a call on the event stream:
// its return value, or its custom error.
func (d *Dispatcher) decodeReply(agent string, reply *execution.Reply) {
	if reply.Contract == nil || reply.Selector == "" {
		return
	}
	set, err := d.abiSet(agent, *reply.Contract)
	if err != nil {
		return
	}
	if reply.Error != "" {
		if revert, err := hexutil.Decode(reply.Revert); err == nil && len(revert) > 0 {
			reply.Error = vm.ErrExecutionReverted.Error() + ": " + set.DecodeRevert(revert)
			reply.Reply = reply.Error
		}
		return
	}
	selector, err := hexutil.Decode(reply.Selector)
	if err != nil {
		return
	}
	reply.Reply = call{fn: set.BySelector(selector)}.decode(reply.Reply)
}
