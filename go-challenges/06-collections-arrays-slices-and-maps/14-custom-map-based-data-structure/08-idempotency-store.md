# Exercise 8: HTTP idempotency-key guard for a POST write path

A client that times out on a `POST /charge` will retry — and a load balancer might
retry on its own. Without a guard, the payment is charged twice. The standard
defense is an idempotency key: the client sends a unique `Idempotency-Key` header,
the server executes the write once, stores the response under that key, and
replays the stored response for any retry. This module builds that guard as HTTP
middleware, including the subtle part: serializing *concurrent* duplicates so the
handler still runs exactly once.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
idempotency/               independent module: example.com/idempotency
  go.mod
  idempotency.go           type Store; New, Middleware (captures + replays)
  cmd/
    demo/
      main.go              a payment handler wrapped by the guard
  idempotency_test.go      once-per-key, distinct keys, concurrent duplicates
```

- Files: `idempotency.go`, `cmd/demo/main.go`, `idempotency_test.go`.
- Implement: a `Store` with `map[string]*entry` under a `sync.Mutex`, keyed by the `Idempotency-Key` header, storing the first response; a per-key TTL; and an in-flight marker (a channel) that serializes concurrent duplicates.
- Test: two identical requests execute the handler once and return identical bodies; distinct keys execute independently; concurrent duplicates still execute once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/08-idempotency-store/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/08-idempotency-store
```

### Capture the first response, and serialize the duplicates

The guard is middleware wrapping the real handler. On a request with an
`Idempotency-Key`, it looks the key up in a `map[string]*entry` under a mutex.
Three cases:

- **First time (or the stored entry has expired):** create an `entry` whose
  `ready` channel is open, store it while still holding the lock (this is the
  in-flight marker), release the lock, then run the real handler against a
  *capturing* `ResponseWriter` that buffers the status, headers, and body instead
  of writing them to the network. Once the handler returns, copy the captured
  response into the entry, `close(ready)` to publish it, and replay it to the
  client.
- **Retry after completion:** the entry exists and its `ready` channel is already
  closed; replay the stored response without touching the handler.
- **Concurrent duplicate (still in flight):** the entry exists but `ready` is not
  yet closed. Block on `<-ready`, and once it closes, replay the stored response.
  The handler runs exactly once; the duplicate waits for it and copies its result.

The in-flight channel is what makes concurrent safety work. Two goroutines
arriving with the same key at the same instant serialize on the mutex; the first
wins, stores the entry, and starts the handler; the second finds the in-flight
entry and parks on `<-ready`. Publishing the captured fields *before* `close(ready)`
and reading them only *after* `<-ready` gives a happens-before edge, so the read
is race-free without holding the lock across the whole handler (which would
serialize *unrelated* keys and defeat the point). Requests with no key pass
straight through — idempotency is opt-in per the client's key.

Create `idempotency.go`:

```go
package idempotency

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

// entry is one in-flight-or-completed idempotent request. ready is closed once
// the captured response has been published.
type entry struct {
	ready   chan struct{}
	status  int
	body    []byte
	header  http.Header
	expires time.Time
}

// Store deduplicates write requests by their Idempotency-Key header.
type Store struct {
	mu      sync.Mutex
	entries map[string]*entry
	ttl     time.Duration
	now     func() time.Time
}

// New returns a Store whose stored responses live for ttl.
func New(ttl time.Duration) *Store {
	return &Store{
		entries: make(map[string]*entry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// Middleware wraps next, replaying the first response for any repeat of the same
// Idempotency-Key. Requests without the header pass straight through.
func (s *Store) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}

		s.mu.Lock()
		if e, ok := s.entries[key]; ok && s.now().Before(e.expires) {
			s.mu.Unlock()
			<-e.ready // wait for the first request to finish
			replay(w, e)
			return
		}
		e := &entry{ready: make(chan struct{}), expires: s.now().Add(s.ttl)}
		s.entries[key] = e // in-flight marker, published under the lock
		s.mu.Unlock()

		rec := newCapture()
		next.ServeHTTP(rec, r)

		e.status = rec.status
		e.body = rec.body.Bytes()
		e.header = rec.header.Clone()
		close(e.ready) // publish the captured response

		replay(w, e)
	})
}

func replay(w http.ResponseWriter, e *entry) {
	for k, vs := range e.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}

// capture is a ResponseWriter that buffers the handler's output so it can be
// stored and replayed.
type capture struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	wroteHeader bool
}

func newCapture() *capture {
	return &capture{header: make(http.Header), status: http.StatusOK}
}

func (c *capture) Header() http.Header { return c.header }

func (c *capture) WriteHeader(status int) {
	if !c.wroteHeader {
		c.status = status
		c.wroteHeader = true
	}
}

func (c *capture) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	return c.body.Write(b)
}
```

### The runnable demo

The demo wraps a payment handler that "charges" on every real execution. Two
requests with the same key produce one charge (the retry replays); a third with a
different key produces a second charge.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"example.com/idempotency"
)

func main() {
	var charges atomic.Int64
	pay := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := charges.Add(1) // a real charge to a payment processor
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "charge #%d created", n)
	})

	h := idempotency.New(time.Minute).Middleware(pay)

	send := func(key string) string {
		req := httptest.NewRequest(http.MethodPost, "/pay", nil)
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Result().Body)
		return string(body)
	}

	fmt.Printf("first: %s\n", send("pay-42"))
	fmt.Printf("retry: %s\n", send("pay-42")) // same key: replays, no new charge
	fmt.Printf("other: %s\n", send("pay-99")) // different key: new charge
	fmt.Printf("total charges: %d\n", charges.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first: charge #1 created
retry: charge #1 created
other: charge #2 created
total charges: 2
```

### Tests

The handler counts its executions and encodes the count in the body, so a double
execution is detectable both by the counter and by mismatched bodies. The
concurrency test fires many identical requests at once; under `-race` it proves
the store's map and the in-flight handoff are correctly synchronized, and the
counter proves the handler ran exactly once.

Create `idempotency_test.go`:

```go
package idempotency

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func countingHandler(calls *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "created-%d", n)
	})
}

func do(h http.Handler, key string) (int, string) {
	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

func TestRetriedRequestExecutesOnce(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	h := New(time.Minute).Middleware(countingHandler(&calls))

	code1, body1 := do(h, "key-1")
	code2, body2 := do(h, "key-1")

	if calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", calls.Load())
	}
	if code1 != http.StatusCreated || code2 != http.StatusCreated {
		t.Fatalf("codes = %d,%d, want 201,201", code1, code2)
	}
	if body1 != body2 || body1 != "created-1" {
		t.Fatalf("bodies = %q,%q, want both created-1", body1, body2)
	}
}

func TestDistinctKeysExecuteIndependently(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	h := New(time.Minute).Middleware(countingHandler(&calls))

	do(h, "key-a")
	do(h, "key-b")

	if calls.Load() != 2 {
		t.Fatalf("handler ran %d times, want 2", calls.Load())
	}
}

func TestNoKeyAlwaysExecutes(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	h := New(time.Minute).Middleware(countingHandler(&calls))

	do(h, "")
	do(h, "")

	if calls.Load() != 2 {
		t.Fatalf("handler ran %d times without a key, want 2", calls.Load())
	}
}

func TestConcurrentDuplicatesExecuteOnce(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	h := New(time.Minute).Middleware(countingHandler(&calls))

	const n = 20
	var wg sync.WaitGroup
	bodies := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, bodies[i] = do(h, "same-key")
		}()
	}
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("handler ran %d times under concurrent duplicates, want 1", calls.Load())
	}
	for i := range n {
		if bodies[i] != "created-1" {
			t.Fatalf("body[%d] = %q, want created-1", i, bodies[i])
		}
	}
}
```

## Review

The guard is correct when the handler executes exactly once per key no matter how
many times a request is repeated — sequentially or concurrently — and every repeat
returns the identical stored response. The mistakes to avoid are holding the mutex
across the whole handler (correct, but it serializes unrelated keys and kills
throughput — the in-flight channel lets you release the lock and still serialize
only the duplicates) and reading the captured response without the happens-before
edge that `close(ready)`/`<-ready` provides (a data race the `-race` detector would
flag). The per-key TTL bounds how long a result is remembered so the store does
not grow without bound. Run `go test -count=1 -race ./...`.

## Resources

- [`net/http` package](https://pkg.go.dev/net/http) — `Handler`, `ResponseWriter`, and `Request.Header.Get`.
- [`net/http/httptest` package](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for driving the middleware in tests.
- [Stripe: Idempotent requests](https://docs.stripe.com/api/idempotent_requests) — the real-world contract this middleware implements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-secondary-index-multimap.md](07-secondary-index-multimap.md) | Next: [09-topk-heavy-hitters.md](09-topk-heavy-hitters.md)
