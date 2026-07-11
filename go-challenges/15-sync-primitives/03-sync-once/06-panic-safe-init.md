# Exercise 6: A plugin loader that recovers a panicking init into a captured error

Third-party init is where the panic-is-done trap turns into a crashed process. A
driver's `Register`, a plugin's setup, a codec's self-registration — any of these
can panic, and if it panics inside a `sync.Once` closure with nothing to catch it,
the panic unwinds the winning goroutine and takes the process down. This exercise
builds a loader that defers a `recover` inside the closure, converting a panicking
init into a controlled `ErrInitFailed` that every caller observes.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
panic-safe-init/              module: example.com/panic-safe-init
  go.mod
  loader.go                   ErrInitFailed; type Driver, Loader; New, Load, Runs
  cmd/
    demo/
      main.go                 runnable demo: panicking init recovered to an error
  loader_test.go              recover-to-error, cached failure, success, concurrency
```

- Files: `loader.go`, `cmd/demo/main.go`, `loader_test.go`.
- Implement: a `Loader` whose `sync.Once` closure calls a fallible `init` func under `defer`/`recover`; a panic becomes `fmt.Errorf("%w: %v", ErrInitFailed, r)` captured in a field; `Load()` returns the resource and the captured error; `Runs()` proves the init body ran once.
- Test: a panicking init makes `Load` return an `ErrInitFailed`-wrapped error, and a second `Load` returns the same error without re-running (panic-is-done); a successful init returns nil and sets the resource; concurrent `Load` with a panicking init runs init once, all callers see the same error, and no goroutine crashes.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p panic-safe-init/cmd/demo
cd panic-safe-init
go mod init example.com/panic-safe-init
```

### Recover inside the closure, not outside it

The naive loader calls `init` directly inside `once.Do`. If `init` panics, two
bad things happen at once: the panic unwinds the goroutine that won the `Do`
election — which is not necessarily the goroutine you would expect, and an
uncaught panic in any goroutine crashes the whole process — and `Do` still marks
the `Once` done, so even if you somehow survived, the failure is cached with no
retry. Recovering at the *call site* of `Load` does not help either: by the time
the panic reaches your `Load` frame it has already left the closure, and only the
one goroutine that ran the closure would see it; the other waiters return normally
with a half-built resource.

The fix is to recover *inside* the closure. A `defer func(){ if r := recover();
r != nil { ... } }()` at the top of the closure catches the panic in the same
frame that ran `init`, converts the recovered value into a captured error, and
lets `Do` complete normally. Now every caller — winner and waiters alike — reads
the same captured `ErrInitFailed`, no goroutine unwinds past the closure, and the
process stays up. You have deliberately opted into panic-is-done (the failure is
cached; a retry needs a fresh `Once`, per the previous exercise) in exchange for
never crashing on a third-party init.

Create `loader.go`:

```go
package loader

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrInitFailed wraps any failure — returned or recovered from a panic — that
// occurred while loading the driver.
var ErrInitFailed = errors.New("loader: init failed")

// Driver is the resource the loader produces.
type Driver struct {
	Name string
}

// Loader loads a Driver exactly once, converting a panicking init into a captured
// error so callers get a controlled failure instead of a crashed goroutine.
type Loader struct {
	once sync.Once
	res  *Driver
	err  error
	runs atomic.Int64
	init func() (*Driver, error)
}

// New returns a Loader that calls init to build the Driver. init may return an
// error or panic; either becomes an ErrInitFailed-wrapped error.
func New(init func() (*Driver, error)) *Loader {
	return &Loader{init: init}
}

// Load runs init exactly once and returns the driver and captured error. A panic
// inside init is recovered and wrapped; the failure is cached (panic-is-done).
func (l *Loader) Load() (*Driver, error) {
	l.once.Do(func() {
		l.runs.Add(1)
		defer func() {
			if r := recover(); r != nil {
				l.res = nil
				l.err = fmt.Errorf("%w: %v", ErrInitFailed, r)
			}
		}()
		res, err := l.init()
		if err != nil {
			l.err = fmt.Errorf("%w: %w", ErrInitFailed, err)
			return
		}
		l.res = res
	})
	return l.res, l.err
}

// Runs reports how many times the init body ran; it must be 0 or 1.
func (l *Loader) Runs() int64 {
	return l.runs.Load()
}
```

The closure handles both failure shapes uniformly: a returned error is wrapped
with `%w` (so `errors.Is(err, ErrInitFailed)` and the original cause both match),
and a panic is recovered and wrapped with `%v` on the recovered value. Either way,
`ErrInitFailed` is the sentinel every caller can assert.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/panic-safe-init"
)

func main() {
	panicking := loader.New(func() (*loader.Driver, error) {
		panic("driver registration blew up")
	})

	_, err := panicking.Load()
	fmt.Println("is ErrInitFailed:", errors.Is(err, loader.ErrInitFailed))
	fmt.Println("err:", err)

	// A second Load returns the same cached error; init does not re-run.
	_, err2 := panicking.Load()
	fmt.Println("same error cached:", err.Error() == err2.Error())
	fmt.Println("init runs:", panicking.Runs())

	ok := loader.New(func() (*loader.Driver, error) {
		return &loader.Driver{Name: "postgres"}, nil
	})
	d, err := ok.Load()
	fmt.Println("driver:", d.Name, "err:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
is ErrInitFailed: true
err: loader: init failed: driver registration blew up
same error cached: true
init runs: 1
driver: postgres err: <nil>
```

### Tests

`TestPanicRecovered` asserts a panicking init yields an `ErrInitFailed` error and
that a second `Load` returns the same value with `Runs() == 1` (panic-is-done).
`TestReturnedError` covers the ordinary error path. `TestSuccess` covers the happy
path. `TestConcurrentPanic` fans goroutines at a panicking init and asserts init
ran once, everyone saw an `ErrInitFailed`, and — implicitly, by not crashing — no
panic escaped a goroutine.

Create `loader_test.go`:

```go
package loader

import (
	"errors"
	"sync"
	"testing"
)

func TestPanicRecovered(t *testing.T) {
	t.Parallel()

	l := New(func() (*Driver, error) {
		panic("boom")
	})

	_, err := l.Load()
	if !errors.Is(err, ErrInitFailed) {
		t.Fatalf("err = %v, want ErrInitFailed", err)
	}

	_, err2 := l.Load()
	if err.Error() != err2.Error() {
		t.Fatalf("second Load error differs: %v vs %v", err, err2)
	}
	if got := l.Runs(); got != 1 {
		t.Fatalf("Runs() = %d, want 1 (panic is done)", got)
	}
}

func TestReturnedError(t *testing.T) {
	t.Parallel()

	cause := errors.New("dsn missing")
	l := New(func() (*Driver, error) {
		return nil, cause
	})

	_, err := l.Load()
	if !errors.Is(err, ErrInitFailed) {
		t.Fatalf("err = %v, want ErrInitFailed", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to wrap the cause", err)
	}
}

func TestSuccess(t *testing.T) {
	t.Parallel()

	l := New(func() (*Driver, error) {
		return &Driver{Name: "mysql"}, nil
	})

	d, err := l.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if d.Name != "mysql" {
		t.Fatalf("driver = %q, want mysql", d.Name)
	}
}

func TestConcurrentPanic(t *testing.T) {
	t.Parallel()

	l := New(func() (*Driver, error) {
		panic("concurrent boom")
	})

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if _, err := l.Load(); !errors.Is(err, ErrInitFailed) {
				t.Errorf("Load = %v, want ErrInitFailed", err)
			}
		}()
	}
	wg.Wait()

	if got := l.Runs(); got != 1 {
		t.Fatalf("Runs() = %d, want exactly 1", got)
	}
}
```

## Review

The loader is correct when a panicking init becomes a captured error rather than a
crash. `TestPanicRecovered` proves the panic is converted to `ErrInitFailed` and
cached (`Runs() == 1` after two `Load`s), which is the deliberate acceptance of
panic-is-done. `TestConcurrentPanic` is the safety proof: 100 goroutines hit a
panicking init and the test process survives, because the `recover` is inside the
closure where the panic unwinds, not at the `Load` call site where it would be too
late for the waiters. The two error shapes are unified under `ErrInitFailed` with
`%w`, so callers assert one sentinel. If you need to retry after a recovered
failure, wrap this loader in the resettable-`Once` pattern from Exercise 5 — the
recover makes the failure controlled, it does not make it retry. Run
`go test -race`.

## Resources

- [Defer, Panic, and Recover — The Go Blog](https://go.dev/blog/defer-panic-and-recover)
- [sync.Once — pkg.go.dev](https://pkg.go.dev/sync#Once)
- [fmt.Errorf and %w — pkg.go.dev](https://pkg.go.dev/fmt#Errorf)

---

Prev: [05-resettable-init-retry.md](05-resettable-init-retry.md) | Back to [00-concepts.md](00-concepts.md) | Next: [07-once-registration-guard.md](07-once-registration-guard.md)
