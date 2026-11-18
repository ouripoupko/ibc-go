package identity

import (
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func TestValidSignature(t *testing.T) {
	for _, s := range []string{"post(string)", "f()", "pay(address,uint256)", "g((uint256,address)[],bytes32)", "_x$(uint8[2][])"} {
		if !ValidSignature(s) {
			t.Errorf("%s: invalid", s)
		}
	}
	for _, s := range []string{"post", "post()x", "post(string", "post(string))", "post (string)", "2f()", "f(uint256, address)", "f(Uint256)", ""} {
		if ValidSignature(s) {
			t.Errorf("%q: valid", s)
		}
	}
	// A grant and a consent name functions by signature.
	g := Grant{Agent: ethcommon.Address{1}, NodeKey: ethcommon.Address{2}, SourceHash: ethcommon.Hash{3}, Functions: []string{"post"}}
	if err := g.check(); err == nil {
		t.Error("a grant naming a function by name only")
	}
}

func TestGrantAllows(t *testing.T) {
	g := Grant{Functions: []string{"post(string)", "like(uint256)"}}
	if hexutil.Encode(Selector("transfer(address,uint256)")) != "0xa9059cbb" {
		t.Fatal("selector of transfer(address,uint256)")
	}
	if !g.Allows(append(Selector("like(uint256)"), make([]byte, 32)...)) || !g.Allows(Selector("post(string)")) {
		t.Error("the grant does not allow its functions")
	}
	if g.Allows(Selector("transfer(address,uint256)")) || g.Allows([]byte{1, 2}) {
		t.Error("the grant allows another function")
	}
}
