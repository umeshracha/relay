package telemetry

import (
	"context"
	"encoding/json"
	"time"
)

type Summary struct {
	API           string
	Requests      int
	Errors        int
	Blocked       int
	P50, P95, P99 time.Duration
}

func (r *Recorder) Summaries(ctx context.Context, since time.Time, apis []string) ([]Summary, error) {
	var scope any
	if apis != nil {
		encoded, _ := json.Marshal(apis)
		scope = string(encoded)
	}
	rows, err := r.db.QueryContext(ctx, `WITH ranked AS (
		SELECT api,status,outcome,duration_us,
			row_number() OVER (PARTITION BY api ORDER BY duration_us) AS rank,
			count(*) OVER (PARTITION BY api) AS total
		FROM request_events WHERE started_ms >= ? AND (? IS NULL OR api IN (SELECT value FROM json_each(?)))
	)
	SELECT api,count(*),sum(status >= 500),sum(outcome IN ('IP blocked','Rate limited','Gateway key rejected')),
		max(CASE WHEN rank = (50*total+99)/100 THEN duration_us END),
        max(CASE WHEN rank = (95*total+99)/100 THEN duration_us END),
        max(CASE WHEN rank = (99*total+99)/100 THEN duration_us END)
	FROM ranked GROUP BY api ORDER BY count(*) DESC,api`, since.UnixMilli(), scope, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var summaries []Summary
	for rows.Next() {
		var summary Summary
		var p50, p95, p99 int64
		if err := rows.Scan(&summary.API, &summary.Requests, &summary.Errors, &summary.Blocked, &p50, &p95, &p99); err != nil {
			return nil, err
		}
		summary.P50 = time.Duration(p50) * time.Microsecond
		summary.P99 = time.Duration(p99) * time.Microsecond
		summary.P95 = time.Duration(p95) * time.Microsecond
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}
