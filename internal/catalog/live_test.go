package catalog

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"api-telemetry-gateway/internal/routing"
)

func TestLiveRoutesSurviveDatabaseClosure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var writes sync.WaitGroup
	for i := range 12 {
		writes.Go(func() {
			slug := fmt.Sprintf("api-%d", i)
			if err := store.Add(ctx, API{Slug: slug, Upstream: "http://localhost"}); err != nil {
				t.Error(err)
			}
		})
	}
	writes.Wait()

	for _, slug := range []string{"published", "draft"} {
		api := API{Slug: slug, Upstream: "http://localhost", Routes: []routing.Route{{Method: "GET", Path: "/invoices"}}}
		if err := store.AddDraft(ctx, api); err != nil {
			t.Fatal(err)
		}
		if _, ok := store.Lookup(slug); ok {
			t.Fatalf("unpublished API %s is live", slug)
		}
	}
	if err := store.Publish(ctx, "published", 1); err != nil {
		t.Fatal(err)
	}
	if api, ok := store.Lookup("published"); !ok || len(api.Routes) != 1 || api.Draft {
		t.Fatalf("publish did not activate the route: %+v", api)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		if _, ok := store.Lookup(fmt.Sprintf("api-%d", i)); !ok {
			t.Errorf("manual route %d missing", i)
		}
	}
	if api, ok := store.Lookup("published"); !ok || len(api.Routes) != 1 || api.Draft {
		t.Fatalf("published route missing after restart: %+v", api)
	}
	if _, ok := store.Lookup("draft"); ok {
		t.Fatal("unpublished route loaded at startup")
	}
	if err := store.Add(ctx, API{Slug: "failed", Upstream: "http://localhost"}); err == nil {
		t.Fatal("write to closed database succeeded")
	}
	if _, ok := store.Lookup("failed"); ok {
		t.Fatal("failed write activated a route")
	}
}
