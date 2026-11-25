// Package auth authenticates clients by API key.
//
// Keys are never stored in plaintext: the gateway keeps only SHA-256 hashes
// and looks a presented key up by its hash. Because the lookup is keyed on a
// cryptographic hash, response timing reveals nothing useful about the secret.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/anujsit1999/llmgate/config"
)

// KeyPrefix marks llmgate keys so secret scanners (and our own redactor) can spot them.
const KeyPrefix = "lg_"

// ErrUnauthenticated is returned for a missing or unknown key.
var ErrUnauthenticated = errors.New("invalid or missing API key")

// Principal is the authenticated caller.
type Principal struct {
	KeyID  string
	Tenant string
	RPM    int
	models []string // empty = all models
}

// CanUse reports whether the caller may use the given model alias.
func (p Principal) CanUse(model string) bool {
	return len(p.models) == 0 || slices.Contains(p.models, model)
}

// Anonymous is the principal used when authentication is disabled:
// every model, no per-key rate limit.
func Anonymous() Principal { return Principal{KeyID: "anonymous", Tenant: "default"} }

// Authenticator resolves API keys to principals.
type Authenticator struct {
	byHash map[string]Principal
}

// New builds an Authenticator from configured keys.
func New(keys []config.Key) *Authenticator {
	a := &Authenticator{byHash: make(map[string]Principal, len(keys))}
	for _, k := range keys {
		a.byHash[strings.TrimPrefix(k.Hash, "sha256:")] = Principal{
			KeyID: k.ID, Tenant: k.Tenant, RPM: k.RPM, models: k.Models,
		}
	}
	return a
}

// Authenticate extracts a key from `Authorization: Bearer` or `x-api-key`.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	key := ""
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		key = strings.TrimSpace(h[7:])
	} else {
		key = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	if key == "" || !strings.HasPrefix(key, KeyPrefix) {
		return Principal{}, ErrUnauthenticated
	}
	p, ok := a.byHash[Hash(key)]
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}

// Hash returns the hex SHA-256 of a key.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Generate creates a new random key (256 bits of entropy) and its config hash.
func Generate() (key, configHash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	key = KeyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return key, "sha256:" + Hash(key), nil
}
