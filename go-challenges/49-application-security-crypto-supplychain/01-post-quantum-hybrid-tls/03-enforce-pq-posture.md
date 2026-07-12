# Exercise 3: Enforcing and Observing a PQ-Required Posture

Negotiating the hybrid group is not the same as *requiring* it. This exercise is
the platform-team deliverable: HTTP middleware that enforces a post-quantum
key-exchange posture on every request, fails closed when there is no TLS at all,
and exports a counter of negotiated groups so operators can watch PQ adoption
climb across a fleet.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
pqpolicy/                  independent module: example.com/pqpolicy
  go.mod                   go 1.26 (r.TLS.CurveID needs 1.25+)
  pqpolicy.go              Policy allow-set, Check, Counter, enforcing Middleware
  cmd/
    demo/
      main.go              runnable demo: allowed and denied request over TLS
  pqpolicy_test.go         httptest allowed/denied, nil-TLS fail-closed, allow-set
```

- Files: `pqpolicy.go`, `cmd/demo/main.go`, `pqpolicy_test.go`.
- Implement: `Policy` (an allow-set of `tls.CurveID`), `(*Policy).Check`, a mutex-guarded `Counter`, and `(*Policy).Middleware` that enforces the posture and records the negotiated group.
- Test: an allowed hybrid request returns 200 and is counted; a classical client is rejected with a structured status and reason; a plain-HTTP request (nil `r.TLS`) fails closed instead of panicking; the allow-set is table-driven.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/49-application-security-crypto-supplychain/01-post-quantum-hybrid-tls/03-enforce-pq-posture/cmd/demo
cd go-solutions/49-application-security-crypto-supplychain/01-post-quantum-hybrid-tls/03-enforce-pq-posture
go mod edit -go=1.26
```

### Enforcement is a membership test, and it must fail closed

The server has already negotiated a group by the time a request reaches your
handler; `r.TLS.CurveID` holds it. Enforcement is therefore a membership test:
is the negotiated group in the approved set? A `Policy` wraps that set so it can
grow — today `X25519MLKEM768`, tomorrow the P-curve hybrids
`SecP256r1MLKEM768` and `SecP384r1MLKEM1024` for FIPS fleets — without touching
the middleware code.

The single most important correctness property is failing closed. A plain-HTTP
request has `r.TLS == nil`. Middleware that dereferences `r.TLS.CurveID` without a
nil check panics on that request — which, depending on your recovery
configuration, can turn into a 500 or a dropped connection, and in any case is not
a deliberate rejection. `Check` treats a nil connection state as a policy
violation (`ErrNoTLS`) and rejects it. No TLS means no post-quantum posture means
deny; the absence of a handshake is never allowed through.

The status codes carry intent. A request that arrived with no TLS at all gets
`403 Forbidden`. A request that completed a TLS handshake but negotiated a group
outside the approved set gets `421 Misdirected Request` — it reached a server that
is not willing to serve this connection's key-exchange posture. Both write the
reason into the body so an operator debugging a rejected client can see *why*.

### Observability is the other half of the job

A fleet migration is only manageable if you can *see* it. The `Counter` records
every negotiated group — allowed or rejected — behind a mutex, and exposes a
snapshot. In production this feeds a metric labeled by group name, so you watch
the `X25519MLKEM768` line climb and the `X25519` line fall as clients upgrade,
alert if PQ adoption regresses, and confirm a rollback took effect. The counter is
mutex-guarded because many connections are served concurrently; the `-race`
detector in the test proves the guard is real.

Create `pqpolicy.go`:

```go
package pqpolicy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// Sentinel errors, wrapped with %w so callers can match with errors.Is.
var (
	ErrNoTLS           = errors.New("pqpolicy: request has no TLS connection state")
	ErrGroupNotAllowed = errors.New("pqpolicy: negotiated group not in the approved post-quantum set")
)

// Policy is an allow-set of acceptable key-exchange groups. New groups (for
// example P-curve hybrids on a FIPS fleet) are added by construction, not by
// editing the middleware.
type Policy struct {
	allowed map[tls.CurveID]bool
}

// NewPolicy builds a policy that admits exactly the given groups.
func NewPolicy(groups ...tls.CurveID) *Policy {
	set := make(map[tls.CurveID]bool, len(groups))
	for _, g := range groups {
		set[g] = true
	}
	return &Policy{allowed: set}
}

// Check reports whether a connection state satisfies the policy. A nil state
// (plain HTTP) fails closed with ErrNoTLS; an unapproved group returns
// ErrGroupNotAllowed. It returns nil when the negotiated group is approved.
func (p *Policy) Check(state *tls.ConnectionState) error {
	if state == nil {
		return ErrNoTLS
	}
	if !p.allowed[state.CurveID] {
		return fmt.Errorf("%w: %s", ErrGroupNotAllowed, GroupName(state.CurveID))
	}
	return nil
}

// Counter records how often each group is negotiated, for a fleet-wide metric.
type Counter struct {
	mu     sync.Mutex
	counts map[tls.CurveID]int64
}

// NewCounter returns an empty counter.
func NewCounter() *Counter {
	return &Counter{counts: make(map[tls.CurveID]int64)}
}

// Record increments the tally for a negotiated group.
func (c *Counter) Record(id tls.CurveID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[id]++
}

// Count returns the tally for one group.
func (c *Counter) Count(id tls.CurveID) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[id]
}

// Snapshot returns a copy of the tallies keyed by human-readable group name.
func (c *Counter) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.counts))
	for id, n := range c.counts {
		out[GroupName(id)] = n
	}
	return out
}

// Middleware enforces the policy on every request and records the negotiated
// group. It fails closed on a nil TLS state rather than panicking.
func (p *Policy) Middleware(counter *Counter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			counter.Record(r.TLS.CurveID)
		}
		if err := p.Check(r.TLS); err != nil {
			status := http.StatusForbidden
			if errors.Is(err, ErrGroupNotAllowed) {
				status = http.StatusMisdirectedRequest
			}
			http.Error(w, err.Error(), status)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GroupName maps a CurveID to a human-readable name for logs and metrics.
func GroupName(id tls.CurveID) string {
	switch id {
	case tls.X25519MLKEM768:
		return "X25519MLKEM768"
	case tls.SecP256r1MLKEM768:
		return "SecP256r1MLKEM768"
	case tls.SecP384r1MLKEM1024:
		return "SecP384r1MLKEM1024"
	case tls.X25519:
		return "X25519"
	case tls.CurveP256:
		return "P-256"
	case tls.CurveP384:
		return "P-384"
	default:
		return fmt.Sprintf("CurveID(%d)", uint16(id))
	}
}
```

### The runnable demo

The demo stands up an `httptest` TLS server whose config offers both the hybrid
group and classical X25519, then drives it twice: once with a hybrid client
(allowed, 200) and once with a classical client (rejected, 421). It prints both
statuses and the counter snapshot so the observability payoff is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"

	"example.com/pqpolicy"
)

func clientFor(ts *httptest.Server, groups ...tls.CurveID) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:          pool,
			MinVersion:       tls.VersionTLS13,
			CurvePreferences: groups,
		},
	}}
}

func main() {
	policy := pqpolicy.NewPolicy(tls.X25519MLKEM768)
	counter := pqpolicy.NewCounter()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})

	ts := httptest.NewUnstartedServer(policy.Middleware(counter, handler))
	ts.TLS = &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519},
	}
	ts.StartTLS()
	defer ts.Close()

	hybrid := clientFor(ts, tls.X25519MLKEM768)
	r1, err := hybrid.Get(ts.URL)
	if err != nil {
		log.Fatal(err)
	}
	r1.Body.Close()
	fmt.Printf("hybrid client:    %d %s\n", r1.StatusCode, http.StatusText(r1.StatusCode))

	classical := clientFor(ts, tls.X25519)
	r2, err := classical.Get(ts.URL)
	if err != nil {
		log.Fatal(err)
	}
	body, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	fmt.Printf("classical client: %d %s\n", r2.StatusCode, http.StatusText(r2.StatusCode))
	fmt.Printf("reason: %s\n", strings.TrimSpace(string(body)))

	snap := counter.Snapshot()
	names := make([]string, 0, len(snap))
	for name := range snap {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("counted %s: %d\n", name, snap[name])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hybrid client:    200 OK
classical client: 421 Misdirected Request
reason: pqpolicy: negotiated group not in the approved post-quantum set: X25519
counted X25519: 1
counted X25519MLKEM768: 1
```

### Tests

`TestAllowedRequest` and `TestDeniedRequest` drive a real `httptest` TLS server
and assert the enforced status, the reason body, and the counter. `TestFailsClosed`
serves a plain (nil-`TLS`) request through the middleware with a recorder and
asserts it rejects rather than panics — the fail-closed guard. `TestCheck` and
`TestAllowSet` unit-test the policy directly, matching sentinel errors with
`errors.Is` and proving the allow-set is forward-compatible. `TestCounterConcurrent`
fires many goroutines at one shared `Counter` so `go test -race` actually observes
concurrent `Record`/`Count` on the same map and would flag a missing lock.

Create `pqpolicy_test.go`:

```go
package pqpolicy

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func clientFor(ts *httptest.Server, groups ...tls.CurveID) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:          pool,
			MinVersion:       tls.VersionTLS13,
			CurvePreferences: groups,
		},
	}}
}

func newServer(t *testing.T, policy *Policy, counter *Counter) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	ts := httptest.NewUnstartedServer(policy.Middleware(counter, handler))
	ts.TLS = &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519},
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func TestAllowedRequest(t *testing.T) {
	t.Parallel()
	counter := NewCounter()
	ts := newServer(t, NewPolicy(tls.X25519MLKEM768), counter)

	resp, err := clientFor(ts, tls.X25519MLKEM768).Get(ts.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if got := counter.Count(tls.X25519MLKEM768); got != 1 {
		t.Errorf("counted X25519MLKEM768 = %d; want 1", got)
	}
}

func TestDeniedRequest(t *testing.T) {
	t.Parallel()
	counter := NewCounter()
	ts := newServer(t, NewPolicy(tls.X25519MLKEM768), counter)

	resp, err := clientFor(ts, tls.X25519).Get(ts.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("status = %d; want 421", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not in the approved") {
		t.Errorf("body = %q; want the policy reason", strings.TrimSpace(string(body)))
	}
	if got := counter.Count(tls.X25519); got != 1 {
		t.Errorf("counted X25519 = %d; want 1", got)
	}
}

func TestFailsClosed(t *testing.T) {
	t.Parallel()
	policy := NewPolicy(tls.X25519MLKEM768)
	handler := policy.Middleware(NewCounter(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran on a nil-TLS request; middleware did not fail closed")
	}))

	// httptest.NewRequest builds a request with r.TLS == nil (plain HTTP).
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req) // must not panic

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403 (fail closed)", rec.Code)
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	policy := NewPolicy(tls.X25519MLKEM768)
	tests := []struct {
		name  string
		state *tls.ConnectionState
		want  error
	}{
		{"nil state", nil, ErrNoTLS},
		{"disallowed group", &tls.ConnectionState{CurveID: tls.X25519}, ErrGroupNotAllowed},
		{"allowed group", &tls.ConnectionState{CurveID: tls.X25519MLKEM768}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := policy.Check(tt.state)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("Check = %v; want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("Check = %v; want errors.Is %v", err, tt.want)
			}
		})
	}
}

func TestAllowSet(t *testing.T) {
	t.Parallel()
	strict := NewPolicy(tls.X25519MLKEM768)
	if err := strict.Check(&tls.ConnectionState{CurveID: tls.SecP256r1MLKEM768}); !errors.Is(err, ErrGroupNotAllowed) {
		t.Errorf("strict policy admitted SecP256r1MLKEM768: %v", err)
	}

	// Forward-compatible: add a P-curve hybrid without touching the middleware.
	extended := NewPolicy(tls.X25519MLKEM768, tls.SecP256r1MLKEM768)
	if err := extended.Check(&tls.ConnectionState{CurveID: tls.SecP256r1MLKEM768}); err != nil {
		t.Errorf("extended policy rejected an approved group: %v", err)
	}
}

// TestCounterConcurrent fires many goroutines at one shared Counter so the race
// detector actually observes concurrent Record/Count on the same map. Run with
// `go test -race`: without the mutex, this test reports a data race.
func TestCounterConcurrent(t *testing.T) {
	t.Parallel()
	counter := NewCounter()
	const goroutines, perGoroutine = 32, 500

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				counter.Record(tls.X25519MLKEM768)
				_ = counter.Count(tls.X25519MLKEM768)
			}
		}()
	}
	wg.Wait()

	if got := counter.Count(tls.X25519MLKEM768); got != goroutines*perGoroutine {
		t.Errorf("Count = %d, want %d", got, goroutines*perGoroutine)
	}
}

func Example() {
	policy := NewPolicy(tls.X25519MLKEM768)
	counter := NewCounter()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	ts := httptest.NewUnstartedServer(policy.Middleware(counter, handler))
	ts.TLS = &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519},
	}
	ts.StartTLS()
	defer ts.Close()

	r1, _ := clientFor(ts, tls.X25519MLKEM768).Get(ts.URL)
	r1.Body.Close()
	r2, _ := clientFor(ts, tls.X25519).Get(ts.URL)
	r2.Body.Close()

	fmt.Println("hybrid:", r1.StatusCode)
	fmt.Println("classical:", r2.StatusCode)
	// Output:
	// hybrid: 200
	// classical: 421
}
```

## Review

The middleware is correct when an approved group passes to the wrapped handler
(200), an unapproved group is rejected with a structured status and reason, and a
nil `r.TLS` fails closed rather than panicking. The counter is correct when it
records every negotiated group under a mutex; `go test -race` proves the lock
actually guards the map when many connections are served at once.

The mistakes to avoid: reading `r.TLS.CurveID` without a nil check (a plain-HTTP
request panics instead of being denied); enforcing by trying to order
`CurvePreferences` rather than checking the negotiated `CurveID` after the
handshake; and treating enforcement as static — the `Policy` allow-set exists so a
P-curve hybrid can be admitted later by construction, not by editing this code.
Confirm the reason body carries the policy message so an operator can debug a
rejected client, and watch the counter snapshot to track PQ adoption across the
fleet.

## Resources

- [`net/http`](https://pkg.go.dev/net/http) — `Handler` middleware, `Request.TLS`, and `http.Error`.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewUnstartedServer`, `Server.StartTLS`, `Server.Certificate`, and `NewRequest`.
- [`crypto/tls` — ConnectionState](https://pkg.go.dev/crypto/tls#ConnectionState) — the `CurveID` field the middleware enforces on.

---

Back to [02-negotiate-pq-tls.md](02-negotiate-pq-tls.md) | Next: [../02-aead-app-side-crypto/00-concepts.md](../02-aead-app-side-crypto/00-concepts.md)
