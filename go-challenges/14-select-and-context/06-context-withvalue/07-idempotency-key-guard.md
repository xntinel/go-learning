# Exercise 7: Idempotency-Key Guard

Payment and order APIs must survive a client that retries a `POST` after a network
blip without charging the card twice. The mechanism is an idempotency key: the
client sends a stable `Idempotency-Key` header, and the server executes the side
effect at most once per key, returning the cached prior response on a repeat. This
exercise builds that guard, carrying the key through the context so the handler
never has to accept it as a parameter.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
idempotency/                 independent module: example.com/idempotency
  go.mod
  idempotency.go             WithKey, KeyFromContext; Extract middleware; Guard
  cmd/
    demo/
      main.go                same key twice -> one execution; different key -> two
  idempotency_test.go        repeat short-circuits; distinct keys re-run; no key; -race
```

Files: `idempotency.go`, `cmd/demo/main.go`, `idempotency_test.go`.
Implement: `WithKey`/`KeyFromContext`, an `Extract` middleware reading the `Idempotency-Key` header into context, and a `Guard` that caches and replays the first response per key.
Test: a first `POST` with `k1` executes the handler and caches; a second with `k1` short-circuits with the cached body; a `POST` with `k2` executes again; a `POST` with no key executes normally; concurrent identical keys execute exactly once under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/06-context-withvalue/07-idempotency-key-guard/cmd/demo
cd go-solutions/14-select-and-context/06-context-withvalue/07-idempotency-key-guard
```

### Why the key rides the context and how single-execution is enforced

The idempotency key is a request-scoped fact the guard needs but the business
handler should not have to know about — a textbook context value. `Extract` reads
the header once and stores it; `Guard` reads it back with `KeyFromContext`. A
request with no key is handled normally: idempotency is opt-in per the client.

The guard must capture the handler's response to replay it later, so it wraps the
`ResponseWriter` in a small recorder that tees status and body into a buffer while
still writing through to the client. On the first request for a key, it runs the
handler, records the outcome, and stores it; on a repeat, it writes the stored
status and body without touching the handler.

The single-execution guarantee is the hard part and the reason the store is guarded
by a `sync.Mutex`. The naive "check the map, then execute, then store" has a race:
two concurrent requests with the same key both see the key absent and both execute
the side effect. This guard holds the mutex across the whole check-execute-store
critical section, so the second concurrent request blocks until the first finishes,
then finds the cached entry and replays it — exactly one execution. That coarse lock
serializes same-key *and* different-key requests, which is fine for a lesson and for
low-contention write paths; the production refinement is a per-key lock (or
`singleflight`) so different keys proceed in parallel. The prose names that
trade-off so the reader knows the simple version's cost. The `-race` test with
concurrent identical keys proves the counter increments exactly once.

Create `idempotency.go`:

```go
package idempotency

import (
	"bytes"
	"context"
	"net/http"
	"sync"
)

type ctxKey struct{}

// WithKey attaches an idempotency key to ctx.
func WithKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, ctxKey{}, key)
}

// KeyFromContext returns the idempotency key, or "" and false if none.
func KeyFromContext(ctx context.Context) (string, bool) {
	k, ok := ctx.Value(ctxKey{}).(string)
	return k, ok
}

// Extract reads the Idempotency-Key header into the request context.
func Extract(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get("Idempotency-Key"); key != "" {
			r = r.WithContext(WithKey(r.Context(), key))
		}
		next.ServeHTTP(w, r)
	})
}

type stored struct {
	status int
	body   []byte
}

// recorder tees a handler's response into a buffer while writing it through.
type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (rec *recorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(b []byte) (int, error) {
	rec.buf.Write(b)
	return rec.ResponseWriter.Write(b)
}

// Guard replays the first response seen for each idempotency key, executing the
// wrapped handler at most once per key.
type Guard struct {
	mu    sync.Mutex
	store map[string]stored
}

// NewGuard builds an empty guard.
func NewGuard() *Guard {
	return &Guard{store: make(map[string]stored)}
}

// Wrap returns a handler that enforces idempotency for keyed requests.
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := KeyFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		// Hold the lock across check-execute-store so concurrent identical
		// keys execute the side effect exactly once.
		g.mu.Lock()
		defer g.mu.Unlock()

		if prev, hit := g.store[key]; hit {
			w.WriteHeader(prev.status)
			_, _ = w.Write(prev.body)
			return
		}

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		g.store[key] = stored{status: rec.status, body: rec.buf.Bytes()}
	})
}
```

### The demo

The demo wires `Extract -> Guard.Wrap(handler)` where the handler increments a
counter and echoes it. Two requests with the same key run the handler once; a third
with a new key runs it again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/idempotency"
)

func main() {
	var executions int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executions++
		fmt.Fprintf(w, "charged #%d", executions)
	})

	guard := idempotency.NewGuard()
	chain := idempotency.Extract(guard.Wrap(handler))

	post := func(key string) {
		req := httptest.NewRequest(http.MethodPost, "/charge", nil)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		fmt.Printf("key=%s -> %s\n", key, rec.Body.String())
	}

	post("k1")
	post("k1")
	post("k2")
	fmt.Printf("total executions: %d\n", executions)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
key=k1 -> charged #1
key=k1 -> charged #1
key=k2 -> charged #2
total executions: 2
```

### The tests

The sequential test drives `k1`, `k1`, `k2`, and a keyless request, asserting the
execution counter and the replayed body at each step. The concurrency test fires
many goroutines with the same key at one guard and asserts, under `-race`, that the
handler ran exactly once.

Create `idempotency_test.go`:

```go
package idempotency

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func newChain(exec *int32) http.Handler {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(exec, 1)
		fmt.Fprintf(w, "exec-%d", n)
	})
	return Extract(NewGuard().Wrap(handler))
}

func post(chain http.Handler, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/charge", nil)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	return rec
}

func TestGuardReplaysAndReExecutes(t *testing.T) {
	t.Parallel()

	var exec int32
	chain := newChain(&exec)

	if got := post(chain, "k1").Body.String(); got != "exec-1" {
		t.Fatalf("first k1 body = %q, want exec-1", got)
	}
	if got := post(chain, "k1").Body.String(); got != "exec-1" {
		t.Fatalf("repeat k1 body = %q, want cached exec-1", got)
	}
	if got := atomic.LoadInt32(&exec); got != 1 {
		t.Fatalf("executions after two k1 = %d, want 1", got)
	}

	if got := post(chain, "k2").Body.String(); got != "exec-2" {
		t.Fatalf("k2 body = %q, want exec-2", got)
	}
	if got := atomic.LoadInt32(&exec); got != 2 {
		t.Fatalf("executions after k2 = %d, want 2", got)
	}
}

func TestNoKeyExecutesEveryTime(t *testing.T) {
	t.Parallel()

	var exec int32
	chain := newChain(&exec)

	post(chain, "")
	post(chain, "")

	if got := atomic.LoadInt32(&exec); got != 2 {
		t.Fatalf("keyless executions = %d, want 2", got)
	}
}

func TestConcurrentSameKeyExecutesOnce(t *testing.T) {
	t.Parallel()

	var exec int32
	chain := newChain(&exec)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(chain, "same")
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&exec); got != 1 {
		t.Fatalf("concurrent same-key executions = %d, want 1", got)
	}
}
```

## Review

The guard is correct when the execution counter increments once per distinct key
and never on a repeat, and when a keyless request always runs the handler. The
subtle correctness property is single-execution under concurrency: holding the mutex
across the check-execute-store section is what closes the "both goroutines see the
key absent" race, and the `-race` test with fifty identical-key requests is the
proof. The cost of that simplicity — serializing unrelated keys — is stated openly;
a production guard would key the lock so different idempotency keys proceed in
parallel, but the correctness argument is identical. Note the response recorder tees
rather than buffers-only, so the first caller still gets its live response while the
copy is cached for replays.

## Resources

- [net/http ResponseWriter](https://pkg.go.dev/net/http#ResponseWriter) — the interface the recorder wraps to capture a response.
- [Stripe API: Idempotent requests](https://docs.stripe.com/api/idempotent_requests) — the real-world contract this guard implements.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the check-execute-store critical section.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-tenant-scoped-repository.md](06-tenant-scoped-repository.md) | Next: [08-boundary-rule-refactor.md](08-boundary-rule-refactor.md)
