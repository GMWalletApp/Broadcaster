package broadcast

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type broadcastEvent struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"eventId"`
	Event         string `json:"event"`
	Family        string `json:"family"`
	Network       string `json:"network"`
	Hash          string `json:"hash"`
	TxHash        string `json:"txHash,omitempty"`
}

func queueInclusion(ctx context.Context, tx *sql.Tx, t Task) error {
	var proof map[string]json.RawMessage
	if err := json.Unmarshal(t.Evidence, &proof); err != nil {
		return err
	}
	key := ""
	for _, field := range []string{"blockHash", "slot", "checkpoint", "ledgerVersion", "transactionHash"} {
		if v := proof[field]; len(v) > 0 {
			key = string(v)
			break
		}
	}
	txHash := t.Winner
	if t.Family == TON {
		if err := json.Unmarshal(proof["transactionHash"], &txHash); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO broadcast_events(id,event,family,network,hash,block_key,tx_hash)
 VALUES(?,'broadcast.included',?,?,?,?,?) ON CONFLICT(event,network,hash,block_key) DO NOTHING`, id(), t.Family, t.Network, t.Winner, key, txHash)
	return err
}

func (s *Service) deliverGlobal(ctx context.Context) (bool, error) {
	var item broadcastEvent
	var attempts int
	item.SchemaVersion = 1
	lease := id()
	now := time.Now()
	err := s.Store.db.QueryRowContext(ctx, `UPDATE broadcast_events SET lease=?,due=?,attempts=attempts+1
 WHERE id=(SELECT id FROM broadcast_events WHERE sent IS NULL AND due<=? ORDER BY due,rowid LIMIT 1)
 RETURNING id,event,family,network,hash,tx_hash,attempts`, lease, now.Add(time.Minute).UnixMilli(), now.UnixMilli()).Scan(
		&item.ID, &item.Event, &item.Family, &item.Network, &item.Hash, &item.TxHash, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	w := s.cfg.BroadcastNotifications
	status, hint, sendErr := s.postSigned(ctx, w.URL, w.Secret, item.ID, bodyJSON([]broadcastEvent{item}))
	if sendErr == nil && status == 204 {
		_, err = s.Store.db.ExecContext(ctx, "UPDATE broadcast_events SET sent=?,lease=NULL WHERE id=? AND lease=?", time.Now().UnixMilli(), item.ID, lease)
		return true, err
	}
	due := time.Now().Add(webhookRetryDelay(attempts-1, hint, time.Now()))
	_, err = s.Store.db.ExecContext(ctx, "UPDATE broadcast_events SET due=?,lease=NULL WHERE id=? AND lease=?", due.UnixMilli(), item.ID, lease)
	if err == nil {
		err = fmt.Errorf("global notification %s deferred (HTTP %d)", item.ID, status)
	}
	return true, err
}

func (s *Service) globalNotifications(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := s.deliverGlobal(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Warn("broadcast notification delivery", "error", err)
		}
		if (!worked || err != nil) && !wait(ctx, time.Second) {
			return
		}
	}
}
