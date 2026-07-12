# Exercise 1: Machine Identity with AppRole and KV v2 Secret Retrieval

A service that reads its configuration from Vault must first prove *who it is*
without a secret baked into its image, then read a static application secret from
the versioned KV v2 engine. This exercise builds that config loader: AppRole login
for machine identity, a KV v2 read that respects the `/data/` path rewrite, and a
wrapper that hands the caller a typed struct instead of `map[string]interface{}`.

Because a live Vault is an external dependency, the runnable code and tests drive an
`httptest.Server` that speaks Vault's HTTP wire protocol; the production
secure-introduction login (response-wrapped `secret_id`) sits behind a
`//go:build integration` tag. This module is fully self-contained: its own
`go mod init`, its own demo, its own tests.

## What you'll build

```text
secretloader/                 independent module: example.com/secretloader
  go.mod                      requires hashicorp/vault/api + .../auth/approle
  loader.go                   Loader, AppConfig, Authenticate, LoadConfig, sentinels
  fake.go                     FakeVault: httptest server speaking Vault's wire protocol
  login.go                    //go:build integration: AuthenticateFromEnv (wrapped secret_id)
  cmd/
    demo/
      main.go                 runnable demo against the fake
  loader_test.go              table tests + forbidden-login test + Example
  login_integration_test.go   //go:build integration: live vault -dev smoke test
```

- Files: `loader.go`, `fake.go`, `login.go`, `cmd/demo/main.go`, `loader_test.go`, `login_integration_test.go`.
- Implement: `New(addr)`, `Authenticate` (AppRole via `approle.NewAppRoleAuth` + `client.Auth().Login`), and `LoadConfig` (via `client.KVv2(mount).Get`) returning a typed `AppConfig`; sentinel errors wrapped with `%w`.
- Test: an `httptest` fake routes `/v1/auth/approle/login` and `/v1/secret/data/<path>`; assert the typed fields, that the KV read hit a `/data/` path, and that a 403 login surfaces `ErrAuth`.
- Verify: `go test -tags integration ./...` against `vault server -dev`; `go test ./...` for the offline fake path.

Set up the module. The Vault client and the AppRole helper are external modules, so
fetch them:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/07-secrets-management-vault/01-approle-kv-secret-loader/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/07-secrets-management-vault/01-approle-kv-secret-loader
go mod edit -go=1.26
go get github.com/hashicorp/vault/api
go get github.com/hashicorp/vault/api/auth/approle
```

### Why a wrapper, and why AppRole

Two design decisions drive this exercise. The first is *never let the Vault SDK's
`map[string]interface{}` leak into your application*. The client returns loosely
typed data; every consumer that pulls fields out of that map with ad-hoc type
assertions is a place a typo silently yields a zero value. The `Loader` does the
assertions once, validates that the required fields are present, and returns a typed
`AppConfig`. Business code sees a struct, not a map.

The second is *identity without secret-zero*. AppRole splits identity into a
`role_id` (non-sensitive, can live in config) and a `secret_id` (sensitive,
short-lived). The Go helper `approle.NewAppRoleAuth(roleID, secretID, opts...)`
builds an `api.AuthMethod`; `client.Auth().Login(ctx, method)` performs the login
and — this is the part people miss — installs the returned client token on the
client for you, so subsequent reads are authenticated without a manual
`client.SetToken`. In production the `secret_id` is delivered as a response-wrapping
token: `approle.WithWrappingToken()` tells the helper the value it holds is a
single-use wrapping token to be unwrapped into the real `secret_id`. That path needs
a real Vault to unwrap against, so it lives behind the `integration` tag; the
default path logs in with a plain `secret_id` against the fake.

### The KV v2 /data/ rewrite

KV v2 versions secrets, and to do so it stores payloads under an internal `data/`
segment. `client.KVv2("secret").Get(ctx, "myapp")` reads `secret/data/myapp` on the
wire and returns a `*api.KVSecret` whose `Data` is the payload and whose
`VersionMetadata.Version` is the version number. Two behaviors matter for correct
error handling: `Get` returns the sentinel `api.ErrSecretNotFound` when the path has
no secret (not a nil result), and the payload lives one level deeper than a naive
`Logical().Read` would look. `LoadConfig` classifies `api.ErrSecretNotFound` into
its own `ErrSecretNotFound` so callers never import the Vault SDK to branch on it.

Create `loader.go`:

```go
package secretloader

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/vault/api"
	auth "github.com/hashicorp/vault/api/auth/approle"
)

// Sentinel errors so callers can classify failures with errors.Is without
// importing the Vault SDK.
var (
	ErrAuth             = errors.New("vault: authentication failed")
	ErrSecretNotFound   = errors.New("vault: secret not found")
	ErrIncompleteSecret = errors.New("vault: secret missing required fields")
)

// AppConfig is the typed view of the secret a caller receives, instead of the
// SDK's map[string]interface{}.
type AppConfig struct {
	DatabaseURL string
	APIKey      string
	Version     int
}

// Loader authenticates a service to Vault and reads its configuration secret.
type Loader struct {
	client  *api.Client
	kvMount string
}

// New builds a Loader whose client points at addr (a real Vault or an httptest
// fake). The KV v2 engine is assumed mounted at "secret".
func New(addr string) (*Loader, error) {
	cfg := api.DefaultConfig()
	if cfg.Error != nil {
		return nil, fmt.Errorf("default config: %w", cfg.Error)
	}
	client, err := api.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	if err := client.SetAddress(addr); err != nil {
		return nil, fmt.Errorf("set address: %w", err)
	}
	return &Loader{client: client, kvMount: "secret"}, nil
}

// Authenticate logs in with AppRole and installs the resulting token on the
// client. Callers pass approle.WithWrappingToken / WithMountPath as opts.
func (l *Loader) Authenticate(ctx context.Context, roleID string, secretID *auth.SecretID, opts ...auth.LoginOption) error {
	method, err := auth.NewAppRoleAuth(roleID, secretID, opts...)
	if err != nil {
		return fmt.Errorf("build approle method: %w", err)
	}
	secret, err := l.client.Auth().Login(ctx, method)
	if err != nil {
		return fmt.Errorf("approle login: %w", errors.Join(ErrAuth, err))
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return fmt.Errorf("approle login returned no token: %w", ErrAuth)
	}
	// Auth().Login already called client.SetToken with secret.Auth.ClientToken,
	// so no manual token install is needed here.
	return nil
}

// LoadConfig reads path from the KV v2 engine and returns a typed AppConfig.
func (l *Loader) LoadConfig(ctx context.Context, path string) (AppConfig, error) {
	kv, err := l.client.KVv2(l.kvMount).Get(ctx, path)
	if err != nil {
		if errors.Is(err, api.ErrSecretNotFound) {
			return AppConfig{}, fmt.Errorf("load %q: %w", path, ErrSecretNotFound)
		}
		return AppConfig{}, fmt.Errorf("load %q: %w", path, err)
	}
	if kv == nil || kv.Data == nil {
		return AppConfig{}, fmt.Errorf("load %q: %w", path, ErrSecretNotFound)
	}

	dbURL, _ := kv.Data["database_url"].(string)
	apiKey, _ := kv.Data["api_key"].(string)
	if dbURL == "" || apiKey == "" {
		return AppConfig{}, fmt.Errorf("load %q: %w", path, ErrIncompleteSecret)
	}

	cfg := AppConfig{DatabaseURL: dbURL, APIKey: apiKey}
	if kv.VersionMetadata != nil {
		cfg.Version = kv.VersionMetadata.Version
	}
	return cfg, nil
}
```

### The fake that speaks Vault's wire protocol

The client is a typed wrapper over Vault's REST API, so a handler that returns the
right JSON shapes *is* a Vault as far as the client is concerned. `FakeVault` routes
on the request path: `/v1/auth/approle/login` returns an `auth` object with a client
token, and `/v1/secret/data/` (a prefix, so it matches any KV path) returns the
`data.data` payload plus `data.metadata.version`. It records the login count and the
KV paths it was asked for, which lets a test prove the `/data/` rewrite happened.

Create `fake.go`:

```go
package secretloader

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// FakeVault is an in-memory stand-in that speaks enough of Vault's HTTP protocol
// to exercise the Loader without a real server.
type FakeVault struct {
	srv *httptest.Server

	mu          sync.Mutex
	loginHits   int
	kvPaths     []string
	loginStatus int
	token       string
	secret      map[string]any
	version     int
	kvStatus    int
}

// NewFakeVault starts a fake serving a default secret at any KV path.
func NewFakeVault() *FakeVault {
	f := &FakeVault{
		loginStatus: http.StatusOK,
		token:       "hvs.faketoken",
		secret: map[string]any{
			"database_url": "postgres://app@db.internal:5432/orders",
			"api_key":      "sk-live-abc123",
		},
		version:  3,
		kvStatus: http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/approle/login", f.handleLogin)
	mux.HandleFunc("/v1/secret/data/", f.handleKV)
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *FakeVault) URL() string { return f.srv.URL }
func (f *FakeVault) Close()      { f.srv.Close() }

// FailLogin makes the next logins return 403.
func (f *FakeVault) FailLogin() {
	f.mu.Lock()
	f.loginStatus = http.StatusForbidden
	f.mu.Unlock()
}

// SetSecret overrides the served KV payload and version.
func (f *FakeVault) SetSecret(data map[string]any, version int) {
	f.mu.Lock()
	f.secret = data
	f.version = version
	f.mu.Unlock()
}

// LoginHits reports how many login requests arrived.
func (f *FakeVault) LoginHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginHits
}

// KVPaths reports the request paths seen by the KV handler.
func (f *FakeVault) KVPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.kvPaths...)
}

func (f *FakeVault) handleLogin(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.loginHits++
	status, token := f.loginStatus, f.token
	f.mu.Unlock()

	if status != http.StatusOK {
		w.WriteHeader(status)
		io.WriteString(w, `{"errors":["invalid role or secret id"]}`)
		return
	}
	writeJSON(w, map[string]any{
		"auth": map[string]any{
			"client_token":   token,
			"lease_duration": 3600,
			"renewable":      true,
		},
	})
}

func (f *FakeVault) handleKV(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.kvPaths = append(f.kvPaths, r.URL.Path)
	status := f.kvStatus
	data := f.secret
	version := f.version
	f.mu.Unlock()

	if status != http.StatusOK {
		w.WriteHeader(status)
		io.WriteString(w, `{"errors":[]}`)
		return
	}
	writeJSON(w, map[string]any{
		"data": map[string]any{
			"data": data,
			"metadata": map[string]any{
				"version":      version,
				"created_time": "2026-01-01T00:00:00Z",
			},
		},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
```

### The production login path (integration only)

`AuthenticateFromEnv` is the secure-introduction path: the `role_id` comes from a
non-sensitive env var, and the `secret_id` is read from a file into which the
orchestrator projected a single-use response-wrapping token. `WithWrappingToken`
makes the helper unwrap it against Vault. It needs a real Vault to unwrap, so it is
compiled only with `-tags integration`.

Create `login.go`:

```go
//go:build integration

package secretloader

import (
	"context"
	"fmt"
	"os"

	auth "github.com/hashicorp/vault/api/auth/approle"
)

// AuthenticateFromEnv performs the production secure-introduction login: a
// non-sensitive role_id from the environment, and a secret_id delivered as a
// single-use response-wrapping token whose value the orchestrator wrote to a file.
func (l *Loader) AuthenticateFromEnv(ctx context.Context) error {
	roleID := os.Getenv("VAULT_ROLE_ID")
	if roleID == "" {
		return fmt.Errorf("VAULT_ROLE_ID not set: %w", ErrAuth)
	}
	secretID := &auth.SecretID{FromFile: os.Getenv("VAULT_SECRET_ID_FILE")}
	return l.Authenticate(ctx, roleID,
		secretID,
		auth.WithWrappingToken(),
		auth.WithMountPath("approle"),
	)
}
```

### The runnable demo

The demo starts the fake, authenticates with a plain `secret_id`, reads the config,
and prints the typed fields.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	auth "github.com/hashicorp/vault/api/auth/approle"

	"example.com/secretloader"
)

func main() {
	fake := secretloader.NewFakeVault()
	defer fake.Close()

	loader, err := secretloader.New(fake.URL())
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	if err := loader.Authenticate(ctx, "role-123", &auth.SecretID{FromString: "secret-abc"}); err != nil {
		log.Fatal(err)
	}
	fmt.Println("authenticated: token acquired")

	cfg, err := loader.LoadConfig(ctx, "myapp")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("database_url: %s\n", cfg.DatabaseURL)
	fmt.Printf("api_key: %s\n", cfg.APIKey)
	fmt.Printf("version: %d\n", cfg.Version)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
authenticated: token acquired
database_url: postgres://app@db.internal:5432/orders
api_key: sk-live-abc123
version: 3
```

### Tests

The table test drives the loader against the fake: a complete secret maps to the
typed struct including the version, and a secret missing `api_key` surfaces
`ErrIncompleteSecret`. The success case also asserts the KV handler saw exactly one
request whose path contains `/data/`, proving the v1-vs-v2 rewrite is understood. A
separate test forces a 403 login and asserts the wrapped `ErrAuth` surfaces via
`errors.Is`.

Create `loader_test.go`:

```go
package secretloader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	auth "github.com/hashicorp/vault/api/auth/approle"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		data    map[string]any
		version int
		want    AppConfig
		wantErr error
	}{
		{
			name:    "complete secret",
			data:    map[string]any{"database_url": "postgres://u@h/db", "api_key": "k1"},
			version: 5,
			want:    AppConfig{DatabaseURL: "postgres://u@h/db", APIKey: "k1", Version: 5},
		},
		{
			name:    "missing api_key",
			data:    map[string]any{"database_url": "postgres://u@h/db"},
			version: 2,
			wantErr: ErrIncompleteSecret,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := NewFakeVault()
			defer fake.Close()
			fake.SetSecret(tc.data, tc.version)

			loader, err := New(fake.URL())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx := context.Background()
			if err := loader.Authenticate(ctx, "r", &auth.SecretID{FromString: "s"}); err != nil {
				t.Fatalf("Authenticate: %v", err)
			}

			got, err := loader.LoadConfig(ctx, "myapp")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("LoadConfig err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if got != tc.want {
				t.Fatalf("LoadConfig = %+v, want %+v", got, tc.want)
			}

			paths := fake.KVPaths()
			if len(paths) != 1 || !strings.Contains(paths[0], "/data/") {
				t.Fatalf("KV path = %v, want exactly one path containing /data/", paths)
			}
		})
	}
}

func TestAuthenticateForbidden(t *testing.T) {
	t.Parallel()
	fake := NewFakeVault()
	defer fake.Close()
	fake.FailLogin()

	loader, err := New(fake.URL())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = loader.Authenticate(context.Background(), "r", &auth.SecretID{FromString: "s"})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("Authenticate err = %v, want ErrAuth", err)
	}
}

func Example() {
	fake := NewFakeVault()
	defer fake.Close()

	loader, _ := New(fake.URL())
	ctx := context.Background()
	_ = loader.Authenticate(ctx, "role-123", &auth.SecretID{FromString: "secret-abc"})
	cfg, _ := loader.LoadConfig(ctx, "myapp")

	fmt.Println(cfg.DatabaseURL)
	fmt.Println(cfg.Version)
	// Output:
	// postgres://app@db.internal:5432/orders
	// 3
}
```

The integration test runs only under `-tags integration` and skips unless
`VAULT_ADDR` and `VAULT_TOKEN` are set, so CI without a Vault never sees it.

Create `login_integration_test.go`:

```go
//go:build integration

package secretloader

import (
	"context"
	"os"
	"testing"
)

// TestLiveKV is a smoke test against `vault server -dev`. Export VAULT_ADDR and
// VAULT_TOKEN and write a KV v2 secret at secret/myapp first, e.g.:
//
//	vault kv put secret/myapp database_url=postgres://u@h/db api_key=k1
//
// Run with: go test -tags integration -run TestLiveKV
func TestLiveKV(t *testing.T) {
	addr := os.Getenv("VAULT_ADDR")
	token := os.Getenv("VAULT_TOKEN")
	if addr == "" || token == "" {
		t.Skip("set VAULT_ADDR and VAULT_TOKEN to run the live Vault test")
	}
	loader, err := New(addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The dev-mode root token authenticates us directly for this smoke test.
	loader.client.SetToken(token)

	cfg, err := loader.LoadConfig(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DatabaseURL == "" {
		t.Fatal("expected database_url in secret/myapp")
	}
}
```

## Review

The loader is correct when three things hold. First, `Authenticate` relies on
`Auth().Login` to install the token — do not add a redundant `client.SetToken` after
it; the login already did that, and the only place a manual `SetToken` belongs is
the dev-token smoke test. Second, `LoadConfig` reads through `KVv2(...).Get`, not a
raw `Logical().Read("secret/myapp")`; the test asserting the request path contains
`/data/` is what proves you did not hand-roll the wrong path. Third, a missing
secret surfaces as `ErrSecretNotFound` because `KVv2.Get` returns
`api.ErrSecretNotFound` (a sentinel), which `LoadConfig` classifies with
`errors.Is` — note this differs from a raw `Logical().Read`, which returns
`(nil, nil)` for a missing path. Confirm the negative path with the 403 test: the
error must satisfy `errors.Is(err, ErrAuth)` because `Authenticate` joins `ErrAuth`
into the wrap.

The common mistakes here are structural. Leaking `map[string]interface{}` into
callers, forgetting that the login already set the token, reading the wrong KV path,
and baking the `secret_id` into the image instead of delivering it as a wrapping
token are each a real production failure. Offline, this module cannot build without
the Vault modules fetched; run the gate against a populated module cache, and run
the integration test against `vault server -dev` for end-to-end proof.

## Resources

- [`vault/api` package reference](https://pkg.go.dev/github.com/hashicorp/vault/api) — `Client`, `KVv2`, `Secret`, `ErrSecretNotFound`.
- [`vault/api/auth/approle`](https://pkg.go.dev/github.com/hashicorp/vault/api/auth/approle) — `NewAppRoleAuth`, `SecretID`, `WithWrappingToken`, `WithMountPath`.
- [Authentication — Vault concepts](https://developer.hashicorp.com/vault/docs/concepts/auth) — AppRole, response wrapping, secure introduction.
- [hashicorp/vault-examples (Go)](https://github.com/hashicorp/vault-examples/tree/main/examples) — official auth and secret-read examples.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-dynamic-db-secrets-lease-lifecycle.md](02-dynamic-db-secrets-lease-lifecycle.md)
