# Exercise 2: Dynamic Database Credentials with Lease Renewal and Revocation

Dynamic secrets are the reason a broker earns its cost: instead of a shared,
long-lived database password, every process gets its own credential that Vault
generated on demand and will revoke when its lease ends. This exercise builds the
credential broker that manages that lifecycle — fetch, inspect the lease, auto-renew
within the grace period with a `LifetimeWatcher`, and explicitly revoke on shutdown
so orphaned database users do not pile up.

Real-Vault calls need a live database secrets engine, so they sit behind a
`//go:build integration` tag; the default build and tests drive an `httptest` fake
serving the creds, renew, and revoke endpoints. This module is fully self-contained.

## What you'll build

```text
dbcreds/                       independent module: example.com/dbcreds
  go.mod                       requires hashicorp/vault/api
  broker.go                    Broker, DBCredential, Fetch, Watch, Revoke, Stop
  fake.go                      FakeVault: creds + sys/leases/renew + sys/leases/revoke
  cmd/
    demo/
      main.go                  runnable demo: fetch then revoke
  broker_test.go               lease parsing, missing-creds, revoke-records-id, auto-renew
  integration_test.go          //go:build integration: live database engine smoke test
```

- Files: `broker.go`, `fake.go`, `cmd/demo/main.go`, `broker_test.go`, `integration_test.go`.
- Implement: `Fetch` (via `client.Logical().ReadWithContext("database/creds/<role>")`), `Watch` (a `LifetimeWatcher` renewing within the grace period), `Revoke` (via `client.Sys().Revoke(leaseID)`), and `Stop`.
- Test: the fake serves `GET /v1/database/creds/<role>`, `PUT /v1/sys/leases/renew`, `PUT /v1/sys/leases/revoke`; assert parsed lease fields, that a renewal arrives, that revoke records the exact lease id, and that a missing path yields `ErrNoCredentials` (not a panic).
- Verify: `go test ./...` for the fake path; `go test -tags integration ./...` against a dev-mode database engine.

Set up the module:

```bash
go mod edit -go=1.26
go get github.com/hashicorp/vault/api
```

### The lease is the contract

Reading `database/creds/<role>` returns an `*api.Secret` whose `Data` holds the
generated `username`/`password` and whose top-level fields describe the lease:
`LeaseID` (path-prefixed, so a whole subtree can be revoked at once), `LeaseDuration`
(the TTL, in seconds), and `Renewable`. The broker converts `LeaseDuration` into a
`time.Duration` and hands back a typed `DBCredential`.

Two footguns hide here. First, `Logical().ReadWithContext` returns `(nil, nil)` for
a missing path — not an error — so the code must nil-check the `*api.Secret` before
touching `Data`, or it panics. (This is the opposite of the KV v2 helper in Exercise
1, which returns `api.ErrSecretNotFound`.) Second, `Secret.Warnings` carries
deprecations and partial-success notices; the broker surfaces them rather than
dropping them silently.

### Renewal with the LifetimeWatcher

`client.NewLifetimeWatcher` takes the fetched `*api.Secret` and renews the lease
automatically. Started with `go watcher.Start()`, it sleeps until a grace-period
threshold before expiry (with jitter, so a fleet does not stampede Vault), then
renews up to the lease's `max_ttl`. It exposes two channels:

- `RenewCh()` emits an `*api.RenewOutput` (with `RenewedAt` and the fresh `Secret`)
  on each successful renewal.
- `DoneCh()` fires exactly once, when renewal stops. The critical rule: **a
  `DoneCh` value, even a `nil` error, means the lease can no longer be extended —
  refetch or re-authenticate, do not keep using the credential.** The broker's
  `consume` loop treats `DoneCh` as a terminal "refetch now" signal, not "success."

`RenewBehavior` is set to `api.RenewBehaviorIgnoreErrors` (the default, `iota` 0) so
transient renewal failures do not immediately kill the watcher; it keeps trying
until `max_ttl` or an explicit stop.

### Explicit revocation bounds the blast radius

Relying only on TTL expiry leaves a real database user alive for the whole lease
after your process is gone. Multiply that by pod churn and you get lease explosions:
Vault's revocation queue and the database's role table fill with dead entries until
something degrades. `Revoke` calls `client.Sys().Revoke(leaseID)` on graceful
shutdown, which triggers the backing `DROP ROLE` immediately. It is a no-op when
there is no lease, so it is always safe to defer.

Create `broker.go`:

```go
package dbcreds

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"time"

	"github.com/hashicorp/vault/api"
)

// Sentinel errors so callers can classify failures without the Vault SDK.
var (
	ErrNoCredentials        = errors.New("dbcreds: no credentials at path")
	ErrIncompleteCredential = errors.New("dbcreds: credential missing username or password")
)

// DBCredential is a typed ephemeral database credential plus its lease.
type DBCredential struct {
	Username      string
	Password      string
	LeaseID       string
	LeaseDuration time.Duration
	Renewable     bool
}

// Broker fetches, renews, and revokes dynamic database credentials.
type Broker struct {
	client  *api.Client
	mount   string
	role    string
	secret  *api.Secret
	watcher *api.LifetimeWatcher

	renewed chan time.Time
	done    chan error
}

// New builds a Broker with an already-authenticated token installed (the token a
// login produced in Exercise 1). The database engine is assumed mounted at
// "database".
func New(addr, token, role string) (*Broker, error) {
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
	client.SetToken(token)
	return &Broker{
		client:  client,
		mount:   "database",
		role:    role,
		renewed: make(chan time.Time, 8),
		done:    make(chan error, 1),
	}, nil
}

// Fetch reads a fresh dynamic credential and its lease.
func (b *Broker) Fetch(ctx context.Context) (*DBCredential, error) {
	credsPath := path.Join(b.mount, "creds", b.role)
	secret, err := b.client.Logical().ReadWithContext(ctx, credsPath)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", credsPath, err)
	}
	// A missing path returns (nil, nil), not an error; nil-check before Data.
	if secret == nil {
		return nil, fmt.Errorf("read %q: %w", credsPath, ErrNoCredentials)
	}

	username, _ := secret.Data["username"].(string)
	password, _ := secret.Data["password"].(string)
	if username == "" || password == "" {
		return nil, fmt.Errorf("read %q: %w", credsPath, ErrIncompleteCredential)
	}
	for _, warn := range secret.Warnings {
		log.Printf("vault warning: %s", warn)
	}

	b.secret = secret
	return &DBCredential{
		Username:      username,
		Password:      password,
		LeaseID:       secret.LeaseID,
		LeaseDuration: time.Duration(secret.LeaseDuration) * time.Second,
		Renewable:     secret.Renewable,
	}, nil
}

// Watch starts a LifetimeWatcher that auto-renews the current lease within its
// grace period. Successful renewals arrive on Renewed(); when the lease can no
// longer be extended, Done() fires (possibly with a nil error) and the caller must
// refetch. increment is the requested renewal TTL, in seconds.
func (b *Broker) Watch(increment int) error {
	if b.secret == nil {
		return ErrNoCredentials
	}
	w, err := b.client.NewLifetimeWatcher(&api.LifetimeWatcherInput{
		Secret:        b.secret,
		Increment:     increment,
		RenewBehavior: api.RenewBehaviorIgnoreErrors,
	})
	if err != nil {
		return fmt.Errorf("new lifetime watcher: %w", err)
	}
	b.watcher = w
	go w.Start()
	go b.consume(w)
	return nil
}

func (b *Broker) consume(w *api.LifetimeWatcher) {
	for {
		select {
		case err := <-w.DoneCh():
			// DoneCh firing means the lease can no longer be extended, even when
			// err is nil: signal the owner to refetch, then stop consuming.
			b.done <- err
			return
		case renew := <-w.RenewCh():
			select {
			case b.renewed <- renew.RenewedAt:
			default:
			}
		}
	}
}

// Renewed emits the timestamp of each successful renewal.
func (b *Broker) Renewed() <-chan time.Time { return b.renewed }

// Done fires once when the lease can no longer be extended.
func (b *Broker) Done() <-chan error { return b.done }

// Revoke explicitly revokes the current lease so the backing database user is
// dropped immediately instead of lingering until TTL expiry. It is a no-op when no
// credential has been fetched.
func (b *Broker) Revoke() error {
	if b.secret == nil || b.secret.LeaseID == "" {
		return nil
	}
	if err := b.client.Sys().Revoke(b.secret.LeaseID); err != nil {
		return fmt.Errorf("revoke lease %q: %w", b.secret.LeaseID, err)
	}
	return nil
}

// Stop halts the renewal watcher.
func (b *Broker) Stop() {
	if b.watcher != nil {
		b.watcher.Stop()
	}
}
```

### The fake serving the lease lifecycle

`FakeVault` serves the three endpoints the broker touches. `GET
/v1/database/creds/<role>` returns the lease and generated credentials, but returns
404 (an empty-data response the client maps to `(nil, nil)`) for any role other than
the one it was configured with — which is how the missing-credential test drives the
nil path. `PUT /v1/sys/leases/renew` counts calls and returns an extended lease; the
`LifetimeWatcher` hits it because the fetched secret has no `Auth`, so it renews a
lease rather than a token. `PUT /v1/sys/leases/revoke` records the exact `lease_id`
it was asked to revoke.

Create `fake.go`:

```go
package dbcreds

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// FakeVault stands in for Vault's database-engine and lease HTTP endpoints.
type FakeVault struct {
	srv *httptest.Server

	mu        sync.Mutex
	role      string
	leaseID   string
	renewHits int
	revoked   []string
	username  string
	password  string
	leaseSecs int
	renewable bool
}

// NewFakeVault starts a fake that issues credentials for exactly one role.
func NewFakeVault(role string) *FakeVault {
	f := &FakeVault{
		role:      role,
		leaseID:   "database/creds/" + role + "/AbC123",
		username:  "v-token-" + role + "-abc",
		password:  "pw-xyz-789",
		leaseSecs: 60,
		renewable: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/database/creds/", f.handleCreds)
	mux.HandleFunc("/v1/sys/leases/renew", f.handleRenew)
	mux.HandleFunc("/v1/sys/leases/revoke", f.handleRevoke)
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *FakeVault) URL() string { return f.srv.URL }
func (f *FakeVault) Close()      { f.srv.Close() }

// SetLeaseSeconds overrides the issued lease TTL.
func (f *FakeVault) SetLeaseSeconds(s int) {
	f.mu.Lock()
	f.leaseSecs = s
	f.mu.Unlock()
}

// RenewHits reports how many renewal requests arrived.
func (f *FakeVault) RenewHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewHits
}

// Revoked reports the lease ids the fake was asked to revoke.
func (f *FakeVault) Revoked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

func (f *FakeVault) handleCreds(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	role := f.role
	leaseID := f.leaseID
	username, password := f.username, f.password
	leaseSecs, renewable := f.leaseSecs, f.renewable
	f.mu.Unlock()

	if !strings.HasSuffix(r.URL.Path, "/creds/"+role) {
		// Unknown role: 404 with empty data maps to (nil, nil) on the client.
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"errors":[]}`)
		return
	}
	writeJSON(w, map[string]any{
		"lease_id":       leaseID,
		"lease_duration": leaseSecs,
		"renewable":      renewable,
		"data": map[string]any{
			"username": username,
			"password": password,
		},
	})
}

func (f *FakeVault) handleRenew(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LeaseID   string `json:"lease_id"`
		Increment int    `json:"increment"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.renewHits++
	leaseID := f.leaseID
	leaseSecs := f.leaseSecs
	f.mu.Unlock()

	writeJSON(w, map[string]any{
		"lease_id":       leaseID,
		"lease_duration": leaseSecs,
		"renewable":      true,
	})
}

func (f *FakeVault) handleRevoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LeaseID string `json:"lease_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.revoked = append(f.revoked, body.LeaseID)
	f.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
```

### The runnable demo

The demo fetches a credential, prints its lease, then revokes it on shutdown — the
full lifecycle in miniature.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/dbcreds"
)

func main() {
	fake := dbcreds.NewFakeVault("app-ro")
	defer fake.Close()

	broker, err := dbcreds.New(fake.URL(), "hvs.apptoken", "app-ro")
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	cred, err := broker.Fetch(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("fetched credential: %s (lease %s, renewable=%t)\n", cred.Username, cred.LeaseDuration, cred.Renewable)
	fmt.Printf("lease id: %s\n", cred.LeaseID)

	if err := broker.Revoke(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("revoked lease on shutdown: %s\n", cred.LeaseID)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fetched credential: v-token-app-ro-abc (lease 1m0s, renewable=true)
lease id: database/creds/app-ro/AbC123
revoked lease on shutdown: database/creds/app-ro/AbC123
```

### Tests

`TestFetchParsesLease` asserts the parsed lease fields. `TestMissingCredentials`
points the broker at a role the fake does not know and asserts the 404 becomes
`ErrNoCredentials` rather than a panic — proving the nil-`*Secret` handling.
`TestRevokeOnShutdown` fetches, revokes, and asserts the fake recorded the exact
`LeaseID`. `TestAutoRenewal` fetches a one-second lease, starts the watcher, and
asserts a renewal arrives before a deadline.

Create `broker_test.go`:

```go
package dbcreds

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestFetchParsesLease(t *testing.T) {
	t.Parallel()
	fake := NewFakeVault("app-ro")
	defer fake.Close()

	b, err := New(fake.URL(), "token", "app-ro")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cred, err := b.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if cred.Username == "" || cred.Password == "" {
		t.Fatalf("empty credential: %+v", cred)
	}
	if cred.LeaseDuration != 60*time.Second {
		t.Fatalf("LeaseDuration = %v, want 60s", cred.LeaseDuration)
	}
	if !cred.Renewable {
		t.Fatal("expected renewable lease")
	}
	if cred.LeaseID == "" {
		t.Fatal("expected non-empty lease id")
	}
}

func TestMissingCredentials(t *testing.T) {
	t.Parallel()
	fake := NewFakeVault("app-ro")
	defer fake.Close()

	b, err := New(fake.URL(), "token", "does-not-exist")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.Fetch(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("Fetch err = %v, want ErrNoCredentials", err)
	}
}

func TestRevokeOnShutdown(t *testing.T) {
	t.Parallel()
	fake := NewFakeVault("app-ro")
	defer fake.Close()

	b, err := New(fake.URL(), "token", "app-ro")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cred, err := b.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := b.Revoke(); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	revoked := fake.Revoked()
	if len(revoked) != 1 || revoked[0] != cred.LeaseID {
		t.Fatalf("revoked = %v, want [%s]", revoked, cred.LeaseID)
	}
}

func TestAutoRenewal(t *testing.T) {
	t.Parallel()
	fake := NewFakeVault("app-ro")
	defer fake.Close()
	fake.SetLeaseSeconds(1)

	b, err := New(fake.URL(), "token", "app-ro")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := b.Watch(1); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer b.Stop()

	select {
	case <-b.Renewed():
		// A renewal arrived within the grace period.
	case err := <-b.Done():
		t.Fatalf("watcher done before any renewal: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("no renewal within deadline; renew hits=%d", fake.RenewHits())
	}
	if got := fake.RenewHits(); got < 1 {
		t.Fatalf("renew hits = %d, want >= 1", got)
	}
}

func Example() {
	fake := NewFakeVault("app-ro")
	defer fake.Close()

	b, _ := New(fake.URL(), "token", "app-ro")
	cred, _ := b.Fetch(context.Background())
	fmt.Println(cred.Username)
	fmt.Println(cred.LeaseDuration)
	fmt.Println(cred.Renewable)
	_ = b.Revoke()
	// Output:
	// v-token-app-ro-abc
	// 1m0s
	// true
}
```

The integration test runs only under `-tags integration` against a dev-mode Vault
with the database engine enabled, and skips otherwise.

Create `integration_test.go`:

```go
//go:build integration

package dbcreds

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveDynamicCreds runs against a dev-mode Vault with the database secrets
// engine enabled and a role configured. Export VAULT_ADDR, VAULT_TOKEN, and
// VAULT_DB_ROLE. Run with: go test -tags integration -run TestLiveDynamicCreds
func TestLiveDynamicCreds(t *testing.T) {
	addr := os.Getenv("VAULT_ADDR")
	token := os.Getenv("VAULT_TOKEN")
	role := os.Getenv("VAULT_DB_ROLE")
	if addr == "" || token == "" || role == "" {
		t.Skip("set VAULT_ADDR, VAULT_TOKEN, VAULT_DB_ROLE to run the live test")
	}
	b, err := New(addr, token, role)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cred, err := b.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if cred.Renewable {
		if err := b.Watch(int(30 * time.Minute / time.Second)); err != nil {
			t.Fatalf("Watch: %v", err)
		}
	}
	// Always revoke to avoid orphaning the database user.
	if err := b.Revoke(); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
}
```

## Review

The broker is correct when the lease lifecycle is complete. `Fetch` must nil-check
the `*api.Secret` because `Logical().ReadWithContext` returns `(nil, nil)` for a
missing path — the `TestMissingCredentials` case is what proves you did not panic
there. `LeaseDuration` is seconds from Vault multiplied by `time.Second`; the
one-minute lease printing as `1m0s` in the demo confirms the conversion. The
watcher's `consume` loop must treat `DoneCh` as terminal even with a nil error;
using the credential after `Done()` fires is the classic "expired mid-request" bug.
And `Revoke` must send the exact `LeaseID` — `TestRevokeOnShutdown` asserts the fake
recorded it — because revoke-on-shutdown is the single most effective defense against
lease explosions.

The mistakes to avoid: never renewing (the credential dies mid-connection), never
revoking (orphaned users accumulate), caching the credential past its TTL (Vault has
already revoked it), and dropping `Secret.Warnings`. Offline, this module needs the
Vault module in the cache to build; the integration test gives the end-to-end proof
against a real database engine and always revokes so it leaves no orphaned user.

## Resources

- [Lease, Renew, and Revoke — Vault concepts](https://developer.hashicorp.com/vault/docs/concepts/lease) — the lease model, renewal, and revocation.
- [`vault/api` package reference](https://pkg.go.dev/github.com/hashicorp/vault/api) — `LifetimeWatcher`, `LifetimeWatcherInput`, `RenewBehavior`, `Sys().Revoke`.
- [Database secrets engine](https://developer.hashicorp.com/vault/docs/secrets/databases) — dynamic credential generation and roles.
- [hashicorp/vault-examples (Go)](https://github.com/hashicorp/vault-examples/tree/main/examples) — official token-renewal and dynamic-secret examples.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-approle-kv-secret-loader.md](01-approle-kv-secret-loader.md) | Next: [03-secrets-provider-cache-degradation.md](03-secrets-provider-cache-degradation.md)
