package service

import (
	"testing"

	"github.com/Bob-xisuke/orb-scheduler-core/internal/model"
)

func placementFor(nodes ...model.Node) *model.Placement {
	return &model.Placement{
		Namespace: "ns",
		Name:      "job",
		Queue:     "q",
		Priority:  0,
		Resources: model.Resources{CPU: 100, Memory: 64},
		Selector:  map[string]string{},
		Nodes:     nodes,
	}
}

func node(name string, cpu, mem int64, labels map[string]string) model.Node {
	if labels == nil {
		labels = map[string]string{}
	}
	return model.Node{Name: name, CPU: cpu, Memory: mem, Labels: labels}
}

func TestSchedulePicksByteSmallestEligibleNode(t *testing.T) {
	p := placementFor(
		node("node-b", 1000, 512, map[string]string{"zone": "cn", "ssd": "true"}),
		node("node-a", 1000, 512, map[string]string{"zone": "cn"}),
		node("node-c", 100, 128, map[string]string{"zone": "cn"}),
		node("node-d", 1000, 512, map[string]string{"zone": "us"}),
	)
	status, picked, reason := schedule(p)
	if status != StatusPlaced || picked == nil || *picked != "node-a" || reason != nil {
		t.Fatalf("decision = %q %v %v, want placed/node-a/nil", status, picked, reason)
	}
}

func TestScheduleRejectsEmptyCandidateList(t *testing.T) {
	p := placementFor()
	status, picked, reason := schedule(p)
	if status != StatusRejected || picked != nil || reason == nil || *reason != ReasonNoNode {
		t.Fatalf("decision = %q %v %v, want rejected/nil/no_eligible_node", status, picked, reason)
	}
}

func TestScheduleRejectsWhenNoNodeFits(t *testing.T) {
	p := placementFor(
		node("node-a", 99, 512, nil),  // CPU too small
		node("node-b", 1000, 63, nil), // memory too small
	)
	status, picked, reason := schedule(p)
	if status != StatusRejected || picked != nil || reason == nil || *reason != ReasonNoNode {
		t.Fatalf("decision = %q %v %v, want rejected", status, picked, reason)
	}
}

func TestScheduleRequiresEverySelectorPair(t *testing.T) {
	p := placementFor(
		node("node-a", 1000, 512, map[string]string{"zone": "cn"}),
		node("node-b", 1000, 512, map[string]string{"zone": "cn", "ssd": "true"}),
	)
	p.Selector = map[string]string{"zone": "cn", "ssd": "true"}
	status, picked, _ := schedule(p)
	if status != StatusPlaced || picked == nil || *picked != "node-b" {
		t.Fatalf("decision = %q %v, want placed/node-b", status, picked)
	}

	// A wrong value for a present key fails containment just like a missing key.
	p.Selector = map[string]string{"zone": "us"}
	if status, _, _ = schedule(p); status != StatusRejected {
		t.Fatalf("selector value mismatch must reject, got %q", status)
	}
}

func TestScheduleUsesUTF8ByteOrder(t *testing.T) {
	// 'a' (0x61) < 'z' (0x7A) < 'ä' (0xC3..) < '日' (0xE6..).
	p := placementFor(
		node("日", 1000, 512, nil),
		node("ä", 1000, 512, nil),
		node("z", 1000, 512, nil),
		node("a", 1000, 512, nil),
	)
	_, picked, _ := schedule(p)
	if picked == nil || *picked != "a" {
		t.Fatalf("picked = %v, want a", picked)
	}
}

func TestScheduleAcceptsExactCapacityAndIgnoresNilSelector(t *testing.T) {
	p := placementFor(node("node-a", 100, 64, nil))
	p.Selector = nil
	status, picked, _ := schedule(p)
	if status != StatusPlaced || picked == nil || *picked != "node-a" {
		t.Fatalf("exact-capacity node with nil selector must place, got %q %v", status, picked)
	}
}

func TestSchedulePriorityDoesNotChangePick(t *testing.T) {
	nodes := []model.Node{
		node("node-a", 1000, 512, nil),
		node("node-b", 1000, 512, nil),
	}
	low := placementFor(nodes...)
	low.Priority = -10
	high := placementFor(nodes...)
	high.Priority = 10
	_, pickedLow, _ := schedule(low)
	_, pickedHigh, _ := schedule(high)
	if *pickedLow != "node-a" || *pickedHigh != "node-a" {
		t.Fatalf("priority changed the pick: %v %v", pickedLow, pickedHigh)
	}
}
