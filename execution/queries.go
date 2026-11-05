package execution

import (
	"encoding/json"
	"fmt"

	"github.com/ouripoupko/ibc-go/common"
)

// Read-only access for the dispatcher (Python dispatcher/navigator.py and
// contract_view.py). None of these change anything.

// SummaryFields are the contract document fields returned by
// get_contracts / get_contract: Python's list, plus abi and the EVM fields.
var SummaryFields = []string{
	"id", "name", "contract", "code", "protocol", "default_app", "pid", "address", "profile",
	common.KeyABI, common.KeyEVMAddress, common.KeyStateRoot, common.KeyBlockNumber, common.KeyDeployError,
}

// AgentExists reports whether agent is registered on this node.
func (n *Navigator) AgentExists(agent string) (bool, error) {
	return n.agents.Has(agent)
}

// AgentAddress returns the node address agent registered with.
func (n *Navigator) AgentAddress(agent string) (string, error) {
	fields, err := n.agents.Document(agent).Fields()
	if err != nil {
		return "", err
	}
	address, _ := fields["address"].(string)
	return address, nil
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
	out := map[string]any{}
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
func (n *Navigator) Read(agent string, record Record) (common.Result, error) {
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
	return st.View(record.Agent, record.Method, record.Message.Values, record.Timestamp)
}
