package router

import (
	"strings"
	"testing"

	"github.com/anujsit1999/llmgate/config"
)

func targets(weights ...int) []*Target {
	out := make([]*Target, len(weights))
	for i, w := range weights {
		out[i] = &Target{Target: config.Target{Model: string(rune('a' + i)), Weight: w}}
	}
	return out
}

func firstPicks(s strategy, ts []*Target, l map[*Target]load, n int) string {
	var sb strings.Builder
	for range n {
		sb.WriteString(s.order(ts, l)[0].Model)
	}
	return sb.String()
}

func TestRoundRobin(t *testing.T) {
	if got := firstPicks(&roundRobin{}, targets(1, 1, 1), nil, 6); got != "abcabc" {
		t.Fatalf("got %s", got)
	}
}

func TestSmoothWeighted(t *testing.T) {
	ts := targets(5, 1, 1)
	got := firstPicks(&weighted{current: make([]int, 3)}, ts, nil, 7)
	if got != "aabacaa" {
		t.Fatalf("smooth WRR 5:1:1 = %s, want aabacaa", got)
	}
}

func TestOrderKeepsFailoverCandidates(t *testing.T) {
	ts := targets(1, 1, 1)
	if got := len((&weighted{current: make([]int, 3)}).order(ts, nil)); got != 3 {
		t.Fatalf("every target must remain as a failover candidate, got %d", got)
	}
}

func TestLeastBusy(t *testing.T) {
	ts := targets(1, 1, 1)
	l := map[*Target]load{ts[0]: {inFlight: 5}, ts[1]: {inFlight: 1}, ts[2]: {inFlight: 3}}
	s := newStrategy(config.StrategyLeastBusy, 3)
	if got := s.order(ts, l); got[0] != ts[1] || got[1] != ts[2] || got[2] != ts[0] {
		t.Fatalf("least busy order wrong: %s%s%s", got[0].Model, got[1].Model, got[2].Model)
	}
}

func TestUsageBasedAndCapacity(t *testing.T) {
	ts := targets(1, 1)
	ts[0].TPM, ts[1].TPM = 1000, 1000
	l := map[*Target]load{ts[0]: {tpm: 900}, ts[1]: {tpm: 100}}
	s := newStrategy(config.StrategyUsageBased, 2)
	if s.order(ts, l)[0] != ts[1] {
		t.Fatal("usage_based should prefer the less utilised deployment")
	}
	if hasCapacity(ts[0], l[ts[0]], 200) {
		t.Fatal("900 + 200 > 1000 tpm: over capacity")
	}
	if !hasCapacity(ts[1], l[ts[1]], 200) {
		t.Fatal("100 + 200 <= 1000 tpm: has capacity")
	}
}
