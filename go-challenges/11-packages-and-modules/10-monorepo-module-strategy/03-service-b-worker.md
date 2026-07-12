# Exercise 3: Wire service-b — a background worker sharing the same library

A second deployable proves the core monorepo value: two services, one shared
implementation, one version. This exercise wires `cmd/worker` — a background job
processor that imports the *same* `platform/httpx` package as the API and formats
its failures through the same error type. Where the API turns errors into HTTP
responses, the worker turns them into wrapped errors and logs.

## What you'll build

```text
mono/                         single module: example.com/mono
  go.mod
  platform/
    httpx/
      httpx.go                shared APIError + ErrNotFound (bundled here)
  cmd/
    worker/
      worker.go               Job, processOne(ctx, log, job), ProcessBatch
      worker_test.go          errors.Is + errors.Join + context-cancel tests
      main.go                 runs a small batch through a discard logger
```

- Files: `platform/httpx/httpx.go`, `cmd/worker/worker.go`, `cmd/worker/worker_test.go`, `cmd/worker/main.go`.
- Implement: `processOne(ctx, log, Job) error` that returns `nil` on success, wraps the shared `httpx.ErrNotFound`-style sentinel on failure, and respects context cancellation; `ProcessBatch` that joins per-job errors with `errors.Join`.
- Test: inject a failing job and assert the error wraps the shared sentinel via `errors.Is`; a successful job returns `nil`; a cancelled context returns a `context.Canceled`-wrapping error.
- Verify: `go test -count=1 -race ./...`

Set up the module (self-contained, with its own copy of the shared package):

```bash
mkdir -p go-solutions/11-packages-and-modules/10-monorepo-module-strategy/03-service-b-worker/platform/httpx go-solutions/11-packages-and-modules/10-monorepo-module-strategy/03-service-b-worker/cmd/worker
cd go-solutions/11-packages-and-modules/10-monorepo-module-strategy/03-service-b-worker
```

### One library, two very different consumers

The API renders errors to HTTP; the worker never touches HTTP at all. Yet both
funnel failures through the same `platform/httpx` error type. That is the point:
the shared library defines *what an error is* (a stable code, a message, a
sentinel you can match), and each service decides *what to do with it*. The worker
wraps `httpx.ErrJobFailed` with `%w` and returns it up the stack, logging through
`log/slog`; a supervisor above `ProcessBatch` can still `errors.Is(err,
httpx.ErrJobFailed)` to branch on the shared condition. Uniform error identity
across services with wildly different transport is exactly the leverage a shared
module buys you.

`processOne` is the unit under test, and it is written to be testable: it takes a
`context.Context` (so cancellation is a first-class input, not a global), a
`*slog.Logger` (so tests inject a discard logger and assert on return values, not
log spew), and a `Job`. It checks `ctx.Err()` first — a cancelled context short-
circuits before any work, wrapping `context.Canceled` so callers can distinguish
"cancelled" from "failed". `ProcessBatch` runs each job and collects failures with
`errors.Join`, which returns a single error that `errors.Is`-matches *every* joined
sentinel — the right primitive for "report all failures, not just the first".

Create `platform/httpx/httpx.go` (bundled shared library):

```go
package httpx

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError is the shared structured error type used across every service.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s (status %d)", e.Code, e.Message, e.Status)
}

// Is lets errors.Is match APIError values by their stable Code, so a wrapped
// sentinel is recognized even through fmt.Errorf("%w").
func (e *APIError) Is(target error) bool {
	var t *APIError
	if !errors.As(target, &t) {
		return false
	}
	return e.Code == t.Code
}

// ErrJobFailed is the shared sentinel a worker wraps when a job cannot be
// processed. It maps to a 500-class condition if a service ever renders it.
var ErrJobFailed = &APIError{
	Status:  http.StatusInternalServerError,
	Code:    "job_failed",
	Message: "job processing failed",
}
```

Create `cmd/worker/worker.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"example.com/mono/platform/httpx"
)

// Job is a unit of background work. Fail marks a job that should error, standing
// in for a job whose real processing fails.
type Job struct {
	ID   string
	Fail bool
}

// processOne processes a single job. It returns nil on success, an error wrapping
// httpx.ErrJobFailed on failure, and a context-error-wrapping error if ctx is
// already done. slog is injected so tests can assert on return values, not logs.
func processOne(ctx context.Context, log *slog.Logger, j Job) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("job %s cancelled before start: %w", j.ID, err)
	}
	if j.Fail {
		log.Error("job failed", "id", j.ID)
		return fmt.Errorf("processing job %s: %w", j.ID, httpx.ErrJobFailed)
	}
	log.Info("job done", "id", j.ID)
	return nil
}

// ProcessBatch processes every job, applying a per-job timeout, and joins all
// failures into a single error that errors.Is-matches each underlying sentinel.
func ProcessBatch(ctx context.Context, log *slog.Logger, jobs []Job, perJob time.Duration) error {
	var errs []error
	for _, j := range jobs {
		jobCtx, cancel := context.WithTimeout(ctx, perJob)
		if err := processOne(jobCtx, log, j); err != nil {
			errs = append(errs, err)
		}
		cancel()
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo runs a three-job batch — two succeed, one fails — through a discard
logger so the output is just the outcomes. It prints each job's result and then
the joined batch error, showing that `errors.Join` surfaces the single failure
while the successes contribute nothing.

Create `cmd/worker/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"
)

func main() {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	jobs := []Job{
		{ID: "a"},
		{ID: "b", Fail: true},
		{ID: "c"},
	}

	for _, j := range jobs {
		if err := processOne(context.Background(), log, j); err != nil {
			fmt.Printf("job %s: %v\n", j.ID, err)
		} else {
			fmt.Printf("job %s: ok\n", j.ID)
		}
	}

	if err := ProcessBatch(context.Background(), log, jobs, time.Second); err != nil {
		fmt.Printf("batch error: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/worker
```

Expected output:

```
job a: ok
job b: processing job b: job_failed: job processing failed (status 500)
job c: ok
batch error: processing job b: job_failed: job processing failed (status 500)
```

### Tests

The tests pin the three behaviors of `processOne` and the join semantics of
`ProcessBatch`. `TestProcessOne` is table-driven over success, failure, and
cancellation; the failure row asserts `errors.Is(err, httpx.ErrJobFailed)` — the
shared-sentinel match — and the cancellation row asserts `errors.Is(err,
context.Canceled)`. `TestProcessBatchJoinsFailures` runs a batch with two failing
jobs and confirms the joined error still matches the shared sentinel, proving
`errors.Join` preserves `Is`-matching through the aggregate.

Create `cmd/worker/worker_test.go`:

```go
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"example.com/mono/platform/httpx"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestProcessOne(t *testing.T) {
	t.Parallel()
	log := discardLogger()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		if err := processOne(t.Context(), log, Job{ID: "ok"}); err != nil {
			t.Fatalf("processOne(ok) = %v, want nil", err)
		}
	})

	t.Run("failure wraps shared sentinel", func(t *testing.T) {
		t.Parallel()
		err := processOne(t.Context(), log, Job{ID: "bad", Fail: true})
		if !errors.Is(err, httpx.ErrJobFailed) {
			t.Fatalf("processOne(fail) = %v, want it to wrap ErrJobFailed", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := processOne(ctx, log, Job{ID: "late"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("processOne(cancelled) = %v, want it to wrap context.Canceled", err)
		}
	})
}

func TestProcessBatchJoinsFailures(t *testing.T) {
	t.Parallel()
	log := discardLogger()

	jobs := []Job{{ID: "a"}, {ID: "b", Fail: true}, {ID: "c", Fail: true}}
	err := ProcessBatch(t.Context(), log, jobs, time.Second)
	if err == nil {
		t.Fatal("ProcessBatch returned nil, want joined failures")
	}
	if !errors.Is(err, httpx.ErrJobFailed) {
		t.Fatalf("joined error = %v, want it to match ErrJobFailed", err)
	}
}
```

## Review

The worker is correct when `processOne` is a pure function of `(ctx, job)`:
cancellation is checked first and wraps `context.Canceled`; a failing job wraps
the shared `httpx.ErrJobFailed`; success returns `nil`. Because the logger is
injected as a `*slog.Logger` writing to `io.Discard`, the tests assert on returned
errors, never on log output — logs are an operational side effect, not the
contract.

The subtle point is why `errors.Join` is the right aggregate: it returns an error
whose `Is` walks every joined branch, so `ProcessBatch` can report all failures at
once while still letting a supervisor match the shared sentinel. Do not
concatenate error strings — that throws away the wrapping and breaks `errors.Is`.
Run `go test -race`; the batch loop derives a fresh context per job with
`context.WithTimeout` and calls `cancel()` each iteration, which the race detector
and `go vet`'s lostcancel check both keep honest.

## Resources

- [`errors`](https://pkg.go.dev/errors) — `errors.Is`, `errors.As`, and `errors.Join` for wrapped and aggregated errors.
- [`log/slog`](https://pkg.go.dev/log/slog) — structured logging with an injectable handler.
- [`context`](https://pkg.go.dev/context) — `WithCancel`/`WithTimeout` and `ctx.Err()` for cancellation.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the `%w` / `Is` / `As` model this worker relies on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-build-and-target-subset.md](04-build-and-target-subset.md)
