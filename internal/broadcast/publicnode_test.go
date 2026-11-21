package broadcast

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPublicNodeAdmissionAndRestart(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "db")
	s := service(t, db)
	s.cfg.Clients[0].Admin = true
	s.cfg.Networks["devnet"] = Network{Family: Solana, GenesisHash: "different"}
	s.cfg.Networks["custom-solana-name"] = Network{Family: Solana, GenesisHash: solanaMainnetGenesis}
	if got := s.defaultProviders(""); len(got) != 2 {
		t.Fatal(got)
	}
	r := httptest.NewRequest("GET", "/rpc-providers/defaults?network=1", nil)
	r.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var defaults []defaultProvider
	if err := json.Unmarshal(w.Body.Bytes(), &defaults); err != nil || w.Code != 200 || len(defaults) != 1 || defaults[0].URL != "https://ethereum-rpc.publicnode.com" || !defaults[0].MEVProtected {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	key := must(crypto.GenerateKey())
	ordinary := rawEVM(key, 0, 1)
	auth := must(types.SignSetCode(key, types.SetCodeAuthorization{ChainID: *uint256.NewInt(1), Address: common.HexToAddress("0x1"), Nonce: 2}))
	tx := types.NewTx(&types.SetCodeTx{ChainID: uint256.NewInt(1), Nonce: 1, Gas: 100000, GasFeeCap: uint256.NewInt(100), GasTipCap: uint256.NewInt(1), To: common.HexToAddress("0x2"), AuthList: []types.SetCodeAuthorization{auth}})
	tx = must(types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(1)), key))
	probes := admissionProbes{EVMProbes: []string{ordinary, hexutil.Encode(must(tx.MarshalBinary()))}}
	file := filepath.Join(dir, "probes.json")
	if err := os.WriteFile(file, bodyJSON(probes), 0600); err != nil {
		t.Fatal(err)
	}
	var reject = true
	var calls, sends int
	s.http.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != defaults[0].URL {
			t.Fatalf("unexpected route %s", r.URL)
		}
		calls++
		var q struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Fatal(err)
		}
		var result any
		reply := map[string]any{"jsonrpc": "2.0", "id": 1}
		switch q.Method {
		case "eth_chainId":
			result = "0x1"
		case "eth_blockNumber":
			result = "0x42"
		case "eth_getBlockByNumber":
			result = map[string]any{"hash": "0x" + strings.Repeat("a", 64), "number": "0x42", "transactions": []string{}}
		case "eth_getTransactionCount":
			result = "0x0"
		case "eth_getTransactionReceipt":
		case "eth_sendRawTransaction":
			var raw string
			if err := json.Unmarshal(q.Params[0], &raw); err != nil {
				t.Fatal(err)
			}
			_, v, err := decodeEVM(raw)
			if err != nil {
				t.Fatal(err)
			}
			result = v.Hash
			sends++
			if reject && raw != ordinary {
				reply["error"] = rpcError{-32000, "type 4 not supported"}
			}
		default:
			t.Fatalf("unexpected RPC %s", q.Method)
		}
		reply["result"] = result
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(bodyJSON(reply))))}, nil
	})
	if err := s.admitDefaultFile(t.Context(), "1", file); err == nil {
		t.Fatal("failed type 4 probe admitted")
	}
	if ps := must(s.endpoints(t.Context(), "1", true)); len(ps) != 0 {
		t.Fatal(ps)
	}
	reject = false
	if err := s.admitDefaultFile(t.Context(), "1", file); err != nil {
		t.Fatal(err)
	}
	if sends != 4 {
		t.Fatal(sends)
	}
	p := must(s.storedDefault(t.Context(), "1"))
	if !p.Enabled || !p.MEVProtected || p.Provider != publicNode {
		t.Fatal(p)
	}
	status, _ := request(s, "PATCH", "/rpc-providers/"+p.ID, "test-token", map[string]bool{"enabled": false})
	if status != 200 {
		t.Fatal(status)
	}
	before := calls
	status, _ = request(s, "POST", "/rpc-providers", "test-token", map[string]string{"chainId": "1", "providerId": "PublicNode"})
	if status != 201 || calls != before {
		t.Fatal(status, calls)
	}
	reopened := service(t, db)
	if err := reopened.admitDefaultFile(t.Context(), "1", file+".removed"); err != nil {
		t.Fatal(err)
	}
	if p := must(reopened.storedDefault(t.Context(), "1")); p.Enabled {
		t.Fatal("restart re-enabled default")
	}
}

func TestPreferredPublicNodeCountsOnceAndHonorsCooldown(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	task, v, _ := decodeEVM(rawEVM(must(crypto.GenerateKey()), 0, 1))
	task.Owner = "merchant"
	if _, _, err := s.Store.submit(t.Context(), task, v); err != nil {
		t.Fatal(err)
	}
	task, token, err := s.claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	alias := registerFixture(t, s, "1", "alias", "https://ethereum.publicnode.com")
	official := registerFixture(t, s, "1", "another-label", "https://ethereum-rpc.publicnode.com")
	for _, name := range []string{"paid-a", "paid-b", "paid-c"} {
		registerFixture(t, s, "1", name, "https://"+name+".example")
	}
	for range 5 {
		jobs := must(s.prepare(t.Context(), task, token))
		if len(jobs) != 3 || jobs[0].ID != official || jobs[0].Provider != publicNode || jobs[1].Provider == publicNode || jobs[2].Provider == publicNode {
			t.Fatalf("wrong selection: %+v", jobs)
		}
	}
	if _, err = s.Store.db.Exec("UPDATE providers SET cooldown=? WHERE id IN (?,?)", time.Now().Add(time.Hour).UnixMilli(), official, alias); err != nil {
		t.Fatal(err)
	}
	jobs := must(s.prepare(t.Context(), task, token))
	if len(jobs) != 3 {
		t.Fatal(jobs)
	}
	for _, j := range jobs {
		if j.Provider == publicNode {
			t.Fatal(j)
		}
	}
}
