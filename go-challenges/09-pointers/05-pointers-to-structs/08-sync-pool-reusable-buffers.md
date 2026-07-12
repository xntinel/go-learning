# Exercise 8: A sync.Pool Of *bytes.Buffer With Reset Discipline

Reusing `*bytes.Buffer` objects through a `sync.Pool` is a standard way to cut
allocations in a hot serialization path. The catch is that a pooled object arrives in
whatever state the last borrower left it, so the Reset discipline is not optional:
skip it and one response's bytes leak into the next. This exercise builds a JSON
encoder around a pool and proves both the correct isolation and the stale-state bug.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
poolencoder/              independent module: example.com/poolencoder
  go.mod
  encoder.go              Encoder over sync.Pool of *bytes.Buffer; Encode; encodeNoReset (buggy demo)
  cmd/
    demo/
      main.go             encode several values, show isolated output
  encoder_test.go         repeated encodes isolated; missing-Reset leaks; -race concurrency
```

Files: `encoder.go`, `cmd/demo/main.go`, `encoder_test.go`.
Implement: an `Encoder` holding a `sync.Pool` whose `New` returns `*bytes.Buffer`; `Encode(v any) ([]byte, error)` that borrows a buffer, resets it, writes JSON, copies the bytes out, and returns it to the pool; a buggy `encodeNoReset(buf, v)` that writes without resetting.
Test: repeated `Encode` calls produce correct isolated output; `encodeNoReset` leaks prior content (documents the hazard); many goroutines encoding under `-race` show no data race and no cross-contamination.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/05-pointers-to-structs/08-sync-pool-reusable-buffers/cmd/demo
cd go-solutions/09-pointers/05-pointers-to-structs/08-sync-pool-reusable-buffers
```

### The pooled-pointer lifecycle and why Reset and copy-out both matter

`sync.Pool` stores `any` values and hands them back to amortize allocation. Its `New`
func supplies a fresh object when the pool is empty; `Get` returns one (as `any`, so
you type-assert it back to `*bytes.Buffer`); `Put` returns it for reuse. The pool may
drop any object during GC, so it is only a cache, never a guarantee — every `Get` must
tolerate a fresh object and every borrow must be self-sufficient.

Two disciplines make pooled buffers safe. First, **Reset on borrow**: a buffer
returned by `Get` still holds the bytes the previous borrower wrote, because `Put`
does not clear it. Calling `buf.Reset()` truncates it to empty (keeping its allocated
capacity — that is the whole point) before you write. Skip it and the new response
starts with the old response's bytes prepended. Second, **copy out before Put**: the
bytes inside the buffer belong to the buffer; once you `Put` it back, another
goroutine may borrow it and overwrite those bytes. So you must copy what you need into
a fresh slice (`bytes.Clone(buf.Bytes())` or `append([]byte(nil), ...)`) *before*
returning the buffer, and return only the copy. Returning `buf.Bytes()` directly is a
use-after-free-style bug: the slice aliases pooled memory that is no longer yours.

`json.Marshal` allocates its own buffer internally, which would defeat the pool, so
the encoder uses `json.NewEncoder(buf).Encode(v)` to write directly into the pooled
buffer. `Encoder.Encode` clones the result out and returns the buffer via `defer`.
The buggy `encodeNoReset` writes into a buffer without resetting it, so reusing one
buffer across calls concatenates outputs — the hazard the test pins. It takes the
buffer as an argument only so the reuse is deterministic; a real pool would hand you
the same dirty buffer nondeterministically.

Create `encoder.go`:

```go
package poolencoder

import (
	"bytes"
	"encoding/json"
	"sync"
)

// Encoder serializes values to JSON, reusing *bytes.Buffer objects from a pool to
// avoid an allocation per call.
type Encoder struct {
	pool sync.Pool
}

func NewEncoder() *Encoder {
	return &Encoder{
		pool: sync.Pool{
			New: func() any { return new(bytes.Buffer) },
		},
	}
}

// Encode borrows a buffer, RESETS it, writes JSON, copies the bytes OUT, and returns
// the buffer to the pool. The returned slice is independent of pooled memory.
func (e *Encoder) Encode(v any) ([]byte, error) {
	buf := e.pool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		e.pool.Put(buf)
	}()

	buf.Reset() // scrub any prior borrower's bytes before writing
	if err := json.NewEncoder(buf).Encode(v); err != nil {
		return nil, err
	}
	// Copy out: the buffer's bytes are reused after Put, so we must not alias them.
	out := bytes.Clone(bytes.TrimRight(buf.Bytes(), "\n"))
	return out, nil
}

// encodeNoReset is the WRONG version: it writes into buf WITHOUT resetting it first,
// so reusing one buffer across calls concatenates the outputs. This is exactly what a
// pool borrow does when you forget Reset; the buffer is passed in explicitly here only
// to make the reuse deterministic for the test. Demonstration only.
func encodeNoReset(buf *bytes.Buffer, v any) ([]byte, error) {
	if err := json.NewEncoder(buf).Encode(v); err != nil {
		return nil, err
	}
	return bytes.Clone(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
```

### The runnable demo

The demo encodes three different values in a row through the same encoder, showing
each result is clean and isolated despite buffer reuse.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/poolencoder"
)

func main() {
	enc := poolencoder.NewEncoder()

	a, _ := enc.Encode(map[string]int{"a": 1})
	b, _ := enc.Encode(map[string]int{"b": 2})
	c, _ := enc.Encode([]string{"x", "y"})

	fmt.Printf("a=%s\n", a)
	fmt.Printf("b=%s\n", b)
	fmt.Printf("c=%s\n", c)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a={"a":1}
b={"b":2}
c=["x","y"]
```

### Tests

`TestEncodeIsIsolated` encodes several values through one encoder and asserts each
result is exactly the value's JSON with no leftover bytes. `TestNoResetLeaksContent`
forces buffer reuse and shows `encodeNoReset` concatenates the prior output — the
documented hazard. `TestConcurrentEncode` runs many goroutines through the pool under
`-race`, asserting every result is well-formed (no cross-contamination, no data race).

Create `encoder_test.go`:

```go
package poolencoder

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestEncodeIsIsolated(t *testing.T) {
	t.Parallel()
	enc := NewEncoder()

	got1, err := enc.Encode(map[string]int{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(got1) != `{"a":1}` {
		t.Fatalf("first = %s", got1)
	}

	// Second encode reuses the same pooled buffer; must not carry got1's bytes.
	got2, err := enc.Encode(map[string]int{"b": 2})
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != `{"b":2}` {
		t.Fatalf("second = %s (buffer not reset?)", got2)
	}
}

func TestNoResetLeaksContent(t *testing.T) {
	t.Parallel()
	// Reuse ONE buffer across two calls, as a pool borrow would, without Reset.
	buf := new(bytes.Buffer)

	if _, err := encodeNoReset(buf, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	// The buffer still holds {"a":1}; the second write appends to it.
	got, err := encodeNoReset(buf, map[string]int{"b": 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `{"a":1}`) {
		t.Fatalf("expected the no-reset bug to leak prior content; got %s", got)
	}
}

func TestConcurrentEncode(t *testing.T) {
	t.Parallel()
	enc := NewEncoder()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := enc.Encode(map[string]int{"n": i})
			if err != nil {
				t.Errorf("encode: %v", err)
				return
			}
			// Each result must be well-formed JSON for exactly its own value.
			if !strings.HasPrefix(string(got), `{"n":`) || !strings.HasSuffix(string(got), `}`) {
				t.Errorf("cross-contaminated output: %s", got)
			}
		}(i)
	}
	wg.Wait()
}
```

## Review

The encoder is correct when repeated calls produce each value's JSON with nothing
left over, and when concurrent callers never see each other's bytes. The isolation
test is the direct proof that Reset ran; the no-reset test pins the failure mode so
the discipline is visible, not folklore.

The mistakes: skipping `Reset`, which leaks the previous borrower's bytes into the
next response (a correctness and potentially a data-disclosure bug); returning
`buf.Bytes()` without copying, which aliases pooled memory that a later borrower
overwrites; and assuming the pool retains objects (it does not — GC may drop them, so
`New` must always be able to mint a fresh one). The `defer` runs `Reset` on return too,
so a buffer is clean both when borrowed and when returned. Run `go test -race` to
confirm no goroutine observes another's buffer.

## Resources

- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — `Get`, `Put`, the `New` func, and the "may be dropped by GC" contract.
- [`bytes.Buffer.Reset`](https://pkg.go.dev/bytes#Buffer.Reset) — truncating to empty while keeping the allocated capacity.
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone) — copying bytes out of a buffer before returning it to the pool.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-nil-receiver-safe-tree.md](09-nil-receiver-safe-tree.md)
