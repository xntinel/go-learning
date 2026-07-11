# Exercise 6: Idempotency Store: comma-ok vs Discarded ok on Map Lookup

An idempotency guard backed by an in-memory result cache keyed by request id.
Discarding the `ok` on a map read (`v, _ := store[key]`) collapses "absent" and
"present but stored as the zero value" into one indistinguishable state — which
for idempotency means replaying a request that never ran, or reprocessing one that
did. The fix is the comma-ok form and a branch on presence.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
idempotency/                    module: example.com/idempotency
  go.mod
  store.go                      Response (Stringer), Store, Get/Put/Once, Drain (channel comma-ok)
  cmd/
    demo/
      main.go                   Once replays a cached response, including an empty one
  store_test.go                 zero-value presence, replay, closed-channel drain, -race
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `Store` with `Get(key) (Response, bool)`, `Put`, and `Once(key, compute)` that replays a cached result; a `Drain` that reads a channel with `v, ok := <-ch`.
- Test: a stored zero-valued `Response` reports present (`ok==true`) while a missing key reports absent; `Once` replays an empty response without recomputing; `Drain` exits only on channel close; a `-race` concurrency test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/idempotency/cmd/demo
cd ~/go-exercises/idempotency
go mod init example.com/idempotency
```

### Why the ok is the whole signal

An idempotency guard answers one question: "have I already processed this request
id?" That is a *presence* question, and a map answers it with the second return
value. `v, ok := store[key]` gives you both the stored value and whether the key
existed. Drop the `ok` — `v, _ := store[key]` — and you get the zero `Response`
for a missing key *and* for a key whose stored value happens to be the zero
`Response`. Those are opposite facts. A request that legitimately produced an
empty body (a 204, an accepted-but-empty result) is stored as a near-zero value;
if your guard treats "zero value" as "never seen," it reprocesses a completed
request. Conversely, treating a missing key's zero value as a real cached result
replays a response for a request that never ran. Only `ok` separates the two, so
`Once` branches on it.

The same idiom governs a channel drain. `for { r, ok := <-ch; if !ok { break } }`
distinguishes a real received value (possibly the zero `Response`) from a closed
channel. Discarding `ok` on a receive makes a closed channel look like an endless
stream of zero values, and the drain loop never terminates.

Create `store.go`:

```go
package idempotency

import (
	"fmt"
	"sync"
)

// Response is a cached result. Its zero value is a valid, cacheable response
// (empty body, zero status), which is exactly why presence must be tracked with
// the comma-ok ok, not inferred from the value.
type Response struct {
	Status int
	Body   string
}

func (r Response) String() string { return fmt.Sprintf("%d %q", r.Status, r.Body) }

// Store is a concurrency-safe idempotency cache keyed by request id.
type Store struct {
	mu   sync.Mutex
	data map[string]Response
}

func NewStore() *Store {
	return &Store{data: make(map[string]Response)}
}

// Get returns the cached response and whether the key was present. The ok is the
// only reliable presence signal; the zero Response is a valid stored value.
func (s *Store) Get(key string) (Response, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data[key]
	return r, ok
}

// Put caches r under key.
func (s *Store) Put(key string, r Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = r
}

// Once returns the cached response for key if present (replayed == true),
// otherwise computes it, caches it, and returns it (replayed == false). The
// branch is on ok, so an empty cached response is replayed, not recomputed.
func (s *Store) Once(key string, compute func() Response) (resp Response, replayed bool) {
	if r, ok := s.Get(key); ok {
		return r, true
	}
	r := compute()
	s.Put(key, r)
	return r, false
}

// Drain reads every value from ch until it is closed, using the comma-ok receive
// to tell a real zero value apart from a closed channel.
func Drain(ch <-chan Response) []Response {
	var out []Response
	for {
		r, ok := <-ch
		if !ok {
			return out
		}
		out = append(out, r)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/idempotency"
)

func main() {
	store := idempotency.NewStore()

	created := func() idempotency.Response {
		return idempotency.Response{Status: 201, Body: "created"}
	}

	r1, replayed1 := store.Once("req-1", created)
	r2, replayed2 := store.Once("req-1", created)
	fmt.Printf("first:  %s replayed=%v\n", r1, replayed1)
	fmt.Printf("second: %s replayed=%v\n", r2, replayed2)

	// A legitimately empty response is still cached and replayed.
	empty := func() idempotency.Response { return idempotency.Response{Status: 204} }
	store.Once("req-2", empty)
	_, replayed3 := store.Once("req-2", empty)
	fmt.Printf("empty replayed=%v\n", replayed3)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first:  201 "created" replayed=false
second: 201 "created" replayed=true
empty replayed=true
```

### Tests

`TestZeroValuePresent` is the core proof: a stored zero `Response` reports present
while a missing key reports absent. A `v, _ := store[key]` implementation cannot
tell them apart and fails this. `TestDrainClosedChannel` proves the receive
comma-ok terminates only on close.

Create `store_test.go`:

```go
package idempotency

import (
	"fmt"
	"sync"
	"testing"
)

func TestZeroValuePresent(t *testing.T) {
	t.Parallel()

	s := NewStore()
	s.Put("stored-zero", Response{}) // a valid, all-zero cached response

	if _, ok := s.Get("stored-zero"); !ok {
		t.Fatal("stored zero-value response reported absent")
	}
	if _, ok := s.Get("never-seen"); ok {
		t.Fatal("missing key reported present")
	}
}

func TestOnceReplaysEmptyResponse(t *testing.T) {
	t.Parallel()

	s := NewStore()
	calls := 0
	compute := func() Response {
		calls++
		return Response{} // empty but legitimately cached
	}

	if _, replayed := s.Once("k", compute); replayed {
		t.Fatal("first Once must not be a replay")
	}
	if _, replayed := s.Once("k", compute); !replayed {
		t.Fatal("second Once must replay the cached empty response")
	}
	if calls != 1 {
		t.Fatalf("compute called %d times, want 1", calls)
	}
}

func TestDrainClosedChannel(t *testing.T) {
	t.Parallel()

	ch := make(chan Response, 2)
	ch <- Response{} // a real zero value, not a closed channel
	ch <- Response{Status: 200, Body: "ok"}
	close(ch)

	got := Drain(ch)
	if len(got) != 2 {
		t.Fatalf("Drain returned %d values, want 2", len(got))
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	s := NewStore()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k-%d", i)
			s.Put(key, Response{Status: 200})
			s.Get(key)
		}()
	}
	wg.Wait()
}

func ExampleStore_Get() {
	s := NewStore()
	s.Put("k", Response{})
	_, present := s.Get("k")
	_, missing := s.Get("absent")
	fmt.Println(present, missing)
	// Output: true false
}
```

## Review

The store is correct when presence is read from `ok`, never inferred from the
value. `TestZeroValuePresent` and `TestOnceReplaysEmptyResponse` both fail if the
implementation discards `ok`: a stored zero `Response` would look absent and get
recomputed. `TestDrainClosedChannel` proves the receive comma-ok terminates the
drain on close rather than spinning on zero values. The mistakes to avoid:
`v, _ := store[key]` treated as a hit, and a bare `<-ch` in a drain loop. Run
`go test -race` to confirm the mutex guards the map.

## Resources

- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — the comma-ok form of a map index.
- [Go Specification: Receive operator](https://go.dev/ref/spec#Receive_operator) — the comma-ok form of a channel receive.
- [Effective Go: Maps](https://go.dev/doc/effective_go#maps) — presence testing with the second value.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-config-loader-shadow.md](07-config-loader-shadow.md)
