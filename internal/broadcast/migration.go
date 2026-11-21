package broadcast

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"strings"
)

//go:embed tasks.sql
var tasksDDL string

// SQLite cannot drop the legacy inline UNIQUE constraint. Rebuild just its
// parent table on a dedicated connection, preserving child rows and IDs.
func (s *Store) initialize(ctx context.Context) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	defer func() {
		_, restoreErr := conn.ExecContext(context.Background(), "PRAGMA foreign_keys=ON")
		err = errors.Join(err, restoreErr)
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return errors.New("unsupported database version")
	}
	if version == 0 {
		var existing bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='tasks' AND type='table')").Scan(&existing); err != nil {
			return err
		}
		if existing {
			if err = migrateTaskSlots(ctx, tx); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, tasksDDL+schema); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "PRAGMA user_version=1"); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateTaskSlots(ctx context.Context, tx *sql.Tx) error {
	const columns = "id,owner_id,family,network,sender,nonce,payment,state,active,outcome,winner,consuming,execution,reason,evidence,revision,forced,created,due,lease,lease_until"
	create := strings.Replace(tasksDDL, "CREATE TABLE IF NOT EXISTS tasks (", "CREATE TABLE tasks_next (", 1)
	_, err := tx.ExecContext(ctx, create+"INSERT INTO tasks_next ("+columns+") SELECT "+columns+" FROM tasks; DROP TABLE tasks; ALTER TABLE tasks_next RENAME TO tasks;")
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign key violation while migrating task slots")
	}
	return rows.Err()
}
