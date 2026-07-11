# Exercise 8: Probe a GOPROXY chain for reachability with timeout and backoff

A build farm must know its proxy is up before it schedules builds, or every job
fails at `go build` time. This exercise builds the readiness probe: it contacts
each proxy in a chain with a per-attempt timeout and bounded backoff, classifies
each healthy/degraded/down, and returns the first proxy that can serve.

## What you'll build

```text
proxyhealth/               independent module: example.com/proxyhealth
  go.mod                   go 1.26
  health.go                Health, Prober, Probe, ProbeChain, sleepCtx
  cmd/
    demo/
      main.go              probes a down corp proxy then a healthy public one
  health_test.go           httptest: healthy, degraded-after-503, hang, deadline, chain
  example_test.go          ExampleProber_Probe with // Output
```

- Files: `health.go`, `cmd/demo/main.go`, `health_test.go`, `example_test.go`.
- Implement: `Probe(ctx, base, module) (Health, error)` with a per-attempt `context.WithTimeout` and exponential backoff retry, and `ProbeChain(ctx, bases, module)` returning the first serving proxy.
- Test: `httptest` servers scripted to return 200, a transient 503 then 200, and to hang past the timeout; assert the classification, that the parent deadline is respected, and that the chain stops at the first serving proxy.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/proxyhealth/cmd/demo
cd ~/go-exercises/proxyhealth
go mod init example.com/proxyhealth
go mod edit -go=1.26
```

### Two timeouts and a bounded retry

A readiness probe that can hang is worse than no probe, so this design has two
independent time bounds. The per-attempt bound is a `context.WithTimeout` derived
fresh for each request: if a proxy accepts the connection but never responds, the
attempt's context deadline fires and the request returns rather than blocking
forever. The overall bound is the parent context the caller passes: if it is
cancelled or hits its own deadline, `Probe` returns promptly with that error
instead of continuing to retry. Distinguishing the two matters — after a request
fails, the code checks `ctx.Err()` (the parent); if the parent is still live, the
failure was an attempt timeout or a transient transport error and is retried,
otherwise the probe stops. The retry itself is a bounded exponential backoff
(`BaseBackoff << (attempt-1)`) capped by `MaxAttempts`, and the backoff sleep is
itself cancellable via `select` on `ctx.Done()`, so a shutdown does not have to
wait out a backoff.

The classification falls out of this: a 200 on the first attempt is `Healthy`; a
200 that required at least one retry is `Degraded` (the proxy flapped but
recovered); exhausting the retry budget on 5xx or timeout is `Down`. `ProbeChain`
then walks the proxies in order and stops at the first `Healthy` or `Degraded`
one — a build farm only needs one serving proxy, and probing the rest wastes time
and load.

Create `health.go`:

```go
// Package proxyhealth probes a GOPROXY chain for reachability, classifying each
// proxy healthy/degraded/down with a per-attempt timeout and bounded backoff, and
// returns the first proxy that can serve a canary module.
package proxyhealth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Health is the classified reachability of one proxy.
type Health int

const (
	// Healthy: served the canary on the first attempt.
	Healthy Health = iota
	// Degraded: served, but only after at least one retry.
	Degraded
	// Down: no successful response within the retry budget.
	Down
)

func (h Health) String() string {
	switch h {
	case Healthy:
		return "healthy"
	case Degraded:
		return "degraded"
	default:
		return "down"
	}
}

// Prober probes proxies. Zero values are not useful; use NewProber.
type Prober struct {
	Client         *http.Client
	MaxAttempts    int
	AttemptTimeout time.Duration
	BaseBackoff    time.Duration
}

// NewProber returns a Prober with sensible defaults for a readiness check.
func NewProber() *Prober {
	return &Prober{
		Client:         &http.Client{},
		MaxAttempts:    3,
		AttemptTimeout: 2 * time.Second,
		BaseBackoff:    100 * time.Millisecond,
	}
}

// canaryURL builds the @latest request URL for the canary module.
func canaryURL(base, module string) string {
	return strings.TrimRight(base, "/") + "/" + module + "/@latest"
}

// Probe classifies one proxy by requesting the canary module's @latest. Each
// attempt gets its own timeout; a transient failure (5xx or attempt timeout) is
// retried with exponential backoff until MaxAttempts is exhausted. If the parent
// context is cancelled, Probe returns promptly with that error.
func (p *Prober) Probe(ctx context.Context, base, module string) (Health, error) {
	var lastErr error
	for attempt := range p.MaxAttempts {
		if attempt > 0 {
			backoff := p.BaseBackoff << (attempt - 1)
			if err := sleepCtx(ctx, backoff); err != nil {
				return Down, err
			}
		}

		actx, cancel := context.WithTimeout(ctx, p.AttemptTimeout)
		req, err := http.NewRequestWithContext(actx, http.MethodGet, canaryURL(base, module), nil)
		if err != nil {
			cancel()
			return Down, err
		}
		resp, err := p.Client.Do(req)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return Down, ctx.Err() // parent cancelled/deadline: stop.
			}
			// Attempt timeout or transient transport error: retry.
			lastErr = err
			continue
		}
		resp.Body.Close()
		cancel()

		if resp.StatusCode == http.StatusOK {
			if attempt == 0 {
				return Healthy, nil
			}
			return Degraded, nil
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("proxy returned %d", resp.StatusCode)
			continue
		}
		return Down, fmt.Errorf("proxy returned %d", resp.StatusCode)
	}
	return Down, lastErr
}

// ProxyResult pairs a proxy with its probed health.
type ProxyResult struct {
	Base   string
	Health Health
}

// Serving reports whether the proxy can serve (healthy or degraded).
func (r ProxyResult) Serving() bool { return r.Health == Healthy || r.Health == Degraded }

// ProbeChain probes proxies in order and stops at the first serving one. It
// returns the results probed so far and the index of the serving proxy, or -1.
func (p *Prober) ProbeChain(ctx context.Context, bases []string, module string) ([]ProxyResult, int) {
	var results []ProxyResult
	for i, base := range bases {
		h, _ := p.Probe(ctx, base, module)
		results = append(results, ProxyResult{Base: base, Health: h})
		if results[len(results)-1].Serving() {
			return results, i
		}
	}
	return results, -1
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
```

### The runnable demo

The demo probes a chain where the corporate proxy is down (persistent 503) and the
public proxy is healthy, showing the classification and the selected serving proxy.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/proxyhealth"
)

func main() {
	// A corporate proxy that is down (persistent 503) and a public proxy that is
	// healthy. The build farm needs the first serving proxy before scheduling.
	corp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer corp.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer public.Close()

	p := &proxyhealth.Prober{
		Client:         &http.Client{},
		MaxAttempts:    3,
		AttemptTimeout: 500 * time.Millisecond,
		BaseBackoff:    time.Millisecond,
	}

	bases := []string{corp.URL, public.URL}
	results, idx := p.ProbeChain(context.Background(), bases, "example.com/canary")

	for i, r := range results {
		name := "corp"
		if i == 1 {
			name = "public"
		}
		fmt.Printf("%-7s %s\n", name, r.Health)
	}
	if idx >= 0 {
		fmt.Printf("serving: proxy #%d\n", idx)
	} else {
		fmt.Println("serving: none")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
corp    down
public  healthy
serving: proxy #1
```

### Tests

The `httptest` servers script each condition: a plain 200 (healthy), a 503 on the
first call then 200 (degraded, and the call counter proves the retry), and a
handler that blocks on `r.Context().Done()` so every attempt times out (down —
blocking on the request context, not `time.Sleep`, avoids a leaked goroutine). A
deadline test wraps a short parent timeout around the hanging server and asserts
`errors.Is(err, context.DeadlineExceeded)` and that the probe returns quickly. The
chain test proves it stops at the first serving proxy and never touches the third.
The prober is tuned fast (short timeout, millisecond backoff) so the suite runs in
well under a second.

Create `health_test.go`:

```go
package proxyhealth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fastProber returns a Prober tuned for tests: short timeout, tiny backoff.
func fastProber() *Prober {
	return &Prober{
		Client:         &http.Client{},
		MaxAttempts:    3,
		AttemptTimeout: 200 * time.Millisecond,
		BaseBackoff:    time.Millisecond,
	}
}

func TestProbeHealthy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h, err := fastProber().Probe(t.Context(), srv.URL, "example.com/canary")
	if err != nil {
		t.Fatal(err)
	}
	if h != Healthy {
		t.Errorf("health = %v; want healthy", h)
	}
}

func TestProbeDegradedAfterTransient503(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h, err := fastProber().Probe(t.Context(), srv.URL, "example.com/canary")
	if err != nil {
		t.Fatal(err)
	}
	if h != Degraded {
		t.Errorf("health = %v; want degraded", h)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d; want 2 (one 503 then a retry)", calls.Load())
	}
}

func TestProbeDownOnHang(t *testing.T) {
	t.Parallel()
	// Handler blocks until the request context is cancelled, so every attempt
	// times out. Blocking on r.Context().Done avoids a leaked goroutine.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	h, _ := fastProber().Probe(t.Context(), srv.URL, "example.com/canary")
	if h != Down {
		t.Errorf("health = %v; want down", h)
	}
}

func TestProbeRespectsParentDeadline(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := fastProber().Probe(ctx, srv.URL, "example.com/canary")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("probe waited %v; parent deadline was 50ms", elapsed)
	}
}

func TestProbeChainStopsAtFirstServing(t *testing.T) {
	t.Parallel()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	var thirdHits atomic.Int32
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(third.Close)

	bases := []string{down.URL, up.URL, third.URL}
	results, idx := fastProber().ProbeChain(t.Context(), bases, "example.com/canary")
	if idx != 1 {
		t.Errorf("serving index = %d; want 1", idx)
	}
	if len(results) != 2 {
		t.Errorf("probed %d proxies; want 2 (stops at first serving)", len(results))
	}
	if thirdHits.Load() != 0 {
		t.Errorf("third proxy was probed %d times; want 0", thirdHits.Load())
	}
	if results[0].Health != Down {
		t.Errorf("first proxy = %v; want down", results[0].Health)
	}
}
```

Create `example_test.go`:

```go
package proxyhealth_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/proxyhealth"
)

func ExampleProber_Probe() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &proxyhealth.Prober{
		Client:         &http.Client{},
		MaxAttempts:    3,
		AttemptTimeout: 500 * time.Millisecond,
		BaseBackoff:    time.Millisecond,
	}
	h, _ := p.Probe(context.Background(), srv.URL, "example.com/canary")
	fmt.Println(h)
	// Output: healthy
}
```

## Review

The probe is correct when it never waits longer than the caller's context allows
and its classification matches the scripted behavior: 200 first try is healthy,
503-then-200 is degraded with exactly one retry, and a hang is down. The design
detail that separates a good probe from a dangerous one is the two-timeout
structure — a fresh `context.WithTimeout` per attempt bounds each request, and the
parent-context check after a failure decides retry versus stop. The trap to avoid
in the test is a handler that hangs on `time.Sleep`: it leaks a goroutine past the
test; block on `r.Context().Done()` instead so it returns when the client cancels.
Run `go test -race` — the counters shared with handlers must be race-free, which is
why they use `sync/atomic`.

## Resources

- [`net/http.NewRequestWithContext`](https://pkg.go.dev/net/http#NewRequestWithContext) — attaching a per-attempt deadline to a request.
- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — the per-attempt and overall deadlines.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — scripting proxy responses for the probe tests.
- [Go blog: Context](https://go.dev/blog/context) — cancellation propagation, the model behind the two-timeout design.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-gosum-tidy-auditor.md](07-gosum-tidy-auditor.md) | Next: [09-modcache-integrity-scan.md](09-modcache-integrity-scan.md)
