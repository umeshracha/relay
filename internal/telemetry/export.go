package telemetry

import (
	"context"
	"encoding/csv"
	"io"
	"strconv"
	"time"
)

func CSVHeader() []string {
	return []string{"request_id", "api", "method", "route", "status", "outcome", "policy", "started_utc", "duration_us"}
}

func (event Event) CSVRow() []string {
	return []string{event.RequestID, event.API, event.Method, event.Route, strconv.Itoa(event.Status),
		event.Outcome, event.Policy, event.Started.Format(time.RFC3339Nano), strconv.FormatInt(event.Duration.Microseconds(), 10)}
}

func (r *Recorder) Export(ctx context.Context, out io.Writer, filter Filter, highWater int64) error {
	writer := csv.NewWriter(out)
	if err := writer.Write(CSVHeader()); err != nil {
		return err
	}
	for before := highWater + 1; ; {
		events, next, err := r.page(ctx, filter, before, 500)
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := writer.Write(event.CSVRow()); err != nil {
				return err
			}
		}
		if next == 0 {
			break
		}
		before = next
	}
	writer.Flush()
	return writer.Error()
}
