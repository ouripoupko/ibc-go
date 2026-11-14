package execution

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ouripoupko/ibc-go/common"
)

// Read-only access for the dispatcher (Python dispatcher/navigator.py and
// contract_view.py). None of these change anything.

// SummaryFields are the contract document fields returned by
// get_contracts / get_contract: Python's list, plus abi and the EVM fields.
var SummaryFields = []string{
	"id", "name", "contract", "code", "protocol", "default_app", "pid", "address", "profile", KeySource, "compiler", KeyRole, KeyHosts, KeyPublic, KeySourceCheck, KeySourceCheckReason,
	common.KeyABI, common.KeyEVMAddress, common.KeyStateRoot, common.KeyBlockNumber, common.KeyDeployError,
}

// AgentFields are what a registration stores about an agent: its node's URL
// and key, and its root key, the root key's scheme and, for a passkey, its
// credential id.
var AgentFields = []string{"address", "nodeKey", "rootKey", "rootScheme", "credential"}

// AgentInfo returns what agent_info shows of a registered agent: its id, root
// key, scheme and credential id. ok is false if the agent is not registered.
func (n *Navigator) AgentInfo(agent string) (map[string]any, bool, error) {
	doc := n.agents.Document(agent)
	exists, err := doc.Exists()
	if err != nil || !exists {
		return nil, false, err
	}
	fields, err := doc.Fields()
	if err != nil {
		return nil, false, err
	}
	return map[string]any{"agent": agent, "rootKey": fields["rootKey"], "rootScheme": fields["rootScheme"],
		"credential": fields["credential"]}, true, nil
}

// AgentByCredential finds the agent whose root passkey has the credential id
// (hex), so that a passkey can sign in on a new device.
func (n *Navigator) AgentByCredential(credential string) (string, bool, error) {
	agents, err := n.agents.Keys()
	if err != nil {
		return "", false, err
	}
	for _, agent := range agents {
		fields, err := n.agents.Document(agent).Fields()
		if err != nil {
			return "", false, err
		}
		if c, _ := fields["credential"].(string); c != "" && strings.EqualFold(c, credential) {
			return agent, true, nil
		}
	}
	return "", false, nil
}

// AgentExists reports whether agent is registered on this node.
func (n *Navigator) AgentExists(agent string) (bool, error) {
	return n.agents.Has(agent)
}

// ContractExists reports whether agent has the contract on this node.
func (n *Navigator) ContractExists(agent, contract string) (bool, error) {
	return n.contracts(agent).Has(contract)
}

// Contracts returns the summary of every contract of agent.
func (n *Navigator) Contracts(agent string) ([]map[string]any, error) {
	contracts := n.contracts(agent)
	hashes, err := contracts.Keys()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(hashes))
	for _, hash := range hashes {
		summary, err := summarize(contracts.Document(hash))
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, nil
}

// Contract returns the summary of one contract, or false if agent does not have it.
func (n *Navigator) Contract(agent, contract string) (map[string]any, bool, error) {
	doc := n.contracts(agent).Document(contract)
	exists, err := doc.Exists()
	if err != nil || !exists {
		return nil, false, err
	}
	summary, err := summarize(doc)
	return summary, err == nil, err
}

func summarize(doc common.Document) (map[string]any, error) {
	fields, err := doc.Fields()
	if err != nil {
		return nil, err
	}
	out := map[string]any{KeyRole: RolePartner}
	for _, key := range SummaryFields {
		if v, ok := fields[key]; ok {
			out[key] = v
		}
	}
	return out, nil
}

// LedgerEntry is one record of a contract's ledger.
type LedgerEntry struct {
	Index  uint64
	Record json.RawMessage
}

// Ledger returns the records of agent's contract with index >= from.
func (n *Navigator) Ledger(agent, contract string, from uint64) ([]LedgerEntry, error) {
	var out []LedgerEntry
	err := n.ledger(agent, contract).Records(from, func(index uint64, record json.RawMessage) error {
		out = append(out, LedgerEntry{Index: index, Record: record})
		return nil
	})
	return out, err
}

// Read runs a read-only call (contract_read, eth_call semantics) on the
// latest applied state of agent's contract. It never deploys: a contract
// whose deploy record has not been applied yet is reported as unknown.
func (n *Navigator) Read(agent, caller string, record Record) (common.Result, error) {
	return n.read(agent, caller, record, common.GasLimit)
}

func (n *Navigator) read(agent, caller string, record Record, gas uint64) (common.Result, error) {
	doc := n.contracts(agent).Document(record.Contract)
	fields, err := doc.Fields()
	if err != nil {
		return common.Result{}, err
	}
	if fields[common.KeyEVMAddress] == nil && fields[common.KeyDeployError] == nil {
		return common.Result{}, fmt.Errorf("%w %q of agent %q", ErrUnknownContract, record.Contract, agent)
	}
	st := common.NewState(doc, doc.GetSubCollection(PartnersCollection),
		common.ContractDatabase(n.cfg.DB, agent, record.Contract))
	if err := st.Run("", ""); err != nil && !isDeterministic(err) {
		return common.Result{}, err
	}
	return st.ViewGas(caller, record.Method, record.Message.Values, record.Timestamp, gas)
}
