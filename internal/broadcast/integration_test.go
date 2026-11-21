package broadcast

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	sol "github.com/gagliardetto/solana-go"
)

func must[T any](v T, e error) T {
	if e != nil {
		panic(e)
	}
	return v
}

func service(t *testing.T, path string) *Service {
	t.Helper()
	st := must(Open(t.Context(), path))
	t.Cleanup(st.Close)
	return must(New(Config{Networks: map[string]Network{"1": {Family: EVM}}, Clients: []Client{{Name: "merchant", Token: "test-token"}, {Name: "other", Token: "other-token"}}}, st, make([]byte, 32)))
}

func rawEVM(key *ecdsa.PrivateKey, n uint64, value int64) string {
	to := common.HexToAddress("0x1234")
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(1), Nonce: n, Gas: 21000, GasFeeCap: big.NewInt(100), GasTipCap: big.NewInt(1), To: &to, Value: big.NewInt(value)})
	tx = must(types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(1)), key))
	return hexutil.Encode(must(tx.MarshalBinary()))
}

func request(s *Service, method, path, token string, v any) (int, map[string]any) {
	r := httptest.NewRequest(method, path, strings.NewReader(string(bodyJSON(v))))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestConcurrentSubmitRestartAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	services := []*Service{service(t, path), service(t, path)}
	key := must(crypto.GenerateKey())
	raw := []string{rawEVM(key, 20, 1), rawEVM(key, 20, 2)}
	type answer struct {
		idx, status int
		data        map[string]any
	}
	ch := make(chan answer, 2)
	for i := range 2 {
		go func() {
			st, b := request(services[i], "POST", "/transactions", "test-token", map[string]string{"rawTransaction": raw[i]})
			ch <- answer{i, st, b}
		}()
	}
	a, b := <-ch, <-ch
	if a.status == 409 {
		a, b = b, a
	}
	if a.status != 202 || b.status != 409 || b.data["error"] != "NONCE_CONFLICT" {
		t.Fatalf("concurrent submission: %+v %+v", a, b)
	}
	s := service(t, path)
	task := a.data["taskId"]
	hash := a.data["txHash"].(string)
	if status, r := request(s, "POST", "/transactions", "test-token", map[string]string{"rawTransaction": raw[a.idx]}); status != 200 || r["taskId"] != task {
		t.Fatalf("retry %d %v", status, r)
	}
	if status, _ := request(s, "GET", "/transactions/"+hash, "other-token", nil); status != 404 {
		t.Fatalf("foreign task visible: %d", status)
	}
	if status, r := request(s, "POST", "/transactions/"+hash+"/replacement", "test-token", map[string]string{"rawTransaction": raw[b.idx]}); status != 202 || r["taskId"] != task {
		t.Fatalf("replacement %d %v", status, r)
	}
	q := must(s.Store.lookup(t.Context(), "merchant", EVM, "1", hash))
	if len(q.Versions) != 2 || q.Active == hash {
		t.Fatal(q)
	}
	if st, _ := request(s, "POST", "/transactions/"+hash+"/force", "test-token", map[string]string{"reason": "test"}); st != 409 {
		t.Fatal(st)
	}
	var mode string
	if e := s.Store.db.QueryRow("pragma journal_mode").Scan(&mode); e != nil || mode != "wal" {
		t.Fatal(mode, e)
	}
}

type rpcFunc func(string, []json.RawMessage) (any, *rpcError)

func rpcServer(t *testing.T, fn rpcFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Method string
			Params []json.RawMessage
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		res, e := fn(in.Method, in.Params)
		reply := map[string]any{"id": 1, "jsonrpc": "2.0"}
		if e != nil {
			reply["error"] = e
		} else {
			reply["result"] = res
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(server.Close)
	return server
}

func registerFixture(t *testing.T, s *Service, network, provider, url string) string {
	t.Helper()
	key := id()
	sec := s.box.Seal(nil, nil, bodyJSON(endpointSecret{URL: url}), []byte(key))
	_, e := s.Store.db.Exec("INSERT INTO providers(id,provider,network,secret,enabled,height) VALUES(?,?,?,?,1,100)", key, provider, network, sec)
	if e != nil {
		t.Fatal(e)
	}
	return key
}

func drive(t *testing.T, s *Service) {
	t.Helper()
	_, e := s.Store.db.Exec("UPDATE tasks SET due=0")
	if e != nil {
		t.Fatal(e)
	}
	task, token, e := s.claim(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if e = s.process(t.Context(), &task, token); e != nil {
		t.Fatal(e)
	}
}

func TestBroadcastThreeVendorsAndReplacedHashWins(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "tasks.db"))
	key := must(crypto.GenerateKey())
	raw := rawEVM(key, 0, 1)
	_, v, _ := decodeEVM(raw)
	block := "0x" + strings.Repeat("a", 64)
	var mined atomic.Bool
	var arrived atomic.Int32
	gate := make(chan struct{})
	var once sync.Once
	for i := range 3 {
		rpc := rpcServer(t, func(m string, p []json.RawMessage) (any, *rpcError) {
			switch m {
			case "eth_sendRawTransaction":
				if arrived.Add(1) == 3 {
					once.Do(func() { close(gate) })
				}
				select {
				case <-gate:
				case <-time.After(time.Second):
					return nil, &rpcError{-32000, "not concurrent"}
				}
				if i == 1 {
					return nil, &rpcError{-32000, "already known"}
				}
				if i == 2 {
					return nil, &rpcError{-32603, "internal error"}
				}
				return v.Hash, nil
			case "debug_traceTransaction":
				return map[string]string{"error": "out of gas"}, nil
			case "eth_getTransactionReceipt":
				if mined.Load() && string(p[0]) == fmt.Sprintf("%q", v.Hash) {
					return map[string]string{"transactionHash": v.Hash, "transactionIndex": "0x0", "blockHash": block, "blockNumber": "0x64", "status": "0x0"}, nil
				}
				return nil, nil
			case "eth_getTransactionByHash":
				return nil, nil
			case "eth_getTransactionCount":
				return "0x0", nil
			case "eth_getBlockByNumber":
				return map[string]any{"hash": block, "number": "0x64", "transactions": []string{v.Hash}}, nil
			}
			return nil, &rpcError{-32601, "unknown method"}
		})
		registerFixture(t, s, "1", fmt.Sprint(i), rpc.URL)
	}
	st, r := request(s, "POST", "/EVM/transactions", "test-token", map[string]string{"rawTransaction": raw})
	if st != 202 {
		t.Fatal(st, r)
	}
	drive(t, s)
	q := must(s.Store.lookup(t.Context(), "merchant", EVM, "1", v.Hash))
	if q.Versions[0].State != "PENDING" || len(q.Versions[0].Broadcasts) != 3 {
		t.Fatal(q)
	}
	st, r = request(s, "POST", "/transactions/"+v.Hash+"/replacement", "test-token", map[string]string{"rawTransaction": rawEVM(key, 0, 2)})
	if st != 202 {
		t.Fatal(st, r)
	}
	mined.Store(true)
	drive(t, s)
	q = must(s.Store.lookup(t.Context(), "merchant", EVM, "1", v.Hash))
	if q.Winner != v.Hash || q.Execution != "FAILED" || q.Reason != "EVM_OUT_OF_GAS" || q.Versions[1].State != "REPLACED" {
		t.Fatalf("wrong closure %+v", q)
	}
}

func TestCircuitProbeFencing(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	ctx := t.Context()
	eID := registerFixture(t, s, "1", "p", "https://example.test")
	key := must(crypto.GenerateKey())
	task, v, e := decodeEVM(rawEVM(key, 0, 1))
	if e != nil {
		t.Fatal(e)
	}
	task.Owner = "merchant"
	_, _, e = s.Store.submit(ctx, task, v)
	if e != nil {
		t.Fatal(e)
	}
	task, token, e := s.claim(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for level, want := range []time.Duration{3 * time.Minute, 6 * time.Minute} {
		for i := range 3 {
			jobs := must(s.prepare(ctx, task, token))
			if len(jobs) != 1 {
				t.Fatalf("%d/%d missing job", level, i)
			}
			if level > 0 {
				if more := must(s.prepare(ctx, task, token)); len(more) > 0 {
					t.Fatal("parallel half-open probe")
				}
			}
			if e = s.record(ctx, jobs[0], sendResult{"UNKNOWN", "timeout", true}, time.Second); e != nil {
				t.Fatal(e)
			}
		}
		p := must(s.endpoint(s.Store.db.QueryRow("SELECT "+endpointCols+" FROM providers WHERE id=?", eID)))
		if p.Cooldown == nil || time.Until(*p.Cooldown) < want-time.Second {
			t.Fatal("wrong cooldown", p)
		}
		if jobs := must(s.prepare(ctx, task, token)); len(jobs) != 0 {
			t.Fatal("used cooling provider")
		}
		if _, e = s.Store.db.Exec("UPDATE providers SET cooldown=0,healthy=true WHERE id=?", eID); e != nil {
			t.Fatal(e)
		}
	}
	jobs := must(s.prepare(ctx, task, token))
	if e = s.record(ctx, jobs[0], sendResult{state: "ALREADY_KNOWN"}, time.Millisecond); e != nil {
		t.Fatal(e)
	}
	p := must(s.endpoint(s.Store.db.QueryRow("SELECT "+endpointCols+" FROM providers WHERE id=?", eID)))
	if p.Level != 0 || p.Failures != 0 || p.Cooldown != nil {
		t.Fatal(p)
	}
}

func solRequest(keys []ed25519.PrivateKey, h sol.Hash) SolanaRequest {
	keysList := sol.PublicKeySlice{}
	for _, key := range keys {
		keysList = append(keysList, sol.PublicKeyFromBytes(key.Public().(ed25519.PublicKey)))
	}
	keysList = append(keysList, sol.SystemProgramID)
	tx := sol.Transaction{Message: sol.Message{Header: sol.MessageHeader{NumRequiredSignatures: 2, NumReadonlyUnsignedAccounts: 1}, AccountKeys: keysList, RecentBlockhash: h, Instructions: []sol.CompiledInstruction{{ProgramIDIndex: 2, Accounts: []uint16{1, 0}, Data: []byte{2, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0}}}}}
	msg := must(tx.Message.MarshalBinary())
	for _, key := range keys {
		tx.Signatures = append(tx.Signatures, sol.SignatureFromBytes(ed25519.Sign(key, msg)))
	}
	r := SolanaRequest{Cluster: "dev", Payment: "pay-9", LastValid: 150, Signed: base64.StdEncoding.EncodeToString(must(tx.MarshalBinary()))}
	r.Context.Slot = 100
	return r
}

func TestSolanaExpiryResignAndRecovery(t *testing.T) {
	ctx := t.Context()
	s := service(t, filepath.Join(t.TempDir(), "db"))
	var keys []ed25519.PrivateKey
	for range 2 {
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		keys = append(keys, key)
	}
	origin := sol.Hash{7}
	next := sol.Hash{8}
	s.cfg.Networks["dev"] = Network{Family: Solana, FeePayers: []string{sol.PublicKeyFromBytes(keys[0].Public().(ed25519.PublicKey)).String()}}
	in := solRequest(keys, origin)
	st, r := request(s, "POST", "/solana/transactions", "test-token", in)
	if st != 202 {
		t.Fatal(st, r)
	}
	sig := r["signature"].(string)
	var archived atomic.Bool
	rpc := rpcServer(t, func(m string, p []json.RawMessage) (any, *rpcError) {
		switch m {
		case "getSignatureStatuses":
			return map[string]any{"value": []any{nil}}, nil
		case "getBlockHeight":
			return 151, nil
		case "isBlockhashValid":
			return map[string]any{"value": false}, nil
		case "getSlot":
			return 180, nil
		case "getBlock":
			if !archived.Load() {
				return nil, nil
			}
			if string(p[0]) == "180" {
				return map[string]any{"blockhash": next.String(), "previousBlockhash": origin.String(), "blockHeight": 151, "parentSlot": 100, "signatures": []string{}}, nil
			}
			return map[string]any{"blockhash": origin.String(), "blockHeight": 100, "signatures": []string{}}, nil
		}
		return nil, &rpcError{-32601, "unknown"}
	})
	registerFixture(t, s, "dev", "p", rpc.URL)
	drive(t, s)
	q := must(s.Store.lookup(ctx, "merchant", Solana, "dev", sig))
	if q.State != "CHECKING_EXPIRY" {
		t.Fatal(q)
	}
	fresh := solRequest(keys, next)
	if st, _ := request(s, "POST", "/solana/transactions/"+sig+"/resign", "test-token", fresh); st != 409 {
		t.Fatal(st)
	}
	archived.Store(true)
	drive(t, s)
	q = must(s.Store.lookup(ctx, "merchant", Solana, "dev", sig))
	if q.State != "RESIGN_REQUIRED" {
		t.Fatal(q)
	}
	raw := must(base64.StdEncoding.DecodeString(fresh.Signed))
	raw[65] ^= 1
	bad := fresh
	bad.Signed = base64.StdEncoding.EncodeToString(raw)
	if st, _ := request(s, "POST", "/solana/transactions/"+sig+"/resign", "test-token", bad); st != 400 {
		t.Fatal("invalid user signature accepted")
	}
	st, r = request(s, "POST", "/solana/transactions/"+sig+"/resign", "test-token", fresh)
	if st != 202 || r["taskId"] != q.ID {
		t.Fatal(st, r)
	}
	if st, _ := request(s, "POST", "/solana/transactions/"+sig+"/resign", "test-token", fresh); st != 200 {
		t.Fatal(st)
	}
}
