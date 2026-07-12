# Exercise 19: Metrics Aggregator — Deferred Flush Without Data Loss on Shutdown

**Nivel: Intermedio** — validacion rapida (un test corto).

A metrics buffer that accumulates gauge updates in memory is only as useful
as its last flush, and a shutdown routine that calls `write(buf.Gauges())`
without checking the returned error silently drops every buffered metric
the moment the disk backing that write is full — right when an operator most
needs to see queue depth and worker counts, during an incident. This module
builds a `Shutdown` function whose deferred flush merges its own error into
the function's named return via `errors.Join`, so a full disk never vanishes
silently, whether or not an earlier shutdown step also failed. The module is
fully self-contained: its own `go mod init`, all code inline, its own demo
and tests.

## What you'll build

```text
metrics/                    independent module: example.com/metrics-aggregator-deferred-flush
  go.mod                     go 1.24
  metrics.go                 Gauge, Buffer (Observe, Gauges), Shutdown(buf, steps, write) error
  cmd/
    demo/
      main.go                runnable demo: clean flush, failed flush, step failure + failed flush
  metrics_test.go            table test over step/flush success and failure combinations
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: `Gauge`, `Buffer` (`Observe`, `Gauges`), and `Shutdown(buf *Buffer, steps []func() error, write func([]Gauge) error) (err error)`.
- Test: a table over a clean flush, a failed flush, and a step failure combined with a failed flush, asserting the merged error wraps both.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the flush error has to be joined, not overwritten

`Shutdown` runs each of `steps` in order, joining any error each one returns
into the named `err`. The deferred closure runs last of all, after every
step has had its chance to fail — and it flushes `buf`'s gauges regardless
of what `err` already holds, because the metrics buffered so far are worth
writing out even if an earlier step failed. The closure's own outcome then
has to be combined with whatever `err` already was, not substituted for it:
`err = errors.Join(err, ...)` preserves an earlier step's error when the
flush also fails, so the caller learns about both problems instead of only
the last one to run. Writing `err = fmt.Errorf(...)` instead would silently
discard an earlier step's failure the moment the flush also failed — exactly
the kind of shutdown-time information loss an operator debugging an incident
cannot afford.

Create `metrics.go`:

```go
package metrics

import (
	"errors"
	"fmt"
)

// ErrDiskFull simulates the write path's most common real failure.
var ErrDiskFull = errors.New("metrics: disk full")

// Gauge is one named metric observation.
type Gauge struct {
	Name  string
	Value float64
}

// Buffer accumulates gauge observations in memory until flushed.
type Buffer struct {
	gauges []Gauge
}

// Observe records one gauge update.
func (b *Buffer) Observe(name string, value float64) {
	b.gauges = append(b.gauges, Gauge{Name: name, Value: value})
}

// Gauges returns a copy of the buffered gauges.
func (b *Buffer) Gauges() []Gauge {
	out := make([]Gauge, len(b.gauges))
	copy(out, b.gauges)
	return out
}

// Shutdown runs each of steps in order, then flushes buf's accumulated
// gauges as the very last thing it does, via a deferred closure.
//
// That closure reads and writes the function's named return value err.
// If an earlier step already failed, a flush failure is joined onto err
// with errors.Join rather than overwriting it, so the caller learns about
// both failures instead of losing the first one. If nothing else failed,
// a flush failure becomes err's only content instead of vanishing --
// which is what would happen to a `write(buf.Gauges())` call whose error
// return was never checked, the common shape of silent metric loss on a
// full disk at the last moment of shutdown.
func Shutdown(buf *Buffer, steps []func() error, write func([]Gauge) error) (err error) {
	defer func() {
		if ferr := write(buf.Gauges()); ferr != nil {
			err = errors.Join(err, fmt.Errorf("metrics flush failed: %w", ferr))
		}
	}()

	for _, step := range steps {
		if serr := step(); serr != nil {
			err = errors.Join(err, serr)
		}
	}
	return err
}
```

### The runnable demo

The demo runs `Shutdown` three times: a clean shutdown with a successful
flush, a clean shutdown whose flush hits a simulated full disk, and a
shutdown where both an earlier step and the flush fail.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/metrics-aggregator-deferred-flush"
)

func main() {
	okWrite := func(gauges []metrics.Gauge) error {
		fmt.Printf("flushed %d gauges\n", len(gauges))
		return nil
	}
	failWrite := func(gauges []metrics.Gauge) error {
		return metrics.ErrDiskFull
	}

	fmt.Println("-- clean shutdown, flush succeeds --")
	buf := &metrics.Buffer{}
	buf.Observe("queue_depth", 3)
	buf.Observe("worker_count", 5)
	err := metrics.Shutdown(buf, []func() error{
		func() error { return nil },
	}, okWrite)
	fmt.Println("error:", err)

	fmt.Println("-- clean shutdown, flush fails --")
	buf2 := &metrics.Buffer{}
	buf2.Observe("queue_depth", 1)
	err = metrics.Shutdown(buf2, []func() error{
		func() error { return nil },
	}, failWrite)
	fmt.Println("error:", err)

	fmt.Println("-- shutdown step fails and flush fails --")
	stepErr := errors.New("connection drain failed")
	buf3 := &metrics.Buffer{}
	buf3.Observe("queue_depth", 9)
	err = metrics.Shutdown(buf3, []func() error{
		func() error { return stepErr },
	}, failWrite)
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
-- clean shutdown, flush succeeds --
flushed 2 gauges
error: <nil>
-- clean shutdown, flush fails --
error: metrics flush failed: metrics: disk full
-- shutdown step fails and flush fails --
connection drain failed
metrics flush failed: metrics: disk full
```

### Tests

`TestShutdownMergesFlushErrorIntoNamedReturn` drives `Shutdown` over three
combinations of step and flush outcomes and uses `errors.Is` to confirm the
returned error wraps every failure that actually happened, never fewer.

Create `metrics_test.go`:

```go
package metrics

import (
	"errors"
	"testing"
)

func TestShutdownMergesFlushErrorIntoNamedReturn(t *testing.T) {
	stepErr := errors.New("step failed")

	tests := []struct {
		name        string
		stepErr     error
		writeErr    error
		wantStepErr bool
		wantDisk    bool
	}{
		{"clean shutdown, flush succeeds", nil, nil, false, false},
		{"clean shutdown, flush fails", nil, ErrDiskFull, false, true},
		{"step fails, flush also fails", stepErr, ErrDiskFull, true, true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			buf := &Buffer{}
			buf.Observe("queue_depth", 3)

			write := func(gauges []Gauge) error {
				if len(gauges) != 1 {
					t.Errorf("write saw %d gauges, want 1", len(gauges))
				}
				return tc.writeErr
			}

			err := Shutdown(buf, []func() error{
				func() error { return tc.stepErr },
			}, write)

			if tc.wantStepErr && !errors.Is(err, stepErr) {
				t.Errorf("err = %v, want it to wrap %v", err, stepErr)
			}
			if tc.wantDisk && !errors.Is(err, ErrDiskFull) {
				t.Errorf("err = %v, want it to wrap %v", err, ErrDiskFull)
			}
			if !tc.wantStepErr && !tc.wantDisk && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Shutdown` is correct when every failure that happened during shutdown is
visible in the returned error, regardless of which combination of step
failures and flush failures actually occurred — never fewer, and the test
checks that with `errors.Is` rather than a brittle string comparison. That
guarantee depends on two choices working together: the flush lives in a
deferred closure so it always runs even when an earlier step already failed,
and it merges its own error with `errors.Join` instead of assigning over
`err`. The mistake this design avoids is treating the metrics flush as a
fire-and-forget `write(...)` call at the end of the function body — which
looks identical on every test where the disk has room, and only shows its
bug during the exact incident where the operator needed those last metrics
most.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — combining multiple independent errors into one that `errors.Is` still unwraps correctly.
- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — deferred functions can read and modify named result parameters.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-batch-job-per-item-cleanup-helper.md](18-batch-job-per-item-cleanup-helper.md) | Next: [20-read-replica-failover-deferred-cleanup.md](20-read-replica-failover-deferred-cleanup.md)
