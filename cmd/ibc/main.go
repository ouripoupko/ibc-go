// Command ibc runs an ibc node, and signs on a user's behalf from the
// command line (for scripts and testing; in apps, users sign in MetaMask).
//
//	ibc [-addr ADDR] [-data DIR]          run a node
//	ibc key new FILE                       create a root key file; prints its address
//	ibc key address FILE                   print a key file's address
//	ibc sign tx -key FILE -kind KIND [-contract ID] -body JSON
//	ibc sign registration -key FILE -node URL -node-key ADDRESS
//	ibc sign query -key FILE -action ACTION [-target AGENT] [-contract ID] [-body JSON]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ouripoupko/ibc-go/identity"
	"github.com/ouripoupko/ibc-go/node"
)

func main() {
	var err error
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "key":
			err = keyCommand(os.Args[2:])
		case "sign":
			err = signCommand(os.Args[2:])
		default:
			err = runCommand(os.Args[1:])
		}
	} else {
		err = runCommand(nil)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ibc:", err)
		os.Exit(1)
	}
}

func runCommand(args []string) error {
	fs := flag.NewFlagSet("ibc", flag.ExitOnError)
	addr := fs.String("addr", envOr("IBC_ADDR", ":2611"), "address to serve the API on (env IBC_ADDR)")
	dataDir := fs.String("data", envOr("IBC_DATA", "data"), "directory of the node's database and key (env IBC_DATA)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	n, err := node.New(node.Config{DataDir: *dataDir})
	if err != nil {
		return err
	}
	slog.Info("ibc node starting", "data", *dataDir, "nodeKey", n.NodeKey())
	return n.ListenAndServe(ctx, *addr)
}

func keyCommand(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: ibc key new FILE | ibc key address FILE")
	}
	switch args[0] {
	case "new":
		if _, err := os.Stat(args[1]); err == nil {
			return fmt.Errorf("%s exists; not overwriting a key", args[1])
		}
		key, err := crypto.GenerateKey()
		if err != nil {
			return err
		}
		if err := crypto.SaveECDSA(args[1], key); err != nil {
			return err
		}
		fmt.Println(identity.Address(key).Hex())
	case "address":
		key, err := crypto.LoadECDSA(args[1])
		if err != nil {
			return err
		}
		fmt.Println(identity.Address(key).Hex())
	default:
		return fmt.Errorf("unknown key command %q", args[0])
	}
	return nil
}

func signCommand(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ibc sign tx|registration|query ...")
	}
	fs := flag.NewFlagSet("ibc sign "+args[0], flag.ExitOnError)
	keyFile := fs.String("key", "", "the root key file")
	var out any
	switch args[0] {
	case "tx":
		kind := fs.String("kind", "", "deploy, call, join, leave or rotate_key")
		contract := fs.String("contract", "", "the contract id (empty for a deploy)")
		body := fs.String("body", "{}", "the transaction body, JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		key, err := crypto.LoadECDSA(*keyFile)
		if err != nil {
			return err
		}
		stx, err := identity.Sign(identity.Transaction{Contract: *contract, Kind: *kind,
			Agent: identity.Address(key), Nonce: identity.NewNonce(), Body: *body}, key)
		if err != nil {
			return err
		}
		if _, err := stx.Verify(); err != nil {
			return err
		}
		out = stx
	case "registration":
		nodeURL := fs.String("node", "", "the node's URL")
		nodeKey := fs.String("node-key", "", "the address of the node's key (from node_info)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if !ethcommon.IsHexAddress(*nodeKey) {
			return errors.New("-node-key must be an address")
		}
		key, err := crypto.LoadECDSA(*keyFile)
		if err != nil {
			return err
		}
		sr, err := identity.SignRegistration(identity.Registration{Agent: identity.Address(key), Node: *nodeURL,
			NodeKey: ethcommon.HexToAddress(*nodeKey), Nonce: identity.NewNonce()}, key)
		if err != nil {
			return err
		}
		out = sr
	case "query":
		action := fs.String("action", "", "contract_read, get_contract, get_contracts or stream")
		target := fs.String("target", "", "the agent whose copy is asked (default: yourself)")
		contract := fs.String("contract", "", "the contract id, if any")
		body := fs.String("body", "", "the query body, JSON (contract_read: {method, values})")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		key, err := crypto.LoadECDSA(*keyFile)
		if err != nil {
			return err
		}
		if *target == "" {
			*target = identity.Address(key).Hex()
		}
		sq, err := identity.SignQuery(identity.Query{Agent: identity.Address(key), Action: *action, Target: *target,
			Contract: *contract, Body: *body, Time: uint64(time.Now().Unix()), Nonce: identity.NewNonce()}, key)
		if err != nil {
			return err
		}
		out = sq
	default:
		return fmt.Errorf("unknown sign command %q", args[0])
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
