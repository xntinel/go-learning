# Exercise 6: Record Latency and Outcome from a Single defer

Every instrumented backend call emits the same shape of telemetry: how long it
took, whether it succeeded, and some result magnitude (rows scanned, bytes sent).
The clean way to do this is one deferred closure that captures a start time up
front and, on exit, reads the final result and error. That closure can only see the
outcome because the result and error are named returns.

This module is self-contained: its own `go mod init`, its own demo, its own tests.

## What you'll build

```text
querymetrics/               independent module: example.com/querymetrics
  go.mod
  querymetrics.go           Observation; Recorder; Repo.Query (one metrics defer)
  cmd/demo/
    main.go                 runnable demo: a success and a failure, observed
  querymetrics_test.go       ok/error status, rows value, non-negative duration, final-err
```

- Files: `querymetrics.go`, `cmd/demo/main.go`, `querymetrics_test.go`.
- Implement: `Query(op) (rows int, err error)` that captures `time.Now()` and, in one deferred closure, reads the named `rows` and `err` to emit a duration + outcome to an injected `Recorder`.
- Test: a fake recorder; success -> one observation with status ok and the rows value; failure -> status error and rows zero; duration non-negative; the defer sees the final `err`.
- Verify: `go test -count=1 -race ./...`

### The one place that sees both duration and outcome

There is no way to emit "how long, and did it work, and how much" from a single
call site *except* a deferred closure over named results:

```go
func (r *Repo) Query(op string) (rows int, err error) {
	start := time.Now()
	defer func() {
		status := "ok"
		if err != nil {
			status = "error"
		}
		r.rec.Observe(Observation{
			Op:       op,
			Status:   status,
			Rows:     rows,
			Duration: time.Since(start),
		})
	}()
	rows, err = r.load()
	return
}
```

`start` is captured before the work runs. The deferred closure runs on exit and
reads `rows` and `err` *after* the function has set them — so it always observes
the final outcome, and it fires on every return path exactly once. This is why the
body uses `rows, err = r.load()` followed by a naked `return`: the assignment lands
on the named results, and the naked return then hands them to both the caller and
the deferred hook. Had the body used `return r.load()` directly, the same thing
would happen (the `return` copies into the named results before defers run); the
explicit-assign-then-naked-return form is written out here to make the mechanism
visible.

Because the hook reads the *final* `err`, an error set on any path — including a
late one, such as a post-processing step that fails after rows were fetched — is
what gets recorded. The metric cannot drift out of sync with the returned result,
because it reads the same variables the caller receives.

Create `querymetrics.go`:

```go
package querymetrics

import (
	"errors"
	"time"
)

// ErrQuery is a sample failure the load function can return.
var ErrQuery = errors.New("query failed")

// Observation is one emitted metric: which operation, its outcome, the row count,
// and how long it took.
type Observation struct {
	Op       string
	Status   string // "ok" or "error"
	Rows     int
	Duration time.Duration
}

// Recorder receives observations. Production wires a Prometheus/OTel adapter; a
// test wires a fake.
type Recorder interface {
	Observe(o Observation)
}

// Repo runs a query and instruments it.
type Repo struct {
	rec  Recorder
	load func() (int, error)
}

// New builds a Repo over a recorder and a load function (the real query).
func New(rec Recorder, load func() (int, error)) *Repo {
	return &Repo{rec: rec, load: load}
}

// Query runs the load and, in one deferred closure, records the duration plus the
// outcome. rows and err are named results so the deferred hook can read the final
// values the caller will receive.
func (r *Repo) Query(op string) (rows int, err error) {
	start := time.Now()
	defer func() {
		status := "ok"
		if err != nil {
			status = "error"
		}
		r.rec.Observe(Observation{
			Op:       op,
			Status:   status,
			Rows:     rows,
			Duration: time.Since(start),
		})
	}()
	rows, err = r.load()
	return
}
```

### The runnable demo

The demo wires a recorder that prints each observation (omitting the non-repeatable
duration) and runs a successful query and a failing one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/querymetrics"
)

type printRecorder struct{}

func (printRecorder) Observe(o querymetrics.Observation) {
	fmt.Printf("op=%s status=%s rows=%d\n", o.Op, o.Status, o.Rows)
}

func main() {
	ok := querymetrics.New(printRecorder{}, func() (int, error) {
		return 7, nil
	})
	ok.Query("list_users")

	bad := querymetrics.New(printRecorder{}, func() (int, error) {
		return 0, querymetrics.ErrQuery
	})
	bad.Query("list_orders")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
op=list_users status=ok rows=7
op=list_orders status=error rows=0
```

### Tests

The fake recorder captures every observation so the test can assert on status,
rows, and a non-negative duration. Because virtual clocks are out of scope here,
the duration assertion is the honest one: elapsed time is monotonic and cannot be
negative.

Create `querymetrics_test.go`:

```go
package querymetrics

import (
	"fmt"
	"sync"
	"testing"
)

type fakeRecorder struct {
	mu  sync.Mutex
	obs []Observation
}

func (r *fakeRecorder) Observe(o Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.obs = append(r.obs, o)
}

func (r *fakeRecorder) only(t *testing.T) Observation {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.obs) != 1 {
		t.Fatalf("got %d observations, want exactly 1", len(r.obs))
	}
	return r.obs[0]
}

func TestQueryRecordsSuccess(t *testing.T) {
	t.Parallel()

	rec := &fakeRecorder{}
	repo := New(rec, func() (int, error) { return 5, nil })

	rows, err := repo.Query("list")
	if err != nil || rows != 5 {
		t.Fatalf("Query = %d,%v, want 5,nil", rows, err)
	}

	o := rec.only(t)
	if o.Op != "list" || o.Status != "ok" || o.Rows != 5 {
		t.Fatalf("observation = %+v, want ok/5", o)
	}
	if o.Duration < 0 {
		t.Fatalf("duration = %v, want non-negative", o.Duration)
	}
}

func TestQueryRecordsFailure(t *testing.T) {
	t.Parallel()

	rec := &fakeRecorder{}
	repo := New(rec, func() (int, error) { return 0, ErrQuery })

	rows, err := repo.Query("list")
	if err == nil || rows != 0 {
		t.Fatalf("Query = %d,%v, want 0,error", rows, err)
	}

	o := rec.only(t)
	if o.Status != "error" || o.Rows != 0 {
		t.Fatalf("observation = %+v, want error/0", o)
	}
}

func TestQuerySeesFinalError(t *testing.T) {
	t.Parallel()

	// The load returns rows AND an error together; the hook must record the
	// error status even though rows is non-zero, because it reads the final err.
	rec := &fakeRecorder{}
	repo := New(rec, func() (int, error) { return 3, ErrQuery })

	repo.Query("list")
	if o := rec.only(t); o.Status != "error" {
		t.Fatalf("status = %s, want error when the final err is non-nil", o.Status)
	}
}

func ExampleRepo_Query() {
	rec := &fakeRecorder{}
	repo := New(rec, func() (int, error) { return 2, nil })
	repo.Query("count")
	fmt.Printf("status=%s rows=%d\n", rec.obs[0].Status, rec.obs[0].Rows)
	// Output: status=ok rows=2
}
```

## Review

The hook is correct when exactly one observation is emitted per call, its status
matches the final `err`, its `Rows` matches the returned count, and its duration is
non-negative. The property that makes it trustworthy is that the deferred closure
reads the same named `rows` and `err` the caller receives, so the metric can never
disagree with the returned result — `TestQuerySeesFinalError` pins this by returning
rows and an error together and asserting the status is still `error`. The mistake to
avoid is computing the outcome *before* the work finishes (capturing status at the
top of the function) instead of reading the named results in the defer; that emits a
metric that lies about late failures. Run `go test -race`, which also exercises the
recorder's mutex.

## Resources

- [`time.Since`](https://pkg.go.dev/time#Since)
- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`time.Duration`](https://pkg.go.dev/time#Duration)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-close-error-not-lost.md](05-close-error-not-lost.md) | Next: [07-shadowed-named-return-bug.md](07-shadowed-named-return-bug.md)
