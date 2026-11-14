// Command ibc runs an ibc node, and signs on a user's behalf from the
// command line (for scripts and testing; in apps, users sign with passkeys).
//
//	ibc [-addr ADDR] [-data DIR]          run a node
//	ibc key new FILE                       create a root key file; prints the agent id
//	ibc key address FILE                   print the agent id of a key file
//	ibc sign tx -key ROOT_KEY -kind KIND [-contract ID] -body JSON
//	ibc sign registration -key ROOT_KEY -node URL -node-key ADDRESS
//	ibc sign query -key FILE -action ACTION [-target AGENT] [-contract ID] [-body JSON]
//	ibc sign publish -key ROOT_KEY -contract ID [-public=false]
//	ibc source-hash FILE                    the hash of a contract's source, as a join names it
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
	"strings"
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
		case "source-hash":
			err = sourceHashCommand(os.Args[2:])
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
	solc := fs.String("solc", envOr("IBC_SOLC", "solc"), "a Solidity compiler command, solc or solcjs, used if it is the pinned version; otherwise the node downloads solc-js into its data directory and runs it with Node.js (env IBC_SOLC)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	n, err := node.New(node.Config{DataDir: *dataDir, Solc: strings.Fields(*solc)})
	if err != nil {
		return err
	}
	slog.Info("ibc node starting", "data", *dataDir, "nodeKey", n.NodeKey())
	return n.ListenAndServe(ctx, *addr)
}

func keyCommand(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ibc key new FILE | ibc key address FILE")
	}
	switch args[0] {
	case "new":
		if len(args) != 2 {
			return errors.New("usage: ibc key new FILE")
		}
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
		fmt.Println(identity.RootAddress(key).Hex())
	case "address":
		if len(args) != 2 {
			return errors.New("usage: ibc key address FILE")
		}
		_, agent, err := loadSigner(args[1])
		if err != nil {
			return err
		}
		fmt.Println(agent.Hex())
	default:
		return fmt.Errorf("unknown key command %q", args[0])
	}
	return nil
}

func signCommand(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ibc sign tx|registration|query|publish ...")
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
		signer, agent, err := loadSigner(*keyFile)
		if err != nil {
			return err
		}
		stx, err := identity.SignTx(identity.Transaction{Contract: *contract, Kind: *kind,
			Agent: agent, Nonce: identity.NewNonce(), Body: *body}, signer)
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
		signer, agent, err := loadSigner(*keyFile)
		if err != nil {
			return err
		}
		sr, err := identity.SignRegistration(identity.Registration{Agent: agent, Node: *nodeURL,
			NodeKey: ethcommon.HexToAddress(*nodeKey), Nonce: identity.NewNonce()}, signer)
		if err != nil {
			return err
		}
		if _, _, err := sr.Verify(); err != nil {
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
		signer, agent, err := loadSigner(*keyFile)
		if err != nil {
			return err
		}
		if *target == "" {
			*target = agent.Hex()
		}
		sq, err := identity.SignQuery(identity.Query{Agent: agent, Action: *action, Target: *target,
			Contract: *contract, Body: *body, Time: uint64(time.Now().Unix()), Nonce: identity.NewNonce()}, signer)
		if err != nil {
			return err
		}
		out = sq
	case "publish":
		contract := fs.String("contract", "", "the contract id")
		public := fs.Bool("public", true, "true: anyone may read your copy; false: private again")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		signer, agent, err := loadSigner(*keyFile)
		if err != nil {
			return err
		}
		sp, err := identity.SignPublish(identity.Publish{Agent: agent, Contract: *contract, Public: *public,
			Time: uint64(time.Now().UnixMilli()), Nonce: identity.NewNonce()}, signer)
		if err != nil {
			return err
		}
		if err := sp.Verify(); err != nil {
			return err
		}
		out = sp
	default:
		return fmt.Errorf("unknown sign command %q", args[0])
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

// sourceHashCommand prints the hash of a Solidity source file, byte for byte.
func sourceHashCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ibc source-hash FILE")
	}
	src, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	fmt.Println(identity.SourceHash(string(src)).Hex())
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadSigner reads a root key file (a secp256k1 key, in hex), and returns
// its signer and the agent it is.
func loadSigner(path string) (identity.KeySigner, ethcommon.Address, error) {
	if path == "" {
		return nil, ethcommon.Address{}, errors.New("-key is required")
	}
	key, err := crypto.LoadECDSA(path)
	if err != nil {
		return nil, ethcommon.Address{}, fmt.Errorf("root key %s: %w", path, err)
	}
	return identity.Secp256k1Signer{Key: key}, identity.RootAddress(key), nil
}
