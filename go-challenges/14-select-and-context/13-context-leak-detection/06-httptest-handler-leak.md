# Exercise 6: Catch A Per-Request Context Leak In An HTTP Handler

The most common place a context leak ships is an HTTP handler: one dropped
`cancel` per request, invisible in a code review, fatal after a day of traffic.
This module injects the detector into a handler, spins the handler up under
`httptest.NewServer`, fires real requests at it, and uses the detector to prove
the buggy handler leaks exactly one context per request while the fixed handler
leaks none.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
handlerleak/                       module example.com/handlerleak
  go.mod
  internal/leakdetect/
    leakdetect.go                  the detector (same core as Exercise 1)
  handler.go                       LeakyHandler vs FixedHandler over a shared Detector
  handler_test.go                  httptest server, N concurrent requests, leak-count assertions
  cmd/
    demo/
      main.go                      one request each way, print ActiveContexts
```

Files: `internal/leakdetect/leakdetect.go`, `handler.go`, `handler_test.go`, `cmd/demo/main.go`.
Implement: `LeakyHandler(d)` (drops cancel) and `FixedHandler(d)` (defers cancel), both deriving a per-request timeout for a downstream call from the same `Detector`.
Test: with `httptest.NewServer`, N concurrent requests leave exactly N leaks for the leaky handler and 0 for the fixed one after the grace period.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/13-context-leak-detection/06-httptest-handler-leak/internal/leakdetect
mkdir -p go-solutions/14-select-and-context/13-context-leak-detection/06-httptest-handler-leak/cmd/demo
cd go-solutions/14-select-and-context/13-context-leak-detection/06-httptest-handler-leak
```

### Why the leaked context is parented on Background, not r.Context

There is a subtlety here that separates a shallow lesson from a correct one. A
Go server cancels the request context the moment `ServeHTTP` returns (the
`net/http` contract: the request context is canceled "when the client's
connection closes ... or when the ServeHTTP method returns"). So a child derived
from `r.Context()` with a *dropped* cancel is *self-healing*: when the handler
returns, `r.Context()` is cancelled, the child becomes `Done`, and the detector's
`AfterFunc` deregisters it. The leak lasts only as long as the request. That is
still a bug — `go vet`'s `lostcancel` rightly flags it, and the timer stays armed
for the request's duration — but it would not show up as a *persistent* leak in an
after-the-fact count.

The genuinely dangerous version, and the one this module reproduces, parents the
downstream context on `context.Background()`. Handlers do this deliberately, to
keep a downstream write alive *past* a client disconnect — a real pattern with a
real motivation. But `Background()` is never cancelled, so a dropped cancel there
is a *permanent* leak: the context node and its armed 2-second timer survive the
request and pile up, one per call. Holding the parent fixed at `Background()` and
varying only whether `cancel` is called isolates the single variable this lesson
is about. (The correct way to detach downstream work *and* keep it bounded is
`WithoutCancel` plus a fresh timeout, which is Exercise 10.)

Create `internal/leakdetect/leakdetect.go` (the same detector as Exercise 1):

```go
// internal/leakdetect/leakdetect.go
package leakdetect

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type record struct {
	createdAt time.Time
	caller    string
	funcName  string
}

// LeakReport describes one context still outstanding past the grace period.
type LeakReport struct {
	Caller   string
	FuncName string
	Age      time.Duration
}

func (r LeakReport) String() string {
	return fmt.Sprintf("context leak: created at %s (%s), age %v", r.Caller, r.FuncName, r.Age)
}

// Detector wraps the cancellable context constructors and tracks every context
// that has not yet become Done. Safe for concurrent use.
type Detector struct {
	mu          sync.Mutex
	records     map[*record]struct{}
	gracePeriod time.Duration
	created     atomic.Int64
}

// New returns a Detector reporting contexts outstanding past gracePeriod.
func New(gracePeriod time.Duration) *Detector {
	return &Detector{
		records:     make(map[*record]struct{}),
		gracePeriod: gracePeriod,
	}
}

func callerInfo(skip int) (location, funcName string) {
	pc, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown:0", "unknown"
	}
	name := "unknown"
	if fn := runtime.FuncForPC(pc); fn != nil {
		name = fn.Name()
	}
	return fmt.Sprintf("%s:%d", filepath.Base(file), line), name
}

func (d *Detector) register(skip int) *record {
	loc, fn := callerInfo(skip)
	r := &record{createdAt: time.Now(), caller: loc, funcName: fn}
	d.mu.Lock()
	d.records[r] = struct{}{}
	d.mu.Unlock()
	d.created.Add(1)
	return r
}

func (d *Detector) deregister(r *record) {
	d.mu.Lock()
	delete(d.records, r)
	d.mu.Unlock()
}

func (d *Detector) track(ctx context.Context, cancel context.CancelFunc) (context.Context, context.CancelFunc) {
	r := d.register(4)
	context.AfterFunc(ctx, func() { d.deregister(r) })
	return ctx, func() {
		cancel()
		d.deregister(r)
	}
}

// WithTimeout wraps context.WithTimeout.
func (d *Detector) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	return d.track(ctx, cancel)
}

// Check returns one LeakReport per context outstanding past the grace period.
func (d *Detector) Check() []LeakReport {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []LeakReport
	for r := range d.records {
		if age := now.Sub(r.createdAt); age >= d.gracePeriod {
			out = append(out, LeakReport{Caller: r.caller, FuncName: r.funcName, Age: age})
		}
	}
	return out
}

// ActiveContexts returns the number of currently outstanding contexts.
func (d *Detector) ActiveContexts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.records)
}

// TotalCreated returns the number of contexts ever created through this detector.
func (d *Detector) TotalCreated() int64 {
	return d.created.Load()
}
```

### The handlers

`fetchUser` stands in for a downstream call that honors its context. Both handlers
derive a 2-second budget for it; the only difference is `LeakyHandler` throws the
cancel away and `FixedHandler` defers it.

Create `handler.go`:

```go
package handlerleak

import (
	"context"
	"net/http"
	"time"

	"example.com/handlerleak/internal/leakdetect"
)

// fetchUser simulates a fast downstream call that respects its context.
func fetchUser(ctx context.Context) string {
	select {
	case <-time.After(time.Millisecond):
		return "alice"
	case <-ctx.Done():
		return ""
	}
}

// LeakyHandler derives a downstream budget from Background (to survive a client
// disconnect) but drops the cancel: one permanent leaked context per request.
func LeakyHandler(d *leakdetect.Detector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := d.WithTimeout(context.Background(), 2*time.Second) // BUG: cancel dropped
		_, _ = w.Write([]byte(fetchUser(ctx)))
	}
}

// FixedHandler is identical except it defers cancel, releasing the context (and
// its timer) as the handler returns.
func FixedHandler(d *leakdetect.Detector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := d.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = w.Write([]byte(fetchUser(ctx)))
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/handlerleak"
	"example.com/handlerleak/internal/leakdetect"
)

func hit(url string) {
	resp, err := http.Get(url)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func main() {
	dLeaky := leakdetect.New(10 * time.Millisecond)
	leaky := httptest.NewServer(handlerleak.LeakyHandler(dLeaky))
	defer leaky.Close()
	for range 3 {
		hit(leaky.URL)
	}
	time.Sleep(30 * time.Millisecond)
	fmt.Println("leaky handler active contexts:", dLeaky.ActiveContexts())

	dFixed := leakdetect.New(10 * time.Millisecond)
	fixed := httptest.NewServer(handlerleak.FixedHandler(dFixed))
	defer fixed.Close()
	for range 3 {
		hit(fixed.URL)
	}
	time.Sleep(30 * time.Millisecond)
	fmt.Println("fixed handler active contexts:", dFixed.ActiveContexts())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaky handler active contexts: 3
fixed handler active contexts: 0
```

### Tests

Create `handler_test.go`:

```go
package handlerleak

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"example.com/handlerleak/internal/leakdetect"
)

func fireRequests(t *testing.T, url string, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(url)
			if err != nil {
				t.Errorf("GET: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
}

func TestLeakyHandlerLeaksOnePerRequest(t *testing.T) {
	t.Parallel()

	d := leakdetect.New(10 * time.Millisecond)
	srv := httptest.NewServer(LeakyHandler(d))
	defer srv.Close()

	const n = 5
	fireRequests(t, srv.URL, n)

	time.Sleep(40 * time.Millisecond) // grace + AfterFunc drain window
	if got := d.ActiveContexts(); got != n {
		t.Fatalf("ActiveContexts = %d after %d requests, want %d", got, n, n)
	}
	if got := len(d.Check()); got != n {
		t.Fatalf("Check reported %d leaks, want %d", got, n)
	}
}

func TestFixedHandlerNoLeak(t *testing.T) {
	t.Parallel()

	d := leakdetect.New(10 * time.Millisecond)
	srv := httptest.NewServer(FixedHandler(d))
	defer srv.Close()

	fireRequests(t, srv.URL, 5)

	time.Sleep(40 * time.Millisecond)
	if got := d.ActiveContexts(); got != 0 {
		t.Fatalf("ActiveContexts = %d, want 0 (fixed handler must not leak)", got)
	}
	if got := len(d.Check()); got != 0 {
		t.Fatalf("Check reported %d leaks, want 0", got)
	}
}
```

## Review

The test is correct when N concurrent requests to the leaky handler leave exactly
N contexts active after the grace period, and the fixed handler leaves zero.
Because the leaked context is parented on `Background()`, it survives the request
and is genuinely counted; a version parented on `r.Context()` would self-heal at
handler return and quietly report zero, which is the trap the explanation warns
about. Running under `-race` is not optional here: the detector's `register` and
`deregister` are called from N server goroutines at once, so an unlocked map would
be caught. The broader point is that a `Detector` injected into a handler makes a
production request path *auditable* — the same wiring, compiled behind a build tag,
turns into a CI leak gate.

## Resources

- [net/http/httptest.NewServer](https://pkg.go.dev/net/http/httptest#NewServer) — spinning a real server around a handler for tests.
- [net/http.Request.Context](https://pkg.go.dev/net/http#Request.Context) — the request-context cancellation contract that makes `r.Context()`-parented leaks self-heal.
- [go vet: lostcancel](https://pkg.go.dev/cmd/vet) — the static check that flags a dropped `CancelFunc` in the same function.

---

Back to [05-goroutine-leak-guard.md](05-goroutine-leak-guard.md) | Next: [07-cancelcause-diagnostics.md](07-cancelcause-diagnostics.md)
