package audit

import (
	"context"
	"database/sql"
	"time"
)

type actorKey struct{}

func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

type Entry struct {
	At                    time.Time
	Actor, Action, Target string
}

func Init(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS audit_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, at_ns INTEGER NOT NULL,
		actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL
	)`)
	return err
}

// Record is committed in the same transaction as the audited change.
func Record(ctx context.Context, tx *sql.Tx, action, target string) error {
	actor, _ := ctx.Value(actorKey{}).(string)
	if actor == "" {
		actor = "system"
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO audit_events (at_ns,actor,action,target) VALUES (?,?,?,?)",
		time.Now().UnixNano(), actor, action, target)
	return err
}

func Recent(ctx context.Context, db *sql.DB) ([]Entry, error) {
	rows, err := db.QueryContext(ctx, "SELECT at_ns,actor,action,target FROM audit_events ORDER BY id DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var entry Entry
		var at int64
		if err := rows.Scan(&at, &entry.Actor, &entry.Action, &entry.Target); err != nil {
			return nil, err
		}
		entry.At = time.Unix(0, at).UTC()
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
