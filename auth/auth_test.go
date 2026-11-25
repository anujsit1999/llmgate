package auth

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anujsit1999/llmgate/config"
)

func TestAuthenticate(t *testing.T) {
	key, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, KeyPrefix) {
		t.Fatalf("key %q lacks prefix", key)
	}
	a := New([]config.Key{{ID: "k1", Tenant: "acme", Hash: hash, Models: []string{"fast"}, RPM: 10}})

	// Table-driven tests are the idiomatic Go style: one loop, many cases.
	tests := []struct {
		name    string
		header  string
		value   string
		wantErr bool
	}{
		{"bearer", "Authorization", "Bearer " + key, false},
		{"bearer lowercase scheme", "Authorization", "bearer " + key, false},
		{"x-api-key", "X-API-Key", key, false},
		{"missing", "", "", true},
		{"wrong key", "Authorization", "Bearer lg_nope", true},
		{"no prefix", "Authorization", "Bearer " + strings.TrimPrefix(key, KeyPrefix), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			p, err := a.Authenticate(r)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (p.Tenant != "acme" || p.KeyID != "k1") {
				t.Fatalf("unexpected principal %+v", p)
			}
		})
	}
}

func TestCanUse(t *testing.T) {
	scoped := Principal{models: []string{"fast"}}
	if !scoped.CanUse("fast") || scoped.CanUse("smart") {
		t.Fatal("scoped key permissions wrong")
	}
	if !(Principal{}).CanUse("anything") {
		t.Fatal("unscoped key should allow every model")
	}
}
