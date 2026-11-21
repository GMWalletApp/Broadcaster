package broadcast

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestNonceViewFillsHolesAcrossOwnersAndReconcilesOnlyLatest(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "queue.db"))
	key := must(crypto.GenerateKey())
	var sender string
	var tasks []Task
	for i, n := range []uint64{19, 20, 21, 23} {
		task, v, err := decodeEVM(rawEVM(key, n, 1))
		if err != nil {
			t.Fatal(err)
		}
		task.Owner = []string{"merchant", "other"}[i%2]
		task, _, err = s.Store.submit(t.Context(), task, v)
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
		sender = task.Sender
	}
	if _, err := s.Store.db.Exec("UPDATE tasks SET due=-1 WHERE id=?", tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	old, token, err := s.claim(t.Context())
	if err != nil || old.Nonce != "19" {
		t.Fatal(old, err)
	}
	var latest = 20
	rpc := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
		if method != "eth_getTransactionCount" {
			t.Errorf("unexpected %s", method)
		}
		if string(args[1]) == `"pending"` {
			return "0x17", nil
		}
		return "0x" + strconv.FormatInt(int64(latest), 16), nil
	})
	registerFixture(t, s, "1", "reader", rpc.URL)
	path := "/accounts/" + sender + "/nonce?chainId=1"
	check := func(suffix string, want string) {
		t.Helper()
		code, body := request(s, "GET", path+suffix, "test-token", nil)
		if code != 200 || body["nonce"] != want || body["chainNonce"] != strconv.Itoa(latest) {
			t.Fatal(code, body)
		}
	}
	check("", "22")
	check("", "22")
	check("&blockTag=pending", "24")
	if err = s.finish(t.Context(), old, token, update{state: "TRACKING"}); err != lostLease {
		t.Fatal("stale worker", err)
	}
	for i, task := range tasks {
		got := must(s.Store.lookup(t.Context(), task.Owner, EVM, "1", task.Active))
		if i == 0 && (got.State != "RESOLVING_NONCE" || got.Revision != 2) || i > 0 && got.State != "QUEUED" {
			t.Fatal(got)
		}
	}
	latest = 24
	check("", "24")
	if _, err = s.Store.db.Exec("UPDATE providers SET enabled=0"); err != nil {
		t.Fatal(err)
	}
	code, body := request(s, "GET", path, "test-token", nil)
	if code != 503 || body["error"] != "NONCE_VIEW_UNAVAILABLE" {
		t.Fatal(code, body)
	}
}

func TestNonceReadStopsAtFirstWorkingRPC(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	var reads atomic.Int32
	for _, provider := range []string{"first", "second", "third"} {
		server := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
			if method != "eth_getTransactionCount" || string(args[1]) != `"latest"` {
				t.Errorf("unexpected read %s %s", method, args)
			}
			if reads.Add(1) == 1 {
				return nil, &rpcError{Code: -32000, Message: "temporarily unavailable"}
			}
			return "0x7", nil
		})
		registerFixture(t, s, "1", provider, server.URL)
	}
	path := "/accounts/0x0000000000000000000000000000000000000001/nonce?chainId=1"
	for _, wantReads := range []int32{2, 3} {
		code, view := request(s, "GET", path, "test-token", nil)
		if code != 200 || view["nonce"] != "7" || reads.Load() != wantReads {
			t.Fatal(code, view, reads.Load())
		}
	}
}

func TestHistoricalNonceReuseIsIdempotent(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	key := must(crypto.GenerateKey())
	raw := rawEVM(key, 9, 1)
	register := func(raw, token string) (int, map[string]any) {
		return request(s, "POST", "/transactions", token, map[string]string{"rawTransaction": raw})
	}
	code, first := register(raw, "test-token")
	if code != 202 {
		t.Fatal(code, first)
	}
	_, err := s.Store.db.Exec("UPDATE tasks SET state='CLOSED',outcome='EXPIRED'")
	if err != nil {
		t.Fatal(err)
	}
	code, second := register(rawEVM(key, 9, 2), "other-token")
	if code != 202 || second["taskId"] == first["taskId"] {
		t.Fatal(code, second)
	}
	code, retry := register(raw, "test-token")
	if code != 200 || retry["taskId"] != first["taskId"] || retry["state"] != "CLOSED" {
		t.Fatal(code, retry)
	}
	if code, _ = register(rawEVM(key, 9, 3), "test-token"); code != 409 {
		t.Fatal(code)
	}
}

func TestNonceUint64Boundaries(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	ctx := t.Context()
	for _, n := range []uint64{99, 100, math.MaxInt64, math.MaxUint64 - 1} {
		task, v, err := decodeEVM(rawEVM(must(crypto.GenerateKey()), n, 1))
		if err != nil {
			t.Fatal(err)
		}
		task.Owner = "merchant"
		if _, _, err = s.Store.submit(ctx, task, v); err != nil {
			t.Fatal(err)
		}
		next, err := s.Store.nextNonce(ctx, "1", task.Sender, n, n)
		if n == math.MaxUint64-1 {
			if err == nil || !strings.Contains(err.Error(), "NONCE_EXHAUSTED") {
				t.Fatal(next, err)
			}
		} else if next != n+1 || err != nil {
			t.Fatal(next, err)
		}
	}
}

func TestMigrateHistoricalNonceIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := must(sql.Open("sqlite", path))
	old := strings.Replace(tasksDDL, "UNIQUE(owner_id,network,payment)", "UNIQUE(network,sender,nonce), UNIQUE(owner_id,network,payment)", 1)
	_, err := db.Exec(old + schema + "DROP INDEX tasks_active_nonce;")
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db}
	key := must(crypto.GenerateKey())
	task, v, err := decodeEVM(rawEVM(key, 7, 1))
	if err != nil {
		t.Fatal(err)
	}
	task.Owner = "merchant"
	task, _, err = store.submit(context.Background(), task, v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE tasks SET state='CLOSED',outcome='EXPIRED'"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s := service(t, path)
	original := must(s.Store.lookup(t.Context(), "merchant", EVM, "1", v.Hash))
	if original.ID != task.ID || original.Versions[0].Raw != v.Raw || eventCount(t, s, "transaction.submitted") != 1 {
		t.Fatal(original)
	}
	code, _ := request(s, "POST", "/transactions", "test-token", map[string]string{"rawTransaction": rawEVM(key, 7, 2)})
	if code != 202 {
		t.Fatal(code)
	}
	var enabled bool
	if err = s.Store.db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || !enabled {
		t.Fatal(enabled, err)
	}
}
