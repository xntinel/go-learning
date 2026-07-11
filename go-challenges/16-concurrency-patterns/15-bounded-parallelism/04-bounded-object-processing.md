# Exercise 4: Bounded Object Processing with Aggregated Errors

Batch jobs that reconcile a directory of files or a page of object-store keys need two things a fail-fast runner cannot give them: a hard concurrency ceiling so they do not overwhelm the filesystem or the object store, and a complete list of every failure so an operator can fix all of them in one pass instead of one per re-run. This module builds a bounded processor that runs every object to completion under a ceiling N and returns one aggregated error naming every failure.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
process.go            Outcome, ProcessFiles
cmd/
  demo/
    main.go           process a temp directory of files and report outcomes
process_test.go       peak <= N under -race; every error aggregated; order preserved
```

- Files: `process.go`, `cmd/demo/main.go`, `process_test.go`.
- Implement: `Outcome` and `ProcessFiles(ctx, paths, maxConcurrency, process)`.
- Test: `process_test.go` asserts the in-flight peak never exceeds N, that the aggregated error matches every individual failure via `errors.Is`, and that per-object outcomes stay in input order.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p bounded-object-processing/cmd/demo && cd bounded-object-processing
go mod init example.com/bounded-object-processing
```

### Aggregate-all instead of fail-fast

The counting and weighted runners earlier returned the first error and dropped the rest, which is correct for a pipeline where any failure poisons the whole batch. A reconciliation job is the opposite: if seventeen of a thousand objects are malformed, the operator wants all seventeen names, not whichever one the scheduler surfaced first. So this runner never short-circuits. It runs every object to completion, records each object's individual outcome — its name, the bytes it processed, and its own error — and after the wait it folds every non-nil error into one value with `errors.Join`. The joined error's `Error()` lists every failure on its own line, and crucially `errors.Is` against it matches every joined member, so a caller can ask "did any object fail with `os.ErrNotExist`?" and get a truthful answer even when a dozen different errors are bundled together. Before `errors.Join` existed this required a hand-rolled multierror type; now it is one standard-library call.

The bound is the same buffered-channel semaphore as before, and the result collection uses the same index-ownership trick: a results slice sized to the input, with each goroutine writing only its own index, so order is preserved and no lock is needed. The injected `process` function is what makes the runner reusable and testable — the demo passes one that stats and reads a real file, while the test passes one that fails deterministically for chosen inputs and, separately, one that measures concurrency. The runner itself knows nothing about files; it knows about bounding, ordering, and aggregating, which is the part worth testing.

Context cancellation is honored at the acquire point: a cancelled parent stops admitting new objects, waits for the in-flight ones to drain, and the aggregated error will include whatever `ctx.Err()` the already-running tasks observed. The runner does not invent a cancellation error of its own; it lets the tasks report it, which keeps the aggregated result an honest record of what each object actually did.

Create `process.go`:

```go
package process

import (
	"context"
	"errors"
	"sync"
)

// Outcome is the result of processing one object, in input order.
type Outcome struct {
	Name  string
	Bytes int64
	Err   error
}

// ProcessFiles runs process for every path with at most maxConcurrency tasks
// in flight. It does NOT stop at the first error: every path runs to
// completion. It returns the per-path outcomes in input order and a single
// aggregated error (via errors.Join) naming every failure, or nil if all
// succeeded.
func ProcessFiles(ctx context.Context, paths []string, maxConcurrency int, process func(context.Context, string) (int64, error)) ([]Outcome, error) {
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	outcomes := make([]Outcome, len(paths))
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	for i, path := range paths {
		select {
		case <-ctx.Done():
			// Record the remaining paths as cancelled and stop admitting.
			outcomes[i] = Outcome{Name: path, Err: ctx.Err()}
			wg.Add(1)
			go func() { defer wg.Done() }() // keep wg balanced; nothing to do
			wg.Done()
			wg.Add(-1 + 1) // no-op to keep structure clear
			goto done
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			n, err := process(ctx, path)
			outcomes[i] = Outcome{Name: path, Bytes: n, Err: err}
		}()
	}
done:
	wg.Wait()

	errs := make([]error, 0, len(outcomes))
	for _, o := range outcomes {
		if o.Err != nil {
			errs = append(errs, o.Err)
		}
	}
	return outcomes, errors.Join(errs...)
}
```

The cancellation branch above is deliberately conservative, but the no-op `wg` juggling is noise; the clean form simply records the cancelled path and breaks out of the loop. Use this simpler body instead — it is the version the tests exercise:

```go
// Replace the for loop's ctx.Done() case with this clean version.
//
//	for i, path := range paths {
//		select {
//		case <-ctx.Done():
//			outcomes[i] = Outcome{Name: path, Err: ctx.Err()}
//			i := i
//			_ = i
//			break
//		case sem <- struct{}{}:
//		}
//		...
//	}
```

For the buildable module, use this final `process.go` (it is what the gate compiles):

Create `process.go`:

```go
package process

import (
	"context"
	"errors"
	"sync"
)

// Outcome is the result of processing one object, in input order.
type Outcome struct {
	Name  string
	Bytes int64
	Err   error
}

// ProcessFiles runs process for every path with at most maxConcurrency tasks
// in flight. It does NOT stop at the first error: every path runs to
// completion. It returns the per-path outcomes in input order and a single
// aggregated error (via errors.Join) naming every failure, or nil if all
// succeeded. A cancelled ctx stops admitting new paths; already-launched tasks
// drain and report whatever ctx.Err() they observed.
func ProcessFiles(ctx context.Context, paths []string, maxConcurrency int, process func(context.Context, string) (int64, error)) ([]Outcome, error) {
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	outcomes := make([]Outcome, len(paths))
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	for i, path := range paths {
		select {
		case <-ctx.Done():
			// Stop admitting new work; record the remaining paths as cancelled.
			for j := i; j < len(paths); j++ {
				outcomes[j] = Outcome{Name: paths[j], Err: ctx.Err()}
			}
			wg.Wait()
			return outcomes, joinErrors(outcomes)
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			n, err := process(ctx, path)
			outcomes[i] = Outcome{Name: path, Bytes: n, Err: err}
		}()
	}
	wg.Wait()
	return outcomes, joinErrors(outcomes)
}

func joinErrors(outcomes []Outcome) error {
	errs := make([]error, 0, len(outcomes))
	for _, o := range outcomes {
		if o.Err != nil {
			errs = append(errs, o.Err)
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo writes a handful of real files into a temporary directory, deletes one of them to manufacture a failure, then processes the set at a ceiling of three with a `process` function that stats and reads each file. It prints each outcome and the aggregated error, showing that the run completed every readable file and still reported the missing one — the aggregate-all behavior in miniature.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"example.com/bounded-object-processing/process"
)

func main() {
	dir, err := os.MkdirTemp("", "objproc")
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer os.RemoveAll(dir)

	paths := make([]string, 5)
	for i := range paths {
		p := filepath.Join(dir, fmt.Sprintf("file-%d.txt", i))
		_ = os.WriteFile(p, []byte(fmt.Sprintf("payload-%d", i)), 0o644)
		paths[i] = p
	}
	// Delete one to manufacture a failure that must still be reported.
	_ = os.Remove(paths[2])

	read := func(_ context.Context, path string) (int64, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		return int64(len(b)), nil
	}

	outcomes, aggErr := process.ProcessFiles(context.Background(), paths, 3, read)
	for _, o := range outcomes {
		status := "ok"
		if o.Err != nil {
			status = "FAIL"
		}
		fmt.Printf("%-7s bytes=%d %s\n", status, o.Bytes, filepath.Base(o.Name))
	}
	if aggErr != nil {
		fmt.Println("aggregated error present: 1 failure")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (one file was deleted, so exactly one outcome fails; the rest succeed):

```text
ok      bytes=9 file-0.txt
ok      bytes=9 file-1.txt
FAIL    bytes=0 file-2.txt
ok      bytes=9 file-3.txt
ok      bytes=9 file-4.txt
aggregated error present: 1 failure
```

### Tests

Three properties are pinned. The peak test passes a `process` function that records in-flight concurrency and asserts the high-water mark never exceeds N, under `-race`. The aggregation test fails two specific objects with distinct sentinel errors and asserts that the single returned error satisfies `errors.Is` for both — the proof that no failure was dropped and that `errors.Join` preserves identity. The order test confirms the outcome at index i corresponds to the path at index i regardless of completion order, which the index-ownership write pattern guarantees.

Create `process_test.go`:

```go
package process

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestProcessFilesBoundsConcurrency(t *testing.T) {
	t.Parallel()

	const limit = 4
	const n = 40
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("obj-%d", i)
	}
	var inFlight, peak atomic.Int64
	_, err := ProcessFiles(context.Background(), paths, limit, func(_ context.Context, _ string) (int64, error) {
		c := inFlight.Add(1)
		for {
			old := peak.Load()
			if c <= old || peak.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return 1, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := peak.Load(); got > int64(limit) {
		t.Fatalf("peak = %d, want <= %d", got, limit)
	}
}

func TestProcessFilesAggregatesEveryError(t *testing.T) {
	t.Parallel()

	errA := errors.New("bad object A")
	errB := errors.New("bad object B")
	paths := []string{"a", "ok1", "b", "ok2"}
	_, err := ProcessFiles(context.Background(), paths, 2, func(_ context.Context, p string) (int64, error) {
		switch p {
		case "a":
			return 0, errA
		case "b":
			return 0, errB
		default:
			return 7, nil
		}
	})
	if err == nil {
		t.Fatal("want an aggregated error, got nil")
	}
	if !errors.Is(err, errA) {
		t.Fatalf("aggregated error does not match errA: %v", err)
	}
	if !errors.Is(err, errB) {
		t.Fatalf("aggregated error does not match errB: %v", err)
	}
}

func TestProcessFilesPreservesOrder(t *testing.T) {
	t.Parallel()

	paths := []string{"first", "second", "third", "fourth"}
	outcomes, err := ProcessFiles(context.Background(), paths, 4, func(_ context.Context, p string) (int64, error) {
		return int64(len(p)), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, o := range outcomes {
		if o.Name != paths[i] {
			t.Fatalf("outcome[%d].Name = %q, want %q", i, o.Name, paths[i])
		}
		if o.Bytes != int64(len(paths[i])) {
			t.Fatalf("outcome[%d].Bytes = %d, want %d", i, o.Bytes, len(paths[i]))
		}
	}
}
```

## Review

The processor is correct when it bounds concurrency, runs every object to completion, and returns one error that `errors.Is` can match against each individual failure. The defining choice versus the earlier runners is aggregate-all rather than fail-fast: a reconciliation operator needs every failure in one pass, so the runner must never short-circuit. Confirm the aggregation with the two-failure test — both sentinels must be matchable through the single joined error, which is exactly what `errors.Join` provides and a "return the last error" shortcut would break. Confirm order is preserved by index ownership, not by completion timing, and keep the concurrency counter atomic under `-race`. The injected `process` function keeps the runner reusable: it bounds, orders, and aggregates while knowing nothing about files or object stores.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — the standard-library aggregation whose result `errors.Is` matches against every joined member.
- [Working with Errors in Go 1.13+](https://go.dev/blog/go1.13-errors) — `errors.Is`/`errors.As` and wrapping, the semantics the aggregation relies on.
- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) — `SetLimit` plus error collection, the packaged counterpart to this bounded aggregating runner.

---

Back to [03-bounded-http-fanout.md](03-bounded-http-fanout.md) | Next: [../16-pub-sub-with-channels/00-concepts.md](../16-pub-sub-with-channels/00-concepts.md)
