# Exercise 1: Aggregate Every Job Error with a Buffered Channel and errors.Join

The most common concurrent workload a backend runs is "do these N independent
things, then tell me everything that went wrong". A batch of cache invalidations,
a set of webhook deliveries, a fan-out of health probes. This module builds that
collect-all runner from the ground up — a buffered error channel sized to the job
count, `sync.WaitGroup.Go` for the fan-out, a per-goroutine `recover`, and
`errors.Join` to fold every failure into one inspectable aggregate — and contrasts
it with the sequential first-error baseline.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
runner/                      independent module: example.com/runner
  go.mod                     go 1.26
  runner.go                  Job, Runner, ErrNoJobs; Run (buffered chan + Join), RunSequential
  cmd/
    demo/
      main.go                runnable demo: aggregate two failures, run one success sequentially
  runner_test.go             table tests: aggregate-all, cancelled ctx, no jobs, job-name-in-message
```

Files: `runner.go`, `cmd/demo/main.go`, `runner_test.go`.
Implement: `Runner.Run` (fan out each job into a goroutine via `wg.Go`, each sends its `%w`-wrapped error or `nil` on a channel buffered to `len(jobs)`, then `errors.Join` the non-nil errors) and `RunSequential` (first error, wrapped with the job name).
Test: all-success returns nil; mixed jobs aggregate so `errors.Is(err, errBoom)` holds; empty jobs returns `ErrNoJobs`; the error message contains the failing job's name.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why buffered to len(jobs)

Each job runs in its own goroutine and reports exactly one value on `errCh` — its
wrapped error, or `nil` for success. The channel is buffered to `len(r.Jobs)` for
a specific reason: every worker must be able to send-and-exit *without* a receiver
waiting. If the channel were unbuffered, a worker would block on the send until
the parent received, and the parent does not start draining until after
`wg.Wait()`. That would deadlock: `Wait` never returns because the workers are
blocked sending, and the drain never starts because `Wait` never returns. Buffered
to the job count, every worker sends into buffer space, returns immediately,
`wg.Wait()` completes, and only then does the parent drain the closed channel.

The drain collects non-nil errors into a slice and hands them to `errors.Join`,
which builds a single error whose `Unwrap() []error` lets `errors.Is`/`errors.As`
traverse each part. So a caller can still ask `errors.Is(err, ErrTimeout)` against
the aggregate and get a true answer if any job timed out. Sending `nil` on success
(rather than nothing) keeps the send count equal to the job count, which is what
makes the "buffer sized to len(jobs)" guarantee exact.

The per-goroutine `recover` is the panic firewall from the concepts file: a job
that panics would otherwise crash the whole process. The deferred `recover` inside
each worker converts the panic into an error carrying the job name and sends it on
the same channel, so a panicking job looks to the caller like any other failed
job — not a dead process.

`RunSequential` is the baseline: run jobs in order, return the first error wrapped
with `fmt.Errorf("job %q: %w", ...)`. It is fail-fast and single-threaded — the
right tool when jobs are cheap, ordered, or must short-circuit. Keeping it beside
the concurrent runner makes the trade-off concrete.

Create `runner.go`:

```go
package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNoJobs is returned by Run when there is nothing to do.
var ErrNoJobs = errors.New("no jobs")

// Job is a named unit of work. Run reports failure by returning a non-nil error.
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

// Runner fans out its jobs concurrently and aggregates every error.
type Runner struct {
	Jobs []Job
}

// Run executes every job concurrently and returns the joined non-nil errors.
// A job that panics is converted into an error, not a process crash. The result
// is nil only if every job succeeded.
func (r *Runner) Run(ctx context.Context) error {
	if len(r.Jobs) == 0 {
		return ErrNoJobs
	}

	// Buffered to len(Jobs): every worker sends exactly once and exits without
	// needing a receiver, so wg.Wait cannot deadlock against a blocked send.
	errCh := make(chan error, len(r.Jobs))
	var wg sync.WaitGroup
	for _, j := range r.Jobs {
		wg.Go(func() {
			defer func() {
				if rec := recover(); rec != nil {
					errCh <- fmt.Errorf("job %q panicked: %v", j.Name, rec)
				}
			}()
			if err := j.Run(ctx); err != nil {
				errCh <- fmt.Errorf("job %q: %w", j.Name, err)
				return
			}
			errCh <- nil
		})
	}
	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RunSequential runs jobs in order and returns the first error, wrapped with the
// job name. It is the fail-fast, single-threaded baseline.
func RunSequential(ctx context.Context, jobs []Job) error {
	for _, j := range jobs {
		if err := j.Run(ctx); err != nil {
			return fmt.Errorf("job %q: %w", j.Name, err)
		}
	}
	return nil
}
```

### The runnable demo

The demo runs three jobs concurrently — two fail with distinct errors, one
succeeds — and prints the joined aggregate, then runs a single successful job
sequentially. `errors.Join` prints its parts one per line, so the two failures
appear as two lines.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/runner"
)

func main() {
	r := &runner.Runner{Jobs: []runner.Job{
		{Name: "flush-cache", Run: func(ctx context.Context) error {
			return errors.New("connection refused")
		}},
		{Name: "warm-cache", Run: func(ctx context.Context) error {
			return nil
		}},
		{Name: "notify", Run: func(ctx context.Context) error {
			return errors.New("timeout")
		}},
	}}

	err := r.Run(context.Background())
	fmt.Println("concurrent aggregate:")
	fmt.Println(err)

	seqErr := runner.RunSequential(context.Background(), []runner.Job{
		{Name: "migrate", Run: func(ctx context.Context) error { return nil }},
	})
	fmt.Printf("sequential result: %v\n", seqErr)
}
```

Run it:

```bash
go run ./cmd/demo
```

Because the two goroutines finish in scheduler-dependent order, the two failure
lines may appear in either order. Expected output (one possible ordering):

```
concurrent aggregate:
job "flush-cache": connection refused
job "notify": timeout
sequential result: <nil>
```

### Tests

`TestRunConcurrentAggregatesAllErrors` is the headline: three jobs, two return the
same wrapped sentinel, and the aggregate must satisfy `errors.Is(err, errBoom)` —
proving `errors.Join` keeps each part inspectable. `TestRunErrorMessageIncludesJobName`
pins the contract that the failing job's name appears in the message, which is
what makes an aggregate triageable. `TestRunRespectsCancelledContext` proves a job
that honors `ctx.Done()` surfaces `context.Canceled` through the aggregate.
`TestRunReturnsNoJobs` pins the empty-input sentinel. The `-race` flag proves the
buffered channel and `errors.Join` have no data race across the fan-out.

Create `runner_test.go`:

```go
package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var errBoom = errors.New("boom")

func TestRunConcurrentSuccess(t *testing.T) {
	t.Parallel()
	r := &Runner{Jobs: []Job{
		{Name: "a", Run: func(ctx context.Context) error { return nil }},
		{Name: "b", Run: func(ctx context.Context) error { return nil }},
	}}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

func TestRunConcurrentAggregatesAllErrors(t *testing.T) {
	t.Parallel()
	r := &Runner{Jobs: []Job{
		{Name: "a", Run: func(ctx context.Context) error { return errBoom }},
		{Name: "b", Run: func(ctx context.Context) error { return errBoom }},
		{Name: "c", Run: func(ctx context.Context) error { return nil }},
	}}
	err := r.Run(context.Background())
	if !errors.Is(err, errBoom) {
		t.Fatalf("Run() = %v, want errors.Is(..., errBoom)", err)
	}
}

func TestRunRecoversPanic(t *testing.T) {
	t.Parallel()
	r := &Runner{Jobs: []Job{
		{Name: "panicky", Run: func(ctx context.Context) error { panic("kaboom") }},
		{Name: "ok", Run: func(ctx context.Context) error { return nil }},
	}}
	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "panicky") {
		t.Fatalf("Run() = %v, want error naming the panicking job", err)
	}
}

func TestRunErrorMessageIncludesJobName(t *testing.T) {
	t.Parallel()
	r := &Runner{Jobs: []Job{
		{Name: "slow", Run: func(ctx context.Context) error { return errBoom }},
	}}
	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "slow") {
		t.Fatalf("Run() = %v, want message containing job name %q", err, "slow")
	}
}

func TestRunRespectsCancelledContext(t *testing.T) {
	t.Parallel()
	r := &Runner{Jobs: []Job{
		{Name: "waiter", Run: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := r.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
}

func TestRunReturnsNoJobs(t *testing.T) {
	t.Parallel()
	r := &Runner{Jobs: nil}
	if err := r.Run(context.Background()); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("Run() = %v, want ErrNoJobs", err)
	}
}

func TestRunSequentialReturnsFirstError(t *testing.T) {
	t.Parallel()
	jobs := []Job{
		{Name: "a", Run: func(ctx context.Context) error { return nil }},
		{Name: "b", Run: func(ctx context.Context) error { return errBoom }},
	}
	err := RunSequential(context.Background(), jobs)
	if !errors.Is(err, errBoom) {
		t.Fatalf("RunSequential() = %v, want errBoom", err)
	}
}
```

## Review

The runner is correct when three properties hold together. Every job's error
reaches the aggregate — proven by `errors.Is(aggregate, errBoom)` surviving the
fan-out, which only works because each part is wrapped with `%w` and joined with
`errors.Join`. No job can crash the process — proven by the panic test surfacing
as a named error rather than a dead binary, which only works because the `recover`
lives *inside* each worker. And the fan-out is race-free — proven by `-race`,
which only holds because the channel is buffered to the job count so no worker
blocks on a send. The classic bug this design forecloses is the unbuffered-channel
leak: shrink the buffer and a worker blocks forever when the parent stops
receiving. Run `go test -race` and `go vet ./...` to confirm all three.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — building a multi-error whose `Is`/`As` traverse every part.
- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 helper that does `Add`/`Done` correctly.
- [Effective Go: Goroutines and channels](https://go.dev/doc/effective_go#goroutines) — the send/receive model the runner is built on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-mutex-results-collector.md](02-mutex-results-collector.md)
