# Exercise 6: Graceful shutdown — a cleanup stack that skips nil Closers

Graceful shutdown closes resources in reverse of the order they were opened. This
module builds a `Cleanup` stack that collects `io.Closer` values during startup
(db, cache, listener) and closes them in LIFO order, skipping optional resources
that arrive as nil interfaces or typed nils, and joining every failure with
`errors.Join`.

## What you'll build

```text
cleanupstack/              independent module: example.com/cleanupstack
  go.mod                   go 1.26
  cleanup.go               Cleanup; Push; Close (LIFO, nil-skip, errors.Join); isNil
  cmd/
    demo/
      main.go              push db/cache/listener + a nil, close in LIFO
  cleanup_test.go          LIFO order; nil skip; typed-nil skip; joined errors; concurrent push
```

- Files: `cleanup.go`, `cmd/demo/main.go`, `cleanup_test.go`.
- Implement: `Cleanup` with `Push(io.Closer)` and `Close() error` that closes in LIFO order, skips nil interfaces and typed nils, and returns `errors.Join` of the failures.
- Test: a mix of real closers, a nil interface, and a typed-nil `*fakeCloser` — `Close` runs LIFO, skips the nils, closes every real one exactly once, and returns the joined errors matchable via `errors.Is`; a concurrent-push subtest under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/07-nil-interface-values/06-closer-cleanup-nil-skip/cmd/demo
cd go-solutions/08-interfaces/07-nil-interface-values/06-closer-cleanup-nil-skip
```

### Why the nil guard and errors.Join matter here

Startup wires resources in dependency order and pushes each onto the stack. Some
are optional: a cache that is only enabled in some tiers, a metrics listener that
may be absent. When those are absent they arrive as a nil interface (someone
pushed a nil `io.Closer`) or a typed nil (someone pushed a `var c *redisCache`
that was never initialized). If `Close` calls `.Close()` on a typed nil whose
method dereferences the receiver, shutdown panics — and every resource that had
not yet been closed leaks, because the panic aborts the loop. So `Close` guards
each entry with `isNil`, the same reflect-based check used for DI containers: a
nil interface (`c == nil`) or a nilable Kind whose value is nil is skipped.

The order is LIFO: resources are closed in reverse of the order they were opened,
because later resources often depend on earlier ones (the listener depends on the
db). And one bad closer must not abort the rest: `Close` collects every error and
returns `errors.Join(errs...)`, so a failing cache close still lets the db and
listener close, and the caller can inspect the aggregate with `errors.Is`.

Create `cleanup.go`:

```go
package cleanupstack

import (
	"errors"
	"io"
	"reflect"
	"sync"
)

// Cleanup is a LIFO stack of io.Closers to run at shutdown. It is safe for
// concurrent Push during startup.
type Cleanup struct {
	mu      sync.Mutex
	closers []io.Closer
}

// Push adds a closer to the stack. A nil or typed-nil closer is allowed here and
// simply skipped at Close time.
func (c *Cleanup) Push(closer io.Closer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closers = append(c.closers, closer)
}

// Close closes every pushed resource in LIFO order, skipping nil and typed-nil
// closers, and returns the joined errors of any that failed.
func (c *Cleanup) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error
	for i := len(c.closers) - 1; i >= 0; i-- {
		closer := c.closers[i]
		if isNil(closer) {
			continue
		}
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	c.closers = nil
	return errors.Join(errs...)
}

// isNil reports whether an io.Closer is a nil interface or a typed nil, without
// panicking on a non-nilable dynamic type.
func isNil(c io.Closer) bool {
	if c == nil {
		return true
	}
	rv := reflect.ValueOf(c)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"

	"example.com/cleanupstack"
)

// namedCloser prints when it closes, so the demo shows LIFO order.
type namedCloser struct{ name string }

func (n namedCloser) Close() error {
	fmt.Println("closing", n.name)
	return nil
}

func main() {
	var stack cleanupstack.Cleanup

	// Pushed in startup order: db, then cache, then listener.
	stack.Push(namedCloser{"db"})
	stack.Push(namedCloser{"cache"})

	// An optional resource that was never initialized: a typed nil, skipped.
	var optional io.Closer
	stack.Push(optional)

	stack.Push(namedCloser{"listener"})

	if err := stack.Close(); err != nil {
		fmt.Println("shutdown errors:", err)
	} else {
		fmt.Println("clean shutdown")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
closing listener
closing cache
closing db
clean shutdown
```

### Tests

Create `cleanup_test.go`:

```go
package cleanupstack

import (
	"errors"
	"io"
	"sync"
	"testing"
)

var errBoom = errors.New("close failed")

// fakeCloser records the order in which closers ran via a shared log.
type fakeCloser struct {
	id     string
	err    error
	closed *[]string
	mu     *sync.Mutex
}

func (f *fakeCloser) Close() error {
	f.mu.Lock()
	*f.closed = append(*f.closed, f.id)
	f.mu.Unlock()
	return f.err
}

func TestCloseLIFOSkipsNilsAndJoinsErrors(t *testing.T) {
	t.Parallel()

	var order []string
	var mu sync.Mutex
	mk := func(id string, err error) *fakeCloser {
		return &fakeCloser{id: id, err: err, closed: &order, mu: &mu}
	}

	var stack Cleanup
	stack.Push(mk("db", nil))
	stack.Push(mk("cache", errBoom)) // this one fails
	stack.Push(nil)                  // nil interface: skipped
	var typedNil *fakeCloser
	stack.Push(typedNil) // typed nil: skipped
	stack.Push(mk("listener", nil))

	err := stack.Close()

	// LIFO over the non-nil closers: listener, cache, db.
	want := []string{"listener", "cache", "db"}
	if len(order) != len(want) {
		t.Fatalf("close order = %v; want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("close order = %v; want %v", order, want)
		}
	}

	if !errors.Is(err, errBoom) {
		t.Fatalf("Close error = %v; want it to join errBoom", err)
	}
}

func TestCloseWithoutFailuresReturnsNil(t *testing.T) {
	t.Parallel()

	var order []string
	var mu sync.Mutex
	var stack Cleanup
	stack.Push(&fakeCloser{id: "a", closed: &order, mu: &mu})
	stack.Push(&fakeCloser{id: "b", closed: &order, mu: &mu})

	if err := stack.Close(); err != nil {
		t.Fatalf("Close() = %v; want nil", err)
	}
	if len(order) != 2 {
		t.Fatalf("expected 2 closes, got %v", order)
	}
}

func TestConcurrentPush(t *testing.T) {
	t.Parallel()

	var stack Cleanup
	var order []string
	var mu sync.Mutex

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stack.Push(&fakeCloser{id: string(rune('a' + i%26)), closed: &order, mu: &mu})
		}()
	}
	wg.Wait()

	if err := stack.Close(); err != nil {
		t.Fatalf("Close() = %v; want nil", err)
	}
	if len(order) != 50 {
		t.Fatalf("expected 50 closes, got %d", len(order))
	}
}

// verify fakeCloser satisfies io.Closer.
var _ io.Closer = (*fakeCloser)(nil)
```

## Review

The stack is correct when `Close` runs LIFO, skips both nil forms, closes every
real resource exactly once, and aggregates failures.
`TestCloseLIFOSkipsNilsAndJoinsErrors` pushes a mix — two good closers, one that
fails, a nil interface, and a typed-nil `*fakeCloser` — and asserts the recorded
order is `listener, cache, db` (the reverse of push, with the two nils skipped)
and that the returned error joins `errBoom`, verified with `errors.Is`.
`TestConcurrentPush` runs `Push` from 50 goroutines under `-race` to prove the
mutex guards the slice. The mistakes to avoid are closing a typed-nil resource
unconditionally (panics, leaking the rest) and returning only the first error
(loses the others); `isNil` and `errors.Join` are the fixes.

## Resources

- [`io.Closer`](https://pkg.go.dev/io#Closer) — the single-method contract this stack aggregates.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining multiple errors into one inspectable with `errors.Is`.
- [`reflect.Value.IsNil`](https://pkg.go.dev/reflect#Value.IsNil) — the typed-nil guard's valid kinds.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-nil-interface-equality-registry.md](07-nil-interface-equality-registry.md)
