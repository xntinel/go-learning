# 12. Connection Pool Health Monitoring

`database/sql` maintains a connection pool whose behavior is almost entirely opaque at runtime unless you explicitly read its statistics. The hard part is not reading the numbers — `db.Stats()` does that in one call — but deciding what they mean, keeping the history thread-safe, and wiring the result into a readiness probe that actually stops traffic when the pool degrades. This lesson builds a `poolmon` package that does all three.

```text
poolmon/
  go.mod
  poolmon.go
  poolmon_test.go
  cmd/demo/main.go
```

## Concepts

### What sql.DBStats Contains

`db.Stats()` returns a `sql.DBStats` snapshot. The fields that matter for health monitoring are:

| Field | Type | Meaning |
|---|---|---|
| `MaxOpenConnections` | `int` | ceiling set by `SetMaxOpenConns` (0 = unlimited) |
| `OpenConnections` | `int` | established connections (in-use + idle) |
| `InUse` | `int` | connections currently held by callers |
| `Idle` | `int` | connections waiting in the pool |
| `WaitCount` | `int64` | cumulative goroutines that had to wait |
| `WaitDuration` | `time.Duration` | cumulative time those goroutines waited |
| `MaxIdleClosed` | `int64` | connections closed because idle count exceeded the cap |
| `MaxIdleTimeClosed` | `int64` | connections closed because they were idle too long |
| `MaxLifetimeClosed` | `int64` | connections closed because they exceeded their lifetime |

These are cumulative counters since `sql.Open`; they never reset. Utilization and average-wait must be derived.

### Utilization And Average Wait

Two derived metrics reveal pool pressure:

```
utilization = InUse / MaxOpenConnections   (0.0–1.0; only valid when MaxOpenConnections > 0)
avgWait     = WaitDuration / WaitCount     (only valid when WaitCount > 0)
```

A utilization above 0.8 means the pool is nearly saturated; any new burst will queue. An average wait above 50 ms means callers are regularly blocked. Both thresholds are configurable because the right values depend on the query latency profile.

### Pool Configuration Parameters

Four parameters shape the pool; set them immediately after `sql.Open`:

```go
db.SetMaxOpenConns(25)               // hard cap on open connections
db.SetMaxIdleConns(10)               // idle connections to keep warm
db.SetConnMaxLifetime(5 * time.Minute)  // recycle connections (avoids server-side timeouts)
db.SetConnMaxIdleTime(1 * time.Minute)  // evict long-idle connections
```

`MaxIdleConns` must be less than or equal to `MaxOpenConns`; the runtime enforces this silently. If `MaxOpenConns` is 0 the pool is unbounded — avoid this in production.

### Thread Safety And History

`db.Stats()` is safe to call from any goroutine; the pool's internal mutex protects it. The `Monitor` in this lesson wraps the snapshot in its own `sync.RWMutex` so callers can read `LastSnapshot()` without triggering a new `Stats()` call, and so `history` (a rolling ring of recent snapshots) is never observed in a partially-written state.

### Status Classification

The monitor maps each snapshot to one of three statuses:

```
Healthy  — utilization < warnThreshold AND avgWait < warnWait
Degraded — utilization >= warnThreshold OR avgWait >= warnWait
Critical — utilization >= critThreshold OR avgWait >= critWait
```

The same classification drives the readiness probe: a `Critical` pool returns HTTP 503 so the load balancer stops sending traffic while connections are exhausted.

### Failure Modes

- **Unlimited pool (`MaxOpenConns == 0`)**: utilization is always 0; the threshold check is skipped. The pool can exhaust the database server's connection limit before your service shows any degradation signal.
- **Over-generous `MaxIdleConns`**: idle connections age out on the database side (server-side `wait_timeout`) and return broken on reuse. Set `SetConnMaxLifetime` below the server's timeout.
- **Not closing `*sql.Rows`**: each un-closed `Rows` holds one connection in `InUse` indefinitely. `defer rows.Close()` is not optional.
- **Reading `WaitCount` without checking for 0**: integer division by zero panics. Always guard before computing `avgWait`.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/30-production-patterns/12-connection-pool-health-monitoring/12-connection-pool-health-monitoring/cmd/demo
cd go-solutions/30-production-patterns/12-connection-pool-health-monitoring/12-connection-pool-health-monitoring
```

This is a library package. Verification is `go test`, not `go run`.

### Exercise 1: The Monitor Type And Status Classification

Create `poolmon.go`:

```go
// poolmon.go
package poolmon

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Status classifies the current health of the connection pool.
type Status string

const (
	// StatusHealthy means the pool is operating within all thresholds.
	StatusHealthy Status = "healthy"
	// StatusDegraded means at least one warn threshold has been exceeded.
	StatusDegraded Status = "degraded"
	// StatusCritical means at least one critical threshold has been exceeded.
	StatusCritical Status = "critical"
)

// ErrCritical is returned by Check when the pool status is critical.
var ErrCritical = errors.New("connection pool critical")

// Snapshot is a point-in-time view of pool metrics derived from sql.DBStats.
type Snapshot struct {
	// Raw fields from sql.DBStats.
	MaxOpen           int
	Open              int
	InUse             int
	Idle              int
	WaitCount         int64
	WaitDuration      time.Duration
	MaxIdleClosed     int64
	MaxLifetimeClosed int64

	// Derived fields.
	Utilization float64       // InUse / MaxOpen; 0 when MaxOpen == 0
	AvgWait     time.Duration // WaitDuration / WaitCount; 0 when WaitCount == 0
	Status      Status
	CapturedAt  time.Time
}

// Thresholds controls when the monitor transitions between statuses.
type Thresholds struct {
	UtilizationWarn     float64       // fraction, e.g. 0.70
	UtilizationCritical float64       // fraction, e.g. 0.90
	AvgWaitWarn         time.Duration // e.g. 50ms
	AvgWaitCritical     time.Duration // e.g. 200ms
}

// DefaultThresholds returns conservative production defaults.
func DefaultThresholds() Thresholds {
	return Thresholds{
		UtilizationWarn:     0.70,
		UtilizationCritical: 0.90,
		AvgWaitWarn:         50 * time.Millisecond,
		AvgWaitCritical:     200 * time.Millisecond,
	}
}

// Monitor collects pool metrics from a *sql.DB and maintains a rolling history.
type Monitor struct {
	db         *sql.DB
	thresholds Thresholds
	maxHistory int

	mu      sync.RWMutex
	last    *Snapshot
	history []Snapshot
}

// New creates a Monitor for db using the given thresholds.
// maxHistory controls how many snapshots are retained (ring buffer).
func New(db *sql.DB, t Thresholds, maxHistory int) *Monitor {
	if maxHistory < 1 {
		maxHistory = 1
	}
	return &Monitor{
		db:         db,
		thresholds: t,
		maxHistory: maxHistory,
	}
}

// Collect reads the current pool statistics, classifies them, stores the
// snapshot in the history ring, and returns the snapshot.
func (m *Monitor) Collect() Snapshot {
	raw := m.db.Stats()

	var util float64
	if raw.MaxOpenConnections > 0 {
		util = float64(raw.InUse) / float64(raw.MaxOpenConnections)
	}

	var avgWait time.Duration
	if raw.WaitCount > 0 {
		avgWait = raw.WaitDuration / time.Duration(raw.WaitCount)
	}

	status := classify(util, avgWait, m.thresholds)

	snap := Snapshot{
		MaxOpen:           raw.MaxOpenConnections,
		Open:              raw.OpenConnections,
		InUse:             raw.InUse,
		Idle:              raw.Idle,
		WaitCount:         raw.WaitCount,
		WaitDuration:      raw.WaitDuration,
		MaxIdleClosed:     raw.MaxIdleClosed,
		MaxLifetimeClosed: raw.MaxLifetimeClosed,
		Utilization:       util,
		AvgWait:           avgWait,
		Status:            status,
		CapturedAt:        time.Now().UTC(),
	}

	m.mu.Lock()
	m.last = &snap
	m.history = append(m.history, snap)
	if len(m.history) > m.maxHistory {
		m.history = m.history[1:]
	}
	m.mu.Unlock()

	return snap
}

// LastSnapshot returns the most recent collected snapshot, or nil if Collect
// has never been called.
func (m *Monitor) LastSnapshot() *Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.last
}

// History returns a copy of the rolling snapshot history (oldest first).
func (m *Monitor) History() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Snapshot, len(m.history))
	copy(out, m.history)
	return out
}

// Check collects a fresh snapshot and returns ErrCritical (wrapped with detail)
// when the pool status is critical.
func (m *Monitor) Check() error {
	snap := m.Collect()
	if snap.Status == StatusCritical {
		return fmt.Errorf("%w: utilization=%.0f%% avg_wait=%v",
			ErrCritical, snap.Utilization*100, snap.AvgWait)
	}
	return nil
}

// classify maps utilization and avgWait to a Status.
func classify(util float64, avgWait time.Duration, t Thresholds) Status {
	if util >= t.UtilizationCritical || avgWait >= t.AvgWaitCritical {
		return StatusCritical
	}
	if util >= t.UtilizationWarn || avgWait >= t.AvgWaitWarn {
		return StatusDegraded
	}
	return StatusHealthy
}
```

`classify` is a pure function kept separate from `Collect` so that tests can verify the threshold logic without a real database.

### Exercise 2: Synthetic Stats For Testing

The test file needs a `*sql.DB` to hand to `New`. Rather than pulling in a cgo SQLite driver, register a minimal no-op driver that satisfies the `database/sql/driver` interfaces and lets the test control `db.Stats()` indirectly by opening and holding connections.

Create `poolmon_test.go`:

```go
// poolmon_test.go
package poolmon

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// -- minimal no-op driver --------------------------------------------------

type fakeDriver struct {
	mu    sync.Mutex
	holds chan struct{} // signals that a conn is being held open
}

func (d *fakeDriver) Open(_ string) (driver.Conn, error) {
	return &fakeConn{}, nil
}

type fakeConn struct{}

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{}, nil
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeStmt struct{}

func (s *fakeStmt) Close() error                                 { return nil }
func (s *fakeStmt) NumInput() int                                { return -1 }
func (s *fakeStmt) Exec(_ []driver.Value) (driver.Result, error) { return fakeResult{}, nil }
func (s *fakeStmt) Query(_ []driver.Value) (driver.Rows, error)  { return &fakeRows{}, nil }

type fakeRows struct{ done bool }

func (r *fakeRows) Columns() []string { return []string{"x"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 0, nil }

type fakeTx struct{}

func (t *fakeTx) Commit() error   { return nil }
func (t *fakeTx) Rollback() error { return nil }

func init() {
	sql.Register("fake", &fakeDriver{})
}

func openFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("fake", "test")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// -- tests ------------------------------------------------------------------

func TestCollectReturnsSnapshot(t *testing.T) {
	t.Parallel()

	db := openFakeDB(t)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	m := New(db, DefaultThresholds(), 60)
	snap := m.Collect()

	if snap.Status == "" {
		t.Fatal("Collect returned snapshot with empty status")
	}
	if snap.CapturedAt.IsZero() {
		t.Fatal("CapturedAt not set")
	}
	if snap.MaxOpen != 10 {
		t.Errorf("MaxOpen = %d, want 10", snap.MaxOpen)
	}
}

func TestLastSnapshotNilBeforeFirstCollect(t *testing.T) {
	t.Parallel()

	db := openFakeDB(t)
	m := New(db, DefaultThresholds(), 10)

	if got := m.LastSnapshot(); got != nil {
		t.Fatalf("LastSnapshot before Collect = %v, want nil", got)
	}
}

func TestLastSnapshotReflectsCollect(t *testing.T) {
	t.Parallel()

	db := openFakeDB(t)
	m := New(db, DefaultThresholds(), 10)
	snap := m.Collect()

	got := m.LastSnapshot()
	if got == nil {
		t.Fatal("LastSnapshot is nil after Collect")
	}
	if got.CapturedAt != snap.CapturedAt {
		t.Errorf("LastSnapshot.CapturedAt = %v, want %v", got.CapturedAt, snap.CapturedAt)
	}
}

func TestHistoryRingBounded(t *testing.T) {
	t.Parallel()

	db := openFakeDB(t)
	m := New(db, DefaultThresholds(), 3)

	for i := 0; i < 7; i++ {
		m.Collect()
	}

	h := m.History()
	if len(h) != 3 {
		t.Fatalf("History len = %d, want 3 (ring size)", len(h))
	}
}

func TestHistoryReturnsCopy(t *testing.T) {
	t.Parallel()

	db := openFakeDB(t)
	m := New(db, DefaultThresholds(), 10)
	m.Collect()

	h := m.History()
	original := h[0].Status
	h[0].Status = "mutated"

	h2 := m.History()
	if h2[0].Status != original {
		t.Fatal("History returned a shared slice; mutation visible in Monitor")
	}
}

func TestCheckReturnsNilWhenHealthy(t *testing.T) {
	t.Parallel()

	db := openFakeDB(t)
	db.SetMaxOpenConns(100)
	// No connections in use; utilization is 0.
	m := New(db, DefaultThresholds(), 10)

	if err := m.Check(); err != nil {
		t.Fatalf("Check() = %v, want nil for idle pool", err)
	}
}

func TestCheckReturnsErrCriticalWhenThresholdBreached(t *testing.T) {
	t.Parallel()

	// Use very tight thresholds so the synthetic pool triggers critical.
	tight := Thresholds{
		UtilizationWarn:     0.01,
		UtilizationCritical: 0.01,
		AvgWaitWarn:         time.Nanosecond,
		AvgWaitCritical:     time.Nanosecond,
	}
	db := openFakeDB(t)
	db.SetMaxOpenConns(1)

	// Hold a connection in-use to drive utilization to 1.0.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	m := New(db, tight, 10)
	err = m.Check()
	if !errors.Is(err, ErrCritical) {
		t.Fatalf("Check() = %v, want errors.Is(err, ErrCritical)", err)
	}
}

func TestClassifyTable(t *testing.T) {
	t.Parallel()

	thresh := Thresholds{
		UtilizationWarn:     0.70,
		UtilizationCritical: 0.90,
		AvgWaitWarn:         50 * time.Millisecond,
		AvgWaitCritical:     200 * time.Millisecond,
	}

	cases := []struct {
		name    string
		util    float64
		avgWait time.Duration
		want    Status
	}{
		{"all_zero", 0, 0, StatusHealthy},
		{"below_warn", 0.5, 10 * time.Millisecond, StatusHealthy},
		{"util_at_warn", 0.70, 0, StatusDegraded},
		{"wait_at_warn", 0, 50 * time.Millisecond, StatusDegraded},
		{"util_at_critical", 0.90, 0, StatusCritical},
		{"wait_at_critical", 0, 200 * time.Millisecond, StatusCritical},
		{"both_critical", 0.95, 300 * time.Millisecond, StatusCritical},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classify(tc.util, tc.avgWait, thresh)
			if got != tc.want {
				t.Errorf("classify(util=%.2f, avgWait=%v) = %q, want %q",
					tc.util, tc.avgWait, got, tc.want)
			}
		})
	}
}

func ExampleMonitor_Check() {
	db, _ := sql.Open("fake", "test")
	defer db.Close()
	db.SetMaxOpenConns(10)

	m := New(db, DefaultThresholds(), 60)
	err := m.Check()
	if err == nil {
		fmt.Println("healthy")
	}
	// Output: healthy
}
```

### Exercise 3: Wire The Monitor Into A Readiness Probe

The demo shows the exported surface of `poolmon` and how to embed the monitor in an HTTP handler. It must not import unexported identifiers.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"example.com/poolmon"
)

// demoDriver is a minimal no-op SQL driver used so the demo compiles and runs
// without a real database. In production, replace this with a real driver
// registered via a blank-identifier import (e.g. _ "github.com/lib/pq").
type demoDriver struct{}
type demoConn struct{}
type demoStmt struct{}
type demoRows struct{ done bool }
type demoResult struct{}
type demoTx struct{}

func (d *demoDriver) Open(_ string) (driver.Conn, error)         { return &demoConn{}, nil }
func (c *demoConn) Prepare(q string) (driver.Stmt, error)        { return &demoStmt{}, nil }
func (c *demoConn) Close() error                                 { return nil }
func (c *demoConn) Begin() (driver.Tx, error)                    { return &demoTx{}, nil }
func (s *demoStmt) Close() error                                 { return nil }
func (s *demoStmt) NumInput() int                                { return -1 }
func (s *demoStmt) Exec(_ []driver.Value) (driver.Result, error) { return demoResult{}, nil }
func (s *demoStmt) Query(_ []driver.Value) (driver.Rows, error)  { return &demoRows{}, nil }
func (r *demoRows) Columns() []string                            { return []string{"x"} }
func (r *demoRows) Close() error                                 { return nil }
func (r *demoRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}
func (demoResult) LastInsertId() (int64, error) { return 0, nil }
func (demoResult) RowsAffected() (int64, error) { return 0, nil }
func (t *demoTx) Commit() error                 { return nil }
func (t *demoTx) Rollback() error               { return nil }

func init() {
	sql.Register("demo", &demoDriver{})
}

func main() {
	db, err := sql.Open("demo", "dsn")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)

	mon := poolmon.New(db, poolmon.DefaultThresholds(), 60)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := mon.Check(); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"status": "unhealthy",
				"detail": err.Error(),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
	})

	mux.HandleFunc("GET /metrics/pool", func(w http.ResponseWriter, r *http.Request) {
		snap := mon.LastSnapshot()
		if snap == nil {
			s := mon.Collect()
			snap = &s
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snap)
	})

	fmt.Println("listening on :8080")
	if err := http.ListenAndServe(":8080", mux); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
```

Your turn: add a `TestMaxHistoryOfOne` test that creates a monitor with `maxHistory=1`, calls `Collect()` twice, and asserts that `len(m.History()) == 1`.

## Common Mistakes

### Forgetting to close sql.Rows

Wrong:

```go
rows, _ := db.Query("SELECT ...")
for rows.Next() { ... }
// rows never closed
```

What happens: each `Query` call acquires a connection from the pool and holds it in `InUse` until `rows.Close()` is called. In the pool's stats you see `InUse` climbing without bound even though no concurrent queries are running. The pool exhausts and new calls block.

Fix: always `defer rows.Close()` immediately after the error check.

### Dividing by WaitCount without guarding zero

Wrong:

```go
avgWait := stats.WaitDuration / time.Duration(stats.WaitCount)
```

What happens: if no waits have occurred, `WaitCount == 0` and the division panics at runtime.

Fix:

```go
var avgWait time.Duration
if stats.WaitCount > 0 {
	avgWait = stats.WaitDuration / time.Duration(stats.WaitCount)
}
```

### Setting MaxOpenConns to 0

Wrong:

```go
db.SetMaxOpenConns(0) // or never calling it
```

What happens: the pool is unbounded. Utilization is always 0 so no threshold ever fires. The database server can run out of connections while your monitor reports "healthy".

Fix: always set a positive `MaxOpenConns`. Use `SetMaxIdleConns` that is less than or equal to `MaxOpenConns`.

### Reading LastSnapshot before the first Collect

Wrong: treating `LastSnapshot()` as always non-nil.

What happens: a nil pointer dereference at the call site.

Fix: check for nil, or call `Collect()` once during startup before serving traffic:

```go
snap := mon.LastSnapshot()
if snap == nil {
	s := mon.Collect()
	snap = &s
}
```

### Holding a connection across a heavy computation

Wrong:

```go
rows, _ := db.Query("SELECT ...")
compute(hugeDataSet)  // 10-second CPU work
rows.Close()
```

What happens: the connection is in `InUse` for the duration of the computation, not just the query. Other goroutines wait.

Fix: materialize query results into a slice first, close `rows`, then do the computation.

## Verification

From `~/go-exercises/poolmon`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The `go test` output should show no failures. The `cmd/demo` binary is verified by `go build ./...`.

## Summary

- `db.Stats()` returns a `sql.DBStats` snapshot: read `InUse`, `MaxOpenConnections`, `WaitCount`, and `WaitDuration` for health assessment.
- Utilization (`InUse / MaxOpenConnections`) and average wait (`WaitDuration / WaitCount`) are the two primary health signals; both require a guard against division by zero.
- Always set `MaxOpenConns` to a positive value; an unbounded pool silently hides exhaustion from any threshold-based monitor.
- Keep history in a bounded ring protected by `sync.RWMutex`; return copies, not slice references, to callers.
- Expose pool status through the readiness probe so the load balancer stops sending traffic when the pool is critical.

## What's Next

Next: [Panic Recovery In Production](../13-panic-recovery-in-production/13-panic-recovery-in-production.md).

## Resources

- [pkg.go.dev/database/sql#DBStats](https://pkg.go.dev/database/sql#DBStats) — exact field definitions and cumulative counter semantics
- [pkg.go.dev/database/sql#DB.Stats](https://pkg.go.dev/database/sql#DB.Stats) — method signature (added Go 1.5)
- [go.dev/doc/database/manage-connections](https://go.dev/doc/database/manage-connections) — official pool tuning guide
- [pkg.go.dev/database/sql/driver](https://pkg.go.dev/database/sql/driver) — driver interfaces used by the test's fake driver
- [go.dev/doc/database/open-handle](https://go.dev/doc/database/open-handle) — when to call Open, SetMax*, and why a single *sql.DB is the right pattern
