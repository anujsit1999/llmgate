package llmgate_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anujsit1999/llmgate"
	"github.com/anujsit1999/llmgate/config"
	"github.com/anujsit1999/llmgate/provider"
)

// Example of embedding: a config built in code, a custom provider instance,
// auth switched off because the host app authenticates.
func TestEmbedAsLibrary(t *testing.T) {
	off := false
	cfg := &config.Config{
		Models:   []config.Model{{Name: "inhouse", Targets: []config.Target{{Provider: "mine", Model: "v1"}}}},
		Features: config.Features{Auth: &off},
	}
	mine := provider.NewMock("mine")
	mine.Delay = 0
	gw, err := llmgate.New(context.Background(), cfg,
		llmgate.WithProvider("mine", mine),
		llmgate.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"inhouse","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "mock(v1) received: hi") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}

	admin := httptest.NewServer(gw.AdminHandler())
	defer admin.Close()
	for _, path := range []string{"/readyz", "/metrics", "/upstreams"} {
		r, err := http.Get(admin.URL + path)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("%s: %v %v", path, err, r.StatusCode)
		}
		r.Body.Close()
	}
}

func TestFeatureTogglesHideAdminEndpoints(t *testing.T) {
	off := false
	cfg := &config.Config{
		Providers: map[string]config.Provider{"m": {Type: "mock"}},
		Models:    []config.Model{{Name: "x", Targets: []config.Target{{Provider: "m", Model: "x"}}}},
		Features:  config.Features{Metrics: &off, AdminAPI: &off},
	}
	gw, err := llmgate.New(context.Background(), cfg, llmgate.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	admin := httptest.NewServer(gw.AdminHandler())
	defer admin.Close()
	for _, path := range []string{"/metrics", "/upstreams"} {
		r, _ := http.Get(admin.URL + path)
		r.Body.Close()
		if r.StatusCode != 404 {
			t.Fatalf("%s should be disabled, got %d", path, r.StatusCode)
		}
	}
}
