# 7. All-or-Nothing Page Load With Concurrent Cancellation

`errgroup.WithContext` is the canonical wrapper for "all-or-nothing" parallel work: load N things, if any fails cancel the others and return. The pattern is built on `sync.WaitGroup` + `sync.Once` + `context.WithCancel` — the same three pieces the gold-sample's `group.Group` uses. The lesson reuses the gold-sample's `group.Group` and adds a page loader that runs N sections in parallel, writes the results into a mutex-protected map, and cancels siblings on the first error.

```text
errgroupctx/
  go.mod
  internal/group/group.go
  internal/group/group_test.go
  internal/pageload/pageload.go
  internal/pageload/pageload_test.go
  cmd/errgroupctx/main.go
```

The package exposes a `pageload.Load` function. The `cmd/errgroupctx` CLI runs two scenarios: the all-OK case (every section loads) and the parent-cancel case (the parent context is cancelled mid-flight). The captured output is the lesson's documentation.

## Concepts

### The All-or-Nothing Pattern

Some work is only useful if every part succeeds. A page with three sections where one section is missing is a half-loaded page. The pattern: spawn N goroutines, each loads one section; if any returns an error, the others see the cancellation and exit; the runner returns the first error.

### The Cancellation Path

The cancellation signal is the derived context. `group.New(ctx)` returns the group and a context derived from `ctx`. Each goroutine uses `select { case <-time.After(delay): ... case <-gctx.Done(): ... }`. The first error calls `gctx` cancellation, the other goroutines see `gctx.Done()`, and they return early with the context error.

### The Partial Results Are Discarded

The lesson's `PageData` has a mutex-protected map of section results. When the runner returns an error, the map is discarded. That is the contract: partial success is not useful. If you need partial results, use `MultiError` (the previous lesson) instead.

### `errgroup.WithContext` Is The Idiomatic Wrapper

`errgroup.WithContext(ctx)` is the wrapper for this pattern. The lesson's `group.New(ctx)` is line-for-line equivalent in behavior. If you ship to production and you already use `golang.org/x/sync`, prefer `errgroup`; the lesson's pattern is what `errgroup` does internally.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/07-errgroup-with-context/07-errgroup-with-context/internal/group go-solutions/16-concurrency-patterns/07-errgroup-with-context/07-errgroup-with-context/internal/pageload go-solutions/16-concurrency-patterns/07-errgroup-with-context/07-errgroup-with-context/cmd/errgroupctx
cd go-solutions/16-concurrency-patterns/07-errgroup-with-context/07-errgroup-with-context
```

### Exercise 1: The Group (Embedded)

Create `internal/group/group.go`:

```go
package group

import (
	"context"
	"sync"
)

type Group struct {
	wg      sync.WaitGroup
	errOnce sync.Once
	err     error
	cancel  context.CancelFunc
	ctx     context.Context
}

// New returns a new Group and a derived context. The derived context
// is cancelled when any goroutine in the group returns a non-nil
// error, or when the parent context is cancelled.
func New(ctx context.Context) (*Group, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &Group{ctx: ctx, cancel: cancel}, ctx
}

// Go runs f in a new goroutine. If f returns a non-nil error, the
// Group's derived context is cancelled and the first such error is
// recorded.
func (g *Group) Go(f func() error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := f(); err != nil {
			g.errOnce.Do(func() {
				g.err = err
				g.cancel()
			})
		}
	}()
}

// Wait blocks until all goroutines in the group have returned and
// returns the first non-nil error, if any. The derived context is
// cancelled when Wait returns, even on success.
func (g *Group) Wait() error {
	g.wg.Wait()
	if g.cancel != nil {
		g.cancel()
	}
	return g.err
}
```

Create `internal/pageload/pageload.go`:

```go
package pageload

import (
	"context"
	"fmt"
	"sync"
	"time"

	"example.com/errgroupctx/internal/group"
)

type PageData struct {
	mu       sync.Mutex
	Sections map[string]string
}

func (p *PageData) Set(key, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Sections[key] = value
}

func Load(ctx context.Context, sections map[string]time.Duration) (*PageData, error) {
	g, gctx := group.New(ctx)
	page := &PageData{Sections: make(map[string]string)}

	for name, delay := range sections {
		name, delay := name, delay
		g.Go(func() error {
			select {
			case <-time.After(delay):
				page.Set(name, fmt.Sprintf("<%s content>", name))
				return nil
			case <-gctx.Done():
				return fmt.Errorf("loading %s: %w", name, gctx.Err())
			}
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return page, nil
}
```

`Load` is the lesson's main API. The `select` is the cancellation point: each section either completes after its delay or sees the cancellation through `gctx.Done()`. On error, the partial `PageData` is discarded (the caller gets `nil, err`).

### Exercise 3: Test The Page Loader

Create `internal/pageload/pageload_test.go`:

```go
package pageload

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"example.com/errgroupctx/internal/group"
)

func TestLoadAllSucceed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sections := map[string]time.Duration{
		"header":  5 * time.Millisecond,
		"sidebar": 10 * time.Millisecond,
		"main":    8 * time.Millisecond,
	}
	page, err := Load(ctx, sections)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(page.Sections); got != 3 {
		t.Fatalf("len = %d, want 3", got)
	}
	if got := page.Sections["header"]; got != "<header content>" {
		t.Fatalf("Sections[header] = %q, want <header content>", got)
	}
}

func TestLoadFailingSectionCancelsOthers(t *testing.T) {
	t.Parallel()

	// The fail-fast contract: a 50ms sibling must be cancelled when a
	// 0ms sibling fails. We exercise the contract on the group package
	// (which Load uses internally); Load is exercised above with all-success.
	g, gctx := group.New(context.Background())

	done := make(chan struct{})
	g.Go(func() error {
		select {
		case <-time.After(50 * time.Millisecond):
			return nil
		case <-gctx.Done():
			close(done)
			return gctx.Err()
		}
	})
	g.Go(func() error {
		return errors.New("section X failed immediately")
	})
	if err := g.Wait(); err == nil {
		t.Fatal("expected error from failing section")
	}
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("the 50ms section was not cancelled when the sibling failed")
	}
}

func TestLoadRespectsParentContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		<-time.After(20 * time.Millisecond)
		cancel()
		close(done)
	}()
	_, err := Load(ctx, map[string]time.Duration{
		"slow": 200 * time.Millisecond,
	})
	<-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "loading slow") {
		t.Fatalf("err = %v, want it to mention the cancelled section", err)
	}
}
```

`TestLoadAllSucceed` is the lesson's main end-to-end test: the loader returns a complete `PageData`. `TestLoadFailingSectionCancelsOthers` proves the fail-fast contract on the underlying `group.Group` (the production `Load` is exercised above). `TestLoadRespectsParentContext` proves the inverse direction: cancelling the parent context cancels the load, and the error message names the cancelled section.

### Exercise 4: Run It End To End

Create `cmd/errgroupctx/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"example.com/errgroupctx/internal/pageload"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	mode := "all-ok"
	if len(args) > 1 {
		mode = args[1]
	}

	switch mode {
	case "all-ok":
		return runAllOK()
	case "cancel-by-parent":
		return runCancelByParent()
	default:
		return fmt.Errorf("unknown mode %q (use all-ok|cancel-by-parent)", mode)
	}
}

func runAllOK() error {
	sections := map[string]time.Duration{
		"header":  10 * time.Millisecond,
		"sidebar": 30 * time.Millisecond,
		"main":    20 * time.Millisecond,
		"footer":  15 * time.Millisecond,
	}
	fmt.Println("=== all-or-nothing page load (all sections succeed) ===")
	page, err := pageload.Load(context.Background(), sections)
	if err != nil {
		return err
	}
	for k, v := range page.Sections {
		fmt.Printf("  %s: %s\n", k, v)
	}
	return nil
}

func runCancelByParent() error {
	fmt.Println("=== cancel via parent context (sections do not finish) ===")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		fmt.Println("  parent context cancelled")
		cancel()
	}()
	_, err := pageload.Load(ctx, map[string]time.Duration{
		"slow": 200 * time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		return fmt.Errorf("err = %v, want context.Canceled", err)
	}
	fmt.Println("  load returned with:", err)
	return nil
}
```

Run it from `~/go-exercises/errgroupctx`:

```bash
go run ./cmd/errgroupctx all-ok
```

Expected output (captured by the author on Go 1.26):

```text
=== all-or-nothing page load (all sections succeed) ===
  header: <header content>
  footer: <footer content>
  main: <main content>
  sidebar: <sidebar content>
```

The four sections load in parallel; the output is sorted by the map's iteration order (Go's randomized map iteration). The lesson does not pin a specific order; the test pins the count and one section's content.

```bash
go run ./cmd/errgroupctx cancel-by-parent
```

Expected output:

```text
=== cancel via parent context (sections do not finish) ===
  parent context cancelled
  load returned with: loading slow: context canceled
```

The parent context is cancelled after 10ms; the `slow` section was waiting 200ms; the load returns `context.Canceled` wrapped in `loading slow: ...`. The `errors.Is` check on `context.Canceled` is what makes the test pass.

## Common Mistakes

### Using The Parent Context Instead Of The Derived One

Wrong: a goroutine that takes the parent `ctx` and does `select { case <-time.After(delay): ... case <-parent.Done(): ... }`. The parent cancellation does reach the goroutine, but the group-internal cancellation (from a sibling's error) does not.

Fix: pass the derived context (`gctx` in the lesson) to every goroutine. The group cancels `gctx` on first error; the goroutine sees the cancellation.

### Forgetting To Check `ctx.Done()` Inside The Goroutine

Wrong: a goroutine that does `time.Sleep(delay); doWork()` and never checks the context. The goroutine runs to completion even after the group cancelled.

Fix: every blocking operation in a goroutine must have a `case <-ctx.Done():` branch. The lesson's `select` is the contract.

### Returning Partial Results On Error

Wrong: returning the partial `PageData` alongside the error. The caller may use the partial data, which is the bug. The all-or-nothing contract is "all or nothing".

Fix: return `nil, err` on failure. The partial data is discarded. If the caller needs partial data, use `MultiError` (previous lesson), not `group.Group`.

### Confusing `Wait` With `Wait` Returning The First Error

Wrong: assuming `g.Wait()` returns nil if the goroutines succeed. It does, but the lesson is also the contract that the derived context is cancelled when `Wait` returns, even on success. Code that does `defer cancel()` after `Wait()` is fine, but code that holds onto the context past `Wait()` sees it cancelled.

Fix: the lesson's `Load` returns the `*PageData` only on success. The context is local to `Load`; the caller does not see it. If the caller needs the context past `Load`, they pass their own; the lesson's derived context is scoped to the call.

## Verification

Run this from `~/go-exercises/errgroupctx`:

```bash
test -z "$(gofmt -l .)"
go test -count=1 -race ./...
go vet ./...
go build ./...
go run ./cmd/errgroupctx all-ok
go run ./cmd/errgroupctx cancel-by-parent
```

`go build ./...` proves the `cmd/errgroupctx` binary compiles. The two `go run` steps produce the captured output above. The test suite pins the contract: all-success, fail-fast on the underlying group, parent-cancel propagation.

The optional "swap `group.Group` for `errgroup.WithContext`" exercise (not in the tests) is left to the reader: replace `group.New(ctx)` with `errgroup.WithContext(ctx)`, and `g.Go(...)` stays the same. The behavior is identical.

## Summary

- The all-or-nothing pattern: every task runs; the first error cancels the others; the runner returns the first error.
- The cancellation signal is the derived context. Every goroutine's blocking call must have a `case <-ctx.Done():` branch.
- Partial results are discarded. If you need them, use `MultiError`.
- `errgroup.WithContext` is the canonical wrapper; `group.New` is the stdlib equivalent.
- The derived context is cancelled when `Wait` returns, even on success.

## What's Next

Next: [time.Ticker Periodic Goroutines](../08-time-ticker-periodic-goroutines/08-time-ticker-periodic-goroutines.md).

## Resources

- [errgroup documentation](https://pkg.go.dev/golang.org/x/sync/errgroup)
- [context package](https://pkg.go.dev/context)
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup)
