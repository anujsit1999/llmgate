package router

import (
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/anujsit1999/llmgate/config"
)

// load is a per-request snapshot of a target's load, fetched once so a
// shared store is queried at most once per target per request.
type load struct {
	inFlight float64
	rpm, tpm float64
}

// strategy orders a model's targets for one request. The first entry is
// the preferred target; the rest are failover candidates in order.
type strategy interface {
	order(ts []*Target, l map[*Target]load) []*Target
}

func newStrategy(name string, n int) strategy {
	switch name {
	case config.StrategyRoundRobin:
		return &roundRobin{}
	case config.StrategyWeighted:
		return &weighted{current: make([]int, n)}
	case config.StrategyLeastBusy:
		return &sorted{key: func(t *Target, l load) float64 { return l.inFlight }}
	case config.StrategyUsageBased:
		return &sorted{key: utilisation}
	default:
		return priority{}
	}
}

// priority keeps list order: classic primary/secondary failover.
type priority struct{}

func (priority) order(ts []*Target, _ map[*Target]load) []*Target { return slices.Clone(ts) }

// roundRobin rotates the starting target on every request.
type roundRobin struct{ next atomic.Uint64 }

func (r *roundRobin) order(ts []*Target, _ map[*Target]load) []*Target {
	n := len(ts)
	start := int((r.next.Add(1) - 1) % uint64(n))
	out := make([]*Target, n)
	for i := range n {
		out[i] = ts[(start+i)%n]
	}
	return out
}

// weighted is smooth weighted round-robin (the algorithm nginx uses): with
// weights 5,1,1 it yields a,a,b,a,c,a,a — proportional and evenly spread,
// with no randomness.
type weighted struct {
	mu      sync.Mutex
	current []int
}

func (w *weighted) order(ts []*Target, _ map[*Target]load) []*Target {
	w.mu.Lock()
	total, best := 0, 0
	for i, t := range ts {
		w.current[i] += t.Weight
		total += t.Weight
		if w.current[i] > w.current[best] {
			best = i
		}
	}
	w.current[best] -= total
	w.mu.Unlock()

	out := make([]*Target, 0, len(ts))
	out = append(out, ts[best])
	for i, t := range ts {
		if i != best {
			out = append(out, t)
		}
	}
	return out
}

// sorted ranks targets by a load metric (lowest first). Ties are broken by
// rotating first, so idle deployments still share traffic evenly.
type sorted struct {
	rr  roundRobin
	key func(*Target, load) float64
}

func (s *sorted) order(ts []*Target, l map[*Target]load) []*Target {
	out := s.rr.order(ts, l)
	keys := make(map[*Target]float64, len(out))
	for _, t := range out {
		keys[t] = s.key(t, l[t])
	}
	sort.SliceStable(out, func(i, j int) bool { return keys[out[i]] < keys[out[j]] })
	return out
}

// utilisation is the busier of a target's request and token usage relative
// to its configured capacity (0 when no capacity is configured).
func utilisation(t *Target, l load) float64 {
	u := 0.0
	if t.RPM > 0 {
		u = max(u, l.rpm/float64(t.RPM))
	}
	if t.TPM > 0 {
		u = max(u, l.tpm/float64(t.TPM))
	}
	return u
}

// hasCapacity reports whether a request of ~estTokens fits within the
// target's per-minute limits.
func hasCapacity(t *Target, l load, estTokens int) bool {
	if t.RPM > 0 && l.rpm+1 > float64(t.RPM) {
		return false
	}
	if t.TPM > 0 && l.tpm+float64(estTokens) > float64(t.TPM) {
		return false
	}
	return true
}
