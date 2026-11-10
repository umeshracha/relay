package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"api-telemetry-gateway/internal/access"
	"api-telemetry-gateway/internal/audit"
	"api-telemetry-gateway/internal/catalog"
	"api-telemetry-gateway/internal/routing"
	"api-telemetry-gateway/internal/telemetry"

	_ "modernc.org/sqlite"
)

const (
	Import   = "OpenAPI import"
	Export   = "CSV export"
	Policies = "Traffic policy import"
)

var ErrState = errors.New("job is missing or cannot perform that action")

type Input struct {
	Label   string
	Payload json.RawMessage
	Problem string
}

type Item struct {
	Label, State, Problem string
	Routes                []routing.Route
}

type Job struct {
	ID, Kind, State                   string
	Created, Expires                  time.Time
	Total, Pending, Succeeded, Failed int
	Items                             []Item
}

func (j Job) Downloadable() bool {
	return j.Kind == Export && j.Succeeded == 1 && time.Now().Before(j.Expires)
}

func (j Job) Retryable() bool {
	for _, item := range j.Items {
		if item.State == "failed" {
			return true
		}
	}
	return false
}

type ExportInput struct {
	Filter    telemetry.Filter
	HighWater int64
}

type Store struct {
	db        *sql.DB
	directory string
	wake      chan struct{}
}

func Open(path string) (*Store, error) {
	directory := path + ".files"
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := audit.Init(db); err != nil {
		db.Close()
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS jobs (
		id TEXT PRIMARY KEY NOT NULL, kind TEXT NOT NULL, state TEXT NOT NULL,
		created INTEGER NOT NULL, expires INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS job_items (
		job_id TEXT NOT NULL, position INTEGER NOT NULL, label TEXT NOT NULL,
		payload TEXT NOT NULL, state TEXT NOT NULL, problem TEXT NOT NULL,
		PRIMARY KEY(job_id,position)
	);
	UPDATE job_items SET state='queued' WHERE state='running';`)
	if err != nil {
		db.Close()
		return nil, err
	}
	// An interrupted export may leave a temporary file. Durable jobs replay
	// their work with the same item identity.
	temps, err := filepath.Glob(filepath.Join(directory, ".export-*"))
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, name := range temps {
		if err := os.Remove(name); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db, directory: directory, wake: make(chan struct{}, 1)}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Create(ctx context.Context, kind string, inputs []Input) (string, error) {
	if len(inputs) == 0 || len(inputs) > 20 || (kind != Import && kind != Export && kind != Policies) {
		return "", ErrState
	}
	id := rand.Text()
	state := "preview"
	if kind == Export {
		state = "queued"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO jobs (id,kind,state,created) VALUES (?,?,?,?)", id, kind, state, time.Now().Unix()); err != nil {
		return "", err
	}
	for index, input := range inputs {
		itemState := state
		if input.Problem != "" {
			itemState = "invalid"
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO job_items VALUES (?,?,?,?,?,?)",
			id, index, input.Label, string(input.Payload), itemState, input.Problem); err != nil {
			return "", err
		}
	}
	if err := audit.Record(ctx, tx, "Bulk job created", id); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.notify()
	return id, nil
}

func (s *Store) Start(ctx context.Context, id string, retry bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	from, action := "preview", "Bulk job started"
	if retry {
		from, action = "failed", "Failed job items retried"
	}
	var pending int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM job_items WHERE job_id=? AND state IN ('queued','running')", id).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return ErrState
	}
	result, err := tx.ExecContext(ctx, "UPDATE job_items SET state='queued',problem='' WHERE job_id=? AND state=?", id, from)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrState
	}
	if _, err := tx.ExecContext(ctx, "UPDATE jobs SET state='queued' WHERE id=?", id); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, action, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify()
	return nil
}

func (s *Store) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

const summarySQL = `SELECT j.id,j.kind,j.state,j.created,j.expires,count(*),
	sum(i.state IN ('queued','running')),sum(i.state='succeeded'),sum(i.state IN ('failed','invalid'))
	FROM jobs j JOIN job_items i ON i.job_id=j.id`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var job Job
	var created, expires int64
	err := row.Scan(&job.ID, &job.Kind, &job.State, &created, &expires, &job.Total, &job.Pending, &job.Succeeded, &job.Failed)
	job.Created = time.Unix(created, 0).UTC()
	job.Expires = time.Unix(expires, 0).UTC()
	if job.State != "preview" && job.Pending == 0 {
		job.State = "Complete"
		if job.Failed != 0 {
			job.State = "Completed with errors"
		}
	}
	return job, err
}

func (s *Store) List(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, summarySQL+" GROUP BY j.id ORDER BY j.created DESC,j.rowid DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, job)
	}
	return list, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, summarySQL+" WHERE j.id=? GROUP BY j.id", id))
	if err != nil {
		return job, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT label,state,problem,payload FROM job_items WHERE job_id=? ORDER BY position", id)
	if err != nil {
		return job, err
	}
	defer rows.Close()
	for rows.Next() {
		var item Item
		var payload string
		if err := rows.Scan(&item.Label, &item.State, &item.Problem, &payload); err != nil {
			return job, err
		}
		if job.Kind == Import && item.State == "preview" {
			var api catalog.API
			if err := json.Unmarshal([]byte(payload), &api); err != nil {
				return job, err
			}
			item.Routes = api.Routes
		}
		job.Items = append(job.Items, item)
	}
	return job, rows.Err()
}

func (s *Store) Download(ctx context.Context, id string) (*os.File, error) {
	job, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !job.Downloadable() {
		return nil, ErrState
	}
	return os.Open(s.exportPath(job.ID))
}

func (s *Store) exportPath(id string) string { return filepath.Join(s.directory, id+".csv") }

func (s *Store) Run(ctx context.Context, catalogStore *catalog.Store, recorder *telemetry.Recorder, keys *access.Store) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	nextCleanup := time.Time{}
	for ctx.Err() == nil {
		if time.Now().After(nextCleanup) {
			if err := s.cleanup(ctx); err != nil && ctx.Err() == nil {
				log.Printf("bulk job cleanup: %v", err)
			}
			nextCleanup = time.Now().Add(time.Minute)
		}
		worked, err := s.work(ctx, catalogStore, recorder, keys)
		if err != nil && ctx.Err() == nil {
			log.Printf("bulk job worker: %v", err)
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-s.wake:
		case <-ticker.C:
		}
	}
}

func (s *Store) Audit(ctx context.Context) ([]audit.Entry, error) {
	return audit.Recent(ctx, s.db)
}
