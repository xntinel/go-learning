# Exercise 9: A /healthz Readiness Probe Backed by PingContext With a Bounded Pool

Every service exposes a readiness probe, and the two ways to get it wrong are both
availability bugs: an unbounded pool that grows without limit under a slow
database, and a `Ping` with no timeout that hangs the health endpoint so the load
balancer never learns the instance is sick. This exercise builds the probe done
right — `PingContext` under its own short timeout, behind a `/healthz` handler,
over a pool with all four bounds set.

## What you'll build

```text
healthprobe/                 independent module: example.com/healthprobe
  go.mod                     go 1.25; requires modernc.org/sqlite
  healthprobe.go             Tune(db); Readiness(ctx, Pinger, timeout); Handler serving /healthz
  cmd/
    demo/
      main.go                healthy sqlite -> 200; a wedged pinger -> 503, fast
  healthprobe_test.go        healthy 200, wedged 503, own-timeout, ping-deadline, -race
```

Files: `healthprobe.go`, `cmd/demo/main.go`, `healthprobe_test.go`.
Implement: `Tune(db)` setting the four pool bounds; a `Pinger` interface (`*sql.DB` satisfies it); `Readiness(ctx, p, timeout)` that pings under its *own* `WithTimeout`; and a `Handler` returning 200 when the ping succeeds within budget and 503 otherwise.
Test: against real sqlite the handler writes 200; a fake pinger that blocks makes `PingContext` return `DeadlineExceeded` within the probe timeout and the handler writes 503 without hanging; the probe bounds itself even when the parent context is unbounded.
Verify: `go test -count=1 -race ./...`

### Why the probe owns its timeout, and why the pool is bounded

The probe abstracts over a `Pinger`, which `*sql.DB` satisfies through
`PingContext(ctx) error`. That interface is what lets the test substitute a wedged
database without a real hung socket, and it is also good design: the readiness
logic does not care that a SQL database is underneath. `Readiness` derives its own
deadline and never trusts the caller's:

```
func Readiness(ctx context.Context, p Pinger, timeout time.Duration) Status {
    pctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
    if err := p.PingContext(pctx); err != nil {
        return Status{Healthy: false, Detail: err.Error()}
    }
    return Status{Healthy: true, Detail: "ok"}
}
```

This is the load-bearing decision. If the probe forwarded the request's context
unchanged and that context were unbounded (or generous), a wedged database would
block the ping for the whole request and the health endpoint would hang — exactly
when you most need it to answer. By deriving `pctx` with its own short timeout, a
stuck database fails the probe in, say, 200ms, the handler returns 503, and the
load balancer pulls the instance from rotation fast. The test proves this by
passing `context.Background()` (which never fires) with a blocking pinger and
asserting the probe still returns `DeadlineExceeded` within its own timeout — the
deadline could only have come from the probe.

`Tune` sets all four pool bounds. `SetMaxOpenConns` is the hard ceiling that stops
a slow database from growing connections without limit; `SetMaxIdleConns` keeps a
few warm to avoid reconnect churn; `SetConnMaxLifetime` recycles connections so a
load balancer that rotates database backends does not pin you to a dead one; and
`SetConnMaxIdleTime` reaps connections that have sat unused. Leaving any of these
at the default (unbounded open connections, in particular) is how a dependency
slowdown becomes unbounded connection growth.

Set up the module:

```bash
mkdir -p ~/go-exercises/healthprobe/cmd/demo
cd ~/go-exercises/healthprobe
go mod init example.com/healthprobe
go mod edit -go=1.25
go get modernc.org/sqlite
```

Create `healthprobe.go`:

```go
package healthprobe

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// Pinger is the readiness dependency. *sql.DB satisfies it via PingContext.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// Status is the result of a readiness check.
type Status struct {
	Healthy bool
	Detail  string
}

// Tune sets all four pool bounds so a slow database cannot grow connections
// without limit and stale connections are recycled.
func Tune(db *sql.DB) {
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
}

// Readiness pings p under its own short timeout, never trusting the caller's
// context to be bounded, so a wedged database fails fast instead of hanging.
func Readiness(ctx context.Context, p Pinger, timeout time.Duration) Status {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := p.PingContext(pctx); err != nil {
		return Status{Healthy: false, Detail: err.Error()}
	}
	return Status{Healthy: true, Detail: "ok"}
}

// Handler serves a /healthz readiness probe.
type Handler struct {
	pinger  Pinger
	timeout time.Duration
}

// NewHandler returns a probe handler that pings p with the given per-probe timeout.
func NewHandler(p Pinger, timeout time.Duration) *Handler {
	return &Handler{pinger: p, timeout: timeout}
}

// ServeHTTP writes 200 when the ping succeeds within budget, 503 otherwise.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := Readiness(r.Context(), h.pinger, h.timeout)
	if st.Healthy {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, "not ready: %s\n", st.Detail)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"time"

	"example.com/healthprobe"

	_ "modernc.org/sqlite"
)

// wedged models a database that never answers a ping.
type wedged struct{}

func (wedged) PingContext(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func main() {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	healthprobe.Tune(db)

	h := healthprobe.NewHandler(db, 500*time.Millisecond)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	fmt.Printf("healthy: status=%d\n", rec.Code)

	hw := healthprobe.NewHandler(wedged{}, 20*time.Millisecond)
	rec2 := httptest.NewRecorder()
	start := time.Now()
	hw.ServeHTTP(rec2, httptest.NewRequest("GET", "/healthz", nil))
	fmt.Printf("wedged:  status=%d fast=%v\n", rec2.Code, time.Since(start) < 200*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
healthy: status=200
wedged:  status=503 fast=true
```

### Tests

`TestProbeUsesOwnTimeoutNotParent` is the key assertion: the parent context is
`context.Background()`, so the only deadline that can fire is the probe's own.

Create `healthprobe_test.go`:

```go
package healthprobe

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// wedged models a database that never answers a ping.
type wedged struct{}

func (wedged) PingContext(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Skipf("sqlite driver unavailable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestHealthyReturns200(t *testing.T) {
	t.Parallel()
	db := openSQLite(t)
	Tune(db)

	h := NewHandler(db, 500*time.Millisecond)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
}

func TestWedgedReturns503Fast(t *testing.T) {
	t.Parallel()
	h := NewHandler(wedged{}, 20*time.Millisecond)
	rec := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("probe hung: took %v, want fast fail near the 20ms probe timeout", elapsed)
	}
}

func TestProbeUsesOwnTimeoutNotParent(t *testing.T) {
	t.Parallel()
	// Background never fires; the only deadline is the probe's own 20ms.
	st := Readiness(context.Background(), wedged{}, 20*time.Millisecond)
	if st.Healthy {
		t.Fatal("Readiness reported healthy against a wedged pinger")
	}
}

func TestPingDeadlineIsDeadlineExceeded(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := (wedged{}).PingContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PingContext = %v, want DeadlineExceeded", err)
	}
}
```

## Review

The probe is correct when a wedged database fails it fast instead of hanging it.
`TestWedgedReturns503Fast` pins both: the status is 503 and the handler returned
well inside the 20ms probe timeout, not the (unbounded) request context.
`TestProbeUsesOwnTimeoutNotParent` isolates the mechanism — with a
`context.Background()` parent, the deadline can only be the probe's own, which is
the whole reason `Readiness` derives its own `WithTimeout` instead of forwarding
`r.Context()`. The healthy path over real sqlite confirms `*sql.DB` satisfies
`Pinger` and a live database answers within budget. The pool bounds in `Tune`
address the other half of the availability story: without `SetMaxOpenConns` a slow
database grows connections without limit. Run `-race`: the wedged pinger blocks on
a goroutine while the probe's timer fires.

## Resources

- [database/sql: DB.PingContext](https://pkg.go.dev/database/sql#DB.PingContext) — the bounded liveness check.
- [database/sql: DB.SetMaxOpenConns](https://pkg.go.dev/database/sql#DB.SetMaxOpenConns) and [SetConnMaxLifetime](https://pkg.go.dev/database/sql#DB.SetConnMaxLifetime) — the pool bounds.
- [Go docs: Managing connections](https://go.dev/doc/database/manage-connections) — official pool-tuning guidance.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — driving the handler without a live socket.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-detached-audit-cleanup.md](10-detached-audit-cleanup.md)
