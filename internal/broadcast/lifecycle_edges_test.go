package broadcast

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	tronpb "github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	sol "github.com/gagliardetto/solana-go"
	"google.golang.org/protobuf/proto"
)

func TestConfirmedSolanaForkCanResumeExpiryScanAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := service(t, path)
	keys := make([]ed25519.PrivateKey, 2)
	for i := range keys {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = k
	}
	net := Network{Family: Solana, FeePayers: []string{sol.PublicKeyFromBytes(keys[0].Public().(ed25519.PublicKey)).String()}}
	s.cfg.Networks["dev"] = net
	origin := sol.Hash{27}
	in := solRequest(keys, origin)
	var missing atomic.Bool
	rpc := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
		switch method {
		case "getSignatureStatuses":
			var st any
			if !missing.Load() {
				st = map[string]any{"slot": 140, "confirmationStatus": "confirmed"}
			}
			return map[string]any{"value": []any{st}}, nil
		case "getTransaction":
			return nil, nil
		case "getLatestBlockhash":
			return map[string]any{"context": map[string]int{"slot": 100}, "value": map[string]any{"blockhash": origin.String(), "lastValidBlockHeight": 150}}, nil
		case "getBlockHeight":
			return 181, nil
		case "isBlockhashValid":
			return map[string]any{"value": false}, nil
		case "getSlot":
			return 180, nil
		case "getBlock":
			var slot int
			if err := json.Unmarshal(args[0], &slot); err != nil {
				t.Fatal(err)
			}
			hash := func(n int) string {
				if n == 100 {
					return origin.String()
				}
				return fmt.Sprint(n)
			}
			return map[string]any{"blockhash": hash(slot), "previousBlockhash": hash(slot - 1), "parentSlot": slot - 1, "blockHeight": slot, "signatures": []string{}}, nil
		}
		t.Errorf("unexpected %s", method)
		return nil, &rpcError{-32601, "unsupported"}
	})
	registerFixture(t, s, "dev", "rpc", rpc.URL)
	code, reply := request(s, "POST", "/solana/transactions", "test-token", in)
	if code != 202 {
		t.Fatal(code, reply)
	}
	sig := reply["signature"].(string)
	drive(t, s)
	missing.Store(true)
	drive(t, s)
	view := must(s.Store.lookup(t.Context(), "merchant", Solana, "dev", sig))
	if view.State != "CONFIRMING" || !bytes.Contains(view.Evidence, []byte("historyCursor")) {
		t.Fatal(view)
	}
	s = service(t, path)
	s.cfg.Networks["dev"] = net
	for range 10 {
		drive(t, s)
		view = must(s.Store.lookup(t.Context(), "merchant", Solana, "dev", sig))
		if view.State == "CLOSED" {
			break
		}
	}
	if view.State != "CLOSED" || view.Outcome != "EXPIRED" || view.Reason != "BLOCKHASH_EXPIRED" || eventCount(t, s, "solana.resign_required") != 0 || eventCount(t, s, "transaction.closed") != 1 {
		t.Fatal(view)
	}
}

func TestTronWeightCorrectionReusesTask(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	f := newNativeFixture(t, Tron)
	s.cfg.Networks["tron"] = Network{Family: Tron}
	url := "/networks/tron/transactions"
	code, first := request(s, "POST", url, "test-token", f.in)
	if code != 202 {
		t.Fatal(first)
	}
	var tx tronpb.Transaction
	if err := proto.Unmarshal(must(hex.DecodeString(f.in.Raw)), &tx); err != nil {
		t.Fatal(err)
	}
	tx.Signature = append(tx.Signature, bytes.Repeat([]byte{1}, 65))
	signed := NativeRequest{Raw: hex.EncodeToString(must(proto.Marshal(&tx)))}
	if code, _ = request(s, "POST", url, "test-token", signed); code != 409 {
		t.Fatal(code)
	}
	if _, err := s.Store.db.Exec("UPDATE tasks SET state='TRACKING',reason='TRON_SIGNATURE_WEIGHT_INSUFFICIENT' WHERE id=?", first["taskId"]); err != nil {
		t.Fatal(err)
	}
	code, updated := request(s, "POST", url, "test-token", signed)
	if code != 202 || updated["txHash"] != first["txHash"] || updated["taskId"] != first["taskId"] {
		t.Fatal(code, updated)
	}
	for _, body := range []NativeRequest{signed, f.in} {
		if code, _ = request(s, "POST", url, "test-token", body); code != 200 {
			t.Fatal(code)
		}
	}
	stored := must(s.Store.lookup(t.Context(), "merchant", Tron, "tron", f.v.Hash))
	if len(stored.Versions) != 1 || stored.Versions[0].Raw != signed.Raw {
		t.Fatal(stored)
	}
}

func TestCallbackClientsProgressIndependently(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	slowStarted, release, fastDone := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(slowStarted); <-release; w.WriteHeader(204) }))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-slowStarted
		fastDone <- struct{}{}
		w.WriteHeader(204)
	}))
	defer fast.Close()
	s.cfg.Clients = []Client{{Name: "slow", Webhook: slow.URL}, {Name: "fast", Webhook: fast.URL}}
	for i, c := range s.cfg.Clients {
		task, v, err := decodeEVM(rawEVM(must(crypto.GenerateKey()), uint64(i), 1))
		if err != nil {
			t.Fatal(err)
		}
		task.Owner = c.Name
		if _, _, err = s.Store.submit(t.Context(), task, v); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { close(release); cancel(); <-done }()
	select {
	case <-fastDone:
	case <-time.After(3 * time.Second):
		t.Fatal("slow receiver blocked healthy receiver")
	}
}

func TestRetryHintsAndAcknowledgements(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for hint, want := range map[string]time.Duration{"20": 20 * time.Second, "invalid": 2 * time.Second, "0": 2 * time.Second, "9999999999999": 24 * time.Hour, now.Add(time.Minute).Format(http.TimeFormat): time.Minute} {
		if got := webhookRetryDelay(1, hint, now); got != want {
			t.Errorf("%s: %s", hint, got)
		}
	}
	for msg, want := range map[string]string{"Already known transaction": "ALREADY_KNOWN", "AlreadyKnown": "ALREADY_KNOWN", "Transaction simulation failed: This transaction has already been processed": "ALREADY_KNOWN", "execution reverted: already processed": "REJECTED", "nonce too low": "REJECTED"} {
		if got := classify(&rpcError{-32000, msg}); got.state != want {
			t.Errorf("%s: %v", msg, got)
		}
	}
}

func TestScheduledFeeCapUsesObservedBaseFee(t *testing.T) {
	service := service(t, filepath.Join(t.TempDir(), "db"))
	task, candidate, err := decodeEVM(rawEVM(must(crypto.GenerateKey()), 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	task.Active, task.Versions = candidate.Hash, []Version{candidate}
	task.Created = time.Now().Add(-3 * time.Minute)
	for _, tc := range []struct {
		baseFee         string
		force, wantBump bool
	}{
		{"0x64", false, false}, // The signed fee cap equals the block's base fee.
		{"0x65", false, true},
		{"0x65", true, false},
		{"", false, false}, // A pre-London block has no base fee.
	} {
		node := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
			switch method {
			case "eth_getTransactionReceipt", "eth_getTransactionByHash":
				return nil, nil
			case "eth_getTransactionCount":
				return "0x0", nil
			case "eth_getBlockByNumber":
				header := map[string]string{"number": "0x1", "hash": fmt.Sprintf("0x%064x", 1)}
				if tc.baseFee != "" {
					header["baseFeePerGas"] = tc.baseFee
				}
				return header, nil
			default:
				t.Errorf("unexpected RPC method %s", method)
				return nil, &rpcError{-32601, "unknown method"}
			}
		})
		task.Force = tc.force
		got, err := service.observeEVM(t.Context(), Endpoint{secret: endpointSecret{URL: node.URL}}, &task)
		if err != nil || got.bump != tc.wantBump || got.send == tc.wantBump {
			t.Fatalf("base fee %q, force %t: update %+v, err %v", tc.baseFee, tc.force, got, err)
		}
	}
}

func TestLegacySolanaExpiryClosesOnceOnUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expiry.db")
	s := service(t, path)
	ctx := t.Context()
	_, err := s.Store.db.ExecContext(ctx, `
		INSERT INTO tasks(id,owner_id,family,network,sender,payment,state,active,reason,evidence)
		VALUES('task','merchant','solana','dev','payer','order','RESIGN_REQUIRED','sig','BLOCKHASH_EXPIRED','{"signature":"sig"}');
		INSERT INTO versions(network,hash,task,raw,state,blockhash,intent,context_slot,last_valid,auth)
		VALUES('dev','sig','task','raw','RESIGN_REQUIRED','blockhash','intent',100,150,'[]');
		INSERT INTO outbox(id,owner_id,task,name,revision,payload)
		VALUES('old','merchant','task','solana.resign_required',1,'{}');
		PRAGMA user_version=1;`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reopened := service(t, path)
		var payload []byte
		err := reopened.Store.db.QueryRow("SELECT payload FROM outbox WHERE name='transaction.closed' AND sent IS NULL").Scan(&payload)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot Task
		if err := json.Unmarshal(payload, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.State != "CLOSED" || snapshot.Outcome != "EXPIRED" || snapshot.Reason != "BLOCKHASH_EXPIRED" ||
			snapshot.Revision != 2 || snapshot.Execution != "" || !snapshot.Next.IsZero() ||
			len(snapshot.Versions) != 1 || snapshot.Versions[0].State != "EXPIRED" ||
			eventCount(t, reopened, "solana.resign_required") != 0 || eventCount(t, reopened, "transaction.closed") != 1 {
			t.Fatalf("unexpected migration result: %s", payload)
		}
		if _, _, err := reopened.claim(ctx); err != notFound {
			t.Fatalf("expired task was schedulable: %v", err)
		}
	}
}
