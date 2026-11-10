package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const (
	queueCapacity = 4096
	batchSize     = 128
	retainedRows  = 100000
)

type Event struct {
	RequestID string
	API       string
	Route     string
	Method    string
	Status    int
	Outcome   string
	Policy    string
	Started   time.Time
	Duration  time.Duration
}

type Recorder struct {
	db      *sql.DB
	queue   chan Event
	done    chan struct{}
	mu      sync.RWMutex
	closed  bool
	dropped atomic.Uint64
}

func Open(path string) (*Recorder, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS request_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT NOT NULL, api TEXT NOT NULL, route TEXT NOT NULL,
		method TEXT NOT NULL, status INTEGER NOT NULL, outcome TEXT NOT NULL,
		started_ms INTEGER NOT NULL, duration_us INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensurePolicyColumn(db); err != nil {
		db.Close()
		return nil, err
	}
	r := &Recorder{db: db, queue: make(chan Event, queueCapacity), done: make(chan struct{})}
	go r.run()
	return r, nil
}

func ensurePolicyColumn(db *sql.DB) error {
	var count int
	if err := db.QueryRow("SELECT count(*) FROM pragma_table_info('request_events') WHERE name='policy'").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	_, err := db.Exec("ALTER TABLE request_events ADD COLUMN policy TEXT NOT NULL DEFAULT ''")
	return err
}

func (r *Recorder) Record(event Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return
	}
	select {
	case r.queue <- event:
	default:
		r.dropped.Add(1)
	}
}

func (r *Recorder) Dropped() uint64 { return r.dropped.Load() }

func (r *Recorder) Close() error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	<-r.done
	return r.db.Close()
}

func (r *Recorder) run() {
	defer close(r.done)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]Event, 0, batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		for attempt := range 2 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := r.write(ctx, batch)
			cancel()
			if err == nil {
				batch = batch[:0]
				return
			}
			if attempt == 0 {
				time.Sleep(100 * time.Millisecond)
			}
		}
		r.dropped.Add(uint64(len(batch)))
		batch = batch[:0]
	}
	for {
		select {
		case event, ok := <-r.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, event)
			if len(batch) == batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *Recorder) write(ctx context.Context, batch []Event) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO request_events
		(request_id, api, route, method, status, outcome, started_ms, duration_us, policy)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, event := range batch {
		if _, err := statement.ExecContext(ctx, event.RequestID, event.API, event.Route, event.Method,
			event.Status, event.Outcome, event.Started.UnixMilli(), event.Duration.Microseconds(), event.Policy); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM request_events WHERE id <=
		(SELECT MAX(id) - ? FROM request_events)`, retainedRows); err != nil {
		return err
	}
	return tx.Commit()
}

type Filter struct {
	API       string
	APIs      []string
	RequestID string
	Status    int
}

func (r *Recorder) Recent(ctx context.Context, filter Filter) ([]Event, error) {
	events, _, err := r.page(ctx, filter, 0, 100)
	return events, err
}

func (r *Recorder) HighWater(ctx context.Context) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, "SELECT coalesce(max(id),0) FROM request_events").Scan(&id)
	return id, err
}

func (r *Recorder) page(ctx context.Context, filter Filter, before int64, limit int) ([]Event, int64, error) {
	query := `SELECT id,request_id, api, route, method, status, outcome, started_ms, duration_us, policy
		FROM request_events WHERE 1=1`
	var args []any
	if filter.APIs != nil {
		scope, _ := json.Marshal(filter.APIs)
		query += " AND api IN (SELECT value FROM json_each(?))"
		args = append(args, string(scope))
	}
	if filter.API != "" {
		query += " AND api = ?"
		args = append(args, filter.API)
	}
	if filter.RequestID != "" {
		query += " AND request_id = ?"
		args = append(args, filter.RequestID)
	}
	if filter.Status != 0 {
		query += " AND status = ?"
		args = append(args, filter.Status)
	}
	if before != 0 {
		query += " AND id < ?"
		args = append(args, before)
	}
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, query+" ORDER BY id DESC LIMIT ?", args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var events []Event
	var cursor int64
	for rows.Next() {
		var event Event
		var started, duration int64
		if err := rows.Scan(&cursor, &event.RequestID, &event.API, &event.Route, &event.Method, &event.Status,
			&event.Outcome, &started, &duration, &event.Policy); err != nil {
			return nil, 0, err
		}
		event.Started = time.UnixMilli(started).UTC()
		event.Duration = time.Duration(duration) * time.Microsecond
		events = append(events, event)
	}
	return events, cursor, rows.Err()
}
