package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/anujsit1999/llmgate/config"
)

// Factory builds a Provider from its config block. `name` is the key the
// operator gave the provider in config (e.g. "azure-eastus"); cfg.Type is the
// registered type (e.g. "azure_openai").
type Factory func(ctx context.Context, name string, cfg config.Provider) (Provider, error)

var (
	registryMu sync.RWMutex
	factories  = map[string]Factory{}
)

// Register makes a provider type available to config. Adapters call it from
// an init() function in their own file, so adding a provider never requires
// editing the router, the server or this file. See docs/adding-a-provider.md.
//
// Register panics on a duplicate type: that is a programming error that
// should fail at startup, not silently replace an adapter.
func Register(typ string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := factories[typ]; dup {
		panic("provider: Register called twice for type " + typ)
	}
	factories[typ] = f
}

// Build constructs a provider of the configured type.
func Build(ctx context.Context, name string, cfg config.Provider) (Provider, error) {
	registryMu.RLock()
	f, ok := factories[cfg.Type]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider %q: unknown type %q (available: %s)", name, cfg.Type, strings.Join(Types(), ", "))
	}
	p, err := f(ctx, name, cfg)
	if err != nil {
		return nil, fmt.Errorf("provider %q (%s): %w", name, cfg.Type, err)
	}
	return p, nil
}

// Types lists registered provider types, sorted.
func Types() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(factories))
	for t := range factories {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
