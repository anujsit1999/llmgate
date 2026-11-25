# Adding a provider

Most vendors already speak the OpenAI Chat Completions protocol. For those you
**don't need code**: add a provider block with `type: openai_compatible` and
its `base_url`, plus `headers` for any extra static headers it needs.

For a vendor with its own protocol, you write **one file**. The router, the
server, the metrics and the config loader never change.

## 1. Implement the interface

Every adapter satisfies `provider.Provider` (see `internal/provider/provider.go`):

```go
type Provider interface {
    Name() string
    Complete(ctx context.Context, req Request) (*Response, error)
    Stream(ctx context.Context, req Request, emit func(Chunk) error) error
}
```

Your adapter translates the gateway's neutral `Request` into the vendor's
format, and translates the vendor's reply back into `Response` / `Chunk`.
Because every adapter returns the same types, clients always get the same
OpenAI-shaped response, whichever vendor served it.

## 2. Register it

```go
// internal/provider/acme.go
package provider

import (
    "context"
    "errors"

    "github.com/anujsit1999/llmgate/internal/config"
)

func init() {
    Register("acme", func(ctx context.Context, name string, cfg config.Provider) (Provider, error) {
        key := cfg.APIKey()
        if key == "" {
            return nil, errors.New("api key missing")
        }
        return &Acme{name: name, key: key, baseURL: cfg.BaseURL}, nil
    })
}

type Acme struct{ name, key, baseURL string }

func (a *Acme) Name() string { return a.name }

func (a *Acme) Complete(ctx context.Context, req Request) (*Response, error) {
    // 1. build the vendor request from req.System, req.Messages, req.MaxTokens
    // 2. merge req.Extra (model-specific params) into the body
    // 3. call the API with ctx so timeouts and cancellation propagate
    // 4. map errors with newStatusError(a.name, status, err) so fallback works
    // 5. return &Response{Content: ..., FinishReason: "stop", Usage: ...}
    return nil, errors.New("TODO")
}

func (a *Acme) Stream(ctx context.Context, req Request, emit func(Chunk) error) error {
    // call emit(Chunk{Delta: text}) per increment, and finish with
    // emit(Chunk{FinishReason: "stop", Usage: &usage})
    return errors.New("TODO")
}
```

`init()` runs automatically when the package loads, so `type: acme` is usable
in config right away.

## 3. Rules every adapter must follow

1. **Classify errors.** Return `*provider.Error` with `Retryable: true` for
   429, 5xx and network failures. That's what enables fallback to the next target.
2. **Respect `ctx`.** Always build requests with the context you're given.
3. **Map finish reasons** to `stop`, `length`, `content_filter` or `tool_calls`.
4. **Report usage** (input and output tokens). Cost metering depends on it.
5. **Never log prompt content or credentials.**
6. **Test with `httptest.Server`.** `openai_test.go` is a template.
