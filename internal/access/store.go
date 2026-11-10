package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"api-telemetry-gateway/internal/audit"

	_ "modernc.org/sqlite"
)

var ErrInput = errors.New("enter valid team, application, API, and key details")
var ErrMissing = errors.New("record missing or inactive")
var ErrConflict = errors.New("team, account, or application already exists")
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var tokenPattern = regexp.MustCompile(`^gw_[A-Za-z0-9_-]{43}$`)

type Key struct {
	ID, Team, App, API string
	Expires            int64
	Revoked            bool
}

func (k Key) Active() bool {
	return !k.Revoked && (k.Expires == 0 || time.Now().Unix() < k.Expires)
}

type Credential struct {
	Key
	Secret string
}

type snapshot struct {
	protected map[string]bool
	rules     []Rule
	keys      map[[32]byte]Key
}

type Store struct {
	db        *sql.DB
	mu        sync.Mutex
	live      atomic.Pointer[snapshot]
	limitMu   sync.Mutex
	buckets   map[bucketKey]bucket
	nextSweep time.Time
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

	_, err = db.Exec(`PRAGMA foreign_keys = ON;
		CREATE TABLE IF NOT EXISTS teams (slug TEXT PRIMARY KEY NOT NULL);
		CREATE TABLE IF NOT EXISTS portal_users (username TEXT PRIMARY KEY NOT NULL, digest BLOB NOT NULL);
		CREATE TABLE IF NOT EXISTS team_members (
			username TEXT NOT NULL REFERENCES portal_users(username), team TEXT NOT NULL REFERENCES teams(slug),
			role TEXT NOT NULL, PRIMARY KEY(username,team)
		);
		CREATE TABLE IF NOT EXISTS team_grants (
			team TEXT NOT NULL REFERENCES teams(slug), api TEXT NOT NULL, allowed INTEGER NOT NULL, PRIMARY KEY(team,api)
		);
		CREATE TABLE IF NOT EXISTS gateway_apps (
			id TEXT PRIMARY KEY NOT NULL, team TEXT NOT NULL REFERENCES teams(slug),
			name TEXT NOT NULL, api TEXT NOT NULL, UNIQUE(team,name)
		);
		CREATE TABLE IF NOT EXISTS gateway_keys (
			id TEXT PRIMARY KEY NOT NULL, digest BLOB NOT NULL UNIQUE CHECK(length(digest)=32),
			app_id TEXT NOT NULL REFERENCES gateway_apps(id),
			expires INTEGER NOT NULL DEFAULT 0, revoked INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS protected_apis (api TEXT PRIMARY KEY NOT NULL);
		CREATE TABLE IF NOT EXISTS policy_receipts (item_id TEXT PRIMARY KEY NOT NULL);
		CREATE TABLE IF NOT EXISTS gateway_rules (
			id TEXT PRIMARY KEY NOT NULL, scope TEXT NOT NULL, target TEXT NOT NULL,
			rate INTEGER NOT NULL, burst INTEGER NOT NULL, observe INTEGER NOT NULL
		);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	initial, err := load(context.Background(), db)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.live.Store(initial)
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Authorize reads one immutable snapshot and returns the key and whether the
// request may proceed. An explicit credential must be valid even on a public API.
func (s *Store) Authorize(api, secret string) (Key, bool) {
	live := s.live.Load()
	if secret == "" {
		return Key{}, !live.protected[api]
	}
	if !tokenPattern.MatchString(secret) {
		return Key{}, false
	}
	key, exists := live.keys[sha256.Sum256([]byte(secret))]
	return key, exists && key.API == api && key.Active()
}

func (s *Store) Protected(api string) bool { return s.live.Load().protected[api] }

func (s *Store) AddTeam(ctx context.Context, team string) error {
	if !slugPattern.MatchString(team) {
		return ErrInput
	}
	return s.change(ctx, "Team created", team, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "INSERT INTO teams VALUES (?) ON CONFLICT DO NOTHING", team)
		return changed(result, err, ErrConflict)
	})
}

func (s *Store) Protect(ctx context.Context, api string) error {
	if !slugPattern.MatchString(api) {
		return ErrInput
	}
	return s.change(ctx, "Gateway key required", api, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO protected_apis VALUES (?) ON CONFLICT DO NOTHING", api)
		return err
	})
}

func (s *Store) Issue(ctx context.Context, team, app, api string) (Credential, error) {
	if !slugPattern.MatchString(team) || !slugPattern.MatchString(app) || !slugPattern.MatchString(api) {
		return Credential{}, ErrInput
	}
	credential := newCredential(team, app, api)
	err := s.change(ctx, "API key issued", team+"/"+app+" · "+api, func(tx *sql.Tx) error {
		if err := canManageKey(ctx, tx, team, api); err != nil {
			return err
		}
		appID := rand.Text()
		result, err := tx.ExecContext(ctx, `INSERT INTO gateway_apps (id,team,name,api)
			SELECT ?,?,?,? WHERE EXISTS(SELECT 1 FROM teams WHERE slug=?)
			ON CONFLICT(team,name) DO NOTHING`, appID, team, app, api, team)
		if err := changed(result, err, ErrConflict); err != nil {
			return err
		}
		return insertKey(ctx, tx, credential, appID)
	})
	return credential, err
}

func (s *Store) Rotate(ctx context.Context, id string, overlap time.Duration) (Credential, error) {
	if overlap < 0 || overlap > 7*24*time.Hour {
		return Credential{}, ErrInput
	}
	var credential Credential
	err := s.change(ctx, "API key rotated", id, func(tx *sql.Tx) error {
		var key Key
		var appID string
		err := tx.QueryRowContext(ctx, `SELECT a.team,a.name,a.api,k.expires,k.revoked,k.app_id
			FROM gateway_keys k JOIN gateway_apps a ON a.id=k.app_id WHERE k.id=?`, id).
			Scan(&key.Team, &key.App, &key.API, &key.Expires, &key.Revoked, &appID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		if err := canManageKey(ctx, tx, key.Team, key.API); err != nil {
			return err
		}
		if !key.Active() || key.Expires != 0 {
			return ErrMissing
		}
		if _, err := tx.ExecContext(ctx, "UPDATE gateway_keys SET expires=? WHERE id=?", time.Now().Add(overlap).Unix(), id); err != nil {
			return err
		}
		credential = newCredential(key.Team, key.App, key.API)
		return insertKey(ctx, tx, credential, appID)
	})
	return credential, err
}

func (s *Store) Revoke(ctx context.Context, id string) error {
	return s.change(ctx, "API key revoked", id, func(tx *sql.Tx) error {
		if !PrincipalFrom(ctx).Workspace {
			var team, api string
			err := tx.QueryRowContext(ctx, `SELECT a.team,a.api FROM gateway_apps a
				JOIN gateway_keys k ON k.app_id=a.id WHERE k.id=?`, id).Scan(&team, &api)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrMissing
			}
			if err != nil {
				return err
			}
			if err := canManageKey(ctx, tx, team, api); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, "UPDATE gateway_keys SET revoked=1 WHERE id=? AND revoked=0", id)
		return changed(result, err, ErrMissing)
	})
}

func newCredential(team, app, api string) Credential {
	var secret [32]byte
	rand.Read(secret[:])
	return Credential{Key: Key{ID: rand.Text(), Team: team, App: app, API: api}, Secret: "gw_" + base64.RawURLEncoding.EncodeToString(secret[:])}
}

func insertKey(ctx context.Context, tx *sql.Tx, credential Credential, appID string) error {
	digest := sha256.Sum256([]byte(credential.Secret))
	_, err := tx.ExecContext(ctx, "INSERT INTO gateway_keys (id,digest,app_id) VALUES (?,?,?)", credential.ID, digest[:], appID)
	return err
}

func changed(result sql.Result, err error, empty error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return empty
	}
	return err
}

// Build the next snapshot inside the transaction and activate it after commit.
func (s *Store) change(ctx context.Context, action, target string, apply func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	next, err := load(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.live.Store(next)
	return nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func load(ctx context.Context, db queryer) (*snapshot, error) {
	s := &snapshot{protected: make(map[string]bool), keys: make(map[[32]byte]Key)}
	rows, err := db.QueryContext(ctx, "SELECT api FROM protected_apis")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var api string
		if err := rows.Scan(&api); err != nil {
			return nil, err
		}
		s.protected[api] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keys, err := db.QueryContext(ctx, `SELECT k.id,k.digest,a.team,a.name,a.api,k.expires,k.revoked
		FROM gateway_keys k JOIN gateway_apps a ON a.id=k.app_id WHERE NOT k.revoked AND (k.expires=0 OR k.expires>?)
		AND NOT EXISTS (SELECT 1 FROM team_grants g WHERE g.team=a.team AND g.api=a.api AND NOT g.allowed)`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer keys.Close()
	for keys.Next() {
		var key Key
		var digest []byte
		if err := keys.Scan(&key.ID, &digest, &key.Team, &key.App, &key.API, &key.Expires, &key.Revoked); err != nil {
			return nil, err
		}
		if len(digest) != sha256.Size {
			return nil, ErrInput
		}
		s.keys[[32]byte(digest)] = key
	}
	if err := keys.Err(); err != nil {
		return nil, err
	}
	s.rules, err = loadRules(ctx, db)
	return s, err
}

func (s *Store) Teams(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT slug FROM teams ORDER BY slug")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var teams []string
	for rows.Next() {
		var team string
		if err := rows.Scan(&team); err != nil {
			return nil, err
		}
		teams = append(teams, team)
	}
	return teams, rows.Err()
}

func (s *Store) Keys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT k.id,a.team,a.name,a.api,k.expires,k.revoked
		FROM gateway_keys k JOIN gateway_apps a ON a.id=k.app_id ORDER BY a.team,a.name,k.rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []Key
	for rows.Next() {
		var key Key
		if err := rows.Scan(&key.ID, &key.Team, &key.App, &key.API, &key.Expires, &key.Revoked); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) Audit(ctx context.Context) ([]audit.Entry, error) {
	return audit.Recent(ctx, s.db)
}
