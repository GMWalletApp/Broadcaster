package broadcast

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Commit the task, candidate states and outgoing events together. A superseded lease cannot publish a result.
func (s *Service) finish(ctx context.Context, t Task, token string, u update) error {
	oldReason, oldWinner := t.Reason, t.Winner
	t.State = u.state
	t.Outcome = u.outcome
	t.Reason = u.reason
	t.Winner = u.winner
	t.Consuming = u.consuming
	t.Execution = u.execution
	t.Evidence = u.evidence
	if u.winner != "" || u.outcome == "REPLACED_EXTERNALLY" {
		var evidence map[string]json.RawMessage
		if err := json.Unmarshal(u.evidence, &evidence); err != nil {
			return err
		}
		if _, exists := evidence["observedAt"]; !exists {
			evidence["observedAt"] = bodyJSON(time.Now().UTC())
		}
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
	if t.State == "CLOSED" {
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
			case t.Reason == "INCLUSION_REORGED" && v.State == "CONFIRMING":
				v.State = "PENDING"
			case t.State == "CONFIRMING" && v.Hash == t.Winner:
				v.State = "CONFIRMING"
			case t.Outcome == "INCLUDED" && v.Hash == t.Winner:
				v.State = "INCLUDED"
			case t.Outcome == "INCLUDED" && v.State != "EXPIRED":
				v.State = "REPLACED"
			case t.Outcome == "REPLACED_EXTERNALLY":
				v.State = t.Outcome
			case u.blockhashExpired() && v.Hash == t.Active:
				v.State = "EXPIRED"
			case t.State == "CHECKING_EXPIRY" && v.Hash == t.Active:
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
			e = tx.QueryRowContext(ctx, `UPDATE versions SET state=CASE
				WHEN state='PENDING' AND ?1 IN ('READY','QUEUED','BROADCASTING','SUBMISSION_UNKNOWN') THEN state
				ELSE ?1 END, auth=?2 WHERE task=?3 AND hash=?4 RETURNING state`, v.State, bodyJSON(v.Authorizations), t.ID, v.Hash).Scan(&v.State)
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
		if t.Winner != "" && t.Winner != oldWinner {
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
		case u.rejected && !u.bump && t.Reason != oldReason:
			return event(ctx, tx, t, "transaction.broadcast_rejected")
		case u.bump && oldReason != t.Reason:
			return event(ctx, tx, t, "transaction.replacement_required")
		}
		return nil
	})
}
