package broadcast

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct{ db *sql.DB }
type scanner interface{ Scan(...any) error }

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database file path required")
	}
	absolute, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	file, e := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = file.Close(); e != nil {
		return nil, e
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	q := uri.Query()
	for _, v := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(FULL)"} {
		q.Add("_pragma", v)
	}
	q.Set("_txlock", "immediate")
	uri.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", uri.String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(8)
	st := &Store{db}
	e = st.initialize(ctx)
	if e != nil {
		db.Close()
		return nil, e
	}
	return st, nil
}

func (s *Store) Close() { s.db.Close() }

func (s *Store) atomic(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.transaction(ctx, false, fn)
}

func (s *Store) transaction(ctx context.Context, readOnly bool, fn func(*sql.Tx) error) error {
	tx, e := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}

func affected(r sql.Result) int64 { n, _ := r.RowsAffected(); return n }

const taskColumns = "id,owner_id,family,network,sender,coalesce(nonce,''),coalesce(payment,''),state,active,outcome,winner,consuming,execution,reason,evidence,revision,forced,created,due"

func scanTask(row scanner) (Task, error) {
	var evidence []byte
	var created, due int64
	var t Task
	err := row.Scan(&t.ID, &t.Owner, &t.Family, &t.Network, &t.Sender, &t.Nonce, &t.Payment, &t.State, &t.Active, &t.Outcome, &t.Winner, &t.Consuming, &t.Execution, &t.Reason, &evidence, &t.Revision, &t.Force, &created, &due)
	t.Evidence = evidence
	t.Created = time.UnixMilli(created).UTC()
	if due > 0 && t.State != "CLOSED" {
		t.Next = time.UnixMilli(due).UTC()
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = notFound
	}
	return t, err
}

func find(ctx context.Context, tx *sql.Tx, owner string, family Family, network, hash string) (Task, error) {
	q := "SELECT " + taskColumns + " FROM tasks WHERE owner_id=?1 AND family=?2 AND (?3='' OR network=?3) AND id IN (SELECT task FROM versions WHERE hash=?4) LIMIT 2"
	// The enclosing immediate transaction serializes competing mutations.
	rows, err := tx.QueryContext(ctx, q, owner, family, network, hash)
	if err != nil {
		return Task{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if rows.Err() != nil {
			return Task{}, rows.Err()
		}
		return Task{}, notFound
	}
	t, err := scanTask(rows)
	if err != nil {
		return Task{}, err
	}
	if rows.Next() {
		return Task{}, conflict("AMBIGUOUS_NETWORK")
	}
	return t, rows.Err()
}

func versions(ctx context.Context, tx *sql.Tx, task string) ([]Version, error) {
	rows, e := tx.QueryContext(ctx, "SELECT hash,state,raw,blockhash,intent,context_slot,last_valid,auth,coalesce((SELECT slot FROM solana_origins o WHERE o.task=versions.task AND o.hash=versions.hash),0) FROM versions WHERE task=?1 ORDER BY created,rowid", task)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		var auth []byte
		if e = rows.Scan(&v.Hash, &v.State, &v.Raw, &v.Blockhash, &v.Intent, &v.ContextSlot, &v.LastValid, &auth, &v.OriginSlot); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(auth, &v.Authorizations); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) lookup(ctx context.Context, owner string, f Family, network, hash string) (Task, error) {
	var t Task
	err := s.transaction(ctx, true, func(tx *sql.Tx) error {
		var e error
		t, e = find(ctx, tx, owner, f, network, hash)
		if e != nil {
			return e
		}
		t.Versions, e = versions(ctx, tx, t.ID)
		if e != nil {
			return e
		}
		indexes := map[string]int{}
		for i, v := range t.Versions {
			indexes[v.Hash] = i
		}
		rows, e := tx.QueryContext(ctx, "SELECT hash,id,round,provider,endpoint,state,detail,started,latency FROM deliveries WHERE task=?1 ORDER BY started", t.ID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var h string
			var d Delivery
			var started int64
			if e = rows.Scan(&h, &d.ID, &d.Round, &d.Provider, &d.Endpoint, &d.State, &d.Detail, &started, &d.Latency); e != nil {
				return e
			}
			if d.State == "REJECTED" {
				d.Reason = rejectionReason(t.Family, d.Detail)
			}
			d.Started = time.UnixMilli(started).UTC()
			v := &t.Versions[indexes[h]]
			v.Broadcasts = append(v.Broadcasts, d)
		}
		return rows.Err()
	})
	return t, err
}

func event(ctx context.Context, tx *sql.Tx, t Task, name string) error {
	_, e := tx.ExecContext(ctx, "INSERT INTO outbox(id,owner_id,task,name,revision,payload) VALUES(?1,?2,?3,?4,?5,?6) ON CONFLICT(task,name,revision) DO NOTHING", id(), t.Owner, t.ID, name, t.Revision, bodyJSON(t))
	return e
}

func insertVersion(ctx context.Context, tx *sql.Tx, t Task, v Version) error {
	r, e := tx.ExecContext(ctx, "INSERT INTO versions(network,hash,task,raw,state,blockhash,intent,context_slot,last_valid,auth) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10) ON CONFLICT DO NOTHING", t.Network, v.Hash, t.ID, v.Raw, v.State, v.Blockhash, v.Intent, v.ContextSlot, v.LastValid, bodyJSON(v.Authorizations))
	if e == nil && affected(r) == 0 {
		return conflict("HASH_ALREADY_REGISTERED")
	}
	if e == nil && v.OriginSlot > 0 {
		_, e = tx.ExecContext(ctx, "INSERT INTO solana_origins(task,hash,slot) VALUES (?,?,?)", t.ID, v.Hash, v.OriginSlot)
	}
	return e
}

// Check hash idempotency and active occupancy before doing any RPC preflight.
func (s *Store) submittedEVM(ctx context.Context, incoming Task, hash string) (Task, error) {
	var known Task
	err := s.transaction(ctx, true, func(tx *sql.Tx) error {
		var err error
		known, err = find(ctx, tx, incoming.Owner, EVM, incoming.Network, hash)
		if err != notFound {
			return err
		}
		var id, owner string
		err = tx.QueryRowContext(ctx, `SELECT id,owner_id FROM tasks WHERE family='evm' AND state<>'CLOSED'
			AND network=? AND sender=? AND nonce=?`, incoming.Network, incoming.Sender, incoming.Nonce).Scan(&id, &owner)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound
		}
		if err != nil {
			return err
		}
		if owner != incoming.Owner {
			id = ""
		}
		return &Fault{409, "NONCE_CONFLICT", id}
	})
	return known, err
}

// submit serializes only competing inserts through unique constraints. A losing
// writer re-reads the winner after ON CONFLICT, inside the same immediate transaction.
func (s *Store) submit(ctx context.Context, t Task, v Version) (Task, bool, error) {
	created := false
	e := s.atomic(ctx, func(tx *sql.Tx) error {
		// A closed task frees the queue slot but keeps ownership of all its hashes.
		if t.Family != Solana {
			known, err := find(ctx, tx, t.Owner, t.Family, t.Network, v.Hash)
			if err == nil {
				t = known
				if t.Family == Tron && t.State != "CLOSED" && t.State != "CONFIRMING" {
					var err error
					created, err = refreshTronSignatures(ctx, tx, &t, v)
					return err
				}
				return nil
			}
			if err != notFound {
				return err
			}
		}
		t.ID = id()
		t.Active = v.Hash
		t.Revision = 1
		t.Created = time.Now().UTC()
		r, e := tx.ExecContext(ctx, "INSERT INTO tasks(id,owner_id,family,network,sender,nonce,payment,state,active) VALUES(?1,?2,?3,?4,?5,nullif(?6,''),nullif(?7,''),?8,?9) ON CONFLICT DO NOTHING", t.ID, t.Owner, t.Family, t.Network, t.Sender, t.Nonce, t.Payment, t.State, t.Active)
		if e != nil {
			return e
		}
		if affected(r) == 0 {
			existing, e := find(ctx, tx, t.Owner, t.Family, t.Network, v.Hash)
			if e == nil && existing.Payment == t.Payment {
				t = existing
				return nil
			}
			if e != nil && e != notFound {
				return e
			}
			if t.Family == Solana {
				return conflict("PAYMENT_CONFLICT")
			}
			return conflict("NONCE_CONFLICT")
		}
		if e = insertVersion(ctx, tx, t, v); e != nil {
			return e
		}
		created = true
		return event(ctx, tx, t, "transaction.submitted")
	})
	return t, created, e
}

func (s *Store) replace(ctx context.Context, owner, old string, input Task, v Version) (Task, bool, error) {
	var t Task
	created := false
	e := s.atomic(ctx, func(tx *sql.Tx) error {
		var e error
		t, e = find(ctx, tx, owner, input.Family, input.Network, old)
		if e != nil {
			return e
		}
		list, e := versions(ctx, tx, t.ID)
		if e != nil {
			return e
		}
		if input.Payment != "" && input.Payment != t.Payment {
			return conflict("PAYMENT_CONFLICT")
		}
		var active Version
		for _, candidate := range list {
			if candidate.Hash == v.Hash {
				return nil
			}
			if candidate.Hash == t.Active {
				active = candidate
			}
		}
		// An application can explicitly start a new signed attempt after proven
		// Solana expiry. Every other closed result is immutable.
		expiredSolana := t.Family == Solana && t.State == "CLOSED" && t.Outcome == "EXPIRED" && t.Reason == "BLOCKHASH_EXPIRED"
		if t.Active != old || (t.State == "CLOSED" && !expiredSolana) || t.State == "RESOLVING_NONCE" || t.State == "CONFIRMING" {
			return conflict("VERSION_NOT_REPLACEABLE")
		}
		if input.Sender != t.Sender || input.Nonce != t.Nonce {
			return conflict("SLOT_MISMATCH")
		}
		if t.Family == Solana && (!expiredSolana || active.Intent != v.Intent || active.Blockhash == v.Blockhash) {
			return conflict("INVALID_RESIGN")
		}
		if e = insertVersion(ctx, tx, t, v); e != nil {
			return e
		}
		t.Active = v.Hash
		t.State = input.State
		t.Outcome, t.Execution, t.Winner, t.Consuming = "", "", "", ""
		t.Revision++
		t.Reason = ""
		t.Evidence = nil
		t.Next = time.Now()
		_, e = tx.ExecContext(ctx, "UPDATE tasks SET active=?2,state=?3,revision=?4,reason='',outcome='',execution='',winner='',consuming='',evidence=NULL,due=0,forced=false,lease=NULL,lease_until=NULL WHERE id=?1", t.ID, t.Active, t.State, t.Revision)
		if e != nil {
			return e
		}
		created = true
		return event(ctx, tx, t, "transaction.submitted")
	})
	return t, created, e
}
