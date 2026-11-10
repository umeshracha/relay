package portal

import (
	"html/template"
	"net/http"

	"api-telemetry-gateway/internal/access"
	"api-telemetry-gateway/internal/catalog"
)

var membersPage = template.Must(template.ParseFS(files, "templates/members.html", "templates/styles.html"))

func registerMembers(mux *http.ServeMux, catalogStore *catalog.Store, keys *access.Store) {
	render := func(w http.ResponseWriter, r *http.Request, username, secret string) {
		ctx := r.Context()
		teams, err := keys.Teams(ctx)
		if err != nil {
			accessError(w, err)
			return
		}
		memberships, err := keys.Memberships(ctx)
		if err != nil {
			accessError(w, err)
			return
		}
		grants, err := keys.Grants(ctx)
		if err != nil {
			accessError(w, err)
			return
		}
		configured, err := catalogStore.List(ctx)
		if err != nil {
			accessError(w, err)
			return
		}
		var apis []string
		for _, api := range configured {
			if api.Active {
				apis = append(apis, api.Slug)
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = membersPage.Execute(w, struct {
			Teams, APIs      []string
			Memberships      []access.Membership
			Grants           []access.Grant
			Username, Secret string
		}{teams, apis, memberships, grants, username, secret})
	}
	mux.HandleFunc("GET /members", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, "", "")
	})
	form := func(pattern string, handle func(http.ResponseWriter, *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, 8192)
			if err := r.ParseForm(); err != nil {
				accessError(w, access.ErrInput)
				return
			}
			handle(w, r)
		})
	}
	form("POST /members/users", func(w http.ResponseWriter, r *http.Request) {
		username := r.PostForm.Get("username")
		secret, err := keys.CreateUser(r.Context(), username)
		if err != nil {
			accessError(w, err)
			return
		}
		render(w, r, username, secret)
	})
	form("POST /members/roles", func(w http.ResponseWriter, r *http.Request) {
		item := access.Membership{User: r.PostForm.Get("username"), Team: r.PostForm.Get("team"), Role: r.PostForm.Get("role")}
		if err := keys.SetMembership(r.Context(), item); err != nil {
			accessError(w, err)
			return
		}
		http.Redirect(w, r, "/members", http.StatusSeeOther)
	})
	form("POST /members/grants", func(w http.ResponseWriter, r *http.Request) {
		api, mode := r.PostForm.Get("api"), r.PostForm.Get("mode")
		if _, ok := catalogStore.Lookup(api); !ok || (mode != "allow" && mode != "revoke") {
			accessError(w, access.ErrInput)
			return
		}
		if err := keys.SetGrant(r.Context(), access.Grant{Team: r.PostForm.Get("team"), API: api, Allowed: mode == "allow"}); err != nil {
			accessError(w, err)
			return
		}
		http.Redirect(w, r, "/members", http.StatusSeeOther)
	})
}
