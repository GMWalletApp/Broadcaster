package broadcast

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/block-vision/sui-go-sdk/pb/sui/rpc/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	tronpb "github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type nativeFixture struct {
	t      *testing.T
	family Family
	in     NativeRequest
	v      Version
	writes atomic.Int32
	mined  atomic.Bool
	fork   atomic.Bool
}

func newNativeFixture(t *testing.T, f Family) *nativeFixture {
	t.Helper()
	var in NativeRequest
	switch f {
	case Bitcoin:
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 2}, Witness: wire.TxWitness{[]byte("witness")}})
		tx.AddTxOut(wire.NewTxOut(7, []byte{0x51}))
		var b bytes.Buffer
		if err := tx.Serialize(&b); err != nil {
			t.Fatal(err)
		}
		in.Raw = hex.EncodeToString(b.Bytes())
	case Tron:
		tx := tronpb.Transaction{RawData: &tronpb.TransactionRaw{Contract: []*tronpb.Transaction_Contract{{Type: tronpb.Transaction_Contract_TransferContract}}}, Signature: [][]byte{make([]byte, 65)}}
		in.Raw = hex.EncodeToString(must(proto.Marshal(&tx)))
	case TON:
		msg := tlb.ExternalMessage{DstAddr: address.NewAddress(0, 0, make([]byte, 32)), Body: cell.BeginCell().MustStoreUInt(1, 32).EndCell()}
		in.Raw = base64.StdEncoding.EncodeToString(must(tlb.ToCell(msg)).ToBOC())
	case Aptos:
		in.Raw = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 180))
	case Sui:
		// Public TransactionData fixture from the SDK's mainnet BCS compatibility suite.
		in.Raw = "AAACACDDjjCKM8ZURlemweMUn8JW8K5xuVRqsJme7pWK0vFfMgIAYG9vHhYAAAAABwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACA3N1aQNTVUkAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAIHYmFsYW5jZQxyZWRlZW1fZnVuZHMBBwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACA3N1aQNTVUkAAQEBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAgdiYWxhbmNlCnNlbmRfZnVuZHMBBwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACA3N1aQNTVUkAAgMAAAAAAQAAdlx3fZwn4bYaVsVYJFXhEknDlDZv5cmECoTV18ZBIQMAdlx3fZwn4bYaVsVYJFXhEknDlDZv5cmECoTV18ZBIQNkAAAAAAAAAEANAwAAAAAAAgEBBQAAAAAAAAECBQAAAAAAAAAAIDWDSorBfKSPsUrI+ZwXyYdH6V3QcpSuQaRrOCJGpEmbQlNgmA=="
		in.Signatures = []string{base64.StdEncoding.EncodeToString(make([]byte, 97))}
	}
	v, err := decodeNative(f, in)
	if err != nil {
		t.Fatal(err)
	}
	if f == Sui && v.Hash != "G8BBZ82tRFogPtXSBUh7h1Q4CyvG8PYJ1wXMNKSVQWQk" {
		t.Fatal(v.Hash)
	}
	return &nativeFixture{t: t, family: f, in: in, v: v}
}

func (f *nativeFixture) serve(w http.ResponseWriter, r *http.Request) {
	var out any
	switch f.family {
	case Bitcoin:
		var rpc struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			f.t.Error(err)
			return
		}
		switch rpc.Method {
		case "getblockhash":
			if string(rpc.Params[0]) == "0" {
				out = bitcoinMainnetGenesis
			} else if f.fork.Load() {
				out = "other"
			} else {
				out = "block"
			}
		case "getblockchaininfo":
			out = map[string]any{"chain": "main", "blocks": 420}
		case "getindexinfo":
			out = map[string]any{"txindex": map[string]bool{"synced": true}}
		case "sendrawtransaction":
			if string(rpc.Params[0]) != string(bodyJSON(f.in.Raw)) {
				f.t.Error("wrong Bitcoin bytes")
			}
			f.writes.Add(1)
			out = f.v.Hash
		case "getrawtransaction":
			if !f.mined.Load() {
				writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -5, "message": "not found"}})
				return
			}
			out = map[string]any{"txid": f.v.Hash, "blockhash": "block", "confirmations": 1}
		case "getblockheader":
			out = map[string]any{"hash": "block", "height": 420, "confirmations": 1}
		default:
			f.t.Error(rpc.Method)
		}
		out = map[string]any{"jsonrpc": "2.0", "id": 1, "result": out}
	case Tron:
		switch r.URL.Path {
		case "/wallet/getblockbynum":
			out = map[string]string{"blockID": "00000000000000001ebf88508a03865c71d452e25f4d51194196a1d22b6653dc"}
		case "/wallet/getnowblock", "/walletsolidity/getblockbynum":
			out = map[string]any{"blockID": "block", "block_header": map[string]any{"raw_data": map[string]int{"number": 420}}, "transactions": []any{map[string]string{"txID": f.v.Hash}}}
		case "/wallet/broadcasthex":
			var req struct{ Transaction string }
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Transaction != f.in.Raw {
				f.t.Error("wrong TRON bytes")
			}
			f.writes.Add(1)
			out = map[string]any{"result": true, "txid": f.v.Hash}
		case "/walletsolidity/gettransactioninfobyid":
			out = map[string]any{}
			if f.mined.Load() {
				out = map[string]any{"id": f.v.Hash, "blockNumber": 420, "receipt": map[string]string{"result": "REVERT"}}
			}
		default:
			f.t.Error(r.URL.Path)
		}
	case Aptos:
		switch r.URL.Path {
		case "/v1":
			out = map[string]any{"chain_id": 1, "ledger_version": "420"}
		case "/v1/transactions":
			b, err := io.ReadAll(r.Body)
			if err != nil || base64.StdEncoding.EncodeToString(b) != f.in.Raw || r.Header.Get("Content-Type") != "application/x.aptos.signed_transaction+bcs" {
				f.t.Error("wrong Aptos payload")
			}
			f.writes.Add(1)
			out = map[string]string{"hash": f.v.Hash}
		case "/v1/transactions/by_hash/" + f.v.Hash:
			if !f.mined.Load() {
				writeJSON(w, 404, map[string]string{"error_code": "transaction_not_found"})
				return
			}
			out = map[string]any{"hash": f.v.Hash, "type": "user_transaction", "version": "420", "success": false}
		default:
			f.t.Error(r.URL.Path)
		}
	case TON:
		switch r.URL.Path {
		case "/api/v2/getMasterchainInfo":
			out = map[string]any{"ok": true, "result": map[string]any{"init": map[string]string{"root_hash": "F6OpKZKqvqeFp6CQmFomXNMfMj2EnaUSOXN+Mh+wVWk="}, "last": map[string]int{"workchain": -1, "seqno": 420}}}
		case "/api/v2/sendBoc":
			var req struct{ BOC string }
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.BOC != f.in.Raw {
				f.t.Error("wrong TON BOC")
			}
			f.writes.Add(1)
			out = map[string]bool{"ok": true}
		case "/api/v3/transactionsByMessage":
			if r.URL.Query().Get("msg_hash") != f.v.Hash {
				f.t.Error("wrong normalized message hash")
			}
			txs := []any{}
			if f.mined.Load() {
				txs = append(txs, map[string]any{"hash": "account-tx", "account": "wallet", "lt": "9", "in_msg": map[string]string{"hash_norm": base64.StdEncoding.EncodeToString(must(hex.DecodeString(f.v.Hash)))}, "block_ref": map[string]any{"workchain": 0, "shard": "8000", "seqno": 420}, "description": map[string]bool{"aborted": false}})
			}
			out = map[string]any{"transactions": txs}
		default:
			f.t.Error(r.URL.Path)
		}
	}
	writeJSON(w, 200, out)
}

type suiFixture struct {
	pb.UnimplementedLedgerServiceServer
	pb.UnimplementedTransactionExecutionServiceServer
	*nativeFixture
}

func (f *suiFixture) GetServiceInfo(context.Context, *pb.GetServiceInfoRequest) (*pb.GetServiceInfoResponse, error) {
	return &pb.GetServiceInfoResponse{ChainId: proto.String("4btiuiMPvEENsttpZC7CZ53DruC3MAgfznDbASZ7DR6S"), CheckpointHeight: proto.Uint64(420)}, nil
}
func (f *suiFixture) ExecuteTransaction(_ context.Context, r *pb.ExecuteTransactionRequest) (*pb.ExecuteTransactionResponse, error) {
	if base64.StdEncoding.EncodeToString(r.GetTransaction().GetBcs().GetValue()) != f.in.Raw || len(r.Signatures) != 1 || base64.StdEncoding.EncodeToString(r.Signatures[0].GetBcs().GetValue()) != f.in.Signatures[0] {
		f.t.Error("wrong Sui envelope")
	}
	f.writes.Add(1)
	return &pb.ExecuteTransactionResponse{Transaction: &pb.ExecutedTransaction{Digest: &f.v.Hash}}, nil
}
func (f *suiFixture) GetTransaction(_ context.Context, r *pb.GetTransactionRequest) (*pb.GetTransactionResponse, error) {
	if r.GetDigest() != f.v.Hash {
		f.t.Error("wrong digest")
	}
	if !f.mined.Load() {
		return nil, status.Error(codes.NotFound, "not indexed")
	}
	return &pb.GetTransactionResponse{Transaction: &pb.ExecutedTransaction{Digest: &f.v.Hash, Checkpoint: proto.Uint64(420), Effects: &pb.TransactionEffects{Status: &pb.ExecutionStatus{Success: proto.Bool(true)}}}}, nil
}

func (f *nativeFixture) endpoint() string {
	if f.family == Sui {
		l := must(net.Listen("tcp", "127.0.0.1:0"))
		server := grpc.NewServer()
		fixture := &suiFixture{nativeFixture: f}
		pb.RegisterLedgerServiceServer(server, fixture)
		pb.RegisterTransactionExecutionServiceServer(server, fixture)
		go server.Serve(l)
		f.t.Cleanup(server.Stop)
		return "http://" + l.Addr().String()
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	if f.family == Aptos {
		return srv.URL + "/v1"
	}
	return srv.URL
}
func TestNativeProviderToInclusion(t *testing.T) {
	for _, family := range []Family{Bitcoin, Tron, Aptos, Sui, TON} {
		t.Run(string(family), func(t *testing.T) {
			f := newNativeFixture(t, family)
			s := service(t, filepath.Join(t.TempDir(), "db"))
			network := string(family)
			s.cfg.Networks[network] = Network{Family: family}
			s.cfg.Clients[0].Admin = true
			root := "/networks/" + network + "/transactions"
			if code, _ := request(s, "POST", root, "test-token", NativeRequest{Raw: "!"}); code != 400 {
				t.Fatal(code)
			}
			url := f.endpoint()
			code, body := request(s, "POST", "/rpc-providers", "test-token", ProviderInput{Network: network, Provider: "fixture", URL: &url, NativeProbe: &f.in})
			if code != 201 {
				t.Fatal(code, body)
			}
			if f.writes.Load() != 1 {
				t.Fatal("admission did not test writes")
			}
			code, body = request(s, "POST", root, "test-token", f.in)
			if code != 202 || body["txHash"] != f.v.Hash {
				t.Fatal(code, body)
			}
			if code, _ := request(s, "POST", root, "test-token", f.in); code != 200 {
				t.Fatal(code)
			}
			drive(t, s)
			code, body = request(s, "GET", root+"/"+f.v.Hash, "test-token", nil)
			if code != 200 || body["taskState"] != "TRACKING" || f.writes.Load() != 2 {
				t.Fatal(code, body, f.writes.Load())
			}
			if code, _ := request(s, "GET", root+"/"+f.v.Hash, "other-token", nil); code != 404 {
				t.Fatal(code)
			}
			f.mined.Store(true)
			drive(t, s)
			code, body = request(s, "GET", root+"/"+f.v.Hash, "test-token", nil)
			want := "SUCCESS"
			if family == Tron || family == Aptos {
				want = "FAILED"
			}
			if code != 200 || body["taskOutcome"] != "INCLUDED" || body["winningHash"] != f.v.Hash || body["executionStatus"] != want || f.writes.Load() != 2 {
				t.Fatal(code, body)
			}
			if eventCount(t, s, "transaction.closed") != 1 {
				t.Fatal("missing close event")
			}
		})
	}
}

func TestBitcoinReorgDuringInclusion(t *testing.T) {
	f := newNativeFixture(t, Bitcoin)
	f.mined.Store(true)
	f.fork.Store(true)
	s := service(t, filepath.Join(t.TempDir(), "db"))
	p := Endpoint{Network: "bitcoin", secret: endpointSecret{URL: f.endpoint()}}
	u, e := (bitcoinAdapter{}).lookup(t.Context(), s, p, f.v.Hash)
	if e != nil || u.state != "TRACKING" || u.outcome == "INCLUDED" {
		t.Fatal(u, e)
	}
}

func TestNativeIdentityChecks(t *testing.T) {
	if supportsSetCode("999") || !supportsSetCode("1") {
		t.Fatal("unexpected Type 4 capability")
	}
	s := service(t, filepath.Join(t.TempDir(), "db"))
	s.cfg.Networks["aptos"] = Network{Family: Aptos}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "by_hash") {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"chain_id": 2, "ledger_version": "42"})
	}))
	defer srv.Close()
	p := Endpoint{Network: "aptos", secret: endpointSecret{URL: srv.URL}}
	if _, err := s.height(t.Context(), p); err == nil {
		t.Fatal("admitted testnet")
	}
	if _, err := (aptosAdapter{}).lookup(t.Context(), s, p, "0x12"); err == nil {
		t.Fatal("treated missing route as absent transaction")
	}
}

func TestEVMMainnetEnvelopesUseIsolatedQueues(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "evm.db"))
	key := must(crypto.GenerateKey())
	dest := common.HexToAddress("0x1234")
	for _, id := range []int64{1, 42161, 56, 43114, 137, 10, 8453, 999, 143, 5042, 9745, 4663} {
		chain := big.NewInt(id)
		network := strconv.FormatInt(id, 10)
		s.cfg.Networks[network] = Network{Family: EVM}
		tx := types.NewTx(&types.DynamicFeeTx{ChainID: chain, Gas: 21000, GasFeeCap: big.NewInt(100), GasTipCap: big.NewInt(1), To: &dest})
		signed := must(types.SignTx(tx, types.LatestSignerForChainID(chain), key))
		raw := hexutil.Encode(must(signed.MarshalBinary()))
		code, body := request(s, "POST", "/transactions?chainId="+network, "test-token", map[string]string{"rawTransaction": raw})
		if code != 202 || body["network"] != network || body["txHash"] != signed.Hash().Hex() {
			t.Fatal(network, code, body)
		}
	}
}
