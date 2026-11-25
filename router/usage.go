package router

import (
	"context"
	"sync/atomic"

	"github.com/anujsit1999/llmgate/state"
)

// usage tracks a deployment's load.
//
//   - inFlight is local to this replica: least_busy balances this replica's
//     own concurrency, which is what it can actually influence.
//   - requests/tokens per minute live in the state.Store, so with the Redis
//     backend every replica sees the deployment's total load and tpm/rpm
//     capacity is enforced cluster-wide.
//
// Store errors fail open (treated as no load) and are reported via onErr.
type usage struct {
	inFlight atomic.Int64
	key      string
	store    state.Store
	onErr    func(error)
}

func (u *usage) begin(ctx context.Context) {
	u.inFlight.Add(1)
	u.report(u.store.Add(ctx, u.key, map[string]float64{"req": 1}))
}

func (u *usage) end(ctx context.Context, tokens int) {
	u.inFlight.Add(-1)
	if tokens > 0 {
		// Record even if the request context is already cancelled: the tokens were spent.
		u.report(u.store.Add(context.WithoutCancel(ctx), u.key, map[string]float64{"tok": float64(tokens)}))
	}
}

// rates returns requests and tokens over the last minute, cluster-wide.
func (u *usage) rates(ctx context.Context) (rpm, tpm float64) {
	v, err := u.store.Rates(ctx, u.key, "req", "tok")
	if err != nil {
		u.report(err)
		return 0, 0
	}
	return v[0], v[1]
}

func (u *usage) report(err error) {
	if err != nil && u.onErr != nil {
		u.onErr(err)
	}
}
