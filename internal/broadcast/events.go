package broadcast

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

type notification struct {
	ID      string          `json:"eventId"`
	Name    string          `json:"event"`
	Version int64           `json:"version"`
	Payload json.RawMessage `json:"data"`
}

func (s *Service) deliverOne(ctx context.Context, c Client) (bool, error) {
	var n notification
	var attempts int
	token := id()
	e := s.Store.atomic(ctx, func(tx *sql.Tx) error {
		now := time.Now()
		e := tx.QueryRowContext(ctx, "SELECT id,name,revision,payload,attempts FROM outbox WHERE owner_id=? AND sent IS NULL AND due<=? ORDER BY due,rowid LIMIT 1", c.Name, now.UnixMilli()).Scan(&n.ID, &n.Name, &n.Version, &n.Payload, &attempts)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE outbox SET token=?,due=?,attempts=attempts+1 WHERE id=?", token, now.Add(time.Minute).UnixMilli(), n.ID)
		return e
	})
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	body := bodyJSON(n)
	status, retryAfter, err := s.postSigned(ctx, c.Webhook, c.WebhookSecret, n.ID, body)
	ok := err == nil && status >= 200 && status < 300
	if ok {
		_, e = s.Store.db.ExecContext(ctx, "UPDATE outbox SET sent=?,token=NULL WHERE id=? AND token=?", time.Now().UnixMilli(), n.ID, token)
	} else {
		backoff := webhookRetryDelay(attempts, retryAfter, time.Now())
		_, e = s.Store.db.ExecContext(ctx, "UPDATE outbox SET due=?,token=NULL WHERE id=? AND token=?", time.Now().Add(backoff).UnixMilli(), n.ID, token)
	}
	return true, e
}

// Retry-After may be seconds or an HTTP date. Bound untrusted delays so a bad
// receiver clock cannot strand a durable event indefinitely.
func webhookRetryDelay(attempts int, hint string, now time.Time) time.Duration {
	delay := time.Second << min(attempts, 10)
	const limit = 24 * time.Hour
	if seconds, err := strconv.ParseUint(hint, 10, 64); err == nil {
		return max(delay, time.Duration(min(seconds, uint64(limit/time.Second)))*time.Second)
	}
	if at, err := http.ParseTime(hint); err == nil {
		return max(delay, min(limit, at.Sub(now)))
	}
	return delay
}

func (s *Service) notifications(ctx context.Context, c Client) {
	for ctx.Err() == nil {
		worked, err := s.deliverOne(ctx, c)
		if err != nil && ctx.Err() == nil {
			slog.Warn("notification delivery deferred", "client", c.Name, "error", err)
		}
		if (!worked || err != nil) && !wait(ctx, time.Second) {
			return
		}
	}
}

func (s *Service) postSigned(ctx context.Context, url, secret, eventID string, body []byte) (int, string, error) {
	req, e := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if e != nil {
		return 0, "", e
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Broadcaster-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("Idempotency-Key", eventID)
	resp, e := s.http.Do(req)
	if e != nil {
		return 0, "", e
	}
	defer resp.Body.Close()
	var hint string
	if resp.StatusCode == 429 || resp.StatusCode == 503 {
		hint = resp.Header.Get("Retry-After")
	}
	return resp.StatusCode, hint, nil
}
