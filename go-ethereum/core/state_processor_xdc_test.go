// Copyright 2026 The XDC Network Authors
// Regression tests for the XDC-specific state-processor fixes shipped on
// the v1.17.3 reset branch. These tests guard against silent re-introduction
// of bugs that have already been root-caused and fixed once.

package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// readFileForTest reads a source file in the core/ package directory.
func readFileForTest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestRegression802Task3_SetHeadInvalidatesCaches guards the fix in commit
// (this one) wiring BlockChain.SetHead to the consensus.CacheInvalidator
// interface. Validates the wire-up at AST level so a refactor that drops
// the hook fails this test.
func TestRegression802Task3_SetHeadInvalidatesCaches(t *testing.T) {
	const src = "blockchain.go"
	data, err := readFileForTest(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if !strings.Contains(data, "consensus.CacheInvalidator") {
		t.Errorf("blockchain.go missing reference to consensus.CacheInvalidator — refs #802 task 3."+
			" SetHead must notify the consensus engine to purge in-memory caches above the new head;"+
			" without it, post-rewind verifyHeader can consult stale snapshot data and reject canonical"+
			" blocks with ErrNotItsTurn (observed live on xdc04 after deep rollback).")
	}
	if !strings.Contains(data, "InvalidateCaches(head)") {
		t.Errorf("BlockChain.SetHead no longer calls InvalidateCaches(head) on the engine — refs #802 task 3.")
	}
}

// TestRegression798_CoinbaseOwnerThreaded protects the fix for issue #798
// (state-root divergence at Apothem block 82,219,687). Root cause: the
// Process() call site dropped the coinbaseOwner argument to
// ApplyTransactionWithEVM, so the TRC21 fee routing in state_transition.go
// always saw st.owner == zero address and credited fees to the masternode
// instead of the registered candidate-owner.
//
// The fix (commit 04bb72f08) resolves coinbaseOwner once per block via
// getCoinbaseOwner() and threads it into the per-tx ApplyTransactionWithEVM
// call. This test asserts the call site still passes coinbaseOwner so a
// future signature refactor can't silently drop the argument again.
//
// We use AST inspection rather than execution to keep the test cheap and
// independent of full chain setup. Any code change that removes
// coinbaseOwner from the call site will fail this test — exactly the
// regression we want to catch.
func TestRegression798_CoinbaseOwnerThreaded(t *testing.T) {
	const src = "state_processor.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	var (
		foundProcessFunc   bool
		foundCoinbaseOwner bool
		foundCallSite      bool
		callSitePassesArg  bool
	)

	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Process" {
			return true
		}
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			return true
		}
		// We want the method on *StateProcessor specifically.
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			return true
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "StateProcessor" {
			return true
		}
		foundProcessFunc = true

		// Walk Process body looking for:
		//   - assignment: coinbaseOwner := getCoinbaseOwner(...)
		//   - call:       ApplyTransactionWithEVM(... , coinbaseOwner)
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			if as, ok := inner.(*ast.AssignStmt); ok {
				for _, lhs := range as.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == "coinbaseOwner" {
						foundCoinbaseOwner = true
					}
				}
			}
			if call, ok := inner.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "ApplyTransactionWithEVM" {
					foundCallSite = true
					if len(call.Args) > 0 {
						lastArg := call.Args[len(call.Args)-1]
						if id, ok := lastArg.(*ast.Ident); ok && id.Name == "coinbaseOwner" {
							callSitePassesArg = true
						}
					}
				}
			}
			return true
		})
		return false
	})

	if !foundProcessFunc {
		t.Fatalf("could not find (*StateProcessor).Process in %s — file structure changed", src)
	}
	if !foundCoinbaseOwner {
		t.Errorf("(*StateProcessor).Process no longer resolves coinbaseOwner via getCoinbaseOwner()" +
			" — refs #798. The TRC21 fee routing in state_transition.go depends on st.owner being" +
			" populated; without resolving coinbaseOwner before the tx loop, every block past" +
			" TIPTRC21Fee whose masternode has a registered TRC21 owner will produce wrong stateRoot.")
	}
	if !foundCallSite {
		t.Fatalf("could not find ApplyTransactionWithEVM call inside Process — refactor broke regression test")
	}
	if !callSitePassesArg {
		t.Errorf("Process() calls ApplyTransactionWithEVM but does NOT pass coinbaseOwner as the last argument." +
			" This silently disables TRC21 fee routing — exactly the #798 bug. Restore the threading," +
			" or update this regression test if the routing mechanism has been intentionally re-designed.")
	}
}

// TestRegression798_GetCoinbaseOwnerExists guards the helper that the call
// site uses. If the helper is renamed or removed without updating the caller,
// the build would fail — but if both are simultaneously refactored away,
// silent loss of TRC21 routing is possible. This test asserts both names
// still resolve.
func TestRegression798_GetCoinbaseOwnerExists(t *testing.T) {
	const src = "state_processor.go"
	data, err := readFileForTest(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if !strings.Contains(data, "func getCoinbaseOwner(") {
		t.Errorf("getCoinbaseOwner helper missing from %s — refs #798."+
			" The Process() method depends on this helper to resolve the TRC21 owner"+
			" from the validator contract (0x88) before each block's tx loop.", src)
	}
	if !strings.Contains(data, "state.GetCandidateOwner") {
		t.Errorf("state.GetCandidateOwner reference missing from %s — refs #798."+
			" Without it, getCoinbaseOwner cannot resolve the on-chain TRC21 owner mapping.", src)
	}
}
