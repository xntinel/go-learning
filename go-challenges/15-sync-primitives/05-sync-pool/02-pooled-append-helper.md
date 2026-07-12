# Exercise 2: A Pool-Backed String Builder the Caller Never Sees

The cleanest way to use a pool is to make it invisible. This module builds the
canonical `Append` helper: it borrows a buffer, formats into it, returns a plain
`string`, and returns the buffer to the pool with a `defer` — so the caller gets
a string and never learns a pool exists. Encapsulating the pool this way is what
lets you introduce (or remove) pooling as an internal optimization without
touching a single call site.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
pooledappend/               independent module: example.com/pooledappend
  go.mod                    go 1.26
  buffers/
    pool.go                 the typed *bytes.Buffer pool (bundled)
    format.go               Append(p, prefix, s) string — pool hidden from caller
  cmd/
    demo/
      main.go               formats a few messages via Append and prints them
  buffers/format_test.go    Append output; defer-Put returns the buffer; Example
```

Files: `buffers/pool.go`, `buffers/format.go`, `cmd/demo/main.go`, `buffers/format_test.go`.
Implement: `Append(p *Pool, prefix, s string) string` that gets a buffer, `defer`s the `Put`, formats with `fmt.Fprintf`, and returns `buf.String()`.
Test: `Append(p, "msg:", "hi") == "msg:hi"`; an `Example` with `// Output:`; confirm the buffer is returned to the pool on the happy path.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/05-sync-pool/02-pooled-append-helper/buffers go-solutions/15-sync-primitives/05-sync-pool/02-pooled-append-helper/cmd/demo
cd go-solutions/15-sync-primitives/05-sync-pool/02-pooled-append-helper
```

### The encapsulation contract

`Append` is three lines of real logic wrapped around one design decision: the
pool is an implementation detail, and the API surface is a value type. The caller
passes strings and receives a string. Whether that string was built with a
pooled buffer, a fresh `bytes.Buffer`, or `+` concatenation is entirely internal
— which means you can profile the path, decide pooling is worth it, and add it
without changing the signature or any caller.

The `defer p.Put(buf)` is the load-bearing detail. It guarantees the buffer
returns to the pool on *every* exit path, including a future one that adds an
early return or a panic-recovery. `buf.String()` copies the buffer's bytes into
a freshly allocated, immutable string, so the returned value is completely
independent of the buffer — it is safe to hand the buffer back to the pool the
instant `String()` has run, even though another goroutine may `Get` that same
buffer microseconds later. If you returned `buf.Bytes()` instead of
`buf.String()`, this would be a bug: `Bytes()` aliases the buffer's backing
array, and the next user's `Reset`-and-write would corrupt the slice you handed
out. Returning a `string` is what makes the `defer Put` safe.

The bundled `Pool` is the same typed wrapper from Exercise 1, included here so
this module stands alone.

Create `buffers/pool.go`:

```go
package buffers

import (
	"bytes"
	"sync"
	"sync/atomic"
)

// Pool is a type-safe wrapper over sync.Pool for *bytes.Buffer.
type Pool struct {
	p         sync.Pool
	allocated atomic.Int64
	gets      atomic.Int64
	puts      atomic.Int64
}

// New returns a Pool whose New allocates and counts a fresh *bytes.Buffer.
func New() *Pool {
	p := &Pool{}
	p.p.New = func() any {
		p.allocated.Add(1)
		return new(bytes.Buffer)
	}
	return p
}

// Get returns a reset buffer, allocating only if the pool is empty.
func (p *Pool) Get() *bytes.Buffer {
	p.gets.Add(1)
	buf := p.p.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// Put returns a buffer to the pool for reuse.
func (p *Pool) Put(buf *bytes.Buffer) {
	p.puts.Add(1)
	p.p.Put(buf)
}

// Stats reports the cumulative counters: allocated, gets, puts.
func (p *Pool) Stats() (allocated, gets, puts int64) {
	return p.allocated.Load(), p.gets.Load(), p.puts.Load()
}
```

Create `buffers/format.go`:

```go
package buffers

import "fmt"

// Append borrows a buffer from p, formats prefix+s into it, and returns the
// result as a string. The pool is an internal detail: the caller sees only a
// string, and the buffer is returned to the pool on every exit path via defer.
// Returning buf.String() (a copy) rather than buf.Bytes() (an alias) is what
// makes the deferred Put safe.
func Append(p *Pool, prefix, s string) string {
	buf := p.Get()
	defer p.Put(buf)
	fmt.Fprintf(buf, "%s%s", prefix, s)
	return buf.String()
}
```

### The runnable demo

The demo formats several messages through `Append`. It never touches a buffer
directly; the counters at the end prove the pool was used behind the scenes.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pooledappend/buffers"
)

func main() {
	p := buffers.New()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		fmt.Println(buffers.Append(p, "user:", name))
	}
	allocated, gets, puts := p.Stats()
	fmt.Printf("allocated=%d gets=%d puts=%d\n", allocated, gets, puts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user:alpha
user:beta
user:gamma
allocated=1 gets=3 puts=3
```

### Tests

The tests assert the string result, verify the buffer really is returned to the
pool (so `gets == puts` after the call), and provide an `Example` for godoc.

Create `buffers/format_test.go`:

```go
package buffers

import (
	"fmt"
	"testing"
)

func TestAppend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		prefix, in string
		want       string
	}{
		{"simple", "msg:", "hi", "msg:hi"},
		{"empty prefix", "", "body", "body"},
		{"empty input", "prefix:", "", "prefix:"},
		{"both empty", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := New()
			if got := Append(p, tc.prefix, tc.in); got != tc.want {
				t.Fatalf("Append(%q, %q) = %q, want %q", tc.prefix, tc.in, got, tc.want)
			}
		})
	}
}

func TestAppendReturnsBufferToPool(t *testing.T) {
	t.Parallel()

	p := New()
	for range 5 {
		Append(p, "k:", "v")
	}
	_, gets, puts := p.Stats()
	if gets != puts {
		t.Fatalf("gets=%d puts=%d: defer Put did not return every buffer", gets, puts)
	}
	if gets != 5 {
		t.Fatalf("gets = %d, want 5", gets)
	}
}

func ExampleAppend() {
	p := New()
	fmt.Println(Append(p, "msg:", "hi"))
	// Output: msg:hi
}
```

## Review

`Append` is correct when its result is a self-contained string and the buffer is
always returned. `TestAppendReturnsBufferToPool` proves the `defer` fires by
checking `gets == puts` — if you ever refactored the helper to return early
without `Put`, this test would catch the leak. The subtle correctness point is
returning `buf.String()` (a fresh copy) rather than `buf.Bytes()` (an alias into
the buffer's array): only the copy is safe to hand out while the buffer goes back
to the pool for another goroutine. Run `go test -race` to confirm concurrent
`Append` calls do not share buffer state.

## Resources

- [`fmt.Fprintf`](https://pkg.go.dev/fmt#Fprintf) — formats into any `io.Writer`, including a `*bytes.Buffer`.
- [`bytes.Buffer.String`](https://pkg.go.dev/bytes#Buffer.String) — returns the contents as a newly allocated string (a copy, not an alias).
- [`sync.Pool`](https://pkg.go.dev/sync#Pool) — the Get/Put contract the helper hides.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-typed-buffer-pool.md](01-typed-buffer-pool.md) | Next: [03-pool-demo-cli.md](03-pool-demo-cli.md)
