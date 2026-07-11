# Exercise 3: A Full ABI Round-Trip with Guest-Managed Allocation

Now the pieces come together. This exercise drives a real guest that exports an
allocator, a deallocator, and a `transform` function, and implements the full
host-side protocol: allocate in the guest, write the input, call across the
boundary, read the packed result, and free both allocations. This is the exact
shape of a plugin invocation — the skeleton the next chapter's plugin host is
built on.

This module is fully self-contained. It embeds its own guest, begins with its own
`go mod init`, and ships its own demo and tests. Nothing here imports any other
exercise; it bundles its own copy of the pointer/length codec.

## What you'll build

```text
transform/                 independent module: example.com/transform
  go.mod                   go 1.26
  transform.go             Host driving alloc/dealloc/transform; Transform, ReadPacked, PackPtrLen
  cmd/
    demo/
      main.go              runnable demo: uppercase two strings through the guest
  transform_test.go        end-to-end table tests, reuse, OOB, large-input-grow + Example
```

- Files: `transform.go`, `cmd/demo/main.go`, `transform_test.go`.
- Implement: a `Host` that instantiates a guest exporting `memory`, `alloc`, `dealloc`, `transform`, and a `Transform(ctx, string) (string, error)` that allocates input in the guest, writes it, calls `transform`, unpacks the returned `(ptr, len)`, defensively copies the result, and frees both the input and the returned allocation.
- Test: end-to-end over ASCII inputs including the empty string; a reuse test proving the module survives repeated calls; an out-of-bounds `ReadPacked`; and a large-input test that forces the guest allocator to grow memory mid-call.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/transform/cmd/demo
cd ~/go-exercises/transform
go mod init example.com/transform
```

### What the guest does, and the contract it exposes

The embedded `guestWASM` is a small hand-built module that exports four things.
`memory` is its linear memory (one growable page to start). `alloc(size i32) ->
i32` is a bump allocator: it returns the current top of a heap pointer and
advances it by `size`, growing linear memory by whole pages when the request would
run past the current end — so a large allocation triggers a `memory.grow` inside
the guest. `dealloc(ptr i32, size i32)` is a no-op here (a bump allocator never
frees individual regions), but it is a real export the host calls, so the host code
is identical to what it would be against a guest with a true allocator.
`transform(ptr i32, len i32) -> i64` reads `len` bytes at `ptr`, ASCII-uppercases
them into a freshly allocated output region, and returns that region as a packed
`(ptr, len)` in one `i64` — the high 32 bits are the pointer, the low 32 bits the
length.

That last signature is why the codec from Exercise 1 reappears here (bundled, so
this module stands alone): the guest can only return one value, so it packs, and
the host must unpack with the pointer in the high half. Getting that backwards is
the single most common way this breaks.

### The host protocol, step by step

`Transform` is the whole lesson in one function, and the order matters. First it
calls the guest `alloc` for the input length; the guest picks the address, which is
the non-negotiable rule — the host must never invent an offset. It decodes the
returned `i32` with `api.DecodeU32` (a Wasm `i32` result is carried in a `uint64`
slot), and immediately schedules the input's release with `defer dealloc`. It
writes the input bytes at the guest-chosen pointer, checking the `ok` bool. It
calls `transform`, unpacks the returned packed value into `(outPtr, outLen)`, and
schedules that region's release too — this second `defer` is the one everybody
forgets, and forgetting it leaks the guest heap on every call. Finally it reads the
output with a defensive copy (`append([]byte(nil), view...)`) *before* the deferred
`dealloc` calls run, because the moment `dealloc` runs the region is no longer
yours to read, and because `Read` returns a view aliasing live memory that a later
call could invalidate. Returning `string(data)` from the copy is safe; returning a
string built from the raw view would not be.

The empty string is a real case, not an afterthought: `alloc(0)` returns a valid
pointer, the input write is skipped, `transform` runs its loop zero times, and
`Read(outPtr, 0)` returns an empty slice with `ok == true`. The code handles it by
guarding the write on `inLen > 0` and letting everything else fall through
naturally.

Create `transform.go`:

```go
package transform

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// guestWASM is a hand-built module exporting memory plus a growing bump
// allocator (alloc), a no-op dealloc, and transform, which ASCII-uppercases
// [ptr,ptr+len) into a fresh region returned as a packed (ptr<<32 | len).
const guestWASM = "AGFzbQEAAAABEQNgAX8Bf2ACf38AYAJ/fwF+AwQDAAECBQMBAAEGBwF/AUGACAsHKAQGbWVtb3J5AgAFYWxsb2MAAAdkZWFsbG9jAAEJdHJhbnNmb3JtAAIKjQEDLgECfyMAIQEjACAAaiECAkADQCACPwBBgIAEbE0NAUEBQAAaDAALCyACJAAgAQsCAAtZAQN/IAEQACECQQAhAwJAA0AgAyABTw0BIAAgA2otAAAhBCAEQeEATyAEQfoATXEEQCAEQSBrIQQLIAIgA2ogBDoAACADQQFqIQMMAAsLIAKtQiCGIAGthAs="

// ErrOutOfBounds is returned when a region does not fit in linear memory.
var ErrOutOfBounds = errors.New("region out of bounds")

// PackPtrLen folds a (pointer, length) pair into one uint64: pointer high,
// length low. This is the guest's return convention, bundled so the module is
// self-contained.
func PackPtrLen(ptr, length uint32) uint64 { return uint64(ptr)<<32 | uint64(length) }

// UnpackPtrLen reverses PackPtrLen: pointer is the high 32 bits, length the low.
func UnpackPtrLen(packed uint64) (ptr, length uint32) {
	return uint32(packed >> 32), uint32(packed)
}

// Host is an instantiated guest plus handles to its exported functions.
type Host struct {
	runtime   wazero.Runtime
	mem       api.Memory
	alloc     api.Function
	dealloc   api.Function
	transform api.Function
}

// New instantiates the embedded guest and resolves its exports.
func New(ctx context.Context) (*Host, error) {
	wasm, err := base64.StdEncoding.DecodeString(guestWASM)
	if err != nil {
		return nil, fmt.Errorf("decode guest: %w", err)
	}
	r := wazero.NewRuntime(ctx)
	m, err := r.Instantiate(ctx, wasm)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("instantiate guest: %w", err)
	}
	h := &Host{
		runtime:   r,
		mem:       m.Memory(),
		alloc:     m.ExportedFunction("alloc"),
		dealloc:   m.ExportedFunction("dealloc"),
		transform: m.ExportedFunction("transform"),
	}
	if h.mem == nil || h.alloc == nil || h.dealloc == nil || h.transform == nil {
		r.Close(ctx)
		return nil, errors.New("guest missing required exports")
	}
	return h, nil
}

// Close releases the runtime, the module, and its memory.
func (h *Host) Close(ctx context.Context) error { return h.runtime.Close(ctx) }

// Size reports the current linear-memory size in bytes.
func (h *Host) Size() uint32 { return h.mem.Size() }

// readCopy reads byteCount bytes at offset and returns an independent copy,
// converting an out-of-range read into a wrapped ErrOutOfBounds.
func (h *Host) readCopy(offset, byteCount uint32) ([]byte, error) {
	view, ok := h.mem.Read(offset, byteCount)
	if !ok {
		return nil, fmt.Errorf("read %d bytes at %d: %w", byteCount, offset, ErrOutOfBounds)
	}
	return append([]byte(nil), view...), nil
}

// ReadPacked reads the region named by a packed (ptr, len), bounds-checked.
func (h *Host) ReadPacked(packed uint64) ([]byte, error) {
	ptr, n := UnpackPtrLen(packed)
	return h.readCopy(ptr, n)
}

// Transform sends input to the guest, uppercases it there, and returns the
// result. It allocates and frees both the input and the guest-returned output.
func (h *Host) Transform(ctx context.Context, input string) (string, error) {
	inLen := uint32(len(input))

	res, err := h.alloc.Call(ctx, api.EncodeU32(inLen))
	if err != nil {
		return "", fmt.Errorf("alloc %d bytes: %w", inLen, err)
	}
	inPtr := api.DecodeU32(res[0])
	defer h.dealloc.Call(ctx, api.EncodeU32(inPtr), api.EncodeU32(inLen))

	if inLen > 0 && !h.mem.Write(inPtr, []byte(input)) {
		return "", fmt.Errorf("write input at %d: %w", inPtr, ErrOutOfBounds)
	}

	out, err := h.transform.Call(ctx, api.EncodeU32(inPtr), api.EncodeU32(inLen))
	if err != nil {
		return "", fmt.Errorf("transform: %w", err)
	}
	outPtr, outLen := UnpackPtrLen(out[0])
	defer h.dealloc.Call(ctx, api.EncodeU32(outPtr), api.EncodeU32(outLen))

	data, err := h.readCopy(outPtr, outLen)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
```

### The runnable demo

The demo instantiates the guest once and transforms two strings through it,
showing the module is reusable across calls.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"example.com/transform"
)

func main() {
	ctx := context.Background()
	h, err := transform.New(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer h.Close(ctx)

	for _, in := range []string{"hello, wasm", "Round-Trip"} {
		out, err := h.Transform(ctx, in)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%q -> %q\n", in, out)
	}
	fmt.Printf("memory after 2 calls: %d bytes\n", h.Size())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"hello, wasm" -> "HELLO, WASM"
"Round-Trip" -> "ROUND-TRIP"
memory after 2 calls: 65536 bytes
```

### Tests

`TestTransform` drives the full round-trip over ASCII inputs including the empty
string, asserting the uppercased result and no bounds error. `TestReusable`
confirms the module survives repeated calls — the bump heap advances and nothing
is truly freed, but the instance stays usable, which is what a long-lived plugin
host relies on. `TestReadPackedOutOfBounds` proves the read path rejects a region
that starts at the end of memory with a wrapped `ErrOutOfBounds`.
`TestLargeInputSpansGrow` is your extension point: an 80000-byte input exceeds the
initial single page, so the guest allocator must grow memory mid-call, and the
result must still be correct and the memory larger than one page afterward — a
direct check that the defensive copy survived the growth.

Create `transform_test.go`:

```go
package transform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestTransform(t *testing.T) {
	t.Parallel()
	h, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(t.Context())
	cases := []struct{ in, want string }{
		{"hello world", "HELLO WORLD"},
		{"", ""},
		{"ABC123xyz", "ABC123XYZ"},
		{"Go1.26", "GO1.26"},
	}
	for _, c := range cases {
		got, err := h.Transform(t.Context(), c.in)
		if err != nil {
			t.Fatalf("Transform(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Transform(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReusable(t *testing.T) {
	t.Parallel()
	h, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(t.Context())
	first, err := h.Transform(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Transform(t.Context(), "beta")
	if err != nil {
		t.Fatal(err)
	}
	if first != "ALPHA" || second != "BETA" {
		t.Fatalf("got %q, %q", first, second)
	}
}

func TestReadPackedOutOfBounds(t *testing.T) {
	t.Parallel()
	h, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(t.Context())
	packed := PackPtrLen(h.Size(), 8) // region starts at the end of memory
	if _, err := h.ReadPacked(packed); !errors.Is(err, ErrOutOfBounds) {
		t.Errorf("want ErrOutOfBounds, got %v", err)
	}
}

// Your turn: transform an input larger than one 64 KiB page, forcing the guest
// allocator to grow linear memory mid-call.
func TestLargeInputSpansGrow(t *testing.T) {
	t.Parallel()
	h, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(t.Context())
	in := strings.Repeat("ab", 40000) // 80000 bytes > 65536
	got, err := h.Transform(t.Context(), in)
	if err != nil {
		t.Fatalf("large transform: %v", err)
	}
	if got != strings.Repeat("AB", 40000) {
		t.Errorf("large transform wrong: len=%d", len(got))
	}
	if h.Size() <= 65536 {
		t.Errorf("memory did not grow: size=%d", h.Size())
	}
}

func ExampleHost_Transform() {
	h, _ := New(context.Background())
	defer h.Close(context.Background())
	out, _ := h.Transform(context.Background(), "wire protocol")
	fmt.Println(out)
	// Output: WIRE PROTOCOL
}
```

## Review

The round-trip is correct when the result matches the expected uppercase for every
input including the empty string, when the module is still usable after repeated
calls, and when a large input that forces a `memory.grow` still returns the right
bytes. If a large input corrupts or truncates, the defensive copy is the first
suspect: reading into a view and returning a string from it — instead of copying
before the guest can grow or free — is the classic failure, and it is exactly what
`readCopy` and the `TestLargeInputSpansGrow` check guard against.

The mistakes to avoid are the ownership ones. Never write to an offset you chose;
call `alloc` and let the guest pick. Always free *both* allocations — the input you
allocated and the region `transform` returned — which is why there are two
`defer dealloc` calls; dropping the second leaks the guest heap on every request.
Unpack the returned `i64` with the pointer in the high half (`uint32(r >> 32)`) and
the length in the low half (`uint32(r)`); swapping them turns a valid pointer into
a length and reads garbage. And read the result before the deferred `dealloc` runs
— a copy taken after free is a use-after-free. Run `go test -race` to confirm the
whole protocol.

## Resources

- [wazero `api.Function`](https://pkg.go.dev/github.com/tetratelabs/wazero/api#Function) — `Call` and the `[]uint64` parameter/result convention.
- [wazero `api.Module`](https://pkg.go.dev/github.com/tetratelabs/wazero/api#Module) — `ExportedFunction` and `Memory`.
- [wazero allocation example (TinyGo)](https://github.com/tetratelabs/wazero/tree/main/examples/allocation/tinygo) — a real host calling guest `malloc`/`free` with the same `ptr<<32 | size` packing.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-linear-memory-io.md](02-linear-memory-io.md) | Next: [../03-wasm-plugin-system/00-concepts.md](../03-wasm-plugin-system/00-concepts.md)
