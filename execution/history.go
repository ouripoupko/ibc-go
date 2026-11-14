package execution

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/core/rawdb"

	"github.com/ouripoupko/ibc-go/common"
	"github.com/ouripoupko/ibc-go/identity"
)

// A contract's history, as a new node receives it from a member: the node
// checks it before it keeps it (consensus/router.go, sync).

// KeyChangePrefix + a membership change's id holds where the change left the
// contract (a changePoint).
const KeyChangePrefix = "change:"

// changePoint is the ledger index of a membership change and the state root
// after it.
type changePoint struct {
	Index     uint64 `json:"index"`
	StateRoot string `json:"stateRoot"`
}

// ChangePoint returns the ledger index and state root after the last
// membership change of member (its join, or its move to another node), as
// this node applied it.
func (n *Navigator) ChangePoint(agent, contract, member string) (uint64, string, bool, error) {
	entries, err := n.Ledger(agent, contract, 0)
	if err != nil {
		return 0, "", false, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		rec, err := DecodeRecord(entries[i].Record)
		if err != nil {
			return 0, "", false, err
		}
		if rec.Tx == nil || rec.Tx.Tx.Agent.Hex() != member ||
			(rec.Tx.Tx.Kind != identity.KindJoin && rec.Tx.Tx.Kind != identity.KindRotateKey) {
			continue
		}
		fields, err := n.contracts(agent).Document(contract).Fields()
		if err != nil {
			return 0, "", false, err
		}
		raw, err := json.Marshal(fields[KeyChangePrefix+rec.HashCode])
		if err != nil {
			return 0, "", false, err
		}
		var p changePoint
		if err := json.Unmarshal(raw, &p); err != nil || p.Index == 0 {
			return 0, "", false, nil // not applied yet
		}
		return p.Index, p.StateRoot, true, nil
	}
	return 0, "", false, nil
}

// ReplayRoot runs a contract's history, records 1 to len(records), on a
// scratch copy, as agent's node would, and returns the state root after it.
// It keeps nothing. The first record must be the deploy.
func ReplayRoot(agent, contract string, records []Record) (string, error) {
	if len(records) == 0 || records[0].Action != ActionDeployContract {
		return "", errors.New("the history does not start with the deploy")
	}
	doc := common.NewMemoryDocument(records[0].Message.Fields)
	ledger := &MemoryLedger{}
	for _, r := range records {
		if err := ledger.Log(r); err != nil {
			return "", err
		}
	}
	exec := NewContractExecution(doc, contract, agent, rawdb.NewMemoryDatabase(), ledger)
	root := ""
	if err := exec.Replay(func(out Outcome) { root = out.Result.StateRoot.Hex() }); err != nil {
		return "", fmt.Errorf("replay: %w", err)
	}
	return root, nil
}
