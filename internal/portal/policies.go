package portal

import (
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"api-telemetry-gateway/internal/access"
)

var policies = template.Must(template.ParseFS(files, "templates/policies.html", "templates/styles.html"))

func registerPolicies(mux *http.ServeMux, keys *access.Store) {
	mux.HandleFunc("GET /policies", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = policies.Execute(w, keys.Rules())
	})
	mux.HandleFunc("POST /policies", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if err := r.ParseForm(); err != nil {
			accessError(w, access.ErrInput)
			return
		}
		scope := r.PostForm.Get("scope")
		rate, rateErr := strconv.Atoi(r.PostForm.Get("rate"))
		burst, burstErr := strconv.Atoi(r.PostForm.Get("burst"))
		if scope != "cidr" && (rateErr != nil || burstErr != nil) {
			accessError(w, access.ErrInput)
			return
		}
		rule := access.Rule{Scope: scope, Target: strings.TrimSpace(r.PostForm.Get("target")),
			Rate: rate, Burst: burst, Observe: r.PostForm.Get("mode") == "observe"}
		if err := keys.AddRule(r.Context(), rule); err != nil {
			accessError(w, err)
			return
		}
		http.Redirect(w, r, "/policies", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /policies/{id}/remove", func(w http.ResponseWriter, r *http.Request) {
		if err := keys.RemoveRule(r.Context(), r.PathValue("id")); err != nil {
			accessError(w, err)
			return
		}
		http.Redirect(w, r, "/policies", http.StatusSeeOther)
	})
}
