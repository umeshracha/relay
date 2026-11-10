package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"api-telemetry-gateway/internal/access"
	"api-telemetry-gateway/internal/audit"
	"api-telemetry-gateway/internal/catalog"
	"api-telemetry-gateway/internal/telemetry"
)

func (s *Store) work(ctx context.Context, catalogStore *catalog.Store, recorder *telemetry.Recorder, keys *access.Store) (bool, error) {
	var id, kind, payload string
	var position int
	err := s.db.QueryRowContext(ctx, `SELECT j.id,j.kind,i.position,i.payload
		FROM jobs j JOIN job_items i ON i.job_id=j.id WHERE i.state IN ('queued','running')
		ORDER BY j.created,j.rowid,i.position LIMIT 1`).Scan(&id, &kind, &position, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE job_items SET state='running' WHERE job_id=? AND position=?", id, position); err != nil {
		return false, err
	}
	var workErr error
	var expires int64
	switch kind {
	case Import:
		var api catalog.API
		workErr = json.Unmarshal([]byte(payload), &api)
		if workErr == nil {
			workErr = catalogStore.ImportDraft(audit.WithActor(ctx, "bulk job "+id), api, fmt.Sprintf("%s/%d", id, position))
		}
	case Policies:
		var rule access.Rule
		workErr = json.Unmarshal([]byte(payload), &rule)
		if workErr == nil {
			workErr = keys.ImportRule(audit.WithActor(ctx, "bulk job "+id), rule, fmt.Sprintf("%s/%d", id, position))
		}
	case Export:
		var input ExportInput
		workErr = json.Unmarshal([]byte(payload), &input)
		if workErr == nil {
			workErr = s.export(ctx, recorder, id, input)
		}
		if workErr == nil {
			expires = time.Now().Add(24 * time.Hour).Unix()
		}
	default:
		workErr = ErrState
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	state, problem := "succeeded", ""
	if workErr != nil {
		state = "failed"
		switch {
		case errors.Is(workErr, catalog.ErrDuplicateSlug):
			problem = "Gateway slug already exists."
		case errors.Is(workErr, catalog.ErrInvalidAPI):
			problem = "Invalid upstream or routes."
		default:
			problem = "Processing failed. Retry this item."
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE job_items SET state=?,problem=? WHERE job_id=? AND position=?", state, problem, id, position); err != nil {
		return true, err
	}
	if expires != 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET expires=? WHERE id=?", expires, id); err != nil {
			return true, err
		}
	}
	return true, tx.Commit()
}

func (s *Store) export(ctx context.Context, recorder *telemetry.Recorder, id string, input ExportInput) error {
	file, err := os.CreateTemp(s.directory, ".export-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := recorder.Export(ctx, file, input.Filter, input.HighWater); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), s.exportPath(id)); err != nil {
		return err
	}
	directory, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s *Store) cleanup(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM jobs WHERE expires>0 AND expires<=?", time.Now().Unix())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := os.Remove(s.exportPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE jobs SET expires=0 WHERE id=?", id); err != nil {
			return err
		}
	}
	return nil
}
