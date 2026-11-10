package portal

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"api-telemetry-gateway/internal/access"
	"api-telemetry-gateway/internal/catalog"
)

var accessPage = template.Must(template.New("access.html").Funcs(template.FuncMap{
	"expiry": func(value int64) string { return time.Unix(value, 0).UTC().Format("2006-01-02 15:04:05 UTC") },
}).ParseFS(files, "templates/access.html", "templates/styles.html"))

func registerAccess(mux *http.ServeMux, catalogStore *catalog.Store, keys *access.Store) {
	render := func(w http.ResponseWriter, r *http.Request, secret string) {
		ctx := r.Context()
		teams, err := keys.Teams(ctx)
		if err != nil {
			accessError(w, err)
			return
		}
		credentials, err := keys.Keys(ctx)
		if err != nil {
			accessError(w, err)
			return
		}
		configured, err := catalogStore.List(ctx)
		if err != nil {
			accessError(w, err)
			return
		}
		type apiOption struct {
			Slug      string
			Protected bool
		}
		permissions := permissionsFrom(r)
		if !permissions.Workspace {
			visibleTeams := teams[:0]
			for _, team := range teams {
				if len(permissions.Managed[team]) != 0 {
					visibleTeams = append(visibleTeams, team)
				}
			}
			teams = visibleTeams
			visibleKeys := credentials[:0]
			for _, key := range credentials {
				if permissions.Managed[key.Team][key.API] {
					visibleKeys = append(visibleKeys, key)
				}
			}
			credentials = visibleKeys
		}
		var apis []apiOption
		for _, api := range configured {
			if !api.Draft && (permissions.Workspace || canIssueForAPI(permissions, api.Slug)) {
				apis = append(apis, apiOption{api.Slug, keys.Protected(api.Slug)})
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = accessPage.Execute(w, struct {
			Teams     []string
			Keys      []access.Key
			APIs      []apiOption
			Secret    string
			Workspace bool
		}{teams, credentials, apis, secret, permissions.Workspace})
	}
	mux.HandleFunc("GET /access", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, "")
	})
	register := func(path string, operation func(*http.Request) (string, error)) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, 8192)
			if err := r.ParseForm(); err != nil {
				accessError(w, access.ErrInput)
				return
			}
			secret, err := operation(r)
			if err != nil {
				accessError(w, err)
				return
			}
			if secret == "" {
				http.Redirect(w, r, "/access", http.StatusSeeOther)
				return
			}
			render(w, r, secret)
		})
	}
	register("/access/teams", func(r *http.Request) (string, error) {
		return "", keys.AddTeam(r.Context(), r.PostForm.Get("team"))
	})
	register("/access/keys", func(r *http.Request) (string, error) {
		api := r.PostForm.Get("api")
		if _, ok := catalogStore.Lookup(api); !ok {
			return "", access.ErrInput
		}
		credential, err := keys.Issue(r.Context(), r.PostForm.Get("team"), r.PostForm.Get("app"), api)
		return credential.Secret, err
	})
	register("/access/keys/{id}/rotate", func(r *http.Request) (string, error) {
		hours, err := strconv.Atoi(r.PostForm.Get("hours"))
		if err != nil || hours < 0 || hours > 168 {
			return "", access.ErrInput
		}
		credential, err := keys.Rotate(r.Context(), r.PathValue("id"), time.Duration(hours)*time.Hour)
		return credential.Secret, err
	})
	register("/access/keys/{id}/revoke", func(r *http.Request) (string, error) {
		return "", keys.Revoke(r.Context(), r.PathValue("id"))
	})
	register("/access/protect", func(r *http.Request) (string, error) {
		api := r.PostForm.Get("api")
		if _, ok := catalogStore.Lookup(api); !ok {
			return "", access.ErrInput
		}
		return "", keys.Protect(r.Context(), api)
	})
}

func accessError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, access.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, access.ErrInput):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, access.ErrMissing):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, access.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "could not access team and key settings", http.StatusServiceUnavailable)
	}
}

func canIssueForAPI(permissions access.Permissions, api string) bool {
	for _, apis := range permissions.Managed {
		if apis[api] {
			return true
		}
	}
	return false
}
