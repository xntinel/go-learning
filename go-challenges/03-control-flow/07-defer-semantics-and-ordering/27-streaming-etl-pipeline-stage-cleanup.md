# Exercise 27: ETL Pipeline — Deferred Stage Cleanup in Reverse Startup Order

**Nivel: Intermedio** — validacion rapida (un test corto).

A log-processing pipeline reads a compressed file, decompresses it,
parses records out of the decompressed stream, validates each record
against a schema, and writes the valid ones onward. Each stage owns a
resource — a file handle, a decompressor, a scanner — that must be
released, and the release order matters exactly as much as the
acquisition order did: you cannot close the file out from under a
decompressor that is still reading from it. When the validate stage
discovers a malformed record and stops the pipeline, only the stages
that actually started (read, decompress, parse) have anything to release
— the write stage, which never got a chance to start, has nothing. This
module builds that unwind as a slice of deferred cleanups, appended in
start order and always run in reverse. The module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
etl/                         independent module: example.com/streaming-etl-pipeline-stage-cleanup
  go.mod                      go 1.24
  etl.go                       Stage, Run(stages) error
  cmd/
    demo/
      main.go                 runnable demo: 5 stages, one fails to start, only started stages unwind
  etl_test.go                  table over failure points; cleanup-error joining case
```

- Files: `etl.go`, `cmd/demo/main.go`, `etl_test.go`.
- Implement: `Stage` (`Name`, `Start func() (cleanup func() error, err error)`) and `Run(stages []Stage) error`.
- Test: a table over which stage fails to start, checking exactly the earlier stages opened and closed in reverse, plus a case joining a cleanup error with the triggering stage error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/27-streaming-etl-pipeline-stage-cleanup/cmd/demo
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/27-streaming-etl-pipeline-stage-cleanup
go mod edit -go=1.24
```

### Why cleanups accumulate in a slice instead of one defer per stage

`Run` cannot write `defer cleanup()` once per stage inline in the loop,
the way a function with a fixed, known set of resources would: the
number of stages that actually start is not known ahead of time — it
depends on where (or whether) `Start` fails. So `Run` defers exactly
once, before the loop even begins, a closure that walks a `cleanups`
slice backward. Each iteration of the loop appends to that slice only
after its stage's `Start` has succeeded; a stage whose `Start` fails
contributes nothing to it and the loop returns immediately. Because the
one deferred closure reads `cleanups` at return time — after the loop
has run as far as it got — it always sees exactly the stages that
started, in the order they started, and unwinds them in reverse. Adding
a fifth, sixth, or tenth stage requires no change to `Run` itself: the
slice-plus-single-defer shape scales to however many stages a pipeline
happens to have.

Create `etl.go`:

```go
package etl

import (
	"errors"
	"fmt"
)

// Stage models one step of an ETL pipeline: Start acquires whatever
// resource the stage needs (a file handle, a decompressor, a parser
// context) and returns a cleanup function for it. If Start fails, no
// cleanup is registered for that stage -- there is nothing to release.
type Stage struct {
	Name  string
	Start func() (cleanup func() error, err error)
}

// Run starts each stage in order, deferring its cleanup only once that
// stage's Start has actually succeeded. If any Start call fails, Run stops
// launching further stages and unwinds -- in reverse order -- exactly the
// stages that did start, then returns the failing stage's error joined
// with any cleanup errors encountered while unwinding.
func Run(stages []Stage) (err error) {
	var cleanups []func() error

	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			if cerr := cleanups[i](); cerr != nil {
				err = errors.Join(err, cerr)
			}
		}
	}()

	for _, s := range stages {
		cleanup, serr := s.Start()
		if serr != nil {
			err = fmt.Errorf("stage %q failed to start: %w", s.Name, serr)
			return err
		}
		cleanups = append(cleanups, cleanup)
	}
	return nil
}
```

### The runnable demo

Five stages, modeled on a real log-processing pipeline: `read`,
`decompress`, `parse`, `validate`, `write`. The `validate` stage fails to
start, simulating a schema check that rejects the input before any
record is even scanned. Only `read`, `decompress`, and `parse` ever
opened, so only those three close, in reverse; `write` never runs at
all.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/streaming-etl-pipeline-stage-cleanup"
)

func main() {
	stages := []etl.Stage{
		{Name: "read", Start: func() (func() error, error) {
			fmt.Println("opened: read (file handle)")
			return func() error { fmt.Println("closed: read"); return nil }, nil
		}},
		{Name: "decompress", Start: func() (func() error, error) {
			fmt.Println("opened: decompress (gzip reader)")
			return func() error { fmt.Println("closed: decompress"); return nil }, nil
		}},
		{Name: "parse", Start: func() (func() error, error) {
			fmt.Println("opened: parse (record scanner)")
			return func() error { fmt.Println("closed: parse"); return nil }, nil
		}},
		{Name: "validate", Start: func() (func() error, error) {
			return nil, errors.New("schema mismatch on record 42")
		}},
		{Name: "write", Start: func() (func() error, error) {
			fmt.Println("opened: write (output file)")
			return func() error { fmt.Println("closed: write"); return nil }, nil
		}},
	}

	if err := etl.Run(stages); err != nil {
		fmt.Println("pipeline failed:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
opened: read (file handle)
opened: decompress (gzip reader)
opened: parse (record scanner)
closed: parse
closed: decompress
closed: read
pipeline failed: stage "validate" failed to start: schema mismatch on record 42
```

### Tests

`TestRunUnwindsOnlyStartedStagesInReverse` is a table over which stage
(if any) fails to start, asserting the exact set and order of stages
opened and closed for each case — including the two edges, "first stage
fails" (nothing to unwind) and "all stages succeed" (everything unwinds
in full reverse). `TestRunJoinsCleanupErrorsWithStageError` proves a
cleanup failure during unwind does not shadow the error that triggered
the unwind in the first place — both are visible through `errors.Is`.

Create `etl_test.go`:

```go
package etl

import (
	"errors"
	"testing"
)

func TestRunUnwindsOnlyStartedStagesInReverse(t *testing.T) {
	tests := []struct {
		name       string
		failAt     int // -1 means no failure
		wantOpened []string
		wantClosed []string
	}{
		{
			name:       "all stages succeed",
			failAt:     -1,
			wantOpened: []string{"a", "b", "c"},
			wantClosed: []string{"c", "b", "a"},
		},
		{
			name:       "middle stage fails: only earlier stages unwind",
			failAt:     1,
			wantOpened: []string{"a"},
			wantClosed: []string{"a"},
		},
		{
			name:       "first stage fails: nothing to unwind",
			failAt:     0,
			wantOpened: nil,
			wantClosed: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var opened, closed []string
			names := []string{"a", "b", "c"}
			var stages []Stage
			for i, name := range names {
				i, name := i, name
				stages = append(stages, Stage{
					Name: name,
					Start: func() (func() error, error) {
						if i == tc.failAt {
							return nil, errors.New("boom")
						}
						opened = append(opened, name)
						return func() error {
							closed = append(closed, name)
							return nil
						}, nil
					},
				})
			}

			err := Run(stages)

			if tc.failAt == -1 {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			} else if err == nil {
				t.Fatalf("err = nil, want failure at stage %q", names[tc.failAt])
			}

			if !equalSlices(opened, tc.wantOpened) {
				t.Errorf("opened = %v, want %v", opened, tc.wantOpened)
			}
			if !equalSlices(closed, tc.wantClosed) {
				t.Errorf("closed = %v, want %v", closed, tc.wantClosed)
			}
		})
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunJoinsCleanupErrorsWithStageError(t *testing.T) {
	errCleanupA := errors.New("cleanup a failed")
	errStartB := errors.New("start b failed")

	stages := []Stage{
		{Name: "a", Start: func() (func() error, error) {
			return func() error { return errCleanupA }, nil
		}},
		{Name: "b", Start: func() (func() error, error) {
			return nil, errStartB
		}},
	}

	err := Run(stages)
	if !errors.Is(err, errStartB) {
		t.Errorf("err = %v, want it to wrap %v", err, errStartB)
	}
	if !errors.Is(err, errCleanupA) {
		t.Errorf("err = %v, want it to also wrap %v", err, errCleanupA)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Run` is correct when the set of stages it unwinds always matches
exactly the set of stages that actually started — never fewer (a leak)
and never more (a nil-pointer panic calling `Close` on something that
was never opened) — and when it unwinds them in the reverse of their
start order. Accumulating cleanups in a slice and deferring exactly one
closure over it, registered before the loop runs, is what makes this
correct for any number of stages without per-stage bookkeeping. The
mistake this design avoids is writing one `defer cleanup()` per stage
inside the loop body: since each iteration's `defer` is scoped to the
whole *function*, not the loop, that would actually still work here for
correctness — but it also depends on knowing, syntactically, that every
loop iteration reaches its `defer`, which stops being true the moment a
stage's `Start` can fail partway through the loop and the loop needs to
`return` before registering a cleanup for a resource that was never
acquired.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — combining a cleanup failure with the error that triggered unwinding.
- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — a function's deferred calls execute in LIFO order at return, regardless of how many were registered.
- [Go Blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the staged-pipeline shape this exercise's `Stage` sequence is a simplified, sequential model of.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-semaphore-bounded-resource-acquire.md](26-semaphore-bounded-resource-acquire.md) | Next: [28-opentelemetry-style-active-span.md](28-opentelemetry-style-active-span.md)
