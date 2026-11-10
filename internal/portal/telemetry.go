package portal

import (
	"encoding/csv"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"api-telemetry-gateway/internal/telemetry"
)

var requests = template.Must(template.New("requests.html").Funcs(template.FuncMap{
	"timestamp":    func(value time.Time) string { return value.Format("2006-01-02 15:04:05.000") },
	"milliseconds": func(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) },
}).ParseFS(files, "templates/requests.html", "templates/styles.html"))

var dashboard = template.Must(template.New("dashboard.html").Funcs(template.FuncMap{
	"milliseconds": func(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) },
}).ParseFS(files, "templates/dashboard.html", "templates/styles.html"))

func registerTelemetry(mux *http.ServeMux, recorder *telemetry.Recorder) {
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		window := r.URL.Query().Get("window")
		if window == "" {
			window = "24h"
		}
		duration := map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}[window]
		if duration == 0 {
			http.Error(w, "window must be 1h, 24h, or 7d", http.StatusBadRequest)
			return
		}
		summaries, err := recorder.Summaries(r.Context(), time.Now().Add(-duration), visibleAPIs(r))
		if err != nil {
			http.Error(w, "could not read request telemetry", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = dashboard.Execute(w, struct {
			Window    string
			Summaries []telemetry.Summary
		}{window, summaries})
	})
	explorer := func(w http.ResponseWriter, r *http.Request) {
		filter, err := parseFilter(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		filter.APIs = visibleAPIs(r)
		events, err := recorder.Recent(r.Context(), filter)
		if err != nil {
			http.Error(w, "could not read request telemetry", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/requests.csv" {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="gateway-requests.csv"`)
			writer := csv.NewWriter(w)
			_ = writer.Write(telemetry.CSVHeader())
			for _, event := range events {
				_ = writer.Write(event.CSVRow())
			}
			writer.Flush()
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = requests.Execute(w, struct {
			Filter    telemetry.Filter
			Events    []telemetry.Event
			Dropped   uint64
			Workspace bool
		}{filter, events, recorder.Dropped(), permissionsFrom(r).Workspace})
	}
	mux.HandleFunc("GET /requests", explorer)
	mux.HandleFunc("GET /requests.csv", explorer)
}

func parseFilter(query url.Values) (telemetry.Filter, error) {
	filter := telemetry.Filter{API: query.Get("api"), RequestID: query.Get("id")}
	if value := query.Get("status"); value != "" {
		code, err := strconv.Atoi(value)
		if err != nil || code < 100 || code > 599 {
			return filter, errors.New("status must be between 100 and 599")
		}
		filter.Status = code
	}
	if len(filter.API) > 63 || len(filter.RequestID) > 64 {
		return filter, errors.New("filter is too long")
	}
	return filter, nil
}

func visibleAPIs(r *http.Request) []string {
	permissions := permissionsFrom(r)
	if permissions.Workspace {
		return nil
	}
	apis := make([]string, 0, len(permissions.Visible))
	for api := range permissions.Visible {
		apis = append(apis, api)
	}
	return apis
}
