package telemetry

import (
	"sort"
	"sync"

	"go.opentelemetry.io/otel/attribute"
)

// MethodLabel is a metric label value that cannot be a user-supplied string.
//
// A Prometheus time series exists for every unique label combination. A
// `path` label containing "/users/12345" produces one series per user; at a
// million users that is a million series and the metrics backend dies,
// usually during the incident you built the metrics for.
//
// Documentation does not prevent this. A closed type does: the field is
// unexported, so the only way to obtain a MethodLabel is through
// FromProcedure, which takes a Connect procedure — always the static
// template "/package.Service/Method" generated from the proto, never
// anything a request can influence.
//
// This is the kind of guarantee a framework can offer and a library cannot,
// and it is one of the better arguments for this project existing.
type MethodLabel struct{ value string }

// FromProcedure builds a label from a Connect procedure name.
func FromProcedure(procedure string) MethodLabel { return MethodLabel{value: procedure} }

// String returns the label value.
func (m MethodLabel) String() string { return m.value }

// Attr returns the OTel attribute, using the semantic convention name so
// off-the-shelf dashboards work.
func (m MethodLabel) Attr() attribute.KeyValue {
	return attribute.String("rpc.method", m.value)
}

// PeerLabel is the same idea for the name of a service being called. It
// comes from configuration, not from a request, so a hostile caller cannot
// mint series by varying a header.
type PeerLabel struct{ value string }

// FromConfiguredPeer builds a peer label from a configured target name.
func FromConfiguredPeer(name string) PeerLabel { return PeerLabel{value: name} }

// String returns the label value.
func (p PeerLabel) String() string { return p.value }

// Attr returns the OTel attribute.
func (p PeerLabel) Attr() attribute.KeyValue {
	return attribute.String("server.address", p.value)
}

// CardinalityGuard counts distinct label combinations per metric and
// reports when one grows past a threshold.
//
// Catching this in staging is worth a great deal; catching it in production
// means someone is already paging. The guard is bounded itself — it stops
// tracking a metric once it is clearly out of control, because a guard that
// OOMs the process it is protecting has made things worse.
type CardinalityGuard struct {
	threshold int
	hardCap   int
	onExceed  func(metric string, series int)

	mu       sync.Mutex
	series   map[string]map[string]struct{}
	reported map[string]bool
	stopped  map[string]bool
}

// NewCardinalityGuard returns a guard. threshold is the number of distinct
// series at which onExceed fires; hardCap is where the guard stops tracking
// a runaway metric.
func NewCardinalityGuard(threshold, hardCap int, onExceed func(string, int)) *CardinalityGuard {
	if threshold < 1 {
		threshold = 1000
	}
	if hardCap < threshold {
		hardCap = threshold * 10
	}
	return &CardinalityGuard{
		threshold: threshold,
		hardCap:   hardCap,
		onExceed:  onExceed,
		series:    make(map[string]map[string]struct{}),
		reported:  make(map[string]bool),
		stopped:   make(map[string]bool),
	}
}

// Observe records one label combination for a metric.
func (g *CardinalityGuard) Observe(metric string, attrs []attribute.KeyValue) {
	key := seriesKey(attrs)

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.stopped[metric] {
		return
	}
	set, ok := g.series[metric]
	if !ok {
		set = make(map[string]struct{})
		g.series[metric] = set
	}
	set[key] = struct{}{}

	n := len(set)
	if n >= g.threshold && !g.reported[metric] {
		g.reported[metric] = true
		if g.onExceed != nil {
			g.onExceed(metric, n)
		}
	}
	if n >= g.hardCap {
		// Stop tracking. The warning has already been emitted, and holding
		// the full key set of a runaway metric is the guard becoming the
		// leak.
		g.stopped[metric] = true
		g.series[metric] = nil
	}
}

// Snapshot returns the series count per metric, for the admin endpoint.
func (g *CardinalityGuard) Snapshot() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]int, len(g.series))
	for metric, set := range g.series {
		if g.stopped[metric] {
			out[metric] = -1 // sentinel: stopped tracking
			continue
		}
		out[metric] = len(set)
	}
	return out
}

func seriesKey(attrs []attribute.KeyValue) string {
	parts := make([]string, 0, len(attrs))
	for _, a := range attrs {
		parts = append(parts, string(a.Key)+"="+a.Value.Emit())
	}
	// Sorted, so the same label set in a different order is one series
	// rather than two.
	sort.Strings(parts)
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
