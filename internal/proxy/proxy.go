package proxy

import (
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"api-telemetry-gateway/internal/access"
	"api-telemetry-gateway/internal/catalog"
	"api-telemetry-gateway/internal/routing"
	"api-telemetry-gateway/internal/telemetry"
)

const Prefix = "/gateway/"

func Handler(store *catalog.Store, recorder *telemetry.Recorder, keys *access.Store, trusted []netip.Prefix) http.Handler {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		relative, ok := strings.CutPrefix(r.URL.EscapedPath(), Prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		slug, suffix, _ := strings.Cut(relative, "/")
		api, found := store.Lookup(slug)
		if !found {
			http.NotFound(w, r)
			return
		}
		start := time.Now()
		method := "OTHER"
		switch r.Method {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
			method = r.Method
		}
		event := telemetry.Event{
			RequestID: rand.Text(), API: api.Slug, Route: "(unmatched)", Method: method,
			Status: http.StatusNotFound, Outcome: "Route rejected", Started: start,
		}
		w.Header().Set("X-Request-ID", event.RequestID)
		defer func() {
			event.Duration = time.Since(start)
			recorder.Record(event)
		}()
		credentials := r.Header.Values("X-Gateway-Key")
		key, allowed := keys.Authorize(api.Slug, r.Header.Get("X-Gateway-Key"))
		if !allowed || len(credentials) > 1 {
			event.Status, event.Outcome = http.StatusUnauthorized, "Gateway key rejected"
			http.Error(w, "valid gateway key required", event.Status)
			return
		}
		target, err := url.Parse(api.Upstream)
		if err != nil {
			event.Status, event.Outcome = http.StatusBadGateway, "Gateway configuration error"
			http.Error(w, "invalid upstream configuration", event.Status)
			return
		}
		rawPath := "/" + suffix
		path, err := url.PathUnescape(rawPath)
		if err != nil {
			event.Status = http.StatusBadRequest
			http.Error(w, "invalid request path", event.Status)
			return
		}
		if len(api.Routes) != 0 {
			parts, valid := routing.Segments(path)
			index := slices.IndexFunc(api.Routes, func(route routing.Route) bool { return route.Matches(r.Method, parts) })
			if !valid || strings.Contains(strings.ToLower(rawPath), "%2f") || index < 0 {
				http.NotFound(w, r)
				return
			}
			event.Route = api.Routes[index].Path
		} else {
			event.Route = "(manual API)"
		}
		client, err := clientAddress(r, trusted)
		if err != nil {
			event.Status, event.Outcome = http.StatusBadRequest, "Invalid client address"
			http.Error(w, event.Outcome, event.Status)
			return
		}
		decision := keys.Evaluate(api.Slug, method+" "+event.Route, key, client, time.Now())
		if decision.Observed != "" {
			event.Policy = decision.Observed
		}
		if decision.Status != 0 {
			event.Status, event.Outcome, event.Policy = decision.Status, "IP blocked", decision.RuleID
			if decision.Status == http.StatusTooManyRequests {
				event.Outcome = "Rate limited"
				w.Header().Set("Retry-After", strconv.Itoa(decision.RetryAfter))
			}
			http.Error(w, event.Outcome, event.Status)
			return
		}
		reverse := httputil.ReverseProxy{
			Transport: transport,
			Rewrite: func(request *httputil.ProxyRequest) {
				request.Out.URL.Path = path
				request.Out.URL.RawPath = rawPath
				request.SetURL(target)
				request.SetXForwarded()
				request.Out.Header.Set("X-Forwarded-For", client.String())
				request.Out.Header.Del("X-Gateway-Key")
				request.Out.Header.Set("X-Request-ID", event.RequestID)
			},
			ModifyResponse: func(response *http.Response) error {
				response.Header.Del("X-Request-ID")
				event.Status, event.Outcome = response.StatusCode, "Upstream response"
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				var timeout interface{ Timeout() bool }
				if errors.As(err, &timeout) && timeout.Timeout() {
					event.Status, event.Outcome = http.StatusGatewayTimeout, "Gateway timeout"
					http.Error(w, "upstream timed out", event.Status)
					return
				}
				event.Status, event.Outcome = http.StatusBadGateway, "Upstream unavailable"
				http.Error(w, "upstream unavailable", event.Status)
			},
		}
		reverse.ServeHTTP(w, r)
	})
}
