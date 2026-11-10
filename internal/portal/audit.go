package portal

import (
	"html/template"
	"net/http"
	"slices"

	"api-telemetry-gateway/internal/access"
	"api-telemetry-gateway/internal/audit"
	"api-telemetry-gateway/internal/catalog"
	"api-telemetry-gateway/internal/jobs"
)

var auditPage = template.Must(template.ParseFS(files, "templates/audit.html", "templates/styles.html"))

func registerAudit(mux *http.ServeMux, store *catalog.Store, keys *access.Store, queue *jobs.Store) {
	mux.HandleFunc("GET /audit", func(w http.ResponseWriter, r *http.Request) {
		catalogEntries, err := store.Audit(r.Context())
		if err != nil {
			http.Error(w, "could not read audit history", http.StatusServiceUnavailable)
			return
		}
		accessEntries, err := keys.Audit(r.Context())
		if err != nil {
			http.Error(w, "could not read audit history", http.StatusServiceUnavailable)
			return
		}
		jobEntries, err := queue.Audit(r.Context())
		if err != nil {
			http.Error(w, "could not read audit history", http.StatusServiceUnavailable)
			return
		}
		entries := append(append(catalogEntries, accessEntries...), jobEntries...)
		slices.SortStableFunc(entries, func(a, b audit.Entry) int { return b.At.Compare(a.At) })
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = auditPage.Execute(w, entries[:min(len(entries), 100)])
	})
}
