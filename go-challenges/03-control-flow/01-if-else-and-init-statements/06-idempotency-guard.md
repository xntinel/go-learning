# Exercise 6: Idempotency Guard: Replaying a Stored Response by Key

Payment and order APIs must be exactly-once: if a client retries a POST because the
first response was lost, the charge must not happen twice. The idempotency-key
pattern is the boundary that guarantees it, and it is built from two `if`s — a
guard-clause check that the key is present, and a comma-ok store lookup that replays
a stored response instead of re-running the side effect.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
idempotency/                independent module: example.com/idempotency
  go.mod                    go 1.26
  guard.go                  store, Guard(next) wrapper, response capture + replay
  cmd/
    demo/
      main.go               send the same keyed request twice, show single execution
  guard_test.go             missing key 400; execute-once; replay; concurrency -race
```

- Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
- Implement: a `Guard(next http.Handler) http.Handler` that requires `Idempotency-Key` on unsafe methods, replays a stored response on a key hit, and captures+stores the response on a miss; a mutex-guarded in-memory store.
- Test: missing key returns 400 and the handler is NOT invoked; first keyed call executes once and stores; identical repeat replays without re-invoking; different keys execute independently; concurrent duplicate keys execute the side effect at most once under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/idempotency/cmd/demo
cd ~/go-exercises/idempotency
go mod init example.com/idempotency
```

## Guard, look up, capture, replay — all under one lock

`Guard` wraps a handler and enforces exactly-once on unsafe methods (POST, PUT,
PATCH, DELETE). Safe methods (GET, HEAD) pass straight through — they have no side
effect to deduplicate. For an unsafe method the control flow is three decisions:

1. Guard clause on the key: `if key := r.Header.Get("Idempotency-Key"); key == "" { http.Error(..., 400); return }`.
   No key, no guarantee — reject before touching the handler.
2. Comma-ok store lookup: `if prev, ok := store.get(key); ok { replay prev; return }`.
   A hit means this exact operation already ran; replay the stored status and body
   and do not execute the handler again.
3. On a miss, run the handler while capturing its response into an
   `httptest.ResponseRecorder`, store the captured status+body under the key, then
   copy it to the real `ResponseWriter`.

The correctness subtlety is concurrency. Two duplicate requests can arrive at the
same instant. If the lookup and the "mark this key as in progress" are not in one
critical section, both see a miss and both execute the side effect — a double charge.
The fix is a per-key lock: the store reserves the key atomically, so the second
request blocks until the first finishes and then replays the stored response. This
module uses a single mutex plus a per-key `sync.Once`-style reservation to keep the
side effect at-most-once. The demo and tests prove an invocation counter never
exceeds one per key.

Create `guard.go`:

```go
package idempotency

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
)

type storedResponse struct {
	status int
	body   []byte
}

// store maps an idempotency key to a reservation. done is closed once the response
// is recorded, so a concurrent duplicate waits and then replays.
type store struct {
	mu    sync.Mutex
	items map[string]*reservation
}

type reservation struct {
	done chan struct{}
	resp storedResponse
}

func newStore() *store { return &store{items: make(map[string]*reservation)} }

// reserve returns (res, true) if the caller is the first to claim key and must
// execute; or (res, false) if another caller already claimed it and this one should
// wait on res.done then replay res.resp.
func (s *store) reserve(key string) (*reservation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.items[key]; ok {
		return r, false
	}
	r := &reservation{done: make(chan struct{})}
	s.items[key] = r
	return r, true
}

// Guard enforces exactly-once on unsafe methods keyed by Idempotency-Key.
func Guard(next http.Handler) http.Handler {
	s := newStore()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			http.Error(w, "missing Idempotency-Key", http.StatusBadRequest)
			return
		}

		res, first := s.reserve(key)
		if !first {
			<-res.done // wait for the original to finish
			writeStored(w, res.resp)
			return
		}

		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		res.resp = storedResponse{status: rec.Code, body: rec.Body.Bytes()}
		close(res.done)
		writeStored(w, res.resp)
	})
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

func writeStored(w http.ResponseWriter, resp storedResponse) {
	w.WriteHeader(resp.status)
	_, _ = w.Write(bytes.Clone(resp.body))
}
```

### The runnable demo

The demo mounts `Guard` on a handler that increments a counter and returns it, then
sends the same key twice and a different key once, showing the side effect ran only
once per key.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	"example.com/idempotency"
)

func main() {
	var charges atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := charges.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "charge #%d", n)
	})
	srv := httptest.NewServer(idempotency.Guard(handler))
	defer srv.Close()

	post := func(key string) string {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err.Error()
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	fmt.Println("key A first :", post("A"))
	fmt.Println("key A repeat:", post("A"))
	fmt.Println("key B first :", post("B"))
	fmt.Println("no key      :", post(""))
	fmt.Println("total charges:", charges.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
key A first : 201 charge #1
key A repeat: 201 charge #1
key B first : 201 charge #2
no key      : 400 missing Idempotency-Key
total charges: 2
```

### Tests

The tests assert the exactly-once contract with an invocation counter. A missing key
returns 400 and the counter stays 0. A first keyed call executes once; an identical
repeat replays without incrementing. Different keys execute independently. The
concurrency test fires many duplicate-key requests at once and asserts the side
effect ran exactly once under `-race`.

Create `guard_test.go`:

```go
package idempotency

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func counterHandler(calls *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "ok")
	})
}

func do(t *testing.T, srv *httptest.Server, method, key string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, srv.URL, nil)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMissingKeyRejectedWithoutInvoking(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := httptest.NewServer(Guard(counterHandler(&calls)))
	t.Cleanup(srv.Close)

	resp := do(t, srv, http.MethodPost, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if calls.Load() != 0 {
		t.Fatalf("handler invoked %d times, want 0", calls.Load())
	}
}

func TestFirstCallExecutesRepeatReplays(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := httptest.NewServer(Guard(counterHandler(&calls)))
	t.Cleanup(srv.Close)

	r1 := do(t, srv, http.MethodPost, "k1")
	r1.Body.Close()
	r2 := do(t, srv, http.MethodPost, "k1")
	r2.Body.Close()

	if calls.Load() != 1 {
		t.Fatalf("handler invoked %d times, want 1 (replay)", calls.Load())
	}
	if r1.StatusCode != http.StatusCreated || r2.StatusCode != http.StatusCreated {
		t.Fatalf("statuses = %d,%d, want 201,201", r1.StatusCode, r2.StatusCode)
	}
}

func TestDifferentKeysExecuteIndependently(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := httptest.NewServer(Guard(counterHandler(&calls)))
	t.Cleanup(srv.Close)

	do(t, srv, http.MethodPost, "a").Body.Close()
	do(t, srv, http.MethodPost, "b").Body.Close()
	if calls.Load() != 2 {
		t.Fatalf("handler invoked %d times, want 2", calls.Load())
	}
}

func TestConcurrentDuplicateKeyExecutesOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	srv := httptest.NewServer(Guard(counterHandler(&calls)))
	t.Cleanup(srv.Close)

	const n = 32
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := do(t, srv, http.MethodPost, "same")
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("handler invoked %d times, want 1 (at most once per key)", calls.Load())
	}
}
```

## Review

The guard is correct when a missing key is rejected before the handler runs, a key
hit replays the stored status and body without re-invoking, and — the hard part —
concurrent duplicates execute the side effect at most once. That last property comes
from reserving the key atomically inside the store's lock: the first caller executes
and closes `done`; every concurrent duplicate blocks on `done` and then replays. The
mistakes to avoid are doing the lookup and the execution in separate critical
sections (both duplicates execute), replaying a shared byte slice without cloning it
(a data race on the buffer), and deduplicating safe methods that have no side effect.
A production store persists in a database with a TTL; the in-memory version here has
the same control-flow shape.

## Resources

- [Stripe: Idempotent Requests](https://docs.stripe.com/api/idempotent_requests)
- [IETF draft: The Idempotency-Key HTTP Header Field](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/)
- [net/http/httptest.ResponseRecorder](https://pkg.go.dev/net/http/httptest#ResponseRecorder)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-retry-classifier.md](05-retry-classifier.md) | Next: [07-optimistic-lock-update.md](07-optimistic-lock-update.md)
