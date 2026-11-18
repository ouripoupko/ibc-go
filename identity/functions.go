package identity

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"

	"github.com/ethereum/go-ethereum/crypto"
)

// Membership functions: a root contract that defines one decides that kind
// of membership change (see DeployBody.Membership). Each takes the subject's
// address and returns bool.
const (
	ApprovePartner = "approve_partner"
	ApproveGuest   = "approve_guest"
	ApproveRemoval = "approve_removal"
)

// MembershipFunctions are the membership functions, in order.
var MembershipFunctions = []string{ApprovePartner, ApproveGuest, ApproveRemoval}

// MembershipSignature is a membership function's signature.
func MembershipSignature(name string) string { return name + "(address)" }

func checkMembership(names []string) error {
	for i, n := range names {
		if !slices.Contains(MembershipFunctions, n) {
			return fmt.Errorf("membership: %q is not a membership function (%v)", n, MembershipFunctions)
		}
		if slices.Contains(names[:i], n) {
			return fmt.Errorf("membership: %s twice", n)
		}
	}
	return nil
}

// signatureName is the start of a function signature: its name and "(".
var signatureName = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*\(`)

// ValidSignature reports whether s has the form of a function signature, as
// grants and consents name functions: "pay(address,uint256)", the name and
// the canonical types of its parameters, without spaces.
func ValidSignature(s string) bool {
	if !signatureName.MatchString(s) || s[len(s)-1] != ')' {
		return false
	}
	depth := 0
	for i := len(signatureName.FindString(s)) - 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return false
			}
		case c == ',' || c == '[' || c == ']' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return depth == 0
}

// Selector is a function signature's selector: the first 4 bytes of its
// keccak256, which start a call's data.
func Selector(signature string) []byte {
	return crypto.Keccak256([]byte(signature))[:4]
}

func checkSignatures(what string, functions []string) error {
	for _, f := range functions {
		if !ValidSignature(f) {
			return fmt.Errorf("%s: %q is not a function signature, such as pay(address,uint256)", what, f)
		}
	}
	return nil
}

// Allows reports whether the grant lets the node make a call with this
// data: whether its selector is one of the grant's functions'.
func (g Grant) Allows(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	for _, f := range g.Functions {
		if bytes.Equal(Selector(f), data[:4]) {
			return true
		}
	}
	return false
}
