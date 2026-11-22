package broadcast

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestGlobalWebhookDurableRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s := service(t, path)
	received := make(chan broadcastEvent, 2)
	secret := "test-notification-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(raw)
		if r.Header.Get("X-Broadcaster-Signature") != hex.EncodeToString(mac.Sum(nil)) {
			t.Error("incorrect signature")
		}
		var batch []broadcastEvent
		if err := json.Unmarshal(raw, &batch); err != nil || len(batch) != 1 {
			t.Error("invalid notification batch")
			return
		}
		received <- batch[0]
		if len(received) == 1 {
			w.Header().Set("Retry-After", "15")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	cfg := &GlobalWebhook{URL: server.URL, Secret: secret}
	s.cfg.BroadcastNotifications = cfg
	ctx := t.Context()
	_, err := s.Store.db.Exec("INSERT INTO broadcast_events VALUES('event1','broadcast.accepted','evm','8453','hash','','',0,0,NULL,NULL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.deliverGlobal(ctx); err == nil {
		t.Fatal("expected retry")
	}
	var due int64
	if err = s.Store.db.QueryRow("SELECT due FROM broadcast_events").Scan(&due); err != nil || time.Until(time.UnixMilli(due)) < 14*time.Second {
		t.Fatal(due, err)
	}
	s2 := service(t, path)
	s2.cfg.BroadcastNotifications = cfg
	if _, err = s2.Store.db.Exec("UPDATE broadcast_events SET due=0"); err != nil {
		t.Fatal(err)
	}
	if worked, err := s2.deliverGlobal(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
	first, second := <-received, <-received
	if first != second || first.ID != "event1" || first.SchemaVersion != 1 || first.TxHash != "" {
		t.Fatal(first, second)
	}
	if worked, err := s2.deliverGlobal(ctx); worked || err != nil {
		t.Fatal(worked, err)
	}
}
