# Exercise 4: sync.Pool: Reusing Buffers in a JSON Response Encoder

An HTTP handler that JSON-encodes a response allocates a fresh `bytes.Buffer` per
request — an escape you cannot remove, because the buffer must outlive the encode
call. `sync.Pool` does not remove it; it *amortizes* it, recycling buffers across
requests. This module builds a pooled JSON encoder with the correct Reset/Put
discipline and proves it neither corrupts data nor races.

This module is fully self-contained.

## What you'll build

```text
jsonpool/                     independent module: example.com/jsonpool
  go.mod                      go 1.26
  encoder.go                  Encoder with sync.Pool of *bytes.Buffer;
                              Marshal (copies out), WriteJSON, HealthHandler;
                              MarshalNaive for comparison; ErrEncode sentinel
  cmd/
    demo/
      main.go                 marshals a value and drives the handler via httptest
  encoder_test.go             round-trip, error wrapping, handler, -race pool hammer
```

Files: `encoder.go`, `cmd/demo/main.go`, `encoder_test.go`.
Implement: an `Encoder` holding a `sync.Pool`, with `Marshal` (Reset on get, Put
on defer, copy bytes out before returning), `WriteJSON`, and a `HealthHandler`;
plus `MarshalNaive` and a wrapped `ErrEncode` sentinel.
Test: JSON round-trip, `errors.Is(err, ErrEncode)` on an unencodable value, an
`httptest` handler assertion, a concurrent `-race` pool hammer, and an
`AllocsPerRun` guard that the pooled path allocates fewer than the naive one.
Verify: `go test -count=1 -race ./...`, then `go test -bench=. -benchmem ./...`.

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/07-escape-analysis/04-sync-pool-buffer-reuse/cmd/demo
cd go-solutions/09-pointers/07-escape-analysis/04-sync-pool-buffer-reuse
```

### The escape sync.Pool amortizes, and the bug it invites

The buffer a JSON encoder writes into must survive the `Encode` call, so it is a
genuine heap escape: there is no way to keep it on the stack. `MarshalNaive`
allocates one every call. `sync.Pool` lets you recycle a small set of buffers
across many requests, so the allocation and its GC cost are paid a handful of
times and reused, not paid per request. Under load, that is the difference between
a steady stream of garbage and almost none.

The discipline is exact and unforgiving. On `Get` you receive a buffer with
*arbitrary previous contents*, so you must `Reset` it before use. On the way out
you must `Put` it back — `defer pool.Put(buf)` is the clean way — so it returns to
circulation. And the classic corruption bug: never let a reference to the pooled
buffer's backing bytes escape past the `Put`. `bytes.Buffer.Bytes()` returns a
slice aliasing the buffer's internal array; if you return that slice to a caller
and then `Put` the buffer, another goroutine can `Get` the same buffer and
overwrite those bytes underneath your caller. It is a data race and a
data-corruption bug at once, and it only shows up under concurrency.

`Marshal` sidesteps it by copying: it `make`s a fresh slice the size of the
encoded output and `copy`s the bytes into it before the deferred `Put` runs, so
the returned slice owns its memory. `WriteJSON` sidesteps it differently — it
writes `buf.Bytes()` straight to the `http.ResponseWriter` *before* returning, so
the bytes are consumed while the buffer is still exclusively ours; nothing pooled
outlives the call. Both are correct; the wrong version is the one that returns
`buf.Bytes()` and then `Put`s.

Errors are wrapped so callers can classify them: an encode failure returns
`ErrEncode` wrapped with `%w`, checkable via `errors.Is`.

Create `encoder.go`:

```go
package jsonpool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// ErrEncode wraps any JSON encoding failure so callers can classify it.
var ErrEncode = errors.New("jsonpool: encode failed")

// Encoder marshals values to JSON reusing buffers from a pool.
type Encoder struct {
	pool sync.Pool
}

// NewEncoder returns an Encoder whose pool mints empty buffers on demand.
func NewEncoder() *Encoder {
	return &Encoder{
		pool: sync.Pool{New: func() any { return new(bytes.Buffer) }},
	}
}

// Marshal encodes v using a pooled buffer and returns an INDEPENDENT copy of the
// bytes. The copy is made before the buffer is returned to the pool, so the
// caller's slice never aliases recycled memory.
func (e *Encoder) Marshal(v any) ([]byte, error) {
	buf := e.pool.Get().(*bytes.Buffer)
	buf.Reset()
	defer e.pool.Put(buf)
	if err := json.NewEncoder(buf).Encode(v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncode, err)
	}
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

// WriteJSON encodes v into a pooled buffer and flushes it to w BEFORE the buffer
// returns to the pool, so no pooled memory outlives the call.
func (e *Encoder) WriteJSON(w http.ResponseWriter, status int, v any) error {
	buf := e.pool.Get().(*bytes.Buffer)
	buf.Reset()
	defer e.pool.Put(buf)
	if err := json.NewEncoder(buf).Encode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrEncode, err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

type health struct {
	Status string `json:"status"`
	Uptime int    `json:"uptime_seconds"`
}

// HealthHandler serves a JSON health document using the pooled encoder.
func (e *Encoder) HealthHandler(uptime int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := e.WriteJSON(w, http.StatusOK, health{Status: "ok", Uptime: uptime}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

// MarshalNaive allocates a fresh buffer every call. Kept for comparison.
func MarshalNaive(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncode, err)
	}
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}
```

### The runnable demo

The demo marshals a value directly, then drives the health handler through
`httptest` so you see a real request/response without opening a socket. Note the
trailing newline: `json.Encoder.Encode` appends one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http/httptest"

	"example.com/jsonpool"
)

type user struct {
	Name string `json:"name"`
	ID   int    `json:"id"`
}

func main() {
	enc := jsonpool.NewEncoder()

	b, _ := enc.Marshal(user{Name: "alice", ID: 7})
	fmt.Printf("marshal: %s", b)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	enc.HealthHandler(42).ServeHTTP(rec, req)

	fmt.Printf("status: %d\n", rec.Code)
	fmt.Printf("ctype: %s\n", rec.Header().Get("Content-Type"))
	fmt.Printf("body: %s", rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
marshal: {"name":"alice","id":7}
status: 200
ctype: application/json
body: {"status":"ok","uptime_seconds":42}
```

### Tests

`TestMarshalRoundTrip` proves the copy is a faithful encoding.
`TestMarshalWrapsErr` feeds an unencodable value (a channel) and asserts the error
is classifiable with `errors.Is(err, ErrEncode)`. `TestHandler` drives the handler
through `httptest`. `TestPoolConcurrentNoCorruption` is the safety proof: many
goroutines marshal distinct values at once, and each result must decode back to
its own input — if the copy-before-`Put` discipline were broken, results would
bleed into each other, and `-race` would flag it. `TestPooledAllocatesLess`
asserts the pooled path allocates fewer times than the naive one.

Create `encoder_test.go`:

```go
package jsonpool

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
)

type payload struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func TestMarshalRoundTrip(t *testing.T) {
	t.Parallel()
	enc := NewEncoder()
	b, err := enc.Marshal(payload{ID: 7, Name: "alice"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got payload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != 7 || got.Name != "alice" {
		t.Errorf("round-trip = %+v, want {7 alice}", got)
	}
}

func TestMarshalWrapsErr(t *testing.T) {
	t.Parallel()
	enc := NewEncoder()
	_, err := enc.Marshal(make(chan int)) // channels cannot be encoded
	if !errors.Is(err, ErrEncode) {
		t.Fatalf("err = %v, want wrapped ErrEncode", err)
	}
}

func TestHandler(t *testing.T) {
	t.Parallel()
	enc := NewEncoder()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	enc.HealthHandler(99).ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if want := `{"status":"ok","uptime_seconds":99}` + "\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestPoolConcurrentNoCorruption(t *testing.T) {
	t.Parallel()
	enc := NewEncoder()
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := enc.Marshal(payload{ID: i, Name: fmt.Sprintf("u%d", i)})
			if err != nil {
				t.Errorf("Marshal: %v", err)
				return
			}
			var got payload
			if err := json.Unmarshal(b, &got); err != nil {
				t.Errorf("Unmarshal: %v", err)
				return
			}
			if got.ID != i {
				t.Errorf("corruption: got ID %d, want %d", got.ID, i)
			}
		}()
	}
	wg.Wait()
}

var sink []byte

func TestPooledAllocatesLess(t *testing.T) {
	enc := NewEncoder()
	v := payload{ID: 7, Name: "alice"}
	naive := testing.AllocsPerRun(1000, func() {
		b, _ := MarshalNaive(v)
		sink = b
	})
	pooled := testing.AllocsPerRun(1000, func() {
		b, _ := enc.Marshal(v)
		sink = b
	})
	if !(pooled < naive) {
		t.Errorf("pooled should allocate less: pooled=%.1f naive=%.1f", pooled, naive)
	}
}

func BenchmarkMarshalNaive(b *testing.B) {
	v := payload{ID: 7, Name: "alice"}
	b.ReportAllocs()
	for b.Loop() {
		out, _ := MarshalNaive(v)
		sink = out
	}
}

func BenchmarkMarshalPooled(b *testing.B) {
	enc := NewEncoder()
	v := payload{ID: 7, Name: "alice"}
	b.ReportAllocs()
	for b.Loop() {
		out, _ := enc.Marshal(v)
		sink = out
	}
}

func ExampleEncoder_Marshal() {
	enc := NewEncoder()
	b, _ := enc.Marshal(payload{ID: 1, Name: "root"})
	fmt.Printf("%s", b)
	// Output: {"id":1,"name":"root"}
}
```

## Review

The encoder is correct when three things hold: `Reset` runs on every `Get` (so a
recycled buffer never leaks a previous response), the returned slice owns its
bytes (so `Put` cannot corrupt it), and errors are wrapped so
`errors.Is(err, ErrEncode)` classifies them. `TestPoolConcurrentNoCorruption`
under `-race` is the real proof of the second point — it is the test that would
fail loudly if you returned `buf.Bytes()` and then `Put`. The benchmark shows the
payoff: the pooled path allocates fewer times per op than the naive one because it
reuses the backing array instead of minting a new one each request. The mistake to
avoid is treating `sync.Pool` as a place to stash long-lived objects or as a
cache; it is a recycler for short-lived, per-call scratch that must be reset on
get and never referenced after put.

## Resources

- [sync.Pool](https://pkg.go.dev/sync#Pool) — `Get`, `Put`, `New`, and the reset discipline.
- [bytes.Buffer](https://pkg.go.dev/bytes#Buffer) — `Reset`, `Bytes`, and the aliasing caveat.
- [encoding/json.Encoder](https://pkg.go.dev/encoding/json#Encoder) — streaming encode and the trailing newline.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-interface-boxing-in-hot-logger.md](03-interface-boxing-in-hot-logger.md) | Next: [05-slice-preallocation-and-growth.md](05-slice-preallocation-and-growth.md)
