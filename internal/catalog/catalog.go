package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"

	"api-telemetry-gateway/internal/audit"
	"api-telemetry-gateway/internal/routing"

	_ "modernc.org/sqlite"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var ErrInvalidAPI = errors.New("enter a lowercase slug and an absolute HTTP or HTTPS upstream URL")
var ErrDuplicateSlug = errors.New("this gateway slug already exists")

type API struct {
	Slug     string
	Upstream string
	Routes   []routing.Route
	Draft    bool
	Revision int
	Active   bool
}

type routeTable map[string]API

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
	live    atomic.Pointer[routeTable]
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := audit.Init(db); err != nil {
		db.Close()
		return nil, err
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS apis (
		slug TEXT PRIMARY KEY NOT NULL,
		upstream TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS api_imports (
		slug TEXT PRIMARY KEY NOT NULL,
		upstream TEXT NOT NULL,
		routes TEXT NOT NULL,
		published INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateRevisions(db); err != nil {
		db.Close()
		return nil, err
	}
	store := &Store{db: db}
	apis, err := store.List(context.Background())
	if err != nil {
		db.Close()
		return nil, err
	}
	table := make(routeTable)
	for _, api := range apis {
		if !api.Draft {
			table[api.Slug] = api
		}
	}
	store.live.Store(&table)
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func Validate(api API) error {
	target, err := url.Parse(api.Upstream)
	if !slugPattern.MatchString(api.Slug) || len(api.Upstream) > 2048 || err != nil ||
		(target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" ||
		target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		return ErrInvalidAPI
	}
	return nil
}

func (s *Store) Add(ctx context.Context, api API) error {
	if err := Validate(api); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.recordChange(ctx, "API registered", api.Slug, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT INTO apis (slug, upstream)
			SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM api_revisions WHERE slug = ?)
			ON CONFLICT(slug) DO NOTHING`, api.Slug, api.Upstream, api.Slug)
		return checkInsert(result, err)
	})
	if err != nil {
		return err
	}
	s.activate(API{Slug: api.Slug, Upstream: api.Upstream, Active: true})
	return nil
}

func checkInsert(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrDuplicateSlug
	}
	return nil
}

// Lookup reads the live route table without touching SQLite. Callers must
// treat the returned API and its route slice as read-only.
func (s *Store) Lookup(slug string) (API, bool) {
	api, ok := (*s.live.Load())[slug]
	return api, ok
}

// activate is called under writeMu after SQLite commits the change.
func (s *Store) activate(api API) {
	table := maps.Clone(*s.live.Load())
	table[api.Slug] = api
	s.live.Store(&table)
}

func scanAPI(row interface{ Scan(...any) error }) (API, error) {
	var api API
	var raw []byte
	if err := row.Scan(&api.Slug, &api.Upstream, &raw, &api.Draft, &api.Revision, &api.Active); err != nil {
		return api, err
	}
	if raw != nil {
		if err := json.Unmarshal(raw, &api.Routes); err != nil {
			return api, err
		}
		if len(api.Routes) == 0 {
			return api, ErrInvalidAPI
		}
	}
	return api, nil
}

func (s *Store) List(ctx context.Context) ([]API, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slug, upstream, NULL, 0, 0, 1 FROM apis
		UNION ALL SELECT r.slug,r.upstream,r.routes,NOT r.published,r.revision,coalesce(a.revision=r.revision,0)
		FROM api_revisions r LEFT JOIN api_active a ON a.slug=r.slug
		WHERE NOT r.published OR r.revision=a.revision ORDER BY 1,5`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var apis []API
	for rows.Next() {
		api, err := scanAPI(rows)
		if err != nil {
			return nil, err
		}
		apis = append(apis, api)
	}
	return apis, rows.Err()
}

func (s *Store) Audit(ctx context.Context) ([]audit.Entry, error) {
	return audit.Recent(ctx, s.db)
}

// recordChange commits the mutation and its audit event together.
func (s *Store) recordChange(ctx context.Context, action, target string, apply func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := apply(tx); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, action, target); err != nil {
		return err
	}
	return tx.Commit()
}
