# Exercise 3: A Production SecretsProvider: Caching, Retry, and Graceful Degradation

A service must not fall over because Vault hiccuped for two seconds during a leader
election. This exercise builds the provider that makes Vault a *soft* dependency: an
application-facing `SecretsProvider` interface, a Vault-backed implementation that
caches by lease-derived expiry, retries transient failures with capped backoff,
collapses concurrent refreshes with `singleflight`, and falls back to the
last-known-good value within a bounded staleness window when Vault is briefly
unreachable. Business code depends on the interface, never on the Vault SDK.

This module is fully self-contained: its own `go mod init`, an in-memory provider
for tests, an `httptest` fake for the Vault path, and a fake clock for deterministic
expiry.

## What you'll build

```text
secretsprovider/               independent module: example.com/secretsprovider
  go.mod                       requires hashicorp/vault/api + golang.org/x/sync
  provider.go                  SecretsProvider, Secret, VaultProvider, options
  memory.go                    MemoryProvider (in-memory impl for tests)
  fake.go                      FakeVault: KV read with 503 + latency + hit counter
  cmd/
    demo/
      main.go                  runnable demo: cache hit, expiry refetch, redacted log
  provider_test.go             cache hit, expiry, degradation, singleflight, no-leak
  integration_test.go          //go:build integration: live vault -dev read
```

- Files: `provider.go`, `memory.go`, `fake.go`, `cmd/demo/main.go`, `provider_test.go`, `integration_test.go`.
- Implement: `SecretsProvider` interface; `VaultProvider` with a TTL cache, capped exponential backoff, `singleflight` refresh collapsing, and bounded last-known-good fallback; a redacting `Secret.String`.
- Test: cache hit (one backend fetch), refetch after expiry, degradation to last-known-good under 503 then error past the window, singleflight collapse, and that the value never appears in formatted output. Inject a fake clock; do not sleep for expiry.
- Verify: `go test -race ./...` for the fake path; `go test -tags integration ./...` against a live Vault.

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/07-secrets-management-vault/03-secrets-provider-cache-degradation/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/07-secrets-management-vault/03-secrets-provider-cache-degradation
go mod edit -go=1.26
go get github.com/hashicorp/vault/api
go get golang.org/x/sync/singleflight
```

### Dependency inversion is the point

If every package imports `github.com/hashicorp/vault/api`, your business logic is
untestable without a Vault and welded to one broker forever. The fix is a one-method
interface, `SecretsProvider`, that business code depends on. The Vault SDK lives
behind exactly one implementation; tests use a trivial `MemoryProvider`. Swapping
brokers, or stubbing secrets in a unit test, becomes a constructor change.

The `Secret` type carries a redacting `String()` method. This is not cosmetic: the
most common way a secret leaks is a debug `log.Printf("%+v", secret)`. Because
`Secret` implements `fmt.Stringer`, `%v`, `%+v`, and `%s` all print
`value:REDACTED` instead of the payload, so a careless log line cannot spill it.

### The cache, the backoff, and the fallback

`GetSecret` layers three behaviors:

1. **TTL cache.** A cached value is served only while `now` is before its
   `ExpiresAt`. For a dynamic secret you derive that instant from `LeaseDuration`;
   for KV v2 (which has no lease) the provider caps freshness with a configured
   `cacheTTL`. Either way, a value is *never* served past its expiry — caching a
   credential past its lease is a top failure mode.

2. **Retry with capped exponential backoff, then graceful degradation.** A refresh
   classifies the error: a 503, a 429, or any 5xx (surfaced as an
   `*api.ResponseError`) is transient and retried with doubling backoff up to a cap.
   When retries are exhausted, the provider serves the *last-known-good* value if it
   is still within a bounded `staleness` window, and only errors once even the stale
   value is too old. A non-retryable error (a missing secret) returns immediately.
   Note that `api.Client` ships its own `retryablehttp` layer (default
   `MaxRetries=2` with roughly 1-1.5s of internal backoff, which *also* retries
   5xx). Left on, it would sit underneath `fetchOnce` and compound with the backoff
   this exercise teaches, so `NewVaultProvider` calls `client.SetMaxRetries(0)` to
   make this provider's loop the single, observable retry path.

3. **`singleflight` refresh collapsing.** When a value expires under load, dozens of
   goroutines would otherwise each hit Vault. `singleflight.Group.Do` ensures exactly
   one refresh runs per key while the rest wait for and share its result.

Time is injected as a `now func() time.Time` so tests advance a fake clock instead
of sleeping — the expiry and staleness math is then deterministic. (Backoff still
uses real `time.After`, but tests set it to milliseconds.)

Create `provider.go`:

```go
package secretsprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/vault/api"
	"golang.org/x/sync/singleflight"
)

// ErrNotFound is returned when a key has no secret. It is not retryable.
var ErrNotFound = errors.New("secretsprovider: secret not found")

// Secret is the application-facing view. String redacts the value so it never
// lands in a log line.
type Secret struct {
	Value     string
	Version   int
	ExpiresAt time.Time
}

func (s Secret) String() string {
	return fmt.Sprintf("Secret{version:%d, expires:%s, value:REDACTED}",
		s.Version, s.ExpiresAt.UTC().Format(time.RFC3339))
}

// SecretsProvider is the dependency-inversion seam: business code depends on this,
// not on the Vault SDK.
type SecretsProvider interface {
	GetSecret(ctx context.Context, key string) (Secret, error)
}

type cacheEntry struct {
	secret Secret
	goodAt time.Time
}

// VaultProvider is the Vault-backed SecretsProvider with caching, retry, and
// bounded graceful degradation.
type VaultProvider struct {
	client      *api.Client
	mount       string
	now         func() time.Time
	cacheTTL    time.Duration
	staleness   time.Duration
	maxRetries  int
	baseBackoff time.Duration
	maxBackoff  time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry
	group singleflight.Group
}

var _ SecretsProvider = (*VaultProvider)(nil)

// Option configures a VaultProvider.
type Option func(*VaultProvider)

func WithClock(now func() time.Time) Option { return func(p *VaultProvider) { p.now = now } }
func WithCacheTTL(d time.Duration) Option   { return func(p *VaultProvider) { p.cacheTTL = d } }
func WithStaleness(d time.Duration) Option  { return func(p *VaultProvider) { p.staleness = d } }
func WithMaxRetries(n int) Option           { return func(p *VaultProvider) { p.maxRetries = n } }
func WithMount(mount string) Option         { return func(p *VaultProvider) { p.mount = mount } }

func WithBackoff(base, ceiling time.Duration) Option {
	return func(p *VaultProvider) { p.baseBackoff, p.maxBackoff = base, ceiling }
}

// NewVaultProvider builds a VaultProvider pointed at addr with an installed token.
func NewVaultProvider(addr, token string, opts ...Option) (*VaultProvider, error) {
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
	// Disable the SDK's own retryablehttp layer (default MaxRetries=2 with its
	// own backoff, which also retries 5xx). We want this provider's backoff and
	// last-known-good degradation to be the sole retry path, not two compounding
	// retry loops.
	client.SetMaxRetries(0)

	p := &VaultProvider{
		client:      client,
		mount:       "secret",
		now:         time.Now,
		cacheTTL:    5 * time.Minute,
		staleness:   1 * time.Hour,
		maxRetries:  3,
		baseBackoff: 50 * time.Millisecond,
		maxBackoff:  2 * time.Second,
		cache:       make(map[string]cacheEntry),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// GetSecret returns the secret for key, serving a fresh cache entry when possible
// and collapsing concurrent refreshes with singleflight.
func (p *VaultProvider) GetSecret(ctx context.Context, key string) (Secret, error) {
	if s, ok := p.fresh(key, p.now()); ok {
		return s, nil
	}
	v, err, _ := p.group.Do(key, func() (any, error) {
		// Re-check inside the flight: a concurrent caller may have refreshed.
		if s, ok := p.fresh(key, p.now()); ok {
			return s, nil
		}
		return p.refresh(ctx, key)
	})
	if err != nil {
		return Secret{}, err
	}
	return v.(Secret), nil
}

func (p *VaultProvider) fresh(key string, now time.Time) (Secret, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.cache[key]
	if ok && now.Before(e.secret.ExpiresAt) {
		return e.secret, true
	}
	return Secret{}, false
}

func (p *VaultProvider) lastGood(key string, now time.Time) (Secret, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.cache[key]
	if ok && now.Sub(e.goodAt) <= p.staleness {
		return e.secret, true
	}
	return Secret{}, false
}

func (p *VaultProvider) store(key string, s Secret) {
	p.mu.Lock()
	p.cache[key] = cacheEntry{secret: s, goodAt: p.now()}
	p.mu.Unlock()
}

func (p *VaultProvider) refresh(ctx context.Context, key string) (Secret, error) {
	var lastErr error
	backoff := p.baseBackoff
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Secret{}, fmt.Errorf("get secret %q: %w", key, ctx.Err())
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > p.maxBackoff {
				backoff = p.maxBackoff
			}
		}
		s, err := p.fetchOnce(ctx, key)
		if err == nil {
			p.store(key, s)
			return s, nil
		}
		lastErr = err
		if !retryable(err) {
			return Secret{}, fmt.Errorf("get secret %q: %w", key, err)
		}
	}
	// Transient failures exhausted: degrade to last-known-good within the window.
	if s, ok := p.lastGood(key, p.now()); ok {
		return s, nil
	}
	return Secret{}, fmt.Errorf("get secret %q after %d retries: %w", key, p.maxRetries, lastErr)
}

func (p *VaultProvider) fetchOnce(ctx context.Context, key string) (Secret, error) {
	kv, err := p.client.KVv2(p.mount).Get(ctx, key)
	if err != nil {
		if errors.Is(err, api.ErrSecretNotFound) {
			return Secret{}, fmt.Errorf("%q: %w", key, ErrNotFound)
		}
		return Secret{}, err
	}
	if kv == nil || kv.Data == nil {
		return Secret{}, fmt.Errorf("%q: %w", key, ErrNotFound)
	}
	value, _ := kv.Data["value"].(string)
	if value == "" {
		return Secret{}, fmt.Errorf("%q: %w", key, ErrNotFound)
	}
	s := Secret{Value: value, ExpiresAt: p.now().Add(p.cacheTTL)}
	if kv.VersionMetadata != nil {
		s.Version = kv.VersionMetadata.Version
	}
	return s, nil
}

// retryable classifies an error as transient (worth retrying and, once exhausted,
// worth degrading to last-known-good).
func retryable(err error) bool {
	var respErr *api.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == http.StatusServiceUnavailable ||
			respErr.StatusCode == http.StatusTooManyRequests ||
			respErr.StatusCode >= 500
	}
	return false
}
```

### The in-memory provider

`MemoryProvider` is the whole payoff of the interface: business logic tests inject
it and never touch Vault or an HTTP fake.

Create `memory.go`:

```go
package secretsprovider

import (
	"context"
	"fmt"
	"sync"
)

// MemoryProvider is an in-memory SecretsProvider for tests and local runs.
type MemoryProvider struct {
	mu      sync.RWMutex
	secrets map[string]Secret
}

var _ SecretsProvider = (*MemoryProvider)(nil)

func NewMemoryProvider() *MemoryProvider {
	return &MemoryProvider{secrets: make(map[string]Secret)}
}

// Set stores a secret under key.
func (m *MemoryProvider) Set(key string, s Secret) {
	m.mu.Lock()
	m.secrets[key] = s
	m.mu.Unlock()
}

func (m *MemoryProvider) GetSecret(_ context.Context, key string) (Secret, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.secrets[key]
	if !ok {
		return Secret{}, fmt.Errorf("%q: %w", key, ErrNotFound)
	}
	return s, nil
}
```

### The fake with 503s and latency

The fake serves a KV v2 read and can be told to fail the next `n` reads with 503
(to drive the degradation path) and to delay each read (so concurrent callers
overlap and `singleflight` has something to collapse). It counts hits so tests can
assert the exact number of backend fetches.

Create `fake.go`:

```go
package secretsprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// FakeVault serves a single KV v2 secret with injectable failure and latency.
type FakeVault struct {
	srv *httptest.Server

	mu      sync.Mutex
	hits    int
	fail    int
	delay   time.Duration
	value   string
	version int
}

func NewFakeVault() *FakeVault {
	f := &FakeVault{value: "s3cr3t-value", version: 7}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/secret/data/", f.handleKV)
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *FakeVault) URL() string { return f.srv.URL }
func (f *FakeVault) Close()      { f.srv.Close() }

// Fail503 makes the next n reads return 503.
func (f *FakeVault) Fail503(n int) {
	f.mu.Lock()
	f.fail = n
	f.mu.Unlock()
}

// SetDelay makes each read block for d, so concurrent reads overlap.
func (f *FakeVault) SetDelay(d time.Duration) {
	f.mu.Lock()
	f.delay = d
	f.mu.Unlock()
}

// Hits reports how many read requests arrived.
func (f *FakeVault) Hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func (f *FakeVault) handleKV(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.hits++
	delay := f.delay
	value := f.value
	version := f.version
	fail := f.fail
	if fail > 0 {
		f.fail--
	}
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if fail > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{
		"data": map[string]any{
			"data": map[string]any{"value": value},
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

### The runnable demo

The demo injects a controllable clock so the cache-hit, cache-expiry, and redacted
logging behaviors are visible and deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"example.com/secretsprovider"
)

func main() {
	fake := secretsprovider.NewFakeVault()
	defer fake.Close()

	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	p, err := secretsprovider.NewVaultProvider(fake.URL(), "hvs.apptoken",
		secretsprovider.WithClock(clock),
		secretsprovider.WithCacheTTL(time.Minute),
	)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	s1, err := p.GetSecret(ctx, "app")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("first read  (backend): version %d\n", s1.Version)

	s2, _ := p.GetSecret(ctx, "app")
	fmt.Printf("second read (cache)  : version %d\n", s2.Version)

	now = now.Add(2 * time.Minute) // advance past the cache TTL
	s3, _ := p.GetSecret(ctx, "app")
	fmt.Printf("after ttl   (backend): version %d\n", s3.Version)

	fmt.Printf("backend hits: %d\n", fake.Hits())
	fmt.Printf("logged form: %s\n", s1)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first read  (backend): version 7
second read (cache)  : version 7
after ttl   (backend): version 7
backend hits: 2
logged form: Secret{version:7, expires:2030-01-01T00:01:00Z, value:REDACTED}
```

### Tests

The tests inject a fake clock so expiry is deterministic. `TestCacheHit` proves two
rapid reads hit the backend once. `TestExpiryRefetch` advances past the TTL and
proves a second fetch. `TestDegradation` primes the cache, expires it, forces 503s,
and proves the provider serves last-known-good inside the staleness window then
errors past it. `TestSingleflight` fires twenty concurrent reads at a slow backend
and proves exactly one fetch. `TestNoSecretLeak` proves the value never appears in
formatted output.

Create `provider_test.go`:

```go
package secretsprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestCacheHit(t *testing.T) {
	t.Parallel()
	fake := NewFakeVault()
	defer fake.Close()
	clk := newClock()

	p, err := NewVaultProvider(fake.URL(), "token", WithClock(clk.Now), WithCacheTTL(time.Minute))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := p.GetSecret(ctx, "app"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := p.GetSecret(ctx, "app"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := fake.Hits(); got != 1 {
		t.Fatalf("hits = %d, want 1 (cache hit)", got)
	}
}

func TestExpiryRefetch(t *testing.T) {
	t.Parallel()
	fake := NewFakeVault()
	defer fake.Close()
	clk := newClock()

	p, _ := NewVaultProvider(fake.URL(), "token", WithClock(clk.Now), WithCacheTTL(time.Minute))
	ctx := context.Background()
	if _, err := p.GetSecret(ctx, "app"); err != nil {
		t.Fatalf("first: %v", err)
	}
	clk.Advance(2 * time.Minute) // past cacheTTL
	if _, err := p.GetSecret(ctx, "app"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := fake.Hits(); got != 2 {
		t.Fatalf("hits = %d, want 2 (refetch after expiry)", got)
	}
}

func TestDegradation(t *testing.T) {
	t.Parallel()
	fake := NewFakeVault()
	defer fake.Close()
	clk := newClock()

	p, _ := NewVaultProvider(fake.URL(), "token",
		WithClock(clk.Now),
		WithCacheTTL(time.Minute),
		WithStaleness(10*time.Minute),
		WithMaxRetries(2),
		WithBackoff(time.Millisecond, 5*time.Millisecond),
	)
	ctx := context.Background()

	first, err := p.GetSecret(ctx, "app") // prime last-known-good
	if err != nil {
		t.Fatalf("prime: %v", err)
	}

	clk.Advance(2 * time.Minute) // expire the cache
	fake.Fail503(100)            // Vault is unreachable

	got, err := p.GetSecret(ctx, "app") // inside staleness window
	if err != nil {
		t.Fatalf("degraded read: %v", err)
	}
	if got.Value != first.Value {
		t.Fatalf("degraded value = %q, want last-known-good %q", got.Value, first.Value)
	}

	clk.Advance(20 * time.Minute) // past staleness window
	if _, err := p.GetSecret(ctx, "app"); err == nil {
		t.Fatal("expected error past staleness window, got nil")
	}
}

func TestSingleflight(t *testing.T) {
	t.Parallel()
	fake := NewFakeVault()
	defer fake.Close()
	fake.SetDelay(50 * time.Millisecond)
	clk := newClock()

	p, _ := NewVaultProvider(fake.URL(), "token", WithClock(clk.Now), WithCacheTTL(time.Minute))
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.GetSecret(ctx, "app"); err != nil {
				t.Errorf("GetSecret: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := fake.Hits(); got != 1 {
		t.Fatalf("hits = %d, want 1 (singleflight collapse)", got)
	}
}

func TestNoSecretLeak(t *testing.T) {
	t.Parallel()
	s := Secret{Value: "top-secret-value", Version: 4, ExpiresAt: time.Unix(0, 0)}
	out := fmt.Sprintf("%v | %s | %+v", s, s, s)
	if strings.Contains(out, "top-secret-value") {
		t.Fatalf("secret value leaked in formatted output: %q", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("expected REDACTED marker, got %q", out)
	}
}

func TestMemoryProvider(t *testing.T) {
	t.Parallel()
	var p SecretsProvider = NewMemoryProvider()
	p.(*MemoryProvider).Set("app", Secret{Value: "v", Version: 1})

	got, err := p.GetSecret(context.Background(), "app")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got.Value != "v" {
		t.Fatalf("value = %q, want v", got.Value)
	}
	if _, err := p.GetSecret(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func Example() {
	fake := NewFakeVault()
	defer fake.Close()
	clk := newClock()

	p, _ := NewVaultProvider(fake.URL(), "token", WithClock(clk.Now), WithCacheTTL(time.Minute))
	ctx := context.Background()

	s1, _ := p.GetSecret(ctx, "app")
	s2, _ := p.GetSecret(ctx, "app") // cache hit
	clk.Advance(2 * time.Minute)
	s3, _ := p.GetSecret(ctx, "app") // refetch

	fmt.Println(s1.Version, s2.Version, s3.Version)
	fmt.Println(fake.Hits())
	fmt.Println(s1)
	// Output:
	// 7 7 7
	// 2
	// Secret{version:7, expires:2030-01-01T00:01:00Z, value:REDACTED}
}
```

The integration test runs only under `-tags integration` and skips unless a live
Vault is configured.

Create `integration_test.go`:

```go
//go:build integration

package secretsprovider

import (
	"context"
	"os"
	"testing"
)

// TestLiveProvider reads secret/app from a dev-mode Vault. Export VAULT_ADDR and
// VAULT_TOKEN and write the secret first:
//
//	vault kv put secret/app value=hunter2
//
// Run with: go test -tags integration -run TestLiveProvider
func TestLiveProvider(t *testing.T) {
	addr := os.Getenv("VAULT_ADDR")
	token := os.Getenv("VAULT_TOKEN")
	if addr == "" || token == "" {
		t.Skip("set VAULT_ADDR and VAULT_TOKEN to run the live test")
	}
	p, err := NewVaultProvider(addr, token)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s, err := p.GetSecret(context.Background(), "app")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if s.Value == "" {
		t.Fatal("expected a value at secret/app")
	}
}
```

## Review

The provider is correct when each behavior is provable in isolation with the fake
clock. A cache hit must not touch the backend (`TestCacheHit` asserts one hit); an
expired entry must (`TestExpiryRefetch` asserts two). Degradation is the subtle one:
after retries are exhausted the provider serves last-known-good *only* while
`now - goodAt <= staleness`, and errors once past it — `store` records `goodAt` on
success only, so a degraded read never refreshes the staleness clock.
`TestSingleflight` under `-race` proves concurrent refreshes collapse to a single
fetch and that the shared cache is race-free. And `TestNoSecretLeak` proves the
`String` method keeps the value out of any `%v`/`%+v`/`%s` output.

The mistakes to avoid: importing the Vault SDK throughout business code instead of
behind the interface; serving a cached value past its expiry; making Vault a hard
dependency with no backoff or fallback so a brief blip cascades into an outage; and
logging the payload. Offline, this module needs `hashicorp/vault/api` and
`golang.org/x/sync` in the module cache to build; the fake-driven tests and the fake
clock make every behavior deterministic once it does, and the integration test gives
the live-Vault proof.

## Resources

- [`vault/api` package reference](https://pkg.go.dev/github.com/hashicorp/vault/api) — `KVv2`, `KVSecret`, `ResponseError`, `ErrSecretNotFound`.
- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) — `Group.Do` for collapsing duplicate refreshes.
- [Lease, Renew, and Revoke — Vault concepts](https://developer.hashicorp.com/vault/docs/concepts/lease) — lease-derived expiry that bounds cache TTLs.
- [hashicorp/vault-examples (Go)](https://github.com/hashicorp/vault-examples/tree/main/examples) — official patterns for wrapping the client behind application code.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-dynamic-db-secrets-lease-lifecycle.md](02-dynamic-db-secrets-lease-lifecycle.md) | Next: [../08-fips-140-3-mode/00-concepts.md](../08-fips-140-3-mode/00-concepts.md)
