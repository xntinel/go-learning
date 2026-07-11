# Exercise 8: sync.Pool of *bytes.Buffer for a JSON Encoding Hot Path

The canonical production use of `new(T)` is the `New` field of a `sync.Pool`. This
exercise builds a JSON encoder that reuses `*bytes.Buffer` across calls via a
`sync.Pool{New: func() any { return new(bytes.Buffer) }}`, with the
Get/Reset/Put discipline that cuts per-call allocations under load, and tests both
correctness and the reset-before-reuse rule.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
bufpool/                      independent module: example.com/bufpool
  go.mod                      go 1.26
  encoder.go                  Encoder with sync.Pool of *bytes.Buffer; Encode, encodeInto (resets)
  cmd/
    demo/
      main.go                 runnable demo: encode several values, show reuse
  encoder_test.go             correctness (seq+concurrent) + fewer-allocs-than-naive + reset-before-reuse
```

Files: `encoder.go`, `cmd/demo/main.go`, `encoder_test.go`.
Implement: an `Encoder` whose `sync.Pool.New` is `func() any { return new(bytes.Buffer) }`,
with Get/Reset/Put around each encode; a naive per-call `new` path for comparison.
Test: output correctness across sequential and concurrent calls; `testing.AllocsPerRun`
shows the pooled path allocates fewer buffers than the naive one; a `-race`
concurrent test; a test proving a buffer is Reset before reuse.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bufpool/cmd/demo
cd ~/go-exercises/bufpool
go mod init example.com/bufpool
```

### Why pool, and why Reset

Encoding a value to JSON needs a scratch buffer. The naive version allocates one
per call: `buf := new(bytes.Buffer)` (or `&bytes.Buffer{}`), encode into it, return
the bytes. Under load that is one heap allocation per encode, all of it short-lived
garbage the GC must later reclaim. A `sync.Pool` amortizes it: the pool holds
idle buffers, `Get` returns a pooled one (or calls `New` to make a fresh
`new(bytes.Buffer)` when the pool is empty), and `Put` returns it for the next
call. Across a sustained load the per-call buffer allocation drops toward zero,
because the same handful of buffers cycle through.

Two disciplines are mandatory and both are tested here. First, **Reset before
reuse**: a pooled buffer still holds whatever the last user wrote, so the encoder
must call `buf.Reset()` before writing, or the previous response's bytes leak into
this one. Second, **the Put/Get pair does not correspond**: the pool may drop any
buffer at a GC, and `Get` may return `New`'s fresh buffer or some other returned
one — so code must never assume the buffer it `Put` is the one it later `Get`s, and
must never hold a reference to a buffer after `Put`ting it. The encoder copies the
bytes out (`slices.Clone` of `buf.Bytes()`) before `Put`, so the returned slice
does not alias a buffer that another goroutine may immediately reuse.

`encodeInto` is factored out precisely so the reset can be unit-tested
deterministically: it takes a buffer, resets it, and encodes into it, so a test can
hand it a dirty buffer and prove the output carries none of the old contents.

Create `encoder.go`:

```go
package bufpool

import (
	"bytes"
	"encoding/json"
	"slices"
	"sync"
)

// Encoder encodes values to JSON, reusing *bytes.Buffer via a sync.Pool to cut
// per-call allocations. Its zero value is not usable; construct with NewEncoder.
type Encoder struct {
	pool sync.Pool
}

// NewEncoder returns an Encoder whose pool mints fresh buffers with new(bytes.Buffer).
func NewEncoder() *Encoder {
	return &Encoder{
		pool: sync.Pool{
			New: func() any { return new(bytes.Buffer) },
		},
	}
}

// Encode marshals v to JSON using a pooled buffer. The returned bytes are a copy,
// so they do not alias the buffer once it is Put back for reuse.
func (e *Encoder) Encode(v any) ([]byte, error) {
	buf := e.pool.Get().(*bytes.Buffer)
	defer e.pool.Put(buf)

	if err := encodeInto(buf, v); err != nil {
		return nil, err
	}
	// json.Encoder writes a trailing newline; trim it, then copy out.
	out := bytes.TrimRight(buf.Bytes(), "\n")
	return slices.Clone(out), nil
}

// encodeInto resets buf (mandatory: a pooled buffer carries old contents) and
// encodes v into it. Factored out so the reset discipline is unit-testable.
func encodeInto(buf *bytes.Buffer, v any) error {
	buf.Reset()
	return json.NewEncoder(buf).Encode(v)
}

// EncodeNaive is the un-pooled baseline: it allocates a fresh buffer per call.
func EncodeNaive(v any) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := json.NewEncoder(buf).Encode(v); err != nil {
		return nil, err
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	return slices.Clone(out), nil
}
```

### The runnable demo

The demo encodes a few values through the pooled encoder, showing identical output
to what a fresh buffer would produce, with the buffers reused underneath.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bufpool"
)

type user struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func main() {
	enc := bufpool.NewEncoder()

	values := []any{
		user{Name: "alice", Age: 30},
		user{Name: "bob", Age: 41},
		map[string]int{"count": 7},
	}
	for _, v := range values {
		b, err := enc.Encode(v)
		if err != nil {
			panic(err)
		}
		fmt.Println(string(b))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"name":"alice","age":30}
{"name":"bob","age":41}
{"count":7}
```

### Tests

The tests cover correctness (sequential and concurrent), the allocation win, and
the reset discipline. `TestPooledAllocatesFewer` uses `testing.AllocsPerRun` to
compare the pooled path against the naive per-call `new` path. `TestEncodeInto...`
hands `encodeInto` a deliberately dirty buffer and proves the reset wipes it.
`TestConcurrentEncode` runs many encodes across goroutines under `-race`.

Create `encoder_test.go`:

```go
package bufpool

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

type payload struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestEncodeCorrect(t *testing.T) {
	t.Parallel()

	enc := NewEncoder()
	got, err := enc.Encode(payload{Name: "alice", Age: 30})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"alice","age":30}`
	if string(got) != want {
		t.Fatalf("Encode = %s, want %s", got, want)
	}
}

func TestPooledMatchesNaive(t *testing.T) {
	t.Parallel()

	enc := NewEncoder()
	v := payload{Name: "bob", Age: 41}
	pooled, err := enc.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	naive, err := EncodeNaive(v)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pooled, naive) {
		t.Fatalf("pooled %s != naive %s", pooled, naive)
	}
}

func TestPooledAllocatesFewer(t *testing.T) {
	// Not parallel: it measures allocations, which shared load would perturb.
	enc := NewEncoder()
	v := payload{Name: "carol", Age: 25}

	// Warm the pool so Get returns a reused buffer rather than New's fresh one.
	for range 100 {
		b, _ := enc.Encode(v)
		_ = b
	}

	pooled := testing.AllocsPerRun(1000, func() {
		b, _ := enc.Encode(v)
		_ = b
	})
	naive := testing.AllocsPerRun(1000, func() {
		b, _ := EncodeNaive(v)
		_ = b
	})

	if pooled >= naive {
		t.Fatalf("pooled allocs/op = %v, naive = %v; pooled should be fewer", pooled, naive)
	}
}

func TestEncodeIntoResetsDirtyBuffer(t *testing.T) {
	t.Parallel()

	buf := new(bytes.Buffer)
	buf.WriteString("LEFTOVER-GARBAGE")

	if err := encodeInto(buf, payload{Name: "x", Age: 1}); err != nil {
		t.Fatal(err)
	}
	got := bytes.TrimRight(buf.Bytes(), "\n")
	want := `{"name":"x","age":1}`
	if string(got) != want {
		t.Fatalf("encodeInto did not reset: got %s, want %s", got, want)
	}
	if bytes.Contains(got, []byte("LEFTOVER")) {
		t.Fatal("leftover contents survived reset")
	}
}

func TestConcurrentEncode(t *testing.T) {
	t.Parallel()

	enc := NewEncoder()
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := payload{Name: "u", Age: i}
			got, err := enc.Encode(v)
			if err != nil {
				t.Errorf("encode: %v", err)
				return
			}
			want := fmt.Sprintf(`{"name":"u","age":%d}`, i)
			if string(got) != want {
				t.Errorf("Encode = %s, want %s", got, want)
			}
		}()
	}
	wg.Wait()
}

func ExampleEncoder_Encode() {
	enc := NewEncoder()
	b, _ := enc.Encode(map[string]string{"status": "ok"})
	fmt.Println(string(b))
	// Output: {"status":"ok"}
}
```

## Review

The encoder is correct when its output matches a fresh-buffer encode byte for byte
(`TestPooledMatchesNaive`) and stays correct under concurrent load with `-race`
clean (`TestConcurrentEncode`). The allocation win is real but only after the pool
is warm, which is why `TestPooledAllocatesFewer` primes it with a hundred encodes
before measuring; a cold pool's first `Get` calls `New` and allocates. The two
disciplines are the whole point: `encodeInto`'s `buf.Reset()` is what
`TestEncodeIntoResetsDirtyBuffer` guards — drop it and a pooled buffer leaks the
previous response into this one — and copying the bytes out with `slices.Clone`
before `Put` is what makes the returned slice safe once another goroutine reuses
the buffer. Never hold a reference to a buffer after `Put`, and never assume a
`Put`/`Get` pair is the same object.

## Resources

- [sync.Pool](https://pkg.go.dev/sync#Pool) — the pool, its `New` field, and the "may be removed at any time" contract.
- [bytes.Buffer.Reset](https://pkg.go.dev/bytes#Buffer.Reset) — why reset-before-reuse is mandatory.
- [Go blog: A Guide to the Go Garbage Collector](https://go.dev/doc/gc-guide) — why cutting per-call allocations reduces GC pressure.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-defaults-copy-vs-alias.md](09-defaults-copy-vs-alias.md)
