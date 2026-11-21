package broadcast

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	sol "github.com/gagliardetto/solana-go"
)

func eventCount(t *testing.T, s *Service, name string) int {
	t.Helper()
	var n int
	if err := s.Store.db.QueryRow("SELECT count(*) FROM outbox WHERE name=?", name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEVMForkConvergenceAndTerminalSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := service(t, path)
	key := must(crypto.GenerateKey())
	oldRaw, newRaw := rawEVM(key, 0, 1), rawEVM(key, 0, 2)
	_, old, _ := decodeEVM(oldRaw)
	_, replacement, _ := decodeEVM(newRaw)
	if status, _ := request(s, "POST", "/transactions", "test-token", map[string]string{"rawTransaction": oldRaw}); status != 202 {
		t.Fatal(status)
	}
	if status, _ := request(s, "POST", "/transactions/"+old.Hash+"/replacement", "test-token", map[string]string{"rawTransaction": newRaw}); status != 202 {
		t.Fatal(status)
	}
	forkA, forkB := "0x"+strings.Repeat("a", 64), "0x"+strings.Repeat("b", 64)
	var converged, closed, receiptReady atomic.Bool
	for i := range 2 {
		rpc := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
			winner, head := replacement.Hash, forkB
			if i == 0 && !converged.Load() || closed.Load() {
				winner, head = old.Hash, forkA
			}
			switch method {
			case "eth_getBlockByNumber":
				return map[string]any{"number": "0x15", "hash": head, "transactions": []string{winner}}, nil
			case "eth_getTransactionReceipt":
				h := must(strconv.Unquote(string(args[0])))
				if h == old.Hash && !closed.Load() {
					// Cached receipt survives fork A even after the block has been displaced.
					return map[string]string{"transactionHash": h, "blockHash": forkA, "blockNumber": "0x15", "transactionIndex": "0x0", "status": "0x1"}, nil
				}
				if h == winner && (closed.Load() || receiptReady.Load()) {
					return map[string]string{"transactionHash": h, "blockHash": head, "blockNumber": "0x15", "transactionIndex": "0x0", "status": "0x0"}, nil
				}
				return nil, nil
			case "eth_getTransactionByHash":
				h := must(strconv.Unquote(string(args[0])))
				if h == winner {
					return map[string]string{"hash": winner, "blockHash": head, "blockNumber": "0x15", "transactionIndex": "0x0"}, nil
				}
				return nil, nil
			}
			return nil, &rpcError{-32601, "unsupported"}
		})
		registerFixture(t, s, "1", strconv.Itoa(i), rpc.URL)
	}
	drive(t, s)
	q := must(s.Store.lookup(t.Context(), "merchant", EVM, "1", old.Hash))
	if q.State != "RESOLVING_NONCE" || q.Reason != "CHAIN_VIEW_CONFLICT" || q.Winner != "" || eventCount(t, s, "transaction.closed") != 0 {
		t.Fatal(q)
	}
	if status, _ := request(s, "POST", "/transactions/"+replacement.Hash+"/force", "test-token", map[string]string{"reason": "manual"}); status != 409 {
		t.Fatal(status)
	}
	converged.Store(true)
	s = service(t, path)
	drive(t, s)
	q = must(s.Store.lookup(t.Context(), "merchant", EVM, "1", old.Hash))
	if q.State != "CONFIRMING" || q.Outcome != "" || q.Winner != replacement.Hash || q.Execution != "PENDING" || eventCount(t, s, "transaction.closed") != 0 {
		t.Fatal(q)
	}
	drive(t, s)
	if eventCount(t, s, "transaction.included") != 1 || eventCount(t, s, "transaction.closed") != 0 {
		t.Fatal("premature notification")
	}
	if status, _ := request(s, "POST", "/transactions/"+replacement.Hash+"/force", "test-token", map[string]string{"reason": "manual"}); status != 409 {
		t.Fatal(status)
	}
	receiptReady.Store(true)
	s = service(t, path)
	drive(t, s)
	closed.Store(true)
	code, body := request(s, "GET", "/transactions/"+replacement.Hash, "test-token", nil)
	if code != 200 || body["winningHash"] != replacement.Hash || body["executionStatus"] != "FAILED" || body["reason"] != "EXECUTION_CAUSE_UNAVAILABLE" || body["evidence"].(map[string]any)["blockHash"] != forkB {
		t.Fatal(code, body)
	}
	if _, _, err := s.claim(t.Context()); err != notFound || eventCount(t, s, "transaction.closed") != 1 {
		t.Fatal(err)
	}
}

func TestSolanaOrphanedBlockhashAndStaleProcessed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := service(t, path)
	keys := make([]ed25519.PrivateKey, 2)
	for i := range keys {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = key
	}
	origin := sol.Hash{27}
	in := solRequest(keys, origin)
	n := Network{Family: Solana, FeePayers: []string{sol.PublicKeyFromBytes(keys[0].Public().(ed25519.PublicKey)).String()}}
	s.cfg.Networks["dev"] = n
	var expired atomic.Bool
	var processed atomic.Uint64
	var archiveGap atomic.Bool
	rpc := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
		switch method {
		case "getLatestBlockhash":
			return map[string]any{"context": map[string]any{"slot": 100}, "value": map[string]any{"blockhash": origin.String(), "lastValidBlockHeight": 150}}, nil
		case "getSignatureStatuses":
			var result any
			if slot := processed.Load(); slot > 0 {
				result = map[string]any{"slot": slot, "confirmationStatus": "processed", "err": nil}
			}
			return map[string]any{"value": []any{result}}, nil
		case "getBlockHeight":
			if expired.Load() {
				return 151, nil
			}
			return 150, nil
		case "isBlockhashValid":
			return map[string]bool{"value": !expired.Load()}, nil
		case "getSlot":
			return 180, nil
		case "getBlock":
			if !expired.Load() {
				return map[string]string{"blockhash": origin.String()}, nil
			}
			if string(args[0]) == "180" {
				return map[string]any{"blockhash": "tip", "blockHeight": 151, "parentSlot": 99, "previousBlockhash": "parent", "signatures": []string{}}, nil
			}
			if archiveGap.Load() {
				return nil, nil
			}
			return map[string]any{"blockhash": "parent", "blockHeight": 150, "signatures": []string{}}, nil
		}
		return nil, &rpcError{-32601, "unavailable"}
	})
	registerFixture(t, s, "dev", "paid", rpc.URL)
	code, r := request(s, "POST", "/solana/transactions", "test-token", in)
	if code != 202 {
		t.Fatal(code, r)
	}
	sig := r["signature"].(string)
	q := must(s.Store.lookup(t.Context(), "merchant", Solana, "dev", sig))
	if q.Versions[0].OriginSlot != 100 {
		t.Fatal(q)
	}
	processed.Store(112)
	drive(t, s)
	q = must(s.Store.lookup(t.Context(), "merchant", Solana, "dev", sig))
	if q.State != "TRACKING" || q.Reason != "OBSERVED_PROCESSED" {
		t.Fatal(q)
	}
	archiveGap.Store(true)
	expired.Store(true)
	drive(t, s)
	q = must(s.Store.lookup(t.Context(), "merchant", Solana, "dev", sig))
	if q.State != "CHECKING_EXPIRY" || eventCount(t, s, "solana.resign_required") != 0 {
		t.Fatal(q)
	}
	expired.Store(false)
	drive(t, s)
	q = must(s.Store.lookup(t.Context(), "merchant", Solana, "dev", sig))
	if q.State != "TRACKING" || q.Versions[0].State != "PENDING" || eventCount(t, s, "solana.resign_required") != 0 {
		t.Fatal(q)
	}
	expired.Store(true)
	drive(t, s)
	s = service(t, path)
	s.cfg.Networks["dev"] = n
	archiveGap.Store(false)
	processed.Store(181) // Still ahead of our latest finalized root.
	drive(t, s)
	q = must(s.Store.lookup(t.Context(), "merchant", Solana, "dev", sig))
	if q.State != "CHECKING_EXPIRY" || eventCount(t, s, "solana.resign_required") != 0 {
		t.Fatal(q)
	}
	processed.Store(112)
	drive(t, s)
	q = must(s.Store.lookup(t.Context(), "merchant", Solana, "dev", sig))
	if q.State != "RESIGN_REQUIRED" || eventCount(t, s, "solana.resign_required") != 1 {
		t.Fatal(q)
	}
}

func TestDeadlineWithNoProviderClosesOnceAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := service(t, path)
	task, v, _ := decodeEVM(rawEVM(must(crypto.GenerateKey()), 9, 1))
	task.Owner = "merchant"
	task, _, err := s.Store.submit(t.Context(), task, v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.db.Exec("UPDATE tasks SET created=? WHERE id=?", time.Now().Add(-48*time.Hour).UnixMilli(), task.ID); err != nil {
		t.Fatal(err)
	}
	drive(t, s)
	s = service(t, path)
	q := must(s.Store.lookup(t.Context(), "merchant", EVM, "1", v.Hash))
	if q.Outcome != "EXPIRED" || q.Winner != "" || q.Execution == "FAILED" || !strings.Contains(string(q.Evidence), "NO_AVAILABLE_PROVIDER") {
		t.Fatal(q)
	}
	if _, _, err = s.claim(t.Context()); err != notFound || eventCount(t, s, "transaction.closed") != 1 {
		t.Fatal(err)
	}
}

func TestNonceRollbackResumesQueueAndForceHonorsChainEvidence(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	task, v, _ := decodeEVM(rawEVM(must(crypto.GenerateKey()), 5, 1))
	task.Owner = "merchant"
	task, _, err := s.Store.submit(t.Context(), task, v)
	if err != nil {
		t.Fatal(err)
	}
	var nonce atomic.Uint64
	nonce.Store(6)
	var sends atomic.Int32
	rpc := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
		switch method {
		case "eth_getTransactionCount":
			return "0x" + strconv.FormatUint(nonce.Load(), 16), nil
		case "eth_getBlockByNumber":
			height := must(strconv.Unquote(string(args[0])))
			if height == "latest" {
				height = "0x10"
			}
			return map[string]any{"number": height, "hash": "0x" + strings.Repeat("c", 64), "transactions": []string{}}, nil
		case "eth_getTransactionReceipt", "eth_getTransactionByHash":
			return nil, nil
		case "eth_sendRawTransaction":
			sends.Add(1)
			return v.Hash, nil
		}
		return nil, &rpcError{-32601, "unsupported"}
	})
	registerFixture(t, s, "1", "paid", rpc.URL)
	drive(t, s)
	q := must(s.Store.lookup(t.Context(), "merchant", EVM, "1", v.Hash))
	if q.State != "RESOLVING_NONCE" || q.Outcome != "" || sends.Load() != 0 {
		t.Fatal(q)
	}
	nonce.Store(4)
	drive(t, s)
	q = must(s.Store.lookup(t.Context(), "merchant", EVM, "1", v.Hash))
	if q.State != "QUEUED" || q.Reason != "NONCE_GAP" || sends.Load() != 0 {
		t.Fatal(q)
	}
	nonce.Store(5)
	drive(t, s)
	q = must(s.Store.lookup(t.Context(), "merchant", EVM, "1", v.Hash))
	if sends.Load() != 1 || q.Versions[0].State != "PENDING" || eventCount(t, s, "transaction.nonce_conflict") != 0 {
		t.Fatal(q)
	}
}

func TestExpiryHintCannotAuthorizeOrBlockResignAlone(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	v := Version{Hash: "sig", Blockhash: "origin", ContextSlot: 99, LastValid: 90000000}
	task := Task{Active: v.Hash, Versions: []Version{v}}
	s.cfg.Networks["dev"] = Network{Family: Solana}
	var provideOrigin atomic.Bool
	rpc := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
		switch method {
		case "getSignatureStatuses":
			return map[string]any{"value": []any{nil}}, nil
		case "getBlockHeight":
			return 300, nil
		case "getLatestBlockhash":
			return map[string]any{"context": map[string]any{"slot": 400}, "value": map[string]any{"blockhash": "latest", "lastValidBlockHeight": 450}}, nil
		case "isBlockhashValid":
			return map[string]bool{"value": false}, nil
		case "getSlot":
			return 400, nil
		case "getBlock":
			if string(args[0]) == "400" {
				return map[string]any{"blockhash": "tip", "previousBlockhash": "origin", "parentSlot": 99, "blockHeight": 300, "signatures": []string{}}, nil
			}
			if provideOrigin.Load() {
				return map[string]any{"blockhash": "origin", "blockHeight": 100, "signatures": []string{}}, nil
			}
			return nil, nil
		}
		return nil, &rpcError{-32601, "unsupported"}
	})
	p := Endpoint{Network: "dev", secret: endpointSecret{URL: rpc.URL}}
	u, err := s.observeSolana(t.Context(), p, &task)
	if err != nil || u.state != "CHECKING_EXPIRY" {
		t.Fatal(u, err)
	}
	task.State, task.Evidence = u.state, u.evidence
	provideOrigin.Store(true)
	u, err = s.observeSolana(t.Context(), p, &task)
	if err != nil || u.state != "RESIGN_REQUIRED" {
		t.Fatal(u, err)
	}
}

func TestRecoveredInvalidCandidateCannotRejectValidSibling(t *testing.T) {
	_, v, _ := decodeEVM(rawEVM(must(crypto.GenerateKey()), 0, 1))
	bad := Version{Hash: "old", Raw: "0x01"}
	task := Task{Active: bad.Hash, Versions: []Version{bad, v}}
	u := validateScheduledEVM(&task, update{state: "TRACKING", send: true})
	if u.state != "TRACKING" || !u.bump || u.send {
		t.Fatal(u)
	}
	task.Versions = task.Versions[:1]
	u = validateScheduledEVM(&task, update{state: "TRACKING", send: true})
	if u.state != "CLOSED" || u.outcome != "REJECTED" || u.send {
		t.Fatal(u)
	}
}

func TestExternalConsumerDisplacedDuringCanonicalCheck(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	task, v, _ := decodeEVM(rawEVM(must(crypto.GenerateKey()), 0, 1))
	task.Owner = "merchant"
	_, _, err := s.Store.submit(t.Context(), task, v)
	if err != nil {
		t.Fatal(err)
	}
	oldBlock, newBlock, consumer := "0x"+strings.Repeat("1", 64), "0x"+strings.Repeat("2", 64), "0x"+strings.Repeat("3", 64)
	var switchFork atomic.Bool
	rpc := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
		switch method {
		case "eth_getTransactionReceipt", "eth_getTransactionByHash":
			return nil, nil
		case "eth_getTransactionCount":
			if string(args[1]) == `"0x0"` {
				return "0x0", nil
			}
			return "0x1", nil
		case "eth_getBlockByNumber":
			if string(args[1]) == "true" {
				return map[string]any{"number": "0x1", "hash": oldBlock, "transactions": []any{map[string]string{"hash": consumer, "from": task.Sender, "nonce": "0x0"}}}, nil
			}
			hash := oldBlock
			if switchFork.Load() && string(args[0]) == `"0x1"` {
				hash = newBlock
			}
			return map[string]any{"number": "0x1", "hash": hash, "transactions": []string{consumer}}, nil
		}
		return nil, &rpcError{-32601, "unsupported"}
	})
	registerFixture(t, s, "1", "paid", rpc.URL)
	switchFork.Store(true)
	drive(t, s)
	q := must(s.Store.lookup(t.Context(), "merchant", EVM, "1", v.Hash))
	if q.State != "RESOLVING_NONCE" || eventCount(t, s, "transaction.nonce_conflict") != 0 {
		t.Fatal(q)
	}
	switchFork.Store(false)
	drive(t, s)
	q = must(s.Store.lookup(t.Context(), "merchant", EVM, "1", v.Hash))
	if q.Outcome != "REPLACED_EXTERNALLY" || q.Consuming != consumer || eventCount(t, s, "transaction.nonce_conflict") != 1 {
		t.Fatal(q)
	}
}

func TestSolanaDivergentCommitments(t *testing.T) {
	confirmed := func(slot uint64, err string) update {
		return solIncluded("signature", &solObservation{Slot: slot, Commitment: "confirmed", Error: json.RawMessage(err)})
	}
	first := confirmed(100, "null")
	reverted := confirmed(100, `{"InstructionError":1}`)
	otherSlot := confirmed(101, "null")
	finalized := first
	finalized.finalized = true
	proof := update{state: "RESIGN_REQUIRED", rootSlot: 100}
	cases := []struct {
		a, b     update
		conflict bool
	}{
		{first, reverted, true}, {first, otherSlot, true}, {first, first, false},
		{first, proof, true}, {finalized, proof, false}, {finalized, otherSlot, false},
		{proof, update{processedSlot: 101}, true}, {proof, update{processedSlot: 100}, false},
		{proof, update{valid: true}, true},
	}
	for _, c := range cases {
		if got := solanaDisagreement(c.a, c.b); got != c.conflict {
			t.Errorf("%+v vs %+v: got %t", c.a, c.b, got)
		}
	}
}
