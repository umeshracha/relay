package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"api-telemetry-gateway/internal/audit"
)

func migrateRevisions(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS api_revisions (
		slug TEXT NOT NULL, revision INTEGER NOT NULL, upstream TEXT NOT NULL,
		routes TEXT NOT NULL, published INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (slug, revision)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS one_api_draft ON api_revisions(slug) WHERE published=0;
	CREATE TABLE IF NOT EXISTS api_active (slug TEXT PRIMARY KEY NOT NULL, revision INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS import_receipts (item_id TEXT PRIMARY KEY NOT NULL);
	INSERT OR IGNORE INTO api_revisions (slug,revision,upstream,routes,published)
		SELECT slug,1,upstream,routes,published FROM api_imports;
	INSERT OR IGNORE INTO api_active SELECT slug,1 FROM api_imports WHERE published=1;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AddDraft(ctx context.Context, api API) error {
	return s.saveDraft(ctx, api, false, "")
}

func (s *Store) AddRevision(ctx context.Context, api API) error {
	return s.saveDraft(ctx, api, true, "")
}

// ImportDraft remembers a job item in the same transaction as its draft.
func (s *Store) ImportDraft(ctx context.Context, api API, itemID string) error {
	return s.saveDraft(ctx, api, false, itemID)
}

func (s *Store) saveDraft(ctx context.Context, api API, revision bool, itemID string) error {
	if err := Validate(api); err != nil {
		return err
	}
	if len(api.Routes) == 0 {
		return ErrInvalidAPI
	}
	routes, err := json.Marshal(api.Routes)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if itemID != "" {
		result, err := tx.ExecContext(ctx, "INSERT INTO import_receipts VALUES (?) ON CONFLICT DO NOTHING", itemID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
	}
	insert := func() error {
		if revision {
			result, err := tx.ExecContext(ctx, `INSERT INTO api_revisions (slug,revision,upstream,routes)
			SELECT ?, (SELECT max(revision)+1 FROM api_revisions WHERE slug=?), ?, ?
			WHERE EXISTS (SELECT 1 FROM api_active WHERE slug=?)
			ON CONFLICT DO NOTHING`, api.Slug, api.Slug, api.Upstream, routes, api.Slug)
			return checkInsert(result, err)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO api_revisions (slug,revision,upstream,routes)
		SELECT ?,1,?,? WHERE NOT EXISTS (SELECT 1 FROM apis WHERE slug=?)
		AND NOT EXISTS (SELECT 1 FROM api_revisions WHERE slug=?) ON CONFLICT DO NOTHING`,
			api.Slug, api.Upstream, routes, api.Slug, api.Slug)
		return checkInsert(result, err)
	}
	if err := insert(); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, "OpenAPI draft saved", api.Slug); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Draft(ctx context.Context, slug string) (API, error) {
	return scanAPI(s.db.QueryRowContext(ctx, `SELECT slug,upstream,routes,1,revision,0 FROM api_revisions
		WHERE slug=? AND published=0`, slug))
}

func (s *Store) History(ctx context.Context, slug string) ([]API, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.slug,r.upstream,r.routes,NOT r.published,r.revision,
		coalesce(a.revision=r.revision,0) FROM api_revisions r LEFT JOIN api_active a ON a.slug=r.slug
		WHERE r.slug=? ORDER BY r.revision DESC`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var versions []API
	for rows.Next() {
		api, err := scanAPI(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, api)
	}
	return versions, rows.Err()
}

func (s *Store) Publish(ctx context.Context, slug string, revision int) error {
	return s.setActive(ctx, slug, revision, true)
}

func (s *Store) Rollback(ctx context.Context, slug string, revision int) error {
	return s.setActive(ctx, slug, revision, false)
}

func (s *Store) setActive(ctx context.Context, slug string, revision int, publish bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	api, err := scanAPI(tx.QueryRowContext(ctx, `SELECT slug,upstream,routes,0,revision,1 FROM api_revisions
		WHERE slug=? AND revision=? AND published=?`, slug, revision, !publish))
	if err != nil {
		return err
	}
	if publish {
		if _, err := tx.ExecContext(ctx, "UPDATE api_revisions SET published=1 WHERE slug=? AND revision=?", slug, revision); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO api_active VALUES (?,?)
		ON CONFLICT(slug) DO UPDATE SET revision=excluded.revision`, slug, revision); err != nil {
		return err
	}
	action := "API revision reactivated"
	if publish {
		action = "API revision published"
	}
	if err := audit.Record(ctx, tx, action, fmt.Sprintf("%s v%d", slug, revision)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.activate(api)
	return nil
}
