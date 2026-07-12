# Exercise 35: Memory Buffer Pool Checkout and Return Guard

A buffer pool only pays for itself if every checkout is matched by exactly
one return — checked out, used, and given back, on every exit path,
including one where the caller's callback fails or panics halfway through.
This exercise builds a `Process` that checks a buffer out, hands it to a
callback, and uses a named `released bool` flag with a deferred closure to
guarantee the buffer goes back to the pool exactly once, whether the
callback succeeds, fails, or panics.

**Nivel: Intermedio** — validacion rapida (exito, fallo, panic, y una prueba concurrente con `-race`).

## What you'll build

```text
bufpool/                    independent module: example.com/bufpool
  go.mod
  bufpool.go                  Pool; Process (released flag, deferred return-to-pool)
  cmd/demo/
    main.go                  runnable demo: a successful checkout and a failing one
  bufpool_test.go             returns buffer on success, on error, on panic, and under concurrent load
```

- Files: `bufpool.go`, `cmd/demo/main.go`, `bufpool_test.go`.
- Implement: `(*Pool) Process(fn func(buf []byte) (int, error)) (n int, err error)` that checks out a buffer, explicitly releases it on the success path (setting a `released` flag), and defers a release that only fires when `released` is still false.
- Test: success releases the buffer once (pool count restored); a callback error still releases it; a callback panic still releases it; 20 concurrent `Process` calls against a 2-buffer pool leave the pool fully restocked under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/35-memory-buffer-pool-checkout-return/cmd/demo
cd go-solutions/04-functions/02-named-return-values/35-memory-buffer-pool-checkout-return
go mod edit -go=1.24
```

### A release flag makes the deferred return a no-op on the happy path

```go
released := false
defer func() {
    if !released {
        p.release(buf)
    }
}()

n, err = fn(buf)
if err != nil {
    return
}
p.release(buf)
released = true
return
```

`Process` releases the buffer explicitly right after a successful `fn` call,
then sets `released = true`. The deferred closure checks that flag before
doing anything: on the success path it is already true, so the defer is a
no-op and the buffer is returned exactly once, not twice. On every other
exit — `fn` returning an error, or `fn` panicking — `released` is still
false when the deferred closure runs, so it performs the release that the
success path would otherwise have done. This is the same "committed" flag
pattern used for the transaction-snapshot and shutdown-drain exercises
earlier in this chapter: one boolean named result (or, here, a local
variable the defer closes over) that tells a single deferred closure whether
its job still needs doing.

Create `bufpool.go`:

```go
package bufpool

import (
	"fmt"
	"sync"
)

// Pool is a fixed-size pool of equally sized byte buffers.
type Pool struct {
	mu   sync.Mutex
	free [][]byte
	size int
}

// NewPool builds a Pool of n buffers, each bufSize bytes long.
func NewPool(bufSize, n int) *Pool {
	p := &Pool{size: bufSize}
	for i := 0; i < n; i++ {
		p.free = append(p.free, make([]byte, bufSize))
	}
	return p
}

// Available reports how many buffers are currently checked in.
func (p *Pool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.free)
}

func (p *Pool) checkout() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.free) == 0 {
		return nil, fmt.Errorf("bufpool: no buffers available")
	}
	n := len(p.free) - 1
	buf := p.free[n]
	p.free = p.free[:n]
	return buf, nil
}

func (p *Pool) release(buf []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free = append(p.free, buf[:p.size])
}

// Process checks out a buffer, hands it to fn, and returns it to the pool
// exactly once regardless of how fn exits.
//
// n and err are named results: a single deferred closure checks a released
// flag once Process is about to return (or unwind from a panic in fn) and
// releases the buffer back to the pool unless the success path already did
// so. This is the same guard used for the transaction/lock exercises earlier
// in this chapter: the explicit release on the success path sets released,
// so the deferred safety net becomes a no-op there and only actually returns
// the buffer on the error or panic paths, preventing a leaked checkout.
func (p *Pool) Process(fn func(buf []byte) (int, error)) (n int, err error) {
	buf, err := p.checkout()
	if err != nil {
		return 0, err
	}

	released := false
	defer func() {
		if !released {
			p.release(buf)
		}
	}()

	n, err = fn(buf)
	if err != nil {
		return
	}
	p.release(buf)
	released = true
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/bufpool"
)

func main() {
	pool := bufpool.NewPool(64, 2)
	fmt.Println("available at start:", pool.Available())

	n, err := pool.Process(func(buf []byte) (int, error) {
		copy(buf, "hello")
		return 5, nil
	})
	fmt.Printf("success: n=%d err=%v available=%d\n", n, err, pool.Available())

	n, err = pool.Process(func(buf []byte) (int, error) {
		return 0, errors.New("write failed")
	})
	fmt.Printf("failure: n=%d err=%v available=%d\n", n, err, pool.Available())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
available at start: 2
success: n=5 err=<nil> available=2
failure: n=0 err=write failed available=2
```

### Tests

Create `bufpool_test.go`:

```go
package bufpool

import (
	"errors"
	"sync"
	"testing"
)

func TestProcessReturnsBufferOnSuccess(t *testing.T) {
	t.Parallel()

	p := NewPool(16, 1)
	n, err := p.Process(func(buf []byte) (int, error) {
		copy(buf, "hi")
		return 2, nil
	})
	if err != nil {
		t.Fatalf("Process: unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}
	if p.Available() != 1 {
		t.Fatalf("Available() = %d, want 1 (buffer returned)", p.Available())
	}
}

func TestProcessReturnsBufferOnError(t *testing.T) {
	t.Parallel()

	p := NewPool(16, 1)
	wantErr := errors.New("boom")
	_, err := p.Process(func(buf []byte) (int, error) {
		return 0, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if p.Available() != 1 {
		t.Fatalf("Available() = %d after error, want 1 (buffer must not leak)", p.Available())
	}
}

func TestProcessReturnsBufferOnPanic(t *testing.T) {
	t.Parallel()

	p := NewPool(16, 1)
	func() {
		defer func() { _ = recover() }()
		_, _ = p.Process(func(buf []byte) (int, error) {
			panic("fn blew up")
		})
	}()
	if p.Available() != 1 {
		t.Fatalf("Available() = %d after panic, want 1 (buffer must not leak)", p.Available())
	}
}

func TestProcessExhaustsAndRefillsPool(t *testing.T) {
	t.Parallel()

	p := NewPool(8, 2)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Process(func(buf []byte) (int, error) {
				return len(buf), nil
			})
		}()
	}
	wg.Wait()

	if p.Available() != 2 {
		t.Fatalf("Available() = %d after all Process calls finished, want 2", p.Available())
	}
}
```

## Review

`Process` is correct when a buffer checked out is always returned exactly
once, regardless of which of `fn`'s outcomes triggered the return — a
property verified three separate ways (success, error, panic) plus a
concurrent stress test that exhausts a two-buffer pool with twenty
goroutines and checks it lands back at full capacity. The `released` flag is
what prevents a double-release: without it, the explicit `p.release(buf)` on
the success path and the deferred one would both fire, corrupting the pool
with the same slice appended twice. The mistake to avoid is skipping the
explicit release-and-flag on the success path and relying on the defer
alone — that also works correctly here, but it forecloses ever adding logic
between a successful `fn` call and the return that needs the buffer already
back in the pool (returning it to a limited pool before doing something else
that also needs a buffer, for instance).

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`sync.Pool`](https://pkg.go.dev/sync#Pool)
- [Go Spec: Slice expressions](https://go.dev/ref/spec#Slice_expressions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [34-idempotency-key-cache-store-result.md](34-idempotency-key-cache-store-result.md) | Next: [../03-variadic-functions/00-concepts.md](../03-variadic-functions/00-concepts.md)
