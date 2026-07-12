# Exercise 8: Encoding Hot Path — Reusing *bytes.Buffer via sync.Pool

A JSON response encoder on a hot path allocates a fresh buffer per request unless
you pool them. This exercise builds a pooled encoder the right way: the pool stores
`*bytes.Buffer` (a pointer, so Get/Put move no copy), each checkout does
`Reset()` before writing, and `Put` is deferred. It shows why the pool holds
pointers, not buffer values, and why Reset-on-checkout is the discipline that keeps
one request's bytes out of the next response.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
encoder/                    independent module: example.com/encoder
  go.mod
  encoder.go                bufPool of *bytes.Buffer; Encode(v any) ([]byte, error) with Reset + defer Put
  cmd/
    demo/
      main.go               encodes a few values through the pooled path
  encoder_test.go           correct across reuse cycles; Reset clears stale bytes; concurrent -race; benchmark
```

- Files: `encoder.go`, `cmd/demo/main.go`, `encoder_test.go`.
- Implement: a `sync.Pool` whose `New` returns `*bytes.Buffer`, and an `Encode` that Gets, Resets, encodes, copies out, and defers Put.
- Test: encoding is correct across many reuse cycles, Reset-on-checkout clears stale bytes, concurrent encoding is race-free, and a benchmark with `b.ReportAllocs()`/`b.Loop()` shows the pooled path allocates less.
- Verify: `go test -count=1 -race ./...`

### Why the pool holds pointers and Reset matters

`sync.Pool` stores `any`. If you pooled `bytes.Buffer` *values*, every `Put(buf)`
would box the value into an interface — a heap allocation — which defeats the whole
purpose of pooling. So the pool's `New` returns `*bytes.Buffer` and `Get`/`Put` move
a single pointer with no copy. `new(bytes.Buffer)` yields a ready-to-use zero buffer;
the pointer is what circulates.

Reset-on-checkout is the second non-negotiable. A buffer coming out of the pool still
holds whatever the previous user wrote. If `Encode` skipped `Reset()`, request N's
JSON would be prepended with request N-1's bytes — a data-leak and a correctness bug.
So `Encode` calls `buf.Reset()` immediately after `Get`, before writing anything. The
final subtlety: `Encode` must not return `buf.Bytes()` directly, because after `Put`
the buffer may be reused and its backing array overwritten. It copies the bytes into
a fresh slice and returns that, so the caller owns stable data and the buffer is safe
to recycle.

Create `encoder.go`:

```go
package encoder

import (
	"bytes"
	"encoding/json"
	"sync"
)

// bufPool recycles *bytes.Buffer. Storing a pointer (not a value) means Get/Put
// move one word and never box or copy the buffer.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// Encode marshals v to JSON using a pooled buffer. It Resets the buffer on
// checkout, returns a freshly-copied []byte, and defers Put so the buffer is
// always recycled. json.Encoder appends a trailing newline.
func Encode(v any) ([]byte, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset() // discard any bytes the previous user left behind
	defer bufPool.Put(buf)

	if err := json.NewEncoder(buf).Encode(v); err != nil {
		return nil, err
	}
	// Copy out: after Put the buffer's backing array may be reused.
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

// encodeInto writes v as JSON into dst. The fresh-allocation benchmark uses it so
// both benchmarks share identical encoding work and differ only in pooled vs new.
func encodeInto(dst *bytes.Buffer, v any) error {
	return json.NewEncoder(dst).Encode(v)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/encoder"
)

func main() {
	type resp struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}
	for _, r := range []resp{{true, "ada"}, {false, "bob"}} {
		b, err := encoder.Encode(r)
		if err != nil {
			panic(err)
		}
		fmt.Print(string(b)) // Encode leaves the trailing newline
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"ok":true,"name":"ada"}
{"ok":false,"name":"bob"}
```

### Tests

The tests pin correctness across reuse, prove the Reset discipline on a dedicated
pool (so it is deterministic and isolated from other tests), and exercise concurrent
encoding under `-race`. The benchmark contrasts the pooled path against allocating a
fresh buffer per call.

Create `encoder_test.go`:

```go
package encoder

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestEncodeCorrectAcrossReuse(t *testing.T) {
	t.Parallel()
	// Many cycles through the shared pool: a leaked or unreset buffer would show
	// up as trailing garbage on some iteration.
	for range 1000 {
		b, err := Encode(map[string]int{"a": 1})
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if got := strings.TrimSpace(string(b)); got != `{"a":1}` {
			t.Fatalf("Encode = %q, want {\"a\":1}", got)
		}
	}
}

func TestResetPreventsLeak(t *testing.T) {
	t.Parallel()
	// A dedicated pool makes the checkout deterministic: the buffer we Put is the
	// one we Get back, so we can prove Reset clears stale bytes.
	pool := &sync.Pool{New: func() any { return new(bytes.Buffer) }}

	dirty := new(bytes.Buffer)
	dirty.WriteString("STALE")
	pool.Put(dirty)

	buf := pool.Get().(*bytes.Buffer)
	buf.Reset() // the checkout discipline Encode performs
	buf.WriteString("fresh")
	if strings.Contains(buf.String(), "STALE") {
		t.Fatalf("Reset did not clear stale bytes: %q", buf.String())
	}
}

func TestConcurrentEncode(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				b, err := Encode([]int{1, 2, 3})
				if err != nil {
					t.Errorf("Encode: %v", err)
					return
				}
				if got := strings.TrimSpace(string(b)); got != "[1,2,3]" {
					t.Errorf("Encode = %q, want [1,2,3]", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func BenchmarkEncodePooled(b *testing.B) {
	b.ReportAllocs()
	v := map[string]int{"a": 1, "b": 2, "c": 3}
	for b.Loop() {
		if _, err := Encode(v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeFresh(b *testing.B) {
	b.ReportAllocs()
	v := map[string]int{"a": 1, "b": 2, "c": 3}
	for b.Loop() {
		var buf bytes.Buffer
		if err := encodeInto(&buf, v); err != nil {
			b.Fatal(err)
		}
		_ = buf.Bytes()
	}
}
```

`BenchmarkEncodeFresh` calls `encodeInto`, the helper added to `encoder.go` above,
so the only difference between the two benchmarks is pooled-vs-fresh, not the
encoding itself.

Run the benchmarks to see the allocation difference:

```bash
go test -bench=. -benchmem -run=^$
```

The pooled path reports fewer allocations per operation (`B/op` and `allocs/op`)
because it reuses the buffer's backing array instead of allocating a new one each
call; exact `ns/op` varies by machine.

## Review

The encoder is correct when repeated reuse never leaks bytes and never races. The
Reset test is deliberately built on its own pool so it is deterministic: on a shared
pool you cannot guarantee which buffer `Get` returns, so the assertion would be
flaky. The `copy`-out is not optional — returning `buf.Bytes()` after `defer Put`
would hand the caller a slice whose backing array the next request can overwrite,
the subtlest bug in this whole lesson. Two disciplines carry the pattern everywhere:
pool pointers (never values, or you box and re-allocate on every `Put`), and Reset on
checkout (or you leak the previous payload). Run `go test -race`; the pool is
concurrency-safe by design, and the concurrent test confirms your usage is too.

## Resources

- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — Get/Put semantics and the note that pooled objects may be dropped at any time.
- [`bytes.Buffer`](https://pkg.go.dev/bytes#Buffer) — `Reset`, `Bytes`, and why the backing array is reused.
- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop used here with `ReportAllocs`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-nil-receiver-optional-dependency.md](07-nil-receiver-optional-dependency.md) | Next: [09-large-struct-copy-cost-benchmark.md](09-large-struct-copy-cost-benchmark.md)
