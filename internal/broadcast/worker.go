package broadcast

import (
	"context"
	"database/sql"
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
		picked := map[string]bool{}
		for rows.Next() {
			p, e := s.endpoint(rows)
			if e != nil {
				rows.Close()
				return e
			}
			if !picked[p.Provider] && len(jobs) < 3 {
				jobs = append(jobs, dispatch{p, id()})
				picked[p.Provider] = true
			}
		}
		if e = rows.Err(); e != nil {
			rows.Close()
			return e
		}
		rows.Close()
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
	case unknown:
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
	t.Revision++
	delay := 3 * time.Second
	if t.Reason == "NO_AVAILABLE_PROVIDER" || t.State == "CHECKING_EXPIRY" {
		delay = time.Second
	}
	if u.bump {
		delay = 30 * time.Second
	}
	t.Next = time.Now().Add(delay)
	return s.Store.atomic(ctx, func(tx *sql.Tx) error {
		r, e := tx.ExecContext(ctx, `UPDATE tasks
            SET state=?, outcome=?, winner=?, consuming=?, execution=?, reason=?,
                evidence=?, revision=?, due=?, forced=CASE WHEN ? THEN false ELSE forced END,
                lease=NULL, lease_until=NULL
            WHERE id=? AND lease=? AND revision=?`, t.State, t.Outcome, t.Winner, t.Consuming, t.Execution, t.Reason, []byte(t.Evidence), t.Revision, t.Next.UnixMilli(), u.send && u.reason != "NO_AVAILABLE_PROVIDER", t.ID, token, t.Revision-1)
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
			}
			_, e = tx.ExecContext(ctx, "UPDATE versions SET state=?,auth=? WHERE task=? AND hash=?", v.State, bodyJSON(v.Authorizations), t.ID, v.Hash)
			if e != nil {
				return e
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

func (s *Service) observe(ctx context.Context, t *Task, ps []Endpoint) update {
	var e error
	u := update{state: t.State, reason: "NO_AVAILABLE_PROVIDER", evidence: t.Evidence}
	var unresolved *update
	var gap *update
	for _, p := range ps {
		var next update
		if t.Family == EVM {
			next, e = s.observeEVM(ctx, p, t)
		} else {
			next, e = s.observeSolana(ctx, p, t)
		}
		if e != nil {
			u.reason = "CHAIN_VIEW_UNAVAILABLE"
			continue
		}
		if next.state == "RESOLVING_NONCE" || next.state == "CHECKING_EXPIRY" {
			if unresolved == nil || string(next.evidence) != string(t.Evidence) {
				snapshot := next
				unresolved = &snapshot
			}
			continue
		}
		if next.reason == "NONCE_GAP" || next.reason == "BLOCKHASH_NOT_VISIBLE" {
			snapshot := next
			gap = &snapshot
			continue
		}
		if unresolved != nil && next.send {
			continue
		}
		u = next
		gap = nil
		unresolved = nil
		if t.Family == Solana && next.state == "RESIGN_REQUIRED" {
			for _, other := range ps {
				if other.ID == p.ID {
					continue
				}
				status, err := s.solStatus(ctx, other, t.Active)
				if err != nil {
					continue
				}
				if status != nil {
					if slices.Contains([]string{"confirmed", "finalized"}, status.Commitment) {
						u = solIncluded(t.Active, status)
					} else {
						u = update{state: "CHECKING_EXPIRY", reason: "CONFLICTING_HISTORY", evidence: t.Evidence}
					}
					break
				}
			}
		}
		break
	}
	if gap != nil {
		u = *gap
	}
	if unresolved != nil {
		u = *unresolved
	}
	return u
}

func (s *Service) process(ctx context.Context, t *Task, token string) error {
	ps, e := s.endpoints(ctx, t.Network, true)
	if e != nil {
		return e
	}
	u := s.observe(ctx, t, ps)
	if t.Family == EVM && u.state != "CLOSED" && time.Since(t.Created) > time.Duration(s.cfg.TrackingHours)*time.Hour {
		u = update{state: "CLOSED", outcome: "EXPIRED", reason: "TRACKING_DEADLINE"}
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
			u, e = s.broadcast(ctx, t, token, u)
			if e != nil {
				return e
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
