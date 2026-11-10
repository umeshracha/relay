package portal

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"api-telemetry-gateway/internal/access"
	"api-telemetry-gateway/internal/catalog"
	"api-telemetry-gateway/internal/jobs"
	"api-telemetry-gateway/internal/openapi"
	"api-telemetry-gateway/internal/telemetry"
)

var jobsPage = template.Must(template.ParseFS(files, "templates/jobs.html", "templates/styles.html"))
var jobPage = template.Must(template.ParseFS(files, "templates/job.html", "templates/styles.html"))

func registerJobs(mux *http.ServeMux, queue *jobs.Store, recorder *telemetry.Recorder) {
	mux.HandleFunc("GET /jobs", func(w http.ResponseWriter, r *http.Request) {
		list, err := queue.List(r.Context())
		if err != nil {
			jobError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = jobsPage.Execute(w, list)
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		job, err := queue.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			jobError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = jobPage.Execute(w, job)
	})
	create := func(w http.ResponseWriter, r *http.Request, kind string, items []jobs.Input) {
		id, err := queue.Create(r.Context(), kind, items)
		if err != nil {
			jobError(w, r, err)
			return
		}
		http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
	}
	mux.HandleFunc("POST /jobs/import", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			http.Error(w, "upload up to 20 documents with a combined size below 8 MiB", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		documents := r.MultipartForm.File["document"]
		slugs, upstreams := r.MultipartForm.Value["slug"], r.MultipartForm.Value["upstream"]
		if len(documents) == 0 || len(documents) > 20 || len(slugs) != len(documents) || len(upstreams) != len(documents) {
			http.Error(w, "provide a slug, upstream URL, and file for each API", http.StatusBadRequest)
			return
		}
		var items []jobs.Input
		for index, document := range documents {
			api := catalog.API{Slug: strings.TrimSpace(slugs[index]), Upstream: strings.TrimSpace(upstreams[index])}
			item := jobs.Input{Label: fmt.Sprintf("API %d · %.63s", index+1, api.Slug)}
			err := catalog.Validate(api)
			if err == nil {
				file, openErr := document.Open()
				err = openErr
				if err == nil {
					api.Routes, err = openapi.Routes(file)
					file.Close()
				}
			}
			if err != nil {
				item.Problem = "Invalid upstream or OpenAPI document."
			} else {
				item.Label = fmt.Sprintf("%s · %d routes · %s", api.Slug, len(api.Routes), api.Upstream)
				item.Payload, _ = json.Marshal(api)
			}
			items = append(items, item)
		}
		create(w, r, jobs.Import, items)
	})
	mux.HandleFunc("POST /jobs/policies", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "could not read the policy list", http.StatusBadRequest)
			return
		}
		var rules []access.Rule
		if err := json.Unmarshal([]byte(r.PostForm.Get("rules")), &rules); err != nil || len(rules) == 0 || len(rules) > 20 {
			http.Error(w, "provide a JSON array of 1 to 20 traffic policies", http.StatusBadRequest)
			return
		}
		var items []jobs.Input
		for index, rule := range rules {
			item := jobs.Input{Label: fmt.Sprintf("Policy %d", index+1)}
			if err := access.ValidateRule(&rule); err != nil {
				item.Problem = "Invalid scope, target, or rate."
			} else {
				item.Label = rule.Scope + " " + rule.Target
				item.Payload, _ = json.Marshal(rule)
			}
			items = append(items, item)
		}
		create(w, r, jobs.Policies, items)
	})
	mux.HandleFunc("POST /jobs/export", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid export filter", http.StatusBadRequest)
			return
		}
		filter, err := parseFilter(r.PostForm)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		highWater, err := recorder.HighWater(r.Context())
		if err != nil {
			jobError(w, r, err)
			return
		}
		payload, _ := json.Marshal(jobs.ExportInput{Filter: filter, HighWater: highWater})
		create(w, r, jobs.Export, []jobs.Input{{Label: "Filtered request telemetry", Payload: payload}})
	})
	mux.HandleFunc("POST /jobs/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		updateJob(w, r, queue, false)
	})
	mux.HandleFunc("POST /jobs/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		updateJob(w, r, queue, true)
	})
	mux.HandleFunc("GET /jobs/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		file, err := queue.Download(r.Context(), r.PathValue("id"))
		if err != nil {
			jobError(w, r, err)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			jobError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="gateway-requests.csv"`)
		http.ServeContent(w, r, "gateway-requests.csv", info.ModTime(), file)
	})
	mux.HandleFunc("GET /jobs/{id}/errors", func(w http.ResponseWriter, r *http.Request) {
		job, err := queue.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			jobError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="job-errors.csv"`)
		writer := csv.NewWriter(w)
		_ = writer.Write([]string{"item", "state", "problem"})
		for _, item := range job.Items {
			if item.Problem != "" {
				label := item.Label
				if label != "" && strings.ContainsAny(label[:1], "=+-@\t\r\n") {
					label = "'" + label
				}
				_ = writer.Write([]string{label, item.State, item.Problem})
			}
		}
		writer.Flush()
	})
}

func updateJob(w http.ResponseWriter, r *http.Request, queue *jobs.Store, retry bool) {
	id := r.PathValue("id")
	if err := queue.Start(r.Context(), id, retry); err != nil {
		jobError(w, r, err)
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

func jobError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.NotFound(w, r)
	case errors.Is(err, jobs.ErrState):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "could not read or update the job", http.StatusServiceUnavailable)
	}
}
