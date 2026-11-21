package broadcast

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestTronRejectedBroadcastNotifiesSubmitter(t *testing.T) {
	for _, tc := range []struct{ name, message, reason string }{
		{"weight", "Validate signature error: sig error", "TRON_SIGNATURE_WEIGHT_INSUFFICIENT"},
		{"permission", "Validate signature error: Permission denied", "TRON_PERMISSION_DENIED"},
		{"invalid", "invalid signature length", "TRON_SIGNATURE_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newNativeFixture(t, Tron)
			s := service(t, filepath.Join(t.TempDir(), "db"))
			s.cfg.Networks["tron"] = Network{Family: Tron}
			rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/wallet/broadcasthex" {
					writeJSON(w, 200, map[string]any{"result": false, "code": "SIGERROR", "message": hex.EncodeToString([]byte(tc.message))})
					return
				}
				fixture.serve(w, r)
			}))
			defer rpc.Close()
			registerFixture(t, s, "tron", "test-rpc", rpc.URL)
			if code, _ := request(s, "POST", "/networks/tron/transactions", "test-token", fixture.in); code != 202 {
				t.Fatal(code)
			}
			drive(t, s)
			task := must(s.Store.lookup(t.Context(), "merchant", Tron, "tron", fixture.v.Hash))
			if task.Reason != tc.reason || task.Execution != "" || task.Outcome != "" || len(task.Versions[0].Broadcasts) != 1 || task.Versions[0].Broadcasts[0].Reason != tc.reason {
				t.Fatal(task)
			}
			var payload []byte
			if err := s.Store.db.QueryRow("SELECT payload FROM outbox WHERE name='transaction.broadcast_rejected' AND owner_id='merchant'").Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var event Task
			if err := json.Unmarshal(payload, &event); err != nil || event.Reason != tc.reason || eventCount(t, s, "transaction.closed") != 0 {
				t.Fatal(string(payload), err)
			}
		})
	}
}

func TestEVMNamespacedRoutesRequireAuthentication(t *testing.T) {
	s := service(t, filepath.Join(t.TempDir(), "db"))
	for _, route := range []struct{ method, path string }{
		{"POST", "/EVM/transactions"}, {"GET", "/EVM/transactions/hash"},
		{"POST", "/EVM/transactions/hash/replacement"}, {"POST", "/EVM/transactions/hash/force"},
		{"GET", "/EVM/accounts/address/nonce"}, {"GET", "/EVM/accounts/address/queue"},
	} {
		r := httptest.NewRequest(route.method, route.path, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", route.method, route.path, w.Code)
		}
	}
}
