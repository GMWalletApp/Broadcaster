package broadcast

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

func TestExternalNonceConsumption(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	key := must(crypto.GenerateKey())
	raw := rawEVM(key, 5, 42)
	task, v, e := decodeEVM(raw)
	if e != nil {
		t.Fatal(e)
	}
	st, _ := request(s, "POST", "/transactions", "test-token", map[string]string{"rawTransaction": raw})
	if st != 202 {
		t.Fatal(st)
	}
	other := "0x" + strings.Repeat("b", 64)
	block := "0x" + strings.Repeat("a", 64)
	p := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
		switch method {
		case "eth_getTransactionReceipt", "eth_getTransactionByHash":
			return nil, nil
		case "eth_getTransactionCount":
			if string(args[1]) == `"latest"` {
				return "0x6", nil
			}
			var tag string
			json.Unmarshal(args[1], &tag)
			n := must(hexutil.DecodeUint64(tag))
			if n >= 99 {
				return "0x6", nil
			}
			return "0x5", nil
		case "eth_getBlockByNumber":
			if string(args[1]) == "true" {
				return map[string]any{"hash": block, "number": "0x63", "transactions": []any{map[string]string{"hash": other, "from": task.Sender, "nonce": "0x5"}}}, nil
			}
			if string(args[0]) == `"0x63"` {
				return map[string]any{"hash": block, "number": "0x63", "transactions": []string{other}}, nil
			}
			return map[string]any{"hash": "0x" + strings.Repeat("c", 64), "number": "0x64", "transactions": []string{}}, nil
		}
		return nil, &rpcError{-32601, "unsupported"}
	})
	registerFixture(t, s, "1", "rpc", p.URL)
	drive(t, s)
	got := must(s.Store.lookup(t.Context(), "merchant", EVM, "1", v.Hash))
	if got.Outcome != "REPLACED_EXTERNALLY" || got.Consuming != other || got.Winner != "" {
		t.Fatal(got)
	}
	st, res := request(s, "GET", "/accounts/"+task.Sender+"/nonce?chainId=1", "test-token", nil)
	if st != 200 || res["nonce"] != "6" {
		t.Fatal(st, res)
	}
	var events int
	if e = s.Store.db.QueryRow("SELECT count(*) FROM outbox WHERE name IN ('transaction.nonce_conflict','transaction.closed')").Scan(&events); e != nil || events != 2 {
		t.Fatal(events, e)
	}
}

func TestType4SenderIncrementAndSequentialTuples(t *testing.T) {
	signer := must(crypto.GenerateKey())
	sender := crypto.PubkeyToAddress(signer.PublicKey)
	delegate := common.HexToAddress("0x123456")
	auth := func(n uint64) types.SetCodeAuthorization {
		return must(types.SignSetCode(signer, types.SetCodeAuthorization{ChainID: *uint256.NewInt(1), Nonce: n, Address: delegate}))
	}
	tx := types.NewTx(&types.SetCodeTx{ChainID: uint256.NewInt(1), Gas: 160000, GasFeeCap: uint256.NewInt(10), GasTipCap: uint256.NewInt(1), Nonce: 10, To: sender, AuthList: []types.SetCodeAuthorization{auth(10), auth(11), auth(12)}})
	tx = must(types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(1)), signer))
	task, v, e := decodeEVM(hexutil.Encode(must(tx.MarshalBinary())))
	if e != nil {
		t.Fatal(e)
	}
	applyAuth(v.Authorizations, map[string]accountState{task.Sender: {Nonce: 10}}, task.Sender, false)
	for i, a := range v.Authorizations {
		if a.Result != "UNKNOWN" || i == 0 && a.Reason != "AUTH_NONCE_MISMATCH" || i > 0 && a.Prediction != "APPLIED" {
			t.Fatal(a)
		}
	}
	applyAuth(v.Authorizations, map[string]accountState{task.Sender: {Nonce: 10}}, task.Sender, true)
	if v.Authorizations[0].Result != "SKIPPED" || v.Authorizations[1].Result != "APPLIED" || v.Authorizations[2].Result != "APPLIED" {
		t.Fatal(v.Authorizations)
	}
}

func TestWebhookRetriesSameEventWithMAC(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	var count atomic.Int32
	const secret = "a-test-webhook-secret"
	ids := make(chan string, 2)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := must(io.ReadAll(r.Body))
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if r.Header.Get("X-Broadcaster-Signature") != hex.EncodeToString(mac.Sum(nil)) {
			t.Error("incorrect webhook MAC")
		}
		ids <- r.Header.Get("Idempotency-Key")
		if count.Add(1) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer h.Close()
	c := Client{Name: "merchant", Webhook: h.URL, WebhookSecret: secret}
	st, _ := request(s, "POST", "/transactions", "test-token", map[string]string{"rawTransaction": rawEVM(must(crypto.GenerateKey()), 0, 1)})
	if st != 202 {
		t.Fatal(st)
	}
	for range 2 {
		if ok, e := s.deliverOne(t.Context(), c); e != nil || !ok {
			t.Fatal(ok, e)
		}
		if _, e := s.Store.db.Exec("UPDATE outbox SET due=0"); e != nil {
			t.Fatal(e)
		}
	}
	if a, b := <-ids, <-ids; a == "" || a != b {
		t.Fatal(a, b)
	}
	if ok, e := s.deliverOne(t.Context(), c); ok || e != nil {
		t.Fatal(ok, e)
	}
}

func TestInterruptedRoundAndLeaseRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s := service(t, path)
	task, v, e := decodeEVM(rawEVM(must(crypto.GenerateKey()), 0, 1))
	if e != nil {
		t.Fatal(e)
	}
	task.Owner = "merchant"
	_, _, e = s.Store.submit(t.Context(), task, v)
	if e != nil {
		t.Fatal(e)
	}
	registerFixture(t, s, "1", "rpc", "https://example.test")
	t1, lease, e := s.claim(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.prepare(t.Context(), t1, lease); e != nil {
		t.Fatal(e)
	}
	s2 := service(t, path)
	if _, _, e = s2.claim(t.Context()); e != notFound {
		t.Fatal(e)
	}
	if _, e = s.Store.db.Exec("UPDATE tasks SET lease_until=0"); e != nil {
		t.Fatal(e)
	}
	recovered, next, e := s2.claim(t.Context())
	if e != nil || recovered.ID != t1.ID || next == lease {
		t.Fatal(e, recovered)
	}
	if e = s.finish(t.Context(), t1, lease, update{state: "CLOSED"}); e != lostLease {
		t.Fatal("old worker wrote result", e)
	}
	var state string
	if e = s2.Store.db.QueryRow("SELECT state FROM deliveries WHERE task=?", t1.ID).Scan(&state); e != nil || state != "UNKNOWN" {
		t.Fatal(state, e)
	}
}

func TestProviderAdmissionPreservesActiveConfiguration(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	key := must(crypto.GenerateKey())
	ordinary := rawEVM(key, 1, 1)
	a := must(types.SignSetCode(key, types.SetCodeAuthorization{ChainID: *uint256.NewInt(1), Nonce: 3, Address: common.HexToAddress("0x5")}))
	tx := types.NewTx(&types.SetCodeTx{ChainID: uint256.NewInt(1), Nonce: 2, Gas: 150000, GasFeeCap: uint256.NewInt(100), GasTipCap: uint256.NewInt(2), To: crypto.PubkeyToAddress(key.PublicKey), AuthList: []types.SetCodeAuthorization{a}})
	tx = must(types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(1)), key))
	raw := hexutil.Encode(must(tx.MarshalBinary()))
	var refuse atomic.Bool
	server := rpcServer(t, func(method string, args []json.RawMessage) (any, *rpcError) {
		switch method {
		case "eth_getTransactionCount":
			return "0x1", nil
		case "eth_getBlockByNumber":
			return map[string]string{"hash": "0x" + strings.Repeat("a", 64), "number": "0x68"}, nil
		case "eth_blockNumber":
			return "0x68", nil
		case "eth_chainId":
			return "0x1", nil
		case "eth_getTransactionReceipt":
			return nil, nil
		case "eth_sendRawTransaction":
			if refuse.Load() {
				return nil, &rpcError{-32000, "denied"}
			}
			str, _ := strconv.Unquote(string(args[0]))
			_, v, e := decodeEVM(str)
			if e != nil {
				t.Error(e)
			}
			return v.Hash, nil
		}
		return nil, &rpcError{-32601, "unsupported"}
	})
	url := server.URL + "/secret123"
	in := ProviderInput{Chain: "1", URL: &url, Provider: "paid-provider", EVMProbes: []string{ordinary, raw}}
	p := must(s.configure(t.Context(), "", in))
	if p.Height != 104 {
		t.Fatal(p)
	}
	refuse.Store(true)
	bad := server.URL + "/a-new-secret"
	in.URL = &bad
	if _, e := s.configure(t.Context(), p.ID, in); e == nil {
		t.Fatal("failed check accepted")
	}
	saved := must(s.endpoint(s.Store.db.QueryRow("SELECT "+endpointCols+" FROM providers WHERE id=?", p.ID)))
	if saved.secret.URL != url || strings.Contains(string(bodyJSON(saved)), "secret123") {
		t.Fatal("credential handling")
	}
	var encrypted []byte
	if e := s.Store.db.QueryRow("SELECT secret FROM providers WHERE id=?", p.ID).Scan(&encrypted); e != nil || strings.Contains(string(encrypted), "secret123") {
		t.Fatal("protection at rest", e)
	}
}

func TestForceCannotStealActiveLease(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	task, v, e := decodeEVM(rawEVM(must(crypto.GenerateKey()), 8, 1))
	if e != nil {
		t.Fatal(e)
	}
	task.Owner = "merchant"
	_, _, e = s.Store.submit(t.Context(), task, v)
	if e != nil {
		t.Fatal(e)
	}
	_, lease, e := s.claim(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	st, _ := request(s, "POST", "/transactions/"+v.Hash+"/force", "test-token", map[string]string{"reason": "operator request"})
	if st != 202 {
		t.Fatal(st)
	}
	var until int64
	var owner sql.NullString
	if e = s.Store.db.QueryRow("SELECT lease,lease_until FROM tasks").Scan(&owner, &until); e != nil || owner.String != lease || until < time.Now().UnixMilli() {
		t.Fatal(owner, e)
	}
}
