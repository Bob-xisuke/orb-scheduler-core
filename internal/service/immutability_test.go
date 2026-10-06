package service

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
	"github.com/Bob-xisuke/orb-scheduler-core/internal/placement"
)

// This file pins the immutability boundary around the Accept and Query
// entries: once a legal request has been parsed, defaulted, trial-scheduled
// and submitted, later in-place edits of memory objects the caller or a reader
// still holds must never change what another entry point returns, and a
// conflicting submission must never replace the registered record.
//
// Two storage backends are exercised side by side:
//   - SQLite (newService): the production store. Submit's created branch
//     returns a SHALLOW copy of the caller's struct (internal/store/placement.go,
//     `out := *p`), so reference-typed fields share memory with the input
//     object until the row is read back; Get/List rebuild fresh objects with
//     model.DecodeInput (internal/store/placement.go decodeRecord).
//   - fakestore (newFakeService): the in-memory double deep-copies through
//     JSON on every Submit/Get/List (internal/service/fakestore/fakestore.go,
//     clone), so returned objects are always independent of the caller's.
//
// The two doubles deliberately differ in transient in-memory aliasing; both
// agree on the boundary that matters — committed content is fixed at Submit
// time and every Query decodes that content anew. The fakestore result is
// never used to infer the SQLite row's behavior; each backend is asserted
// from its own source path.

// immutEnv binds one backend to the entry points under test.
type immutEnv struct {
	name  string
	svc   *Service
	get   func(namespace, name string) *model.Placement
	count func() int
}

func immutEnvs(t *testing.T) []immutEnv {
	t.Helper()
	ctx := context.Background()

	st, sqliteSvc := newService(t)
	sqliteEnv := immutEnv{
		name: "sqlite",
		svc:  sqliteSvc,
		get: func(namespace, name string) *model.Placement {
			rec, err := st.Get(ctx, namespace, name)
			if err != nil {
				t.Fatalf("%s direct get: %v", "sqlite", err)
			}
			return rec
		},
		count: func() int {
			recs, err := st.List(ctx, model.ListFilter{})
			if err != nil {
				t.Fatalf("sqlite list: %v", err)
			}
			return len(recs)
		},
	}

	fake, fakeSvc := newFakeService()
	fakeEnv := immutEnv{
		name: "fake",
		svc:  fakeSvc,
		get: func(namespace, name string) *model.Placement {
			rec, err := fake.Get(ctx, namespace, name)
			if err != nil {
				t.Fatalf("fake get: %v", err)
			}
			return rec
		},
		count: func() int {
			recs, err := fake.List(ctx, model.ListFilter{})
			if err != nil {
				t.Fatalf("fake list: %v", err)
			}
			return len(recs)
		},
	}
	return []immutEnv{sqliteEnv, fakeEnv}
}

// A placed request exercising every field category: ordinary scalars
// (priority/resources), a selector map, an ordered candidate array and
// per-node label maps. Both nodes fit; n1 is byte-smallest and carries the
// extra ssd label, so the trial picks n1.
const immutPlacedBody = `{
  "namespace": "ns", "name": "job", "queue": "q", "priority": 1,
  "resources": {"cpu": 100, "memory": 64},
  "selector": {"zone": "cn"},
  "nodes": [
    {"name": "n1", "cpu": 200, "memory": 128, "labels": {"zone": "cn", "ssd": "true"}},
    {"name": "n2", "cpu": 200, "memory": 128, "labels": {"zone": "cn"}}
  ]
}`

// A rejected request: the single node cannot satisfy 2000 millicores, so
// status=rejected, node=null, reason="no_eligible_node". Selector and labels
// are omitted here and arrive as empty maps through normalization.
const immutRejectedBody = `{
  "namespace": "ns", "name": "rej", "queue": "q", "priority": 1,
  "resources": {"cpu": 2000, "memory": 256},
  "nodes": [{"name": "n1", "cpu": 1000, "memory": 512}]
}`

func parseImmut(t *testing.T, body string) *model.Placement {
	t.Helper()
	p, err := placement.ParsePlacementInput([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return p
}

func immutJSON(t *testing.T, p *model.Placement) string {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

func queryImmutRecord(t *testing.T, env immutEnv, namespace, name string) *model.Placement {
	t.Helper()
	result, err := env.svc.Query(context.Background(), url.Values{
		"namespace": {namespace},
		"name":      {name},
	})
	if err != nil {
		t.Fatalf("%s query: %v", env.name, err)
	}
	if result.Record == nil || result.Items != nil {
		t.Fatalf("%s query must set only Record: %+v", env.name, result)
	}
	return result.Record
}

// poisonInput edits one of each field category in place, including the
// scheduling-result pointers when set. The values are deliberately unlike
// anything the setup submitted.
func poisonInput(p *model.Placement) {
	p.Priority = 99
	p.Resources = model.Resources{CPU: 999, Memory: 999}
	if p.Selector != nil {
		p.Selector["zone"] = "ZZ"
		p.Selector["injected"] = "x"
	}
	if len(p.Nodes) > 0 {
		p.Nodes[0].Name = "mut-node"
		p.Nodes[0].CPU = 1
		if p.Nodes[0].Labels != nil {
			p.Nodes[0].Labels["zone"] = "LL"
		}
	}
	if p.Node != nil {
		*p.Node = "mut-choice"
	}
	if p.Reason != nil {
		*p.Reason = "mut-reason"
	}
}

func expectOriginalPlaced(t *testing.T, env immutEnv, got *model.Placement) {
	t.Helper()
	if got.Priority != 1 || got.Resources != (model.Resources{CPU: 100, Memory: 64}) {
		t.Fatalf("%s ordinary fields changed: %+v", env.name, got)
	}
	if got.Selector["zone"] != "cn" || len(got.Selector) != 1 {
		t.Fatalf("%s selector changed: %v", env.name, got.Selector)
	}
	if len(got.Nodes) != 2 || got.Nodes[0].Name != "n1" || got.Nodes[0].CPU != 200 ||
		got.Nodes[0].Labels["zone"] != "cn" || got.Nodes[0].Labels["ssd"] != "true" ||
		len(got.Nodes[0].Labels) != 2 || got.Nodes[1].Name != "n2" {
		t.Fatalf("%s candidate array or labels changed: %+v", env.name, got.Nodes)
	}
	if got.Status != StatusPlaced || got.Node == nil || *got.Node != "n1" || got.Reason != nil {
		t.Fatalf("%s scheduling result changed: %+v node=%v reason=%v",
			env.name, got.Status, got.Node, got.Reason)
	}
}

// SQLite-only: the record handed back from a first Submit is a shallow copy of
// the caller struct (internal/store/placement.go created branch, `out := *p`),
// so ordinary scalar fields are already detached while the reference-typed
// fields still alias the caller object. That aliasing is a transient property
// of the returned object only — canonical JSON was serialized into the row at
// Submit time, so the committed record and every later read stay untouched.
func TestImmutSQLiteCreatedReturnShallowCopiesCaller(t *testing.T) {
	env := immutEnvs(t)[0] // sqlite
	if env.name != "sqlite" {
		t.Fatalf("env ordering changed: %s", env.name)
	}
	ctx := context.Background()

	p := parseImmut(t, immutPlacedBody)
	rec, outcome, err := env.svc.Accept(ctx, p)
	if err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}
	original := immutJSON(t, rec)

	// The returned struct is not the caller's struct...
	if rec == p {
		t.Fatalf("created record must not be the caller struct itself")
	}
	// ...so an ordinary scalar field is already independent.
	p.Priority = 42
	if rec.Priority != 1 {
		t.Fatalf("scalar change leaked through the shallow copy: rec.Priority=%d", rec.Priority)
	}

	// Reference-typed fields DO alias the caller on the created path.
	// Selector map:
	p.Selector["zone"] = "ZZ"
	if rec.Selector["zone"] != "ZZ" {
		t.Fatalf("selector map not aliased on the SQLite created path: %v", rec.Selector)
	}
	// Candidate array backing storage (mutating an element in place):
	p.Nodes[0].Name = "mut-node"
	if rec.Nodes[0].Name != "mut-node" {
		t.Fatalf("nodes backing array not aliased on the SQLite created path: %+v", rec.Nodes)
	}
	// Node label maps:
	p.Nodes[0].Labels["ssd"] = "false"
	if rec.Nodes[0].Labels["ssd"] != "false" {
		t.Fatalf("labels map not aliased on the SQLite created path: %v", rec.Nodes[0].Labels)
	}
	// Scheduling-result pointer (schedule returns &n; Accept stores it on p,
	// and the shallow copy copies the pointer):
	if rec.Node != p.Node {
		t.Fatalf("scheduling node pointer not shared with caller on created path")
	}
	*p.Node = "mut-choice"
	if *rec.Node != "mut-choice" {
		t.Fatalf("node pointer target edit did not reach the returned record")
	}

	// Replacing the caller's slice header must not touch the returned slice.
	p.Nodes = []model.Node{{Name: "totally-other"}}
	if rec.Nodes[0].Name != "mut-node" || len(rec.Nodes) != 2 {
		t.Fatalf("slice header replacement leaked: %+v", rec.Nodes)
	}

	// The aliasing never reached storage: the row still holds the original
	// canonical bytes, both through the business Query entry and directly.
	expectOriginalPlaced(t, env, queryImmutRecord(t, env, "ns", "job"))
	expectOriginalPlaced(t, env, env.get("ns", "job"))
	if got := immutJSON(t, queryImmutRecord(t, env, "ns", "job")); got != original {
		t.Fatalf("re-query differs from the pre-mutation record:\n got %s\nwant %s", got, original)
	}
}

// The rejected counterpart shares the same shallow-copy boundary, including
// its reason pointer (node is null for a rejection and has no pointer to
// share).
func TestImmutSQLiteCreatedRejectedReasonPointerAliases(t *testing.T) {
	env := immutEnvs(t)[0] // sqlite
	ctx := context.Background()

	p := parseImmut(t, immutRejectedBody)
	rec, outcome, err := env.svc.Accept(ctx, p)
	if err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}
	if rec.Status != StatusRejected || rec.Node != nil || rec.Reason == nil {
		t.Fatalf("setup: want rejected record, got %+v", rec)
	}
	if rec.Reason != p.Reason {
		t.Fatalf("reason pointer not shared with caller on created path")
	}
	*p.Reason = "mut-reason"
	if *rec.Reason != "mut-reason" {
		t.Fatalf("reason pointer edit did not reach the returned record")
	}

	got := queryImmutRecord(t, env, "ns", "rej")
	if got.Status != StatusRejected || got.Node != nil ||
		got.Reason == nil || *got.Reason != ReasonNoNode {
		t.Fatalf("committed rejection changed: %+v", got)
	}
}

// fakestore-only: its Submit/Get/List all hand back JSON deep copies
// (internal/service/fakestore/fakestore.go clone), so even the first returned
// record is independent of the caller object in every field. This is stronger
// isolation than the SQLite created path; the committed-content boundary is
// identical.
func TestImmutFakeCreatedReturnIsDeepCopy(t *testing.T) {
	env := immutEnvs(t)[1] // fake
	if env.name != "fake" {
		t.Fatalf("env ordering changed: %s", env.name)
	}
	ctx := context.Background()

	p := parseImmut(t, immutPlacedBody)
	rec, outcome, err := env.svc.Accept(ctx, p)
	if err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}
	if rec == p {
		t.Fatalf("created record must not be the caller struct itself")
	}
	original := immutJSON(t, rec)

	poisonInput(p)
	if got := immutJSON(t, rec); got != original {
		t.Fatalf("deep copy changed with the caller object:\n got %s\nwant %s", got, original)
	}
	expectOriginalPlaced(t, env, queryImmutRecord(t, env, "ns", "job"))
	expectOriginalPlaced(t, env, env.get("ns", "job"))
}

// On both backends, an identical retry returns the registered original (200
// semantics), and that object is rebuilt from storage — SQLite reloads the row
// through decodeRecord after the unique-constraint collision, the double
// clones its kept record — so poisoning the retry input afterwards changes
// neither the returned object nor the store, and no second record appears.
func TestImmutIdenticalRetryReturnsStoredOriginalOnBothBackends(t *testing.T) {
	for _, env := range immutEnvs(t) {
		t.Run(env.name, func(t *testing.T) {
			ctx := context.Background()

			first, outcome, err := env.svc.Accept(ctx, parseImmut(t, immutPlacedBody))
			if err != nil || outcome != Created {
				t.Fatalf("first: outcome=%d err=%v", outcome, err)
			}
			firstJSON := immutJSON(t, first)

			retry := parseImmut(t, immutPlacedBody)
			again, outcome, err := env.svc.Accept(ctx, retry)
			if err != nil || outcome != Identical {
				t.Fatalf("retry: outcome=%d err=%v, want Identical", outcome, err)
			}
			if immutJSON(t, again) != firstJSON {
				t.Fatalf("retry did not return the original record")
			}

			poisonInput(retry)
			expectOriginalPlaced(t, env, again)
			expectOriginalPlaced(t, env, queryImmutRecord(t, env, "ns", "job"))
			if env.count() != 1 {
				t.Fatalf("retry changed record count: %d, want 1", env.count())
			}
		})
	}
}

// A conflicting submission returns the untouched original as Conflict on both
// backends; the conflicting object is never stored, and editing it after the
// call cannot touch the returned record. Includes the rejected case: a
// conflicting retry that would now place still loses to content comparison.
func TestImmutConflictLeavesOriginalAndReturnIndependentOnBothBackends(t *testing.T) {
	for _, env := range immutEnvs(t) {
		t.Run(env.name+"/placed", func(t *testing.T) {
			ctx := context.Background()
			first, outcome, err := env.svc.Accept(ctx, parseImmut(t, immutPlacedBody))
			if err != nil || outcome != Created {
				t.Fatalf("first: outcome=%d err=%v", outcome, err)
			}
			original := immutJSON(t, first)

			conflicting := parseImmut(t, immutPlacedBody)
			conflicting.Priority = 99
			rec, outcome, err := env.svc.Accept(ctx, conflicting)
			if err != nil || outcome != Conflict {
				t.Fatalf("conflict: outcome=%d err=%v, want Conflict", outcome, err)
			}
			if immutJSON(t, rec) != original {
				t.Fatalf("conflict must return the registered original")
			}

			poisonInput(conflicting)
			if immutJSON(t, rec) != original {
				t.Fatalf("conflict return aliased the rejected input object")
			}
			if got := immutJSON(t, queryImmutRecord(t, env, "ns", "job")); got != original {
				t.Fatalf("stored record rewritten by conflict:\n got %s\nwant %s", got, original)
			}
			if env.count() != 1 {
				t.Fatalf("conflict changed record count: %d, want 1", env.count())
			}
		})

		t.Run(env.name+"/rejected", func(t *testing.T) {
			ctx := context.Background()
			first, outcome, err := env.svc.Accept(ctx, parseImmut(t, immutRejectedBody))
			if err != nil || outcome != Created {
				t.Fatalf("first: outcome=%d err=%v", outcome, err)
			}
			if first.Status != StatusRejected {
				t.Fatalf("setup: want rejected, got %q", first.Status)
			}
			original := immutJSON(t, first)

			// Same identity, smaller request: this content would place, but
			// scheduling outcomes never participate in content comparison.
			placeable := parseImmut(t, immutRejectedBody)
			placeable.Resources = model.Resources{CPU: 100, Memory: 64}
			rec, outcome, err := env.svc.Accept(ctx, placeable)
			if err != nil || outcome != Conflict {
				t.Fatalf("conflict: outcome=%d err=%v, want Conflict", outcome, err)
			}
			if rec.Status != StatusRejected || rec.Node != nil ||
				rec.Reason == nil || *rec.Reason != ReasonNoNode || rec.Resources.CPU != 2000 {
				t.Fatalf("conflict did not return the original rejection: %+v", rec)
			}

			poisonInput(placeable)
			if got := immutJSON(t, rec); got != original {
				t.Fatalf("conflict return aliased the rejected input object")
			}
			got := queryImmutRecord(t, env, "ns", "rej")
			if immutJSON(t, got) != original {
				t.Fatalf("committed rejection rewritten:\n got %s\nwant %s", immutJSON(t, got), original)
			}
		})
	}
}

// Objects a reader obtains from Query (single record and list items) are
// disposable reconstructions on both backends (decodeRecord / clone): mutating
// them — including their maps, array elements and scheduling pointers — never
// reaches storage, the service's state, or the next query.
func TestImmutQueriedObjectsAreDisposableCopiesOnBothBackends(t *testing.T) {
	for _, env := range immutEnvs(t) {
		t.Run(env.name, func(t *testing.T) {
			ctx := context.Background()
			if _, outcome, err := env.svc.Accept(ctx, parseImmut(t, immutPlacedBody)); err != nil || outcome != Created {
				t.Fatalf("seed: outcome=%d err=%v", outcome, err)
			}
			original := immutJSON(t, env.get("ns", "job"))

			// Single-record form.
			rec := queryImmutRecord(t, env, "ns", "job")
			poisonInput(rec)
			if got := immutJSON(t, queryImmutRecord(t, env, "ns", "job")); got != original {
				t.Fatalf("single-record object was writable through:\n got %s\nwant %s", got, original)
			}
			expectOriginalPlaced(t, env, env.get("ns", "job"))

			// List form.
			list, err := env.svc.Query(ctx, url.Values{})
			if err != nil || len(list.Items) != 1 {
				t.Fatalf("list: %+v err=%v", list.Items, err)
			}
			poisonInput(list.Items[0])
			listAgain, err := env.svc.Query(ctx, url.Values{})
			if err != nil || len(listAgain.Items) != 1 {
				t.Fatalf("second list: %+v err=%v", listAgain.Items, err)
			}
			if got := immutJSON(t, listAgain.Items[0]); got != original {
				t.Fatalf("list item was writable through:\n got %s\nwant %s", got, original)
			}
		})
	}
}

// Ordering semantics through the Accept entry on both backends: JSON object
// member order is irrelevant to content (keys are sorted by the canonical
// encoding), while candidate-node array order IS content.
func TestImmutMemberOrderVersusArrayOrderOnBothBackends(t *testing.T) {
	body := `{
	  "namespace": "ns", "name": "ord", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`
	// Same content: members shuffled at every level, omitted defaults spelled
	// out, escape spellings avoided here (covered over HTTP in the api suite).
	membersReordered := `{
	  "nodes": [{"labels": {}, "memory": 10, "cpu": 10, "name": "n1"},
	            {"labels": {}, "memory": 10, "cpu": 10, "name": "n2"}],
	  "selector": {},
	  "resources": {"memory": 1, "cpu": 1},
	  "priority": 1, "queue": "q", "name": "ord", "namespace": "ns"
	}`
	// Different content: only the array order changes; both submissions
	// schedule onto n1, so an equal decision must not read as equal content.
	nodesSwapped := `{
	  "namespace": "ns", "name": "ord", "queue": "q", "priority": 1,
	  "resources": {"cpu": 1, "memory": 1},
	  "nodes": [
	    {"name": "n2", "cpu": 10, "memory": 10, "labels": {}},
	    {"name": "n1", "cpu": 10, "memory": 10, "labels": {}}
	  ]
	}`

	for _, env := range immutEnvs(t) {
		t.Run(env.name, func(t *testing.T) {
			ctx := context.Background()
			first, outcome, err := env.svc.Accept(ctx, parseImmut(t, body))
			if err != nil || outcome != Created {
				t.Fatalf("first: outcome=%d err=%v", outcome, err)
			}
			original := immutJSON(t, first)

			again, outcome, err := env.svc.Accept(ctx, parseImmut(t, membersReordered))
			if err != nil || outcome != Identical {
				t.Fatalf("reordered members: outcome=%d err=%v, want Identical", outcome, err)
			}
			if immutJSON(t, again) != original {
				t.Fatalf("member reorder returned a different record")
			}

			if _, outcome, err = env.svc.Accept(ctx, parseImmut(t, nodesSwapped)); err != nil || outcome != Conflict {
				t.Fatalf("swapped nodes: outcome=%d err=%v, want Conflict", outcome, err)
			}
			got := queryImmutRecord(t, env, "ns", "ord")
			if immutJSON(t, got) != original ||
				got.Nodes[0].Name != "n1" || got.Nodes[1].Name != "n2" {
				t.Fatalf("original node order did not survive: %s", immutJSON(t, got))
			}
			if env.count() != 1 {
				t.Fatalf("record count = %d, want 1", env.count())
			}
		})
	}
}

// Defaulting vs. comparison side effects. NormalizeInput fills defaults IN
// PLACE on the object Accept receives (internal/service/service.go calls
// model.NormalizeInput(p) before the trial), so a direct caller sees its
// struct filled. CanonicalInput/SameInput instead normalize a copy
// (internal/model/input.go CanonicalInput, `cp := *p`), so the content
// comparison Accept performs after a Submit never has the same side effect.
func TestImmutNormalizeFillsCallerButComparisonLeavesNil(t *testing.T) {
	env := immutEnvs(t)[0] // behavior lives in the service/model layers; one backend suffices
	ctx := context.Background()

	// Hand-built (not parsed): omitted selector and node labels are still nil.
	caller := &model.Placement{
		Namespace: "ns", Name: "job-norm", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Nodes:     []model.Node{{Name: "n1", CPU: 200, Memory: 128}},
	}
	if _, outcome, err := env.svc.Accept(ctx, caller); err != nil || outcome != Created {
		t.Fatalf("accept: outcome=%d err=%v", outcome, err)
	}
	if caller.Selector == nil || len(caller.Selector) != 0 {
		t.Fatalf("Accept must fill the omitted selector on the caller object: %v", caller.Selector)
	}
	if caller.Nodes[0].Labels == nil || len(caller.Nodes[0].Labels) != 0 {
		t.Fatalf("Accept must fill omitted labels on the caller object: %v", caller.Nodes[0].Labels)
	}
	if caller.Nodes == nil {
		t.Fatalf("Accept must leave a non-nil nodes array")
	}

	// SameInput on a still-omitted object compares equal AND fills nothing.
	omitted := &model.Placement{
		Namespace: "ns", Name: "x", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Nodes:     []model.Node{{Name: "n1", CPU: 200, Memory: 128}},
	}
	explicit := &model.Placement{
		Namespace: "ns", Name: "x", Queue: "q", Priority: 1,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     []model.Node{{Name: "n1", CPU: 200, Memory: 128, Labels: map[string]string{}}},
	}
	same, err := model.SameInput(omitted, explicit)
	if err != nil || !same {
		t.Fatalf("omitted defaults must compare equal: same=%v err=%v", same, err)
	}
	if omitted.Selector != nil || omitted.Nodes[0].Labels != nil {
		t.Fatalf("SameInput must not normalize its arguments: %+v", omitted)
	}
}
