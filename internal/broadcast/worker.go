package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const leaseDuration = 2 * time.Minute

var lostLease = errors.New("task lease was superseded")

func (s *Service) claim(ctx context.Context) (Task, string, error) {
	token := id()
	var t Task
	now := time.Now().UnixMilli()
	err := s.Store.atomic(ctx, func(tx *sql.Tx) error {
		var e error
		t, e = scanTask(tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE state<>'CLOSED' AND due<=? AND (lease_until IS NULL OR lease_until<=?) ORDER BY due,created LIMIT 1", now, now))
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE tasks SET lease=?,lease_until=? WHERE id=?", token, time.Now().Add(leaseDuration).UnixMilli(), t.ID)
		if e != nil {
			return e
		}
		// Leftover calls are ambiguous after a process dies. Never infer rejection.
		_, e = tx.ExecContext(ctx, "UPDATE deliveries SET state='UNKNOWN',detail='interrupted broadcast' WHERE task=? AND state='IN_FLIGHT'", t.ID)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE versions SET state='SUBMISSION_UNKNOWN' WHERE task=? AND state='BROADCASTING'", t.ID); e != nil {
			return e
		}
		t.Versions, e = versions(ctx, tx, t.ID)
		return e
	})
	return t, token, err
}

// Recovered tasks can predate today's intake checks. Only immutable signed
// bytes can prove a permanent local rejection. RPC refusals are observations.
func validateScheduledEVM(t *Task, next update) update {
	var activeInvalid bool
	allInvalid := true
	for i := range t.Versions {
		v := &t.Versions[i]
		_, _, err := decodeEVM(v.Raw)
		if err == nil {
			allInvalid = false
			continue
		}
		v.State = "REJECTED"
		if v.Hash == t.Active {
			activeInvalid = true
		}
	}
	if allInvalid && len(t.Versions) > 0 {
		return update{state: "CLOSED", outcome: "REJECTED", reason: "INVALID_SIGNED_TRANSACTION"}
	}
	if activeInvalid {
		return update{state: "TRACKING", reason: "REPLACEMENT_REQUIRED", bump: true}
	}
	return next
}

func (s *Service) process(ctx context.Context, t *Task, token string) error {
	ps, e := s.endpoints(ctx, t.Network, true)
	if e != nil {
		return e
	}
	// Keep time for a durable transition even if archival reads time out.
	observation, cancel := context.WithTimeout(ctx, 45*time.Second)
	u := s.observe(observation, t, ps)
	// After observing inclusion, require positive fork evidence before resuming sends or reporting expiry.
	if t.State == "CONFIRMING" && !u.hasChainResult() {
		if t.Family == EVM && (u.send || u.state == "QUEUED") && s.evmInclusionOrphaned(observation, ps, t.Evidence) {
			u = update{state: "TRACKING", reason: "INCLUSION_REORGED"}
		} else if t.Family == Solana {
			u = reconcileSolanaConfirmation(t, u)
		} else {
			u = update{state: t.State, reason: t.Reason, winner: t.Winner, execution: t.Execution, evidence: t.Evidence}
		}
	}
	cancel()
	if t.Family != Solana && u.state != "CONFIRMING" && u.state != "CLOSED" && time.Since(t.Created) > time.Duration(s.cfg.TrackingHours)*time.Hour {
		u = update{state: "CLOSED", outcome: "EXPIRED", reason: "TRACKING_DEADLINE", evidence: bodyJSON(map[string]any{"lastState": u.state, "lastReason": u.reason, "lastEvidence": u.evidence, "previousReason": t.Reason, "previousEvidence": t.Evidence})}
	}
	if u.send {
		// Continuously observe receipts. Throttle resends independently of reads.
		var last int64
		e = s.Store.db.QueryRowContext(ctx, "SELECT coalesce(max(started),0) FROM deliveries WHERE task=? AND hash=?", t.ID, t.Active).Scan(&last)
		if e != nil {
			return e
		}
		if !t.Force && time.Since(time.UnixMilli(last)) < 15*time.Second {
			u.send = false
			if u.reason == "" {
				u.reason, u.evidence = t.Reason, t.Evidence
			}
		} else {
			if t.Family == EVM {
				u = validateScheduledEVM(t, u)
			}
			if u.send {
				u, e = s.broadcast(ctx, t, token, u)
				if e != nil {
					return e
				}
			}
		}
	}
	return s.finish(ctx, *t, token, u)
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { s.admitConfiguredDefaults(ctx) })
	wg.Go(func() {
		for ctx.Err() == nil {
			s.health(ctx)
			if !wait(ctx, 15*time.Second) {
				return
			}
		}
	})
	// Isolate receivers: a slow merchant callback cannot delay another merchant.
	for _, client := range s.cfg.Clients {
		if client.Webhook != "" {
			wg.Go(func() { s.notifications(ctx, client) })
		}
	}
	for range s.cfg.Workers {
		wg.Go(func() {
			for ctx.Err() == nil {
				t, token, e := s.claim(ctx)
				if e == nil {
					job, cancel := context.WithTimeout(ctx, leaseDuration-10*time.Second)
					e = s.process(job, &t, token)
					cancel()
				}
				if e != nil && !errors.Is(e, notFound) && !errors.Is(e, lostLease) && ctx.Err() == nil {
					slog.Warn("worker step failed", "task", t.ID, "error", e)
				}
				if e != nil && !wait(ctx, 300*time.Millisecond) {
					return
				}
			}
		})
	}
	wg.Wait()
}
