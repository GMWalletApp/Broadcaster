package broadcast

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	sol "github.com/gagliardetto/solana-go"
)

func TestDispatchWidthCountsIndependentProviders(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	task, v, e := decodeEVM(rawEVM(must(crypto.GenerateKey()), 0, 1))
	if e != nil {
		t.Fatal(e)
	}
	task.Owner = "merchant"
	_, _, e = s.Store.submit(t.Context(), task, v)
	if e != nil {
		t.Fatal(e)
	}
	task, token, e := s.claim(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	for i, name := range []string{"", "A", "A", "B", "C", "D"} {
		if name != "" {
			registerFixture(t, s, "1", name, "https://example.test")
		}
		jobs := must(s.prepare(t.Context(), task, token))
		want := []int{0, 1, 1, 2, 3, 3}[i]
		if len(jobs) != want {
			t.Fatalf("step %d: %d providers, want %d", i, len(jobs), want)
		}
		seen := map[string]bool{}
		for _, j := range jobs {
			if seen[j.Provider] {
				t.Fatalf("duplicated provider %s", j.Provider)
			}
			seen[j.Provider] = true
		}
	}
}

func TestSolanaSignedBytesAndConfirmedExecutionError(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	var keys []ed25519.PrivateKey
	for range 2 {
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		keys = append(keys, key)
	}
	payer := sol.PublicKeyFromBytes(keys[0].Public().(ed25519.PublicKey)).String()
	s.cfg.Networks["dev"] = Network{Family: Solana, FeePayers: []string{payer}}
	in := solRequest(keys, sol.Hash{42})
	status, r := request(s, "POST", "/solana/transactions", "test-token", in)
	if status != 202 {
		t.Fatal(status, r)
	}
	hash := r["signature"].(string)
	var included atomic.Bool
	var sends atomic.Int32
	for _, name := range []string{"A", "B", "C"} {
		rpc := rpcServer(t, func(m string, p []json.RawMessage) (any, *rpcError) {
			switch m {
			case "getSignatureStatuses":
				var st any
				if included.Load() {
					st = map[string]any{"slot": 111, "confirmationStatus": "confirmed", "err": map[string]any{"InstructionError": []any{0, "Custom"}}}
				}
				return map[string]any{"value": []any{st}}, nil
			case "isBlockhashValid":
				return map[string]any{"value": true}, nil
			case "getBlockHeight":
				return 125, nil
			case "sendTransaction":
				var bytes string
				if json.Unmarshal(p[0], &bytes) != nil || bytes != in.Signed {
					t.Error("changed signed bytes")
				}
				sends.Add(1)
				return hash, nil
			}
			return nil, &rpcError{-32601, "unsupported"}
		})
		registerFixture(t, s, "dev", name, rpc.URL)
	}
	drive(t, s)
	if sends.Load() != 3 {
		t.Fatal("expected three sends", sends.Load())
	}
	included.Store(true)
	drive(t, s)
	q := must(s.Store.lookup(t.Context(), "merchant", Solana, "dev", hash))
	if q.Execution != "FAILED" || q.Winner != hash || q.State != "CLOSED" {
		t.Fatal(q)
	}
	if status, _ := request(s, "POST", "/solana/transactions/"+hash+"/resign", "test-token", solRequest(keys, sol.Hash{55})); status != 409 {
		t.Fatal(status)
	}
}

func TestExpiredSolanaSignatureFoundInFinalizedHistory(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	v := Version{Hash: "original-signature", Blockhash: "signed-blockhash", ContextSlot: 100, LastValid: 110}
	srv := rpcServer(t, func(method string, params []json.RawMessage) (any, *rpcError) {
		switch method {
		case "getBlockHeight":
			return 111, nil
		case "isBlockhashValid":
			return map[string]bool{"value": false}, nil
		case "getSlot":
			return 141, nil
		case "getBlock":
			return map[string]any{"blockhash": "observed-blockhash", "previousBlockhash": "parent", "parentSlot": 140, "blockHeight": 111, "signatures": []string{v.Hash}}, nil
		}
		return nil, &rpcError{-32601, "unsupported"}
	})
	p := Endpoint{secret: endpointSecret{URL: srv.URL}}
	result, e := s.solExpiry(t.Context(), p, &Task{}, v)
	if e != nil || result.state != "CLOSED" || result.winner != v.Hash || result.outcome != "INCLUDED" {
		t.Fatal(result, e)
	}
}
