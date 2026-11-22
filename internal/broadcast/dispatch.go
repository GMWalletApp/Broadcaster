package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

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
			if s.cfg.BroadcastNotifications != nil {
				_, err := tx.ExecContext(ctx, `INSERT INTO broadcast_events(id,event,family,network,hash,block_key,tx_hash)
                    SELECT ?, 'broadcast.accepted', t.family,t.network,d.hash,'',''
                    FROM deliveries d JOIN tasks t ON t.id=d.task WHERE d.id=?
                    ON CONFLICT(event,network,hash,block_key) DO NOTHING`, id(), j.delivery)
				if err != nil {
					return err
				}
			}
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
		provider string
		result   sendResult
		err      error
	}
	ch := make(chan completed, len(jobs))
	for _, j := range jobs {
		go func() {
			start := time.Now()
			r := s.send(ctx, j.Endpoint, t.Family, *active)
			err := s.record(ctx, j, r, time.Since(start))
			ch <- completed{j.Provider, r, err}
		}()
	}
	accepted, unknown := false, false
	rejected := ""
	refusals := make(map[string]string)
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
		case "REJECTED":
			reason := rejectionReason(t.Family, c.result.detail)
			refusals[c.provider] = reason
			if rejected == "" {
				rejected = reason
			} else if rejected != reason {
				rejected = "RPC_REJECTED"
			}
		}
	}
	if e != nil {
		return u, e
	}
	switch {
	case accepted || active.State == "PENDING":
		active.State = "PENDING"
	case unknown || active.State == "SUBMISSION_UNKNOWN":
		active.State = "SUBMISSION_UNKNOWN"
	default:
		active.State = "QUEUED"
		u.reason = rejected
		u.rejected = true
		u.evidence = bodyJSON(map[string]any{"providerReasons": refusals})
	}
	u.bump = t.Family == EVM && u.rejected && rejected == "FEE_TOO_LOW"
	if u.bump {
		u.reason = "REPLACEMENT_REQUIRED"
	}
	return u, nil
}
