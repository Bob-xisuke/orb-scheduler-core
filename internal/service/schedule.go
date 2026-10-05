package service

import "github.com/Bob-xisuke/orb-scheduler-core/internal/model"

// schedule performs the trial placement. Among nodes whose labels contain
// every selector pair and whose CPU and memory capacities both fit the
// request, it picks the name that is smallest in UTF-8 byte order. Capacity
// is only probed and never deducted; priority does not affect the choice.
// It returns the record's status together with its node and reason fields:
// placed with the chosen node and no reason, or rejected with no node and
// reason no_eligible_node. An empty candidate list rejects like any list
// without an eligible node.
func schedule(p *model.Placement) (status string, node *string, reason *string) {
	var chosen string
	found := false
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if !labelsContain(n.Labels, p.Selector) {
			continue
		}
		if n.CPU < p.Resources.CPU || n.Memory < p.Resources.Memory {
			continue
		}
		if !found || n.Name < chosen {
			chosen = n.Name
			found = true
		}
	}
	if found {
		n := chosen
		return StatusPlaced, &n, nil
	}
	r := ReasonNoNode
	return StatusRejected, nil, &r
}

// labelsContain reports whether labels holds every selector key with the same
// value.
func labelsContain(labels, selector map[string]string) bool {
	for key, want := range selector {
		if got, ok := labels[key]; !ok || got != want {
			return false
		}
	}
	return true
}
