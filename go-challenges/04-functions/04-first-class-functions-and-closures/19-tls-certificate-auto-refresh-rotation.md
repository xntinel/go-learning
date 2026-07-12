# Exercise 19: TLS Certificate Auto-Refresh Within Expiry Window

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

An HTTPS server's `tls.Config.GetCertificate` callback runs on every TLS
handshake, potentially from hundreds of concurrent connections, and must
transparently serve a rotated certificate once the current one is close to
expiring — without ever handing out a certificate that's already too old or
paying the cost of refreshing on every single handshake. `NewCertProvider`
closes over the current cert, a refresh function, and an injected clock,
guarded by a mutex whose critical section covers the entire
check-then-refresh decision.

## What you'll build

```text
tlscert/                   independent module: example.com/tls-cert-auto-refresh
  go.mod                   go 1.24
  tlscert.go               NewCertProvider returns func() Cert
  cmd/
    demo/
      main.go               advances a fake clock across a refresh boundary
  tlscert_test.go           table test: refresh boundary, isolation, concurrency
```

- Files: `tlscert.go`, `cmd/demo/main.go`, `tlscert_test.go`.
- Implement: `NewCertProvider(initial Cert, refresh func() Cert, now func() time.Time, window time.Duration) func() Cert`, closing over a mutex-guarded `cert Cert`.
- Test: a table advances a fake clock across the refresh boundary and checks the cert only changes once inside the window; two providers never share state; 100 concurrent callers past the boundary trigger exactly one refresh call under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/19-tls-certificate-auto-refresh-rotation/cmd/demo
cd go-solutions/04-functions/04-first-class-functions-and-closures/19-tls-certificate-auto-refresh-rotation
go mod edit -go=1.24
```

### One critical section for the whole check-then-refresh

`NewCertProvider` captures `cert`, starting at `initial`, plus a mutex. The
returned closure locks, checks whether `now() + window` has reached or passed
`cert.Expiry` — i.e., whether the cert expires within `window` of now — and
if so calls `refresh()` and stores the result, all before unlocking and
returning the (possibly just-updated) cert. The whole decision, not just the
read or just the write, happens inside one `mu.Lock()`/`mu.Unlock()` pair.

That matters because a TLS handshake can arrive on any goroutine at any
moment, and once the cert is within its refresh window, many handshakes may
land in the same instant. If the check and the refresh were separate locked
steps, a hundred concurrent callers could all observe the stale cert before
any of them replaced it, and all hundred would call `refresh()` — wasteful at
best, and a correctness problem if `refresh()` talks to a certificate
authority with rate limits. Because the whole decision is one critical
section, only the first caller to acquire the lock while the cert is stale
calls `refresh`; every other caller queued behind the lock re-runs the check
after acquiring it, finds the cert already fresh, and returns it without
calling `refresh` again. The concurrency test below asserts exactly that:
`refreshCalls` is 1, not 100, after a concurrent burst.

Create `tlscert.go`:

```go
package tlscert

import (
	"sync"
	"time"
)

// Cert is a minimal stand-in for an *x509.Certificate / tls.Certificate
// pair: just the data an HTTPS server would serve and when it expires.
type Cert struct {
	Data   string
	Expiry time.Time
}

// NewCertProvider returns a closure suitable for tls.Config.GetCertificate:
// it captures the current cert, a refresh function, an injected clock, and a
// mutex. Every call checks whether the cert is within window of expiring and,
// if so, calls refresh to obtain a new one before returning it.
//
// A TLS handshake can happen on any goroutine at any time, so the whole
// check-then-act — read now(), compare to Expiry, call refresh, store the
// result — runs inside a single critical section. That serializes concurrent
// callers: if fifty goroutines call the closure while the cert is stale,
// only the first to acquire the lock finds it stale and calls refresh: by
// the time the next one acquires the lock, the cert has already been
// replaced and its check finds nothing to do.
func NewCertProvider(initial Cert, refresh func() Cert, now func() time.Time, window time.Duration) func() Cert {
	var mu sync.Mutex
	cert := initial

	return func() Cert {
		mu.Lock()
		defer mu.Unlock()

		if !now().Add(window).Before(cert.Expiry) {
			cert = refresh()
		}
		return cert
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/tls-cert-auto-refresh"
)

func main() {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clockNow := start
	clock := func() time.Time { return clockNow }

	refreshCount := 0
	refresh := func() tlscert.Cert {
		refreshCount++
		return tlscert.Cert{
			Data:   fmt.Sprintf("cert-v%d", refreshCount+1),
			Expiry: clockNow.Add(90 * 24 * time.Hour),
		}
	}

	initial := tlscert.Cert{Data: "cert-v1", Expiry: start.Add(100 * 24 * time.Hour)}
	getCert := tlscert.NewCertProvider(initial, refresh, clock, 30*24*time.Hour)

	fmt.Println("day 0:", getCert().Data)

	clockNow = start.Add(15 * 24 * time.Hour)
	fmt.Println("day 15 (not yet within window):", getCert().Data)

	clockNow = start.Add(75 * 24 * time.Hour)
	fmt.Println("day 75 (within 30-day window, refreshed):", getCert().Data)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
day 0: cert-v1
day 15 (not yet within window): cert-v1
day 75 (within 30-day window, refreshed): cert-v2
```

### Tests

Create `tlscert_test.go`:

```go
package tlscert

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakeClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	cur := start
	now = func() time.Time { return cur }
	advance = func(d time.Duration) { cur = cur.Add(d) }
	return now, advance
}

func TestGetCertRefreshesOnlyWithinWindow(t *testing.T) {
	start := time.Unix(0, 0)
	now, advance := fakeClock(start)

	var refreshCalls atomic.Int32
	refresh := func() Cert {
		refreshCalls.Add(1)
		return Cert{Data: "refreshed", Expiry: now().Add(90 * 24 * time.Hour)}
	}

	initial := Cert{Data: "initial", Expiry: start.Add(100 * 24 * time.Hour)}
	getCert := NewCertProvider(initial, refresh, now, 30*24*time.Hour)

	tests := []struct {
		name        string
		advance     time.Duration
		wantData    string
		wantRefresh int32
	}{
		{"day 0, far from expiry", 0, "initial", 0},
		{"day 15, still outside window", 15 * 24 * time.Hour, "initial", 0},
		{"day 75, inside 30-day window: refresh", 60 * 24 * time.Hour, "refreshed", 1},
		{"day 76, freshly refreshed cert still valid", 24 * time.Hour, "refreshed", 1},
	}

	for _, tc := range tests {
		advance(tc.advance)
		got := getCert()
		if got.Data != tc.wantData {
			t.Fatalf("%s: cert = %q, want %q", tc.name, got.Data, tc.wantData)
		}
		if refreshCalls.Load() != tc.wantRefresh {
			t.Fatalf("%s: refreshCalls = %d, want %d", tc.name, refreshCalls.Load(), tc.wantRefresh)
		}
	}
}

func TestTwoProvidersDoNotShareCert(t *testing.T) {
	now, _ := fakeClock(time.Unix(0, 0))
	refreshA := func() Cert { return Cert{Data: "a-refreshed", Expiry: now().Add(time.Hour)} }
	refreshB := func() Cert { return Cert{Data: "b-refreshed", Expiry: now().Add(time.Hour)} }

	a := NewCertProvider(Cert{Data: "a-initial", Expiry: now().Add(time.Hour)}, refreshA, now, time.Minute)
	b := NewCertProvider(Cert{Data: "b-initial", Expiry: now().Add(time.Hour)}, refreshB, now, time.Minute)

	if got := a().Data; got != "a-initial" {
		t.Fatalf("a() = %q, want a-initial", got)
	}
	if got := b().Data; got != "b-initial" {
		t.Fatalf("b() = %q, want b-initial — providers must not share captured cert state", got)
	}
}

func TestGetCertConcurrentRefreshesExactlyOnce(t *testing.T) {
	start := time.Unix(0, 0)
	now, advance := fakeClock(start)

	var refreshCalls atomic.Int32
	refresh := func() Cert {
		refreshCalls.Add(1)
		return Cert{Data: "refreshed", Expiry: now().Add(90 * 24 * time.Hour)}
	}

	initial := Cert{Data: "initial", Expiry: start.Add(40 * 24 * time.Hour)}
	getCert := NewCertProvider(initial, refresh, now, 30*24*time.Hour)

	// Advance past the refresh threshold, then hammer getCert from many
	// goroutines at once. Because the check-then-act is inside one lock,
	// only the first caller to observe a stale cert should call refresh;
	// every later caller (already serialized behind the lock) re-checks
	// after acquiring it and finds the cert already fresh.
	advance(15 * 24 * time.Hour)

	const callers = 100
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = getCert()
		}()
	}
	wg.Wait()

	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refreshCalls = %d, want exactly 1", got)
	}
	if got := getCert().Data; got != "refreshed" {
		t.Fatalf("final cert = %q, want refreshed", got)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The first table walks the exact boundary a real deployment cares about: far
from expiry (no refresh), still outside the window (no refresh), just inside
the window (refresh, exactly once), and immediately after (already fresh, no
further refresh). The isolation test confirms two providers never share the
captured `cert`. The concurrency test is the one that matters most: it proves
the whole check-then-refresh is atomic with respect to other callers — a
hundred simultaneous handshakes past the refresh boundary must still result
in exactly one call to `refresh`, which only holds because the mutex guards
the complete decision and not just the read or just the write.

## Resources

- [pkg.go.dev: crypto/tls Config.GetCertificate](https://pkg.go.dev/crypto/tls#Config.GetCertificate) — the real callback this closure's signature is modeled on.
- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the full check-then-refresh critical section.
- [pkg.go.dev: sync/atomic](https://pkg.go.dev/sync/atomic) — the counter the concurrency test uses to prove exactly-once refresh.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-cache-invalidation-subscriber-broadcast.md](18-cache-invalidation-subscriber-broadcast.md) | Next: [20-multi-layer-cache-fallback-chain.md](20-multi-layer-cache-fallback-chain.md)
