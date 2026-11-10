package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	ethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/ouripoupko/ibc-go/common"
)

// Guests. A guest partner of a contract takes no part in its consensus. On
// the hosts' nodes (the full partners), a guest is in the partners
// collection with the role "guest". On the guest's own node, the contract is
// in the agent's list of contracts like any other, with the role "guest",
// the contract's deploy fields and its hosts, and no state: the node sends
// its transactions and reads to a host.

// Contract document fields of a guest's contract.
const (
	KeyRole  = "role"  // RoleGuest; a partner's contract has no role field
	KeyHosts = "hosts" // the hosts, []Host
	KeyHost  = "host"  // the host the guest joined through
)

// Host is one of a guest contract's hosts: a full partner and its node.
type Host struct {
	Agent   ethcommon.Address `json:"agent"`
	NodeKey ethcommon.Address `json:"nodeKey"`
	Node    string            `json:"node"`
}

// GuestEntry is what a guest's node knows of a contract the agent is a guest of.
type GuestEntry struct {
	Host  ethcommon.Address // the host the agent joined through
	Hosts []Host
}

// Guest is a guest of a contract, as its hosts know it.
type Guest struct {
	Agent   ethcommon.Address
	Node    string
	NodeKey ethcommon.Address
	Host    ethcommon.Address
}

// StoreGuest adds a contract to agent's list as a guest's contract: deploy
// is the fields of its deploy record's message.
func (n *Navigator) StoreGuest(agent, contract string, deploy map[string]any, e GuestEntry) error {
	registered, err := n.agents.Has(agent)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("%w %q", ErrUnknownAgent, agent)
	}
	contracts := n.contracts(agent)
	if have, err := contracts.Has(contract); err != nil || have {
		if err == nil {
			err = fmt.Errorf("agent %s already has contract %s", agent, contract)
		}
		return err
	}
	fields := map[string]any{}
	for k, v := range deploy {
		switch k {
		case common.KeyEVMAddress, common.KeyStateRoot, common.KeyBlockNumber, common.KeyDeployError,
			common.KeyLastTimestamp, common.KeyTimestamp:
			continue // a guest's node has no state
		}
		fields[k] = v
	}
	fields["id"] = contract
	fields[KeyRole] = RoleGuest
	fields[KeyHost] = e.Host.Hex()
	fields[KeyHosts] = hostsValue(e.Hosts)
	return contracts.Document(contract).Update(fields)
}

// hostsValue is hosts as a document value.
func hostsValue(hosts []Host) []any {
	out := make([]any, len(hosts))
	for i, h := range hosts {
		out[i] = map[string]any{"agent": h.Agent.Hex(), "nodeKey": h.NodeKey.Hex(), "node": h.Node}
	}
	return out
}

// Guest returns agent's entry for a contract it is a guest of; false if the
// agent does not have the contract, or is a full partner of it.
func (n *Navigator) Guest(agent, contract string) (GuestEntry, bool, error) {
	doc := n.contracts(agent).Document(contract)
	exists, err := doc.Exists()
	if err != nil || !exists {
		return GuestEntry{}, false, err
	}
	fields, err := doc.Fields()
	if err != nil {
		return GuestEntry{}, false, err
	}
	if role, _ := fields[KeyRole].(string); role != RoleGuest {
		return GuestEntry{}, false, nil
	}
	raw, err := json.Marshal(fields[KeyHosts])
	if err != nil {
		return GuestEntry{}, false, err
	}
	var e GuestEntry
	if err := json.Unmarshal(raw, &e.Hosts); err != nil {
		return GuestEntry{}, false, fmt.Errorf("contract %s: hosts: %w", contract, err)
	}
	host, _ := fields[KeyHost].(string)
	e.Host = ethcommon.HexToAddress(host)
	return e, true, nil
}

// SetHosts replaces the hosts of a guest's contract.
func (n *Navigator) SetHosts(agent, contract string, hosts []Host) error {
	if _, ok, err := n.Guest(agent, contract); err != nil || !ok {
		return err
	}
	return n.contracts(agent).Document(contract).Update(map[string]any{KeyHosts: hostsValue(hosts)})
}

// RemoveGuest removes a guest's contract from agent's list.
func (n *Navigator) RemoveGuest(agent, contract string) error {
	if _, ok, err := n.Guest(agent, contract); err != nil || !ok {
		if err == nil {
			err = errors.New("not a guest of this contract")
		}
		return err
	}
	return n.contracts(agent).Delete(contract)
}

// Guests returns the guests of agent's copy of a contract (on a host's node).
func (n *Navigator) Guests(agent, contract string) ([]Guest, error) {
	partners := n.contracts(agent).Document(contract).GetSubCollection(PartnersCollection)
	ids, err := partners.Keys()
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	var out []Guest
	for _, id := range ids {
		g, ok, err := guestOf(partners, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, g)
		}
	}
	return out, nil
}

// GuestOf returns a guest of agent's copy of a contract, by its agent id.
func (n *Navigator) GuestOf(agent, contract, guest string) (Guest, bool, error) {
	partners := n.contracts(agent).Document(contract).GetSubCollection(PartnersCollection)
	return guestOf(partners, guest)
}

func guestOf(partners common.Collection, id string) (Guest, bool, error) {
	if !ethcommon.IsHexAddress(id) {
		return Guest{}, false, nil
	}
	if have, err := partners.Has(id); err != nil || !have {
		return Guest{}, false, err
	}
	fields, err := partners.Document(id).Fields()
	if err != nil {
		return Guest{}, false, err
	}
	if role, _ := fields["role"].(string); role != RoleGuest {
		return Guest{}, false, nil
	}
	node, _ := fields["address"].(string)
	key, _ := fields["nodeKey"].(string)
	host, _ := fields["host"].(string)
	return Guest{Agent: ethcommon.HexToAddress(id), Node: node, NodeKey: ethcommon.HexToAddress(key),
		Host: ethcommon.HexToAddress(host)}, true, nil
}

// DeployRecord returns the first record of agent's contract: its deploy.
func (n *Navigator) DeployRecord(agent, contract string) (json.RawMessage, error) {
	raw, ok, err := n.ledger(agent, contract).Get(1)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w %q of agent %q", ErrUnknownContract, contract, agent)
	}
	return raw, nil
}
