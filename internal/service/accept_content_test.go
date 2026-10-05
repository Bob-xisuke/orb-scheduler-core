package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/placement"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/store"
)

// acceptBody parses body with the shared strict parser and submits it through
// the business entry point, exactly like the HTTP transport does.
func acceptBody(t *testing.T, svc *Service, body string) (*store.Placement, Outcome) {
	t.Helper()
	p, err := placement.ParsePlacementInput([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rec, outcome, err := svc.Accept(context.Background(), p)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return rec, outcome
}

func listCount(t *testing.T, st *store.Store) int {
	t.Helper()
	recs, err := st.List(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(recs)
}

// Textual variants of one submission — members reordered, names escaped, and
// omitted defaults spelled out — are the same content for registration: the
// first create is followed by Identical retries and the stored record never
// changes. This exercises the single default/comparison definition across the
// parser, the service and the store at once.
func TestAcceptTextualVariantsAreIdentical(t *testing.T) {
	st, svc := newService(t)

	canonical := `{
	  "namespace": "team-a", "name": "job-1", "queue": "default", "priority": 10,
	  "resources": {"cpu": 500, "memory": 256},
	  "selector": {"zone": "cn"},
	  "nodes": [
	    {"name": "node-b", "cpu": 1000, "memory": 512, "labels": {"zone": "cn", "ssd": "true"}},
	    {"name": "node-a", "cpu": 1000, "memory": 512}
	  ]
	}`
	variants := []string{
		// Members reordered at every level.
		`{
		  "nodes": [
		    {"labels": {"ssd": "true", "zone": "cn"}, "memory": 512, "cpu": 1000, "name": "node-b"},
		    {"memory": 512, "cpu": 1000, "name": "node-a", "labels": {}}
		  ],
		  "selector": {"zone": "cn"},
		  "resources": {"memory": 256, "cpu": 500},
		  "priority": 10, "queue": "default", "name": "job-1", "namespace": "team-a"
		}`,
		// Member names and string values written with Unicode escapes.
		`{
		  "namespac\u0065": "team-a", "name": "job-1", "queue": "default", "priority": 10,
		  "resources": {"cpu": 500, "memory": 256},
		  "selector": {"zone": "cn"},
		  "nodes": [
		    {"name": "node-\u0062", "cpu": 1000, "memory": 512, "labels": {"zone": "c\u006e", "ssd": "true"}},
		    {"name": "node-a", "cpu": 1000, "memory": 512, "labels": {}}
		  ]
		}`,
	}

	first, outcome := acceptBody(t, svc, canonical)
	if outcome != Created {
		t.Fatalf("first outcome = %d, want Created", outcome)
	}
	want, _ := json.Marshal(first)

	for i, body := range variants {
		rec, outcome := acceptBody(t, svc, body)
		if outcome != Identical {
			t.Fatalf("variant %d: outcome = %d, want Identical", i, outcome)
		}
		got, _ := json.Marshal(rec)
		if string(got) != string(want) {
			t.Fatalf("variant %d returned a different record:\n%s\n%s", i, got, want)
		}
	}
	if n := listCount(t, st); n != 1 {
		t.Fatalf("stored records = %d, want 1", n)
	}
}

// A rejected record follows the same registration rules as a placed one:
// retrying its content is Identical, different content is a Conflict, and
// neither adds nor rewrites a record.
func TestAcceptRejectedRecordFollowsSameRetryAndConflictRules(t *testing.T) {
	st, svc := newService(t)

	rejected := `{
	  "namespace": "team-a", "name": "job-2", "queue": "default", "priority": -1,
	  "resources": {"cpu": 2000, "memory": 256},
	  "nodes": [{"name": "node-a", "cpu": 1000, "memory": 512}]
	}`
	first, outcome := acceptBody(t, svc, rejected)
	if outcome != Created {
		t.Fatalf("first outcome = %d, want Created", outcome)
	}
	if first.Status != StatusRejected || first.Node != nil || first.Reason == nil || *first.Reason != ReasonNoNode {
		t.Fatalf("setup: want rejected record, got %+v", first)
	}
	want, _ := json.Marshal(first)

	// Same content again: Identical, original record, still one row.
	again, outcome := acceptBody(t, svc, rejected)
	if outcome != Identical {
		t.Fatalf("retry outcome = %d, want Identical", outcome)
	}
	if got, _ := json.Marshal(again); string(got) != string(want) {
		t.Fatalf("retry returned a different record:\n%s\n%s", got, want)
	}

	// Different content (here: small enough to be placed): Conflict, and the
	// stored rejection is untouched — the new scheduling result must not
	// replace the recorded one.
	smaller := `{
	  "namespace": "team-a", "name": "job-2", "queue": "default", "priority": -1,
	  "resources": {"cpu": 100, "memory": 256},
	  "nodes": [{"name": "node-a", "cpu": 1000, "memory": 512}]
	}`
	conflict, outcome := acceptBody(t, svc, smaller)
	if outcome != Conflict {
		t.Fatalf("changed content outcome = %d, want Conflict", outcome)
	}
	if got, _ := json.Marshal(conflict); string(got) != string(want) {
		t.Fatalf("conflict did not return the original record:\n%s\n%s", got, want)
	}

	if n := listCount(t, st); n != 1 {
		t.Fatalf("stored records = %d, want 1", n)
	}
	stored, err := st.Get(context.Background(), "team-a", "job-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, _ := json.Marshal(stored); string(got) != string(want) {
		t.Fatalf("stored record changed:\n%s\n%s", got, want)
	}
}

// Records written to an SQLite file survive a restart: after closing and
// reopening the same file they are still readable and still drive the
// retry/conflict comparison.
func TestAcceptRetryAndConflictSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")
	ctx := context.Background()

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first, outcome, err := New(st).Accept(ctx, samplePlacement())
	if err != nil || outcome != Created {
		t.Fatalf("first accept: outcome=%d err=%v", outcome, err)
	}
	want, _ := json.Marshal(first)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	svc := New(reopened)

	// The pre-restart record is readable.
	got, err := reopened.Get(ctx, "team-a", "job-1")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if data, _ := json.Marshal(got); string(data) != string(want) {
		t.Fatalf("record changed across reopen:\n%s\n%s", data, want)
	}

	// Same content retries against the stored record.
	rec, outcome, err := svc.Accept(ctx, samplePlacement())
	if err != nil || outcome != Identical {
		t.Fatalf("retry after reopen: outcome=%d err=%v", outcome, err)
	}
	if data, _ := json.Marshal(rec); string(data) != string(want) {
		t.Fatalf("retry after reopen returned a different record:\n%s\n%s", data, want)
	}

	// Different content still conflicts and leaves the stored record alone.
	changed := samplePlacement()
	changed.Priority = 99
	if _, outcome, err = svc.Accept(ctx, changed); err != nil || outcome != Conflict {
		t.Fatalf("conflict after reopen: outcome=%d err=%v", outcome, err)
	}
	got, err = reopened.Get(ctx, "team-a", "job-1")
	if err != nil {
		t.Fatalf("get after conflict: %v", err)
	}
	if data, _ := json.Marshal(got); string(data) != string(want) {
		t.Fatalf("conflict rewrote the stored record:\n%s\n%s", data, want)
	}
}
