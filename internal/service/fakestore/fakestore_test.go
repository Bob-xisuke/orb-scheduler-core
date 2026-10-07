package fakestore_test

// This test binary links no SQLite driver: the business packages it exercises
// (internal/service, internal/placement's input rules, internal/model) and
// this in-memory double depend only on the pure definitions in
// internal/model. `go list -test -deps ./internal/service/fakestore` contains
// no modernc.org/sqlite package, which is the check that the business path is
// verifiable without loading the driver.

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/service"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/service/fakestore"
)

// The double satisfies the service storage contract.
var _ service.Store = (*fakestore.Store)(nil)

func request() *model.Placement {
	return &model.Placement{
		Namespace: "ns", Name: "job", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Nodes:     []model.Node{{Name: "n", CPU: 200, Memory: 128}},
	}
}

// The full accept flow — defaults, trial scheduling, idempotent retry and
// conflict detection — runs against the double alone.
func TestBusinessPathRunsAgainstDouble(t *testing.T) {
	svc := service.New(&fakestore.Store{})
	ctx := context.Background()

	first, outcome, err := svc.Accept(ctx, request())
	if err != nil || outcome != service.Created {
		t.Fatalf("first accept: outcome=%d err=%v", outcome, err)
	}
	if first.Status != service.StatusPlaced || first.Node == nil || *first.Node != "n" {
		t.Fatalf("trial scheduling did not run: %+v", first)
	}
	if first.Selector == nil || first.Nodes[0].Labels == nil {
		t.Fatalf("defaults not filled: %+v", first)
	}

	// Same content, spelled with explicit empty defaults: identical retry.
	again := request()
	again.Selector = map[string]string{}
	again.Nodes[0].Labels = map[string]string{}
	retry, outcome, err := svc.Accept(ctx, again)
	if err != nil || outcome != service.Identical {
		t.Fatalf("retry: outcome=%d err=%v", outcome, err)
	}
	if retry != first && *retry.Node != *first.Node {
		t.Fatalf("retry returned a different record")
	}

	// Different content on the same identity: conflict, record untouched.
	changed := request()
	changed.Priority = 2
	if _, outcome, err = svc.Accept(ctx, changed); err != nil || outcome != service.Conflict {
		t.Fatalf("conflict: outcome=%d err=%v", outcome, err)
	}
	got, err := svc.Query(ctx, url.Values{"namespace": {"ns"}, "name": {"job"}})
	if err != nil || got.Record == nil || got.Record.Priority != 1 {
		t.Fatalf("stored record changed after conflict: %+v err=%v", got.Record, err)
	}

	// Query validation failures never reach storage and stay recognizable.
	if _, err := svc.Query(ctx, url.Values{"bogus": {"x"}}); !errors.Is(err, service.ErrInvalidPlacementQuery) {
		t.Fatalf("invalid query err = %v, want ErrInvalidPlacementQuery", err)
	}
	if _, err := svc.Query(ctx, url.Values{"namespace": {"ns"}, "name": {"missing"}}); !errors.Is(err, service.ErrPlacementNotFound) {
		t.Fatalf("missing identity err = %v, want ErrPlacementNotFound", err)
	}
}

// Ping is healthy by default, each probe is counted, and PingErr makes it
// report the injected failure without the accept or query paths probing
// storage themselves.
func TestPingDouble(t *testing.T) {
	fake := &fakestore.Store{}
	ctx := context.Background()

	if err := fake.Ping(ctx); err != nil {
		t.Fatalf("default ping = %v, want nil", err)
	}
	if err := fake.Ping(ctx); err != nil {
		t.Fatalf("second ping = %v, want nil", err)
	}
	if fake.Pings != 2 {
		t.Fatalf("pings = %d, want 2", fake.Pings)
	}

	root := errors.New("probe failed")
	fake.PingErr = root
	if err := fake.Ping(ctx); !errors.Is(err, root) {
		t.Fatalf("injected ping err = %v, want %v", err, root)
	}
	if fake.Pings != 3 {
		t.Fatalf("failing ping still counted: pings = %d, want 3", fake.Pings)
	}

	// Ping failures are isolated to the health path: acceptance still
	// runs its full flow and never increments the probe count.
	svc := service.New(fake)
	before := fake.Pings
	if _, _, err := svc.Accept(ctx, request()); err != nil {
		t.Fatalf("accept after ping failure: %v", err)
	}
	if fake.Pings != before {
		t.Fatalf("accept probed storage: pings %d -> %d", before, fake.Pings)
	}
}
