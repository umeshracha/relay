package portal

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"api-telemetry-gateway/internal/access"
	"api-telemetry-gateway/internal/audit"
	"api-telemetry-gateway/internal/catalog"
	"api-telemetry-gateway/internal/jobs"
	"api-telemetry-gateway/internal/openapi"
	"api-telemetry-gateway/internal/telemetry"
)

//go:embed templates
var files embed.FS

var home = template.Must(template.ParseFS(files, "templates/catalog.html", "templates/styles.html"))

func Handler(store *catalog.Store, recorder *telemetry.Recorder, keys *access.Store, queue *jobs.Store, password string) http.Handler {
	mux := http.NewServeMux()
	render := func(w http.ResponseWriter, r *http.Request, status int, message string) {
		apis, err := store.List(r.Context())
		if err != nil {
			http.Error(w, "gateway catalog unavailable", http.StatusServiceUnavailable)
			return
		}
		permissions := permissionsFrom(r)
		if !permissions.Workspace {
			visible := apis[:0]
			for _, api := range apis {
				if permissions.Visible[api.Slug] && (!api.Draft || permissions.Editable[api.Slug]) {
					visible = append(visible, api)
				}
			}
			apis = visible
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_ = home.Execute(w, struct {
			APIs      []catalog.API
			Error     string
			Workspace bool
			CanImport bool
		}{apis, message, permissions.Workspace, permissions.Workspace || len(permissions.Editable) > 0})
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, http.StatusOK, "")
	})
	mux.HandleFunc("POST /apis", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if err := r.ParseForm(); err != nil {
			render(w, r, http.StatusBadRequest, "Could not read the API form.")
			return
		}
		api := catalog.API{Slug: strings.TrimSpace(r.PostForm.Get("slug")), Upstream: strings.TrimSpace(r.PostForm.Get("upstream"))}
		switch err := store.Add(r.Context(), api); {
		case errors.Is(err, catalog.ErrInvalidAPI):
			render(w, r, http.StatusBadRequest, err.Error())
		case errors.Is(err, catalog.ErrDuplicateSlug):
			render(w, r, http.StatusConflict, err.Error())
		case err != nil:
			render(w, r, http.StatusServiceUnavailable, "Could not save the API.")
		default:
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
	})
	mux.HandleFunc("POST /imports", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, openapi.MaxDocument+(16<<10))
		if err := r.ParseMultipartForm(openapi.MaxDocument); err != nil {
			render(w, r, http.StatusBadRequest, "Upload a single OpenAPI file of up to 1 MiB.")
			return
		}
		defer r.MultipartForm.RemoveAll()
		if len(r.MultipartForm.File["document"]) != 1 {
			render(w, r, http.StatusBadRequest, "Choose one OpenAPI file.")
			return
		}
		file, _, err := r.FormFile("document")
		if err != nil {
			render(w, r, http.StatusBadRequest, "Could not read the OpenAPI file.")
			return
		}
		defer file.Close()
		routes, err := openapi.Routes(file)
		if err != nil {
			render(w, r, http.StatusBadRequest, err.Error())
			return
		}
		api := catalog.API{Slug: strings.TrimSpace(r.PostForm.Get("slug")), Upstream: strings.TrimSpace(r.PostForm.Get("upstream")), Routes: routes}
		permissions := permissionsFrom(r)
		if !permissions.Workspace && (r.PostForm.Get("revision") != "1" || !permissions.Editable[api.Slug]) {
			http.Error(w, "this account cannot edit that API", http.StatusForbidden)
			return
		}
		save := store.AddDraft
		if r.PostForm.Get("revision") == "1" {
			save = store.AddRevision
		}
		switch err := save(r.Context(), api); {
		case errors.Is(err, catalog.ErrInvalidAPI):
			render(w, r, http.StatusBadRequest, err.Error())
		case errors.Is(err, catalog.ErrDuplicateSlug):
			render(w, r, http.StatusConflict, err.Error())
		case err != nil:
			render(w, r, http.StatusServiceUnavailable, "Could not save the draft.")
		default:
			http.Redirect(w, r, "/imports/"+api.Slug, http.StatusSeeOther)
		}
	})
	registerRevisions(mux, store)
	registerTelemetry(mux, recorder)
	registerAccess(mux, store, keys)
	registerMembers(mux, store, keys)
	registerPolicies(mux, keys)
	registerAudit(mux, store, keys, queue)
	registerJobs(mux, queue, recorder)
	protected := http.NewCrossOriginProtection().Handler(mux)
	expected := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		user, supplied, ok := r.BasicAuth()
		actual := sha256.Sum256([]byte(supplied))
		workspace := user == "admin" && subtle.ConstantTimeCompare(actual[:], expected[:]) == 1
		authenticated := ok && workspace
		if ok && user != "admin" {
			var err error
			authenticated, err = keys.Authenticate(r.Context(), user, supplied)
			if err != nil {
				http.Error(w, "could not verify portal credentials", http.StatusServiceUnavailable)
				return
			}
		}
		if !authenticated {
			w.Header().Set("WWW-Authenticate", `Basic realm="Gateway administration", charset="UTF-8"`)
			http.Error(w, "sign in to manage APIs", http.StatusUnauthorized)
			return
		}
		ctx := access.WithPrincipal(audit.WithActor(r.Context(), user), access.Principal{User: user, Workspace: workspace})
		permissions, err := keys.Permissions(ctx)
		if err != nil {
			http.Error(w, "could not load portal permissions", http.StatusServiceUnavailable)
			return
		}
		if !workspace {
			_, pattern := mux.Handler(r)
			if !delegatedRoute(pattern, permissions) {
				http.Error(w, "this page requires additional access", http.StatusForbidden)
				return
			}
		}
		protected.ServeHTTP(w, r.WithContext(context.WithValue(ctx, permissionsKey{}, permissions)))
	})
}

type permissionsKey struct{}

func permissionsFrom(r *http.Request) access.Permissions {
	permissions, _ := r.Context().Value(permissionsKey{}).(access.Permissions)
	return permissions
}

func delegatedRoute(pattern string, permissions access.Permissions) bool {
	switch pattern {
	case "GET /{$}", "GET /dashboard", "GET /requests", "GET /requests.csv":
		return true
	case "GET /access", "POST /access/keys", "POST /access/keys/{id}/rotate", "POST /access/keys/{id}/revoke":
		return len(permissions.Managed) != 0
	case "POST /imports", "GET /imports/{slug}", "GET /revisions/{slug}":
		return len(permissions.Editable) != 0
	default:
		return false
	}
}
