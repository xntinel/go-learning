# Exercise 2: Reading and Writing Guest Linear Memory

With the codec settled, the next primitive is the raw I/O: getting bytes into and
out of a guest's linear memory through `api.Memory`, with every out-of-range
access turned into an error and every kept byte defensively copied. This exercise
instantiates a minimal memory-only Wasm module with wazero's interpreter (no cgo,
no network) and builds the host-side helpers that a plugin host is made of.

This module is fully self-contained. It embeds its own guest as a base64 string,
begins with its own `go mod init`, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
memio/                     independent module: example.com/memio
  go.mod                   go 1.26
  memio.go                 Guest{} wrapping api.Memory: WriteAt, ReadCopy, ReadView, GrowPages
  cmd/
    demo/
      main.go              runnable demo: echo a payload through memory, grow, reject OOB
  memio_test.go            round-trip, OOB, write-through, stale-after-grow tests + Example
```

- Files: `memio.go`, `cmd/demo/main.go`, `memio_test.go`.
- Implement: a `Guest` that instantiates an embedded memory-only module and exposes `WriteAt`, `ReadCopy` (defensive), `ReadView` (aliasing), `Size`, `GrowPages`, and `Close`, converting every `ok == false` into a wrapped `ErrOutOfBounds`.
- Test: table-driven round-trips; out-of-range write and read asserted with `errors.Is`; a write-through proof (mutating a view is visible in a later read); a proof that a defensive copy survives a `Grow` while a retained view goes stale.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/memio/cmd/demo
cd ~/go-exercises/memio
go mod init example.com/memio
```

### The embedded guest, and why it is 25 bytes

To exercise linear-memory I/O you need a module that *has* a memory and exports
it, and nothing else. The `memoryWASM` constant below is exactly that: a complete
25-byte WebAssembly binary with two sections — a memory section declaring one page
(minimum 1, no maximum, so it can grow) and an export section publishing that
memory under the name `"memory"`. It has no functions at all. It is embedded as a
base64 string so the lesson stays a single text file; `New` decodes it once at
startup. In a real project you would compile a guest from Go or Rust with TinyGo
(the next chapter's topic) and embed the resulting `.wasm` with `//go:embed`; the
host code here is identical either way, because from the host's side a guest is
just bytes handed to `Runtime.Instantiate`.

wazero runs this in its pure-Go interpreter by default, so there is no cgo, no
external toolchain, and no network — the test is hermetic. `NewRuntime` returns a
runtime, `Instantiate` returns an `api.Module`, and `Module.Memory()` returns the
`api.Memory` handle that all the I/O goes through. Closing the runtime frees the
module and its memory.

### The page model, in code

Two units meet here and mixing them is the classic bug. Linear memory is measured
in *bytes* by `Memory.Size()` but grown in *pages* by `Memory.Grow(deltaPages)`,
where a page is exactly 65536 bytes. `Grow` returns the *previous* page count (and
`false` if growth would exceed the module's declared maximum). `GrowPages` below
keeps the units honest by multiplying the returned page count by `PageSize` to
report the previous size in bytes, so callers never have to remember which unit a
number is in.

### Write-through views versus defensive copies

`api.Memory.Read` does not copy — it returns a `[]byte` aliasing the live memory
buffer. That is the single most important fact in this file, and the two helpers
make the choice explicit. `ReadView` returns that raw aliasing slice: writing to
it writes through to guest memory, and it becomes stale the instant memory grows
and wazero reallocates the backing array. `ReadCopy` returns
`append([]byte(nil), view...)` — an independent copy that is safe to keep across
later calls and across growth. The rule the tests enforce is: use `ReadCopy` for
anything you keep; only reach for `ReadView` when you deliberately want to write
through, and never hold the result across a `Call` or a `Grow`.

Create `memio.go`:

```go
package memio

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// PageSize is the fixed WebAssembly linear-memory page size in bytes.
const PageSize = 65536

// memoryWASM is a complete 25-byte module: one growable page of memory exported
// as "memory", and no functions. It is the smallest guest that has a heap to
// read and write.
const memoryWASM = "AGFzbQEAAAAFAwEAAQcKAQZtZW1vcnkCAA=="

// ErrOutOfBounds is returned when a read, write, or grow addresses memory that
// does not exist. It is always wrapped with %w so callers can use errors.Is.
var ErrOutOfBounds = errors.New("region out of bounds")

// Guest is an instantiated memory-only module plus the runtime that owns it.
type Guest struct {
	runtime wazero.Runtime
	mod     api.Module
	mem     api.Memory
}

// New instantiates the embedded guest in wazero's interpreter.
func New(ctx context.Context) (*Guest, error) {
	wasm, err := base64.StdEncoding.DecodeString(memoryWASM)
	if err != nil {
		return nil, fmt.Errorf("decode guest: %w", err)
	}
	r := wazero.NewRuntime(ctx)
	m, err := r.Instantiate(ctx, wasm)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("instantiate guest: %w", err)
	}
	return &Guest{runtime: r, mod: m, mem: m.Memory()}, nil
}

// Close releases the runtime and the module's memory.
func (g *Guest) Close(ctx context.Context) error { return g.runtime.Close(ctx) }

// Size reports the current linear-memory size in bytes.
func (g *Guest) Size() uint32 { return g.mem.Size() }

// WriteAt copies v into linear memory at offset. An out-of-range write returns a
// wrapped ErrOutOfBounds instead of silently doing nothing.
func (g *Guest) WriteAt(offset uint32, v []byte) error {
	if !g.mem.Write(offset, v) {
		return fmt.Errorf("write %d bytes at %d: %w", len(v), offset, ErrOutOfBounds)
	}
	return nil
}

// ReadCopy returns an independent copy of byteCount bytes at offset. The copy is
// decoupled from linear memory: safe to keep across later calls and growth.
func (g *Guest) ReadCopy(offset, byteCount uint32) ([]byte, error) {
	view, ok := g.mem.Read(offset, byteCount)
	if !ok {
		return nil, fmt.Errorf("read %d bytes at %d: %w", byteCount, offset, ErrOutOfBounds)
	}
	return append([]byte(nil), view...), nil
}

// ReadView returns the write-through slice aliasing live memory. Mutating it
// mutates the guest; it MUST NOT be held across a Call or a Grow.
func (g *Guest) ReadView(offset, byteCount uint32) ([]byte, error) {
	view, ok := g.mem.Read(offset, byteCount)
	if !ok {
		return nil, fmt.Errorf("read %d bytes at %d: %w", byteCount, offset, ErrOutOfBounds)
	}
	return view, nil
}

// GrowPages grows linear memory by deltaPages 65536-byte pages and returns the
// previous size in bytes.
func (g *Guest) GrowPages(deltaPages uint32) (prevBytes uint32, err error) {
	prevPages, ok := g.mem.Grow(deltaPages)
	if !ok {
		return 0, fmt.Errorf("grow by %d pages: %w", deltaPages, ErrOutOfBounds)
	}
	return prevPages * PageSize, nil
}
```

### The runnable demo

The demo instantiates the guest, echoes a payload through memory, grows it by one
page, and shows an out-of-range write being rejected rather than silently lost.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/memio"
)

func main() {
	ctx := context.Background()
	g, err := memio.New(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer g.Close(ctx)

	fmt.Printf("initial memory: %d bytes (%d page)\n", g.Size(), g.Size()/memio.PageSize)

	msg := []byte("echo through linear memory")
	if err := g.WriteAt(2048, msg); err != nil {
		log.Fatal(err)
	}
	echoed, err := g.ReadCopy(2048, uint32(len(msg)))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("echoed: %s\n", echoed)

	prev, err := g.GrowPages(1)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("grew from %d to %d bytes\n", prev, g.Size())

	if err := g.WriteAt(g.Size(), []byte("x")); err != nil {
		fmt.Println("write past end rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial memory: 65536 bytes (1 page)
echoed: echo through linear memory
grew from 65536 to 131072 bytes
write past end rejected: write 1 bytes at 131072: region out of bounds
```

### Tests

`TestRoundTrip` writes several payloads (including an empty one) at different
offsets and reads each back with `ReadCopy`. `TestOutOfRange` proves the boundary
is real: a write at `Size()`, a read at `Size()`, and a read one byte longer than
memory all return `ErrOutOfBounds`. `TestWriteThrough` proves `Read` aliases live
memory — mutating a `ReadView` slice is visible in a later `ReadCopy`.
`TestCopyDecoupledAfterGrow` is the payoff: it takes both a view and a copy of the
same region, grows memory, overwrites the region, and asserts the copy still holds
the old bytes (decoupled and safe) while the retained view no longer reflects the
new write (stale — which is exactly why you must not hold it). The last test is
your extension point.

Create `memio_test.go`:

```go
package memio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	g, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close(t.Context())
	cases := []struct {
		offset uint32
		data   []byte
	}{
		{0, []byte("hello")},
		{100, []byte{0, 1, 2, 255}},
		{1000, []byte("")},
	}
	for _, c := range cases {
		if err := g.WriteAt(c.offset, c.data); err != nil {
			t.Fatalf("WriteAt(%d): %v", c.offset, err)
		}
		got, err := g.ReadCopy(c.offset, uint32(len(c.data)))
		if err != nil {
			t.Fatalf("ReadCopy(%d): %v", c.offset, err)
		}
		if !bytes.Equal(got, c.data) {
			t.Errorf("offset %d: got %v want %v", c.offset, got, c.data)
		}
	}
}

func TestOutOfRange(t *testing.T) {
	t.Parallel()
	g, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close(t.Context())
	if err := g.WriteAt(g.Size(), []byte("x")); !errors.Is(err, ErrOutOfBounds) {
		t.Errorf("write past end: want ErrOutOfBounds, got %v", err)
	}
	if _, err := g.ReadCopy(g.Size(), 1); !errors.Is(err, ErrOutOfBounds) {
		t.Errorf("read past end: want ErrOutOfBounds, got %v", err)
	}
	if _, err := g.ReadCopy(0, g.Size()+1); !errors.Is(err, ErrOutOfBounds) {
		t.Errorf("read beyond size: want ErrOutOfBounds, got %v", err)
	}
}

func TestWriteThrough(t *testing.T) {
	t.Parallel()
	g, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close(t.Context())
	if err := g.WriteAt(100, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	view, err := g.ReadView(100, 3)
	if err != nil {
		t.Fatal(err)
	}
	view[0] = 'X' // write-through: mutates live guest memory in place
	again, err := g.ReadCopy(100, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != "Xbc" {
		t.Errorf("write-through failed: got %q want %q", again, "Xbc")
	}
}

func TestCopyDecoupledAfterGrow(t *testing.T) {
	t.Parallel()
	g, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close(t.Context())
	if err := g.WriteAt(0, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	view, err := g.ReadView(0, 5)
	if err != nil {
		t.Fatal(err)
	}
	safe, err := g.ReadCopy(0, 5)
	if err != nil {
		t.Fatal(err)
	}
	prevBytes, err := g.GrowPages(1)
	if err != nil {
		t.Fatal(err)
	}
	if prevBytes != PageSize {
		t.Errorf("prevBytes = %d, want %d", prevBytes, PageSize)
	}
	if err := g.WriteAt(0, []byte("WORLD")); err != nil {
		t.Fatal(err)
	}
	if string(safe) != "hello" {
		t.Errorf("defensive copy changed: got %q want hello", safe)
	}
	fresh, err := g.ReadCopy(0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(fresh) != "WORLD" {
		t.Errorf("fresh read: got %q want WORLD", fresh)
	}
	if string(view) == "WORLD" {
		t.Error("retained view reflected post-grow write; it must be treated as stale")
	}
}

// Your turn: prove a write that starts in the original page and ends in a page
// added by Grow succeeds once the memory is large enough.
func TestWriteSpanningGrownPage(t *testing.T) {
	t.Parallel()
	g, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close(t.Context())
	if _, err := g.GrowPages(1); err != nil {
		t.Fatal(err)
	}
	span := make([]byte, 16)
	for i := range span {
		span[i] = byte('a' + i)
	}
	if err := g.WriteAt(PageSize-8, span); err != nil {
		t.Fatalf("cross-page write: %v", err)
	}
	got, err := g.ReadCopy(PageSize-8, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, span) {
		t.Errorf("cross-page round trip: got %v want %v", got, span)
	}
}

func ExampleGuest() {
	g, _ := New(context.Background())
	defer g.Close(context.Background())
	_ = g.WriteAt(0, []byte("wasm"))
	data, _ := g.ReadCopy(0, 4)
	fmt.Printf("%s (memory=%d bytes)\n", data, g.Size())
	// Output: wasm (memory=65536 bytes)
}
```

## Review

The I/O layer is correct when every `ok == false` becomes a wrapped
`ErrOutOfBounds` and never a silently short read or dropped write — that is the
security boundary between your host and an untrusted `(ptr, len)`. Confirm it with
`TestOutOfRange`: a write at `Size()` and a read past `Size()` must both be
rejected. The subtler correctness property is the view-versus-copy distinction.
`TestWriteThrough` shows `Read` aliases live memory, so a slice you treat as
scratch is actually guest state; `TestCopyDecoupledAfterGrow` shows the aliasing
slice goes stale after growth while a defensive copy does not. If either of those
flakes, you either copied when you meant to alias or, far worse, kept an aliasing
view across a `Grow`.

The mistakes to avoid: discarding the `ok` bool (an ignored `false` is a latent
out-of-bounds bug), confusing `Size()` bytes with `Grow` pages (multiply by
`PageSize` to convert), and holding a `ReadView` result across any call that might
grow memory. When in doubt, `ReadCopy`. Run `go test -race` to confirm the whole
file, including the concurrent-safe defensive copies.

## Resources

- [wazero `api.Memory`](https://pkg.go.dev/github.com/tetratelabs/wazero/api#Memory) — `Read`, `Write`, `Size`, `Grow`, and the documented aliasing of `Read`.
- [wazero `Runtime`](https://pkg.go.dev/github.com/tetratelabs/wazero#Runtime) — `NewRuntime` and `Instantiate`.
- [WebAssembly.Memory (MDN)](https://developer.mozilla.org/en-US/docs/WebAssembly/JavaScript_interface/Memory) — the linear-memory model and the 64 KiB page size.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-ptr-len-abi-codec.md](01-ptr-len-abi-codec.md) | Next: [03-string-transform-roundtrip.md](03-string-transform-roundtrip.md)
