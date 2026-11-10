package portal

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"api-telemetry-gateway/internal/catalog"
)

var preview = template.Must(template.ParseFS(files, "templates/preview.html", "templates/styles.html"))
var history = template.Must(template.ParseFS(files, "templates/history.html", "templates/styles.html"))

func registerRevisions(mux *http.ServeMux, store *catalog.Store) {
	probeClient := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	showDraft := func(w http.ResponseWriter, r *http.Request) {
		permissions := permissionsFrom(r)
		if !permissions.Workspace && !permissions.Editable[r.PathValue("slug")] {
			http.NotFound(w, r)
			return
		}
		api, err := store.Draft(r.Context(), r.PathValue("slug"))
		if err != nil {
			revisionError(w, r, err)
			return
		}
		var probe string
		if r.Method == http.MethodPost {
			revision := revisionNumber(w, r)
			if revision == 0 {
				return
			}
			if revision != api.Revision {
				http.NotFound(w, r)
				return
			}
			upstream, err := http.NewRequestWithContext(r.Context(), http.MethodHead, api.Upstream, nil)
			if err != nil {
				revisionError(w, r, err)
				return
			}
			response, err := probeClient.Do(upstream)
			if err != nil {
				probe = "The gateway could not reach the upstream."
			} else {
				response.Body.Close()
				probe = fmt.Sprintf("Upstream reachable. HEAD returned HTTP %d.", response.StatusCode)
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = preview.Execute(w, struct {
			catalog.API
			Probe     string
			Workspace bool
		}{api, probe, permissions.Workspace})
	}
	mux.HandleFunc("GET /imports/{slug}", showDraft)
	mux.HandleFunc("POST /imports/{slug}/test", showDraft)
	mux.HandleFunc("GET /revisions/{slug}", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		permissions := permissionsFrom(r)
		if !permissions.Workspace && !permissions.Editable[r.PathValue("slug")] {
			http.NotFound(w, r)
			return
		}
		revisions, err := store.History(r.Context(), slug)
		if err != nil {
			revisionError(w, r, err)
			return
		}
		if len(revisions) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = history.Execute(w, struct {
			Slug      string
			Revisions []catalog.API
			Workspace bool
		}{slug, revisions, permissions.Workspace})
	})
	register := func(pattern string, apply func(*http.Request, int) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			revision := revisionNumber(w, r)
			if revision == 0 {
				return
			}
			if err := apply(r, revision); err != nil {
				revisionError(w, r, err)
				return
			}
			http.Redirect(w, r, "/revisions/"+r.PathValue("slug"), http.StatusSeeOther)
		})
	}
	register("POST /imports/{slug}/publish", func(r *http.Request, revision int) error {
		return store.Publish(r.Context(), r.PathValue("slug"), revision)
	})
	register("POST /revisions/{slug}/rollback", func(r *http.Request, revision int) error {
		return store.Rollback(r.Context(), r.PathValue("slug"), revision)
	})
}

func revisionNumber(w http.ResponseWriter, r *http.Request) int {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err == nil {
		if number, err := strconv.Atoi(r.PostForm.Get("revision")); err == nil && number > 0 {
			return number
		}
	}
	http.Error(w, "invalid revision number", http.StatusBadRequest)
	return 0
}

func revisionError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	http.Error(w, "could not read or update the API revision", http.StatusServiceUnavailable)
}
