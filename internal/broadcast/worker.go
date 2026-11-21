package broadcast

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

const leaseDuration = 2 * time.Minute

var lostLease = errors.New("task lease was superseded")

func (s *Service) claim(ctx context.Context) (Task, string, error) {
	token := id()
	var t Task
	now := time.Now().UnixMilli()
	err := s.Store.atomic(ctx, func(tx *sql.Tx) error {
		var e error
		t, e = scanTask(tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE state NOT IN ('CLOSED','RESIGN_REQUIRED') AND due<=? AND (lease_until IS NULL OR lease_until<=?) ORDER BY due,created LIMIT 1", now, now))
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

type dispatch struct {
	Endpoint
	delivery string
}

func (s *Service) prepare(ctx context.Context, t Task, token string) ([]dispatch, error) {
	var jobs []dispatch
	e := s.Store.atomic(ctx, func(tx *sql.Tx) error {
		var held bool
		if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE id=? AND lease=? AND active=?)", t.ID, token, t.Active).Scan(&held); e != nil {
			return e
		}
		if !held {
			return lostLease
		}
		now := time.Now()
		rows, e := tx.QueryContext(ctx, "SELECT "+endpointCols+` FROM providers
            WHERE network=? AND enabled AND healthy AND checked>=?
              AND (cooldown IS NULL OR cooldown<=?) AND (probe_until IS NULL OR probe_until<=?)
            ORDER BY random()`, t.Network, now.Add(-2*time.Minute).UnixMilli(), now.UnixMilli(), now.UnixMilli())
		if e != nil {
			return e
		}
		var candidates []Endpoint
		for rows.Next() {
			p, e := s.endpoint(rows)
			if e != nil {
				rows.Close()
				return e
			}
			candidates = append(candidates, p)
		}
		if e = rows.Err(); e != nil {
			rows.Close()
			return e
		}
		rows.Close()
		// Keep the random order within each class, then pick distinct providers.
		slices.SortStableFunc(candidates, func(a, b Endpoint) int {
			if a.MEVProtected == b.MEVProtected {
				return 0
			}
			if a.MEVProtected {
				return -1
			}
			return 1
		})
		picked := map[string]bool{}
		for _, p := range candidates {
			if picked[p.Provider] {
				continue
			}
			jobs = append(jobs, dispatch{p, id()})
			picked[p.Provider] = true
			if len(jobs) == 3 {
				break
			}
		}
		round := id()
		for i := range jobs {
			j := &jobs[i]
			if j.Level > 0 {
				j.generation++
				_, e = tx.ExecContext(ctx, "UPDATE providers SET probe_until=?,generation=? WHERE id=?", now.Add(20*time.Second).UnixMilli(), j.generation, j.ID)
				if e != nil {
					return e
				}
			}
			_, e = tx.ExecContext(ctx, "INSERT INTO deliveries(id,task,hash,round,provider,endpoint,state) VALUES (?,?,?,?,?,?,'IN_FLIGHT')", j.delivery, t.ID, t.Active, round, j.Provider, j.ID)
			if e != nil {
				return e
			}
		}
		if len(jobs) > 0 {
			if _, e = tx.ExecContext(ctx, "UPDATE tasks SET state='TRACKING' WHERE id=? AND lease=?", t.ID, token); e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, "UPDATE versions SET state='BROADCASTING' WHERE task=? AND hash=? AND state<>'PENDING'", t.ID, t.Active)
		}
		return e
	})
	return jobs, e
}

func (s *Service) record(ctx context.Context, j dispatch, r sendResult, elapsed time.Duration) error {
	return s.Store.atomic(ctx, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE deliveries SET state=?,detail=?,latency=? WHERE id=?", r.state, r.detail, elapsed.Milliseconds(), j.delivery)
		if e != nil {
			return e
		}
		if r.state == "ACCEPTED" || r.state == "ALREADY_KNOWN" {
			// Make the first acknowledgement observable while the other calls
			// are still running. A late acknowledgement cannot undo inclusion.
			_, e = tx.ExecContext(ctx, `UPDATE versions SET state='PENDING'
               WHERE (task,hash)=(SELECT task,hash FROM deliveries WHERE id=?)
               AND state IN ('READY','QUEUED','BROADCASTING','SUBMISSION_UNKNOWN')
               AND EXISTS (SELECT 1 FROM tasks WHERE id=versions.task AND state<>'CLOSED')`, j.delivery)
			if e != nil {
				return e
			}
		}
		var failures, level int
		var generation int64
		e = tx.QueryRowContext(ctx, "SELECT failures,level,generation FROM providers WHERE id=?", j.ID).Scan(&failures, &level, &generation)
		if e != nil || generation != j.generation {
			return e
		}
		var cooldown any
		if r.endpointFailure {
			failures++
			if failures == 3 {
				level = min(level+1, 10)
				failures = 0
				cooldown = time.Now().Add(closeCircuitDelay(level)).UnixMilli()
				generation++
			}
		} else if r.state == "ACCEPTED" || r.state == "ALREADY_KNOWN" {
			failures = 0
			level = 0
		}
		_, e = tx.ExecContext(ctx, `UPDATE providers
            SET failures=?, level=?, cooldown=?, probe_until=NULL, generation=?,
                last_error=CASE WHEN ? THEN ? WHEN ? THEN '' ELSE last_error END
            WHERE id=?`, failures, level, cooldown, generation, r.endpointFailure, r.detail, r.state == "ACCEPTED" || r.state == "ALREADY_KNOWN", j.ID)
		return e
	})
}

func (s *Service) broadcast(ctx context.Context, t *Task, token string, u update) (update, error) {
	jobs, e := s.prepare(ctx, *t, token)
	if e != nil {
		return u, e
	}
	if len(jobs) == 0 {
		if t.State == "TRACKING" {
			u.state = t.State
			u.reason = "NO_AVAILABLE_PROVIDER"
			return u, nil
		}
		u.reason = "NO_AVAILABLE_PROVIDER"
		if t.Family == Solana {
			u.state = "READY"
		} else {
			u.state = "QUEUED"
		}
		return u, nil
	}
	var active *Version
	for i := range t.Versions {
		if t.Versions[i].Hash == t.Active {
			active = &t.Versions[i]
		}
	}
	if active == nil {
		return u, errors.New("missing active transaction version")
	}
	type completed struct {
		result sendResult
		err    error
	}
	ch := make(chan completed, len(jobs))
	for _, j := range jobs {
		go func() {
			start := time.Now()
			r := s.send(ctx, j.Endpoint, t.Family, *active)
			err := s.record(ctx, j, r, time.Since(start))
			ch <- completed{r, err}
		}()
	}
	accepted, unknown := false, false
	for range jobs {
		c := <-ch
		if c.err != nil {
			e = c.err
		}
		switch c.result.state {
		case "ACCEPTED", "ALREADY_KNOWN":
			accepted = true
		case "UNKNOWN":
			unknown = true
		}
		msg := strings.ToLower(c.result.detail)
		if strings.Contains(msg, "underpriced") || strings.Contains(msg, "base fee") {
			u.bump = t.Family == EVM
		}
	}
	if e != nil {
		return u, e
	}
	switch {
	case accepted || active.State == "PENDING":
		active.State = "PENDING"
		if accepted {
			u.bump = false
		}
	case unknown || active.State == "SUBMISSION_UNKNOWN":
		active.State = "SUBMISSION_UNKNOWN"
	default:
		active.State = "QUEUED"
		u.reason = "RPC_REJECTED"
	}
	if u.bump {
		u.reason = "REPLACEMENT_REQUIRED"
	}
	return u, nil
}

func (s *Service) finish(ctx context.Context, t Task, token string, u update) error {
	oldReason, oldState := t.Reason, t.State
	t.State = u.state
	t.Outcome = u.outcome
	t.Reason = u.reason
	t.Winner = u.winner
	t.Consuming = u.consuming
	t.Execution = u.execution
	t.Evidence = u.evidence
	if u.outcome == "INCLUDED" || u.outcome == "REPLACED_EXTERNALLY" {
		var evidence map[string]json.RawMessage
		if err := json.Unmarshal(u.evidence, &evidence); err != nil {
			return err
		}
		evidence["observedAt"] = bodyJSON(time.Now().UTC())
		t.Evidence = bodyJSON(evidence)
	}
	t.Revision++
	delay := 3 * time.Second
	if t.Reason == "NO_AVAILABLE_PROVIDER" || t.State == "CHECKING_EXPIRY" {
		delay = time.Second
	}
	if u.bump {
		delay = 30 * time.Second
	}
	t.Next = time.Now().Add(delay)
	due := t.Next.UnixMilli()
	if t.State == "CLOSED" || t.State == "RESIGN_REQUIRED" {
		t.Next = time.Time{}
		due = 0
	}
	return s.Store.atomic(ctx, func(tx *sql.Tx) error {
		r, e := tx.ExecContext(ctx, `UPDATE tasks
            SET state=?, outcome=?, winner=?, consuming=?, execution=?, reason=?,
                evidence=?, revision=?, due=?, forced=CASE WHEN ? THEN false ELSE forced END,
                lease=NULL, lease_until=NULL
            WHERE id=? AND lease=? AND revision=?`, t.State, t.Outcome, t.Winner, t.Consuming, t.Execution, t.Reason, []byte(t.Evidence), t.Revision, due, u.send && u.reason != "NO_AVAILABLE_PROVIDER", t.ID, token, t.Revision-1)
		if e != nil {
			return e
		}
		if affected(r) == 0 {
			return lostLease
		}
		for i := range t.Versions {
			v := &t.Versions[i]
			switch {
			case t.Outcome == "INCLUDED" && v.Hash == t.Winner:
				v.State = "INCLUDED"
			case t.Outcome == "INCLUDED" && v.State != "EXPIRED":
				v.State = "REPLACED"
			case t.Outcome == "REPLACED_EXTERNALLY":
				v.State = t.Outcome
			case (t.State == "CHECKING_EXPIRY" || t.State == "RESIGN_REQUIRED") && v.Hash == t.Active:
				v.State = t.State
			case t.State == "TRACKING" && t.Family == Solana && v.Hash == t.Active && u.processedSlot > 0:
				v.State = "PENDING"
			case t.Family == Solana && v.Hash == t.Active && v.State == "CHECKING_EXPIRY" && u.valid:
				// The old hash is live again. Restore what we know about any
				// previous sends even if rebroadcast is throttled this round.
				e = tx.QueryRowContext(ctx, `SELECT CASE
				  WHEN count(*) FILTER (WHERE state IN ('ACCEPTED','ALREADY_KNOWN'))>0 THEN 'PENDING'
				  WHEN count(*) FILTER (WHERE state IN ('UNKNOWN','IN_FLIGHT'))>0 THEN 'SUBMISSION_UNKNOWN'
				  ELSE 'READY' END FROM deliveries WHERE task=? AND hash=?`, t.ID, v.Hash).Scan(&v.State)
				if e != nil {
					return e
				}
			}
			_, e = tx.ExecContext(ctx, `UPDATE versions SET state=CASE
				WHEN state='PENDING' AND ?1 IN ('READY','QUEUED','BROADCASTING','SUBMISSION_UNKNOWN') THEN state
				ELSE ?1 END, auth=?2 WHERE task=?3 AND hash=?4`, v.State, bodyJSON(v.Authorizations), t.ID, v.Hash)
			if e != nil {
				return e
			}
			if v.OriginSlot != 0 {
				if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO solana_origins(task,hash,slot) VALUES (?,?,?)", t.ID, v.Hash, v.OriginSlot); e != nil {
					return e
				}
			}
		}
		if t.State == "CLOSED" && t.Family == EVM {
			if _, e = tx.ExecContext(ctx, "UPDATE tasks SET due=0 WHERE network=? AND sender=? AND state='QUEUED'", t.Network, t.Sender); e != nil {
				return e
			}
		}
		if t.Outcome == "INCLUDED" {
			if e = event(ctx, tx, t, "transaction.included"); e != nil {
				return e
			}
		}
		if t.Outcome == "REPLACED_EXTERNALLY" {
			if e = event(ctx, tx, t, "transaction.nonce_conflict"); e != nil {
				return e
			}
		}
		switch {
		case t.State == "CLOSED":
			return event(ctx, tx, t, "transaction.closed")
		case t.State == "RESIGN_REQUIRED" && oldState != t.State:
			return event(ctx, tx, t, "solana.resign_required")
		case u.bump && oldReason != t.Reason:
			return event(ctx, tx, t, "transaction.replacement_required")
		}
		return nil
	})
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
	cancel()
	if t.Family == EVM && u.state != "CLOSED" && time.Since(t.Created) > time.Duration(s.cfg.TrackingHours)*time.Hour {
		u = update{state: "CLOSED", outcome: "EXPIRED", reason: "TRACKING_DEADLINE", evidence: bodyJSON(map[string]any{"lastState": u.state, "lastReason": u.reason, "lastEvidence": u.evidence})}
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
	if t.Family == EVM && u.state == "TRACKING" && time.Since(t.Created) > 2*time.Minute && len(ps) > 0 {
		for _, v := range t.Versions {
			if v.Hash != t.Active {
				continue
			}
			var price hexutil.Big
			var tx types.Transaction
			raw, _ := hexutil.Decode(v.Raw)
			if tx.UnmarshalBinary(raw) == nil && s.call(ctx, ps[0], "eth_gasPrice", []any{}, &price) == nil && tx.GasFeeCap().Cmp(price.ToInt()) < 0 {
				u.reason = "REPLACEMENT_REQUIRED"
				u.bump = true
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
	wg.Go(func() { s.notifications(ctx) })
	if s.cfg.Scanner != nil {
		wg.Go(func() { s.scan(ctx) })
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
