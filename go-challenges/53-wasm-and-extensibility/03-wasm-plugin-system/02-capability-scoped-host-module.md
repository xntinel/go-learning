# Exercise 2: Assembling a Capability-Scoped Host Module

The trust boundary is the import list: a guest can call only the host functions
that exist in its namespace at instantiation. This exercise builds the mechanism
that makes that true — a `HostAPI` that, given a granted capability set, registers
on a wazero host module *only* the functions those grants permit, and wraps each
with a call-time guard for defense in depth. No guest binary is needed: the whole
boundary is provable from the host side.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
hostapi/                    independent module: example.com/hostapi
  go.mod                    go 1.26; requires github.com/tetratelabs/wazero
  hostapi.go                Capability set; HostAPI; guard; log/kv_get/kv_set/http_fetch;
                            Instantiate builds the scoped "env" host module
  cmd/
    demo/
      main.go               builds a scoped host module and lists its exports
  hostapi_test.go           scoped-export table, guard deny, memory-bounds, KV round-trip
```

- Files: `hostapi.go`, `cmd/demo/main.go`, `hostapi_test.go`.
- Implement: a `HostAPI` whose `Instantiate` registers a host function per granted capability (guarded), reading guest memory via `mod.Memory().Read` with bounds handling and returning an errno.
- Test: `ExportedFunctionDefinitions()` key set equals exactly the granted function names (deny-by-default); a guard closure invoked directly writes the deny errno; an out-of-bounds `Read` maps to a fault errno; a KV round-trip through the host functions.
- Verify: `go test -count=1 -race ./...`

Set up the module. wazero requires a recent toolchain, so add the dependency:

```bash
mkdir -p ~/go-exercises/hostapi/cmd/demo
cd ~/go-exercises/hostapi
go mod init example.com/hostapi
go get github.com/tetratelabs/wazero@latest
```

### Scoping at assembly time is the enforcement

A wazero host module is assembled with a `HostModuleBuilder`: you call
`NewFunctionBuilder()`, describe a function with `WithGoModuleFunction`, name it,
`Export` it, and finally `Instantiate` the module under a chosen namespace — here
`"env"`, the conventional import namespace guests expect. The single most
important line of this exercise is the `if h.granted.Has(c)` that gates each
registration: a function is added to the builder only when its capability was
granted. After `Instantiate`, the module's `ExportedFunctionDefinitions()` returns
exactly the granted surface. A guest instantiated against this module physically
cannot import `kv_get` if `cap.kv.read` was not granted, because the symbol does
not exist. That is enforcement by construction, not by convention — the test
asserts it directly by comparing the exported key set to the expected names.

### Guarding at call time is defense in depth

Assembly-time scoping is the primary control. The `guard` wrapper is the second,
independent one: every registered function is wrapped so that, when it runs, it
re-checks `h.granted.Has(c)` and — if the capability is somehow absent — writes a
deny errno to the result slot and returns *without* performing the side effect.
In the normal flow this branch never fires, because a function is only registered
when granted. It exists to catch a different class of bug: a builder mis-wired by
a future refactor, or a `HostAPI` value accidentally shared across two grants.
Two independent checks on two different failure modes is what "defense in depth"
means, and the cost is a single map lookup per call. The guard is testable on its
own: invoke it with a grant set that omits the capability and assert it wrote the
deny errno and never called the body.

### The Go-module function ABI, and why every read is checked

A host function registered with `WithGoModuleFunction` has the signature
`func(ctx context.Context, mod api.Module, stack []uint64)`. There are no typed
parameters and no typed return: the `stack` slice carries the call's parameters
on entry and must hold the results on exit. For a function declared as
`(i32, i32) -> i32`, `stack[0]` and `stack[1]` are the two `i32` parameters on
entry (decode them with `api.DecodeU32`), and `stack[0]` is where the single `i32`
result goes on exit (encode it with `api.EncodeI32`). Read the parameters before
you overwrite `stack[0]` with the result.

The two parameters name a region of the guest's linear memory — a `(pointer,
length)` pair — and the bytes are read with `mod.Memory().Read(ptr, len)`, which
returns `([]byte, bool)`. The `bool` is the whole safety story: it is `false` when
the region does not fit in the current memory, and the pointer and length came
from untrusted guest code. Every body here checks that `ok` and returns a fault
errno on failure rather than indexing a slice it never validated. A host function
that panics on bad guest input is a denial-of-service the guest can trigger at
will; returning an errno keeps the fault inside the guest's own result value.

These bodies use a deliberately simple `(ptr, len) -> errno` shape so the lesson
stays about the boundary, not the payload. A production host would also return a
result *region* to the guest using the packed `(ptr, len)` return convention from
the previous lesson; here `kv_get` reports only presence, which is enough to prove
the scoping and the memory discipline.

Create `hostapi.go`:

```go
// Package hostapi assembles a wazero host module scoped to a plugin's granted
// capabilities: only the host functions a grant permits are exported, and each
// is wrapped with a call-time capability guard for defense in depth.
package hostapi

import (
	"bytes"
	"context"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Capability is one entry in the closed set of powers the host can grant.
type Capability string

const (
	CapLog       Capability = "cap.log"
	CapKVRead    Capability = "cap.kv.read"
	CapKVWrite   Capability = "cap.kv.write"
	CapHTTPFetch Capability = "cap.http.fetch"
)

// CapabilitySet is a set of granted capabilities.
type CapabilitySet map[Capability]struct{}

// NewSet builds a CapabilitySet from a list of capabilities.
func NewSet(caps ...Capability) CapabilitySet {
	s := make(CapabilitySet, len(caps))
	for _, c := range caps {
		s[c] = struct{}{}
	}
	return s
}

// Has reports whether c is in the set.
func (s CapabilitySet) Has(c Capability) bool {
	_, ok := s[c]
	return ok
}

// errno values a host function writes into its result slot. Zero is success.
const (
	errnoOK       int32 = 0
	errnoDenied   int32 = 1
	errnoFault    int32 = 2
	errnoNotFound int32 = 3
)

// HostAPI holds the granted capabilities and the state the host functions act on.
// A fresh HostAPI is built per plugin, so the granted set is per plugin.
type HostAPI struct {
	granted CapabilitySet

	mu   sync.Mutex
	logs []string
	kv   map[string][]byte
}

// New builds a HostAPI for a granted capability set.
func New(granted CapabilitySet) *HostAPI {
	return &HostAPI{granted: granted, kv: make(map[string][]byte)}
}

// Logs returns a copy of the messages the guest logged.
func (h *HostAPI) Logs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

// guard wraps a host function body with a call-time capability check. If the
// capability is not granted it writes the deny errno and skips the body. This is
// defense in depth against a mis-wired builder; the exported surface is already
// scoped at assembly time.
func (h *HostAPI) guard(c Capability, body api.GoModuleFunc) api.GoModuleFunc {
	return func(ctx context.Context, mod api.Module, stack []uint64) {
		if !h.granted.Has(c) {
			stack[0] = api.EncodeI32(errnoDenied)
			return
		}
		body(ctx, mod, stack)
	}
}

// Instantiate builds the "env" host module exporting exactly the functions the
// granted capabilities permit. A capability that was not granted has no exported
// function, so a guest cannot import it.
func (h *HostAPI) Instantiate(ctx context.Context, rt wazero.Runtime) (api.Module, error) {
	b := rt.NewHostModuleBuilder("env")
	params := []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}
	results := []api.ValueType{api.ValueTypeI32}

	reg := func(c Capability, name string, body api.GoModuleFunc) {
		if !h.granted.Has(c) {
			return
		}
		b.NewFunctionBuilder().
			WithGoModuleFunction(h.guard(c, body), params, results).
			WithName(name).
			Export(name)
	}

	reg(CapLog, "log", h.log)
	reg(CapKVRead, "kv_get", h.kvGet)
	reg(CapKVWrite, "kv_set", h.kvSet)
	reg(CapHTTPFetch, "http_fetch", h.httpFetch)

	return b.Instantiate(ctx)
}

// log records a message read from guest memory. stack: (ptr, len) -> errno.
func (h *HostAPI) log(ctx context.Context, mod api.Module, stack []uint64) {
	ptr, length := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
	msg, ok := mod.Memory().Read(ptr, length)
	if !ok {
		stack[0] = api.EncodeI32(errnoFault)
		return
	}
	h.mu.Lock()
	h.logs = append(h.logs, string(msg))
	h.mu.Unlock()
	stack[0] = api.EncodeI32(errnoOK)
}

// kvGet reports whether a key (read from guest memory) exists. stack: (ptr, len)
// -> errno. A production host would also return the value region to the guest.
func (h *HostAPI) kvGet(ctx context.Context, mod api.Module, stack []uint64) {
	ptr, length := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
	key, ok := mod.Memory().Read(ptr, length)
	if !ok {
		stack[0] = api.EncodeI32(errnoFault)
		return
	}
	h.mu.Lock()
	_, found := h.kv[string(key)]
	h.mu.Unlock()
	if !found {
		stack[0] = api.EncodeI32(errnoNotFound)
		return
	}
	stack[0] = api.EncodeI32(errnoOK)
}

// kvSet stores a "key=value" pair read from guest memory. stack: (ptr, len) ->
// errno. A region without a '=' separator is rejected as a fault.
func (h *HostAPI) kvSet(ctx context.Context, mod api.Module, stack []uint64) {
	ptr, length := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
	raw, ok := mod.Memory().Read(ptr, length)
	if !ok {
		stack[0] = api.EncodeI32(errnoFault)
		return
	}
	eq := bytes.IndexByte(raw, '=')
	if eq < 0 {
		stack[0] = api.EncodeI32(errnoFault)
		return
	}
	key := string(raw[:eq])
	val := append([]byte(nil), raw[eq+1:]...)
	h.mu.Lock()
	h.kv[key] = val
	h.mu.Unlock()
	stack[0] = api.EncodeI32(errnoOK)
}

// httpFetch validates a URL region read from guest memory. stack: (ptr, len) ->
// errno. A real implementation would perform the fetch here, gated by the guard.
func (h *HostAPI) httpFetch(ctx context.Context, mod api.Module, stack []uint64) {
	ptr, length := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
	url, ok := mod.Memory().Read(ptr, length)
	if !ok || len(url) == 0 {
		stack[0] = api.EncodeI32(errnoFault)
		return
	}
	stack[0] = api.EncodeI32(errnoOK)
}
```

### The runnable demo

The demo grants three capabilities, builds the scoped host module, and lists its
exports — the concrete proof that the surface equals the grant.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"slices"

	"github.com/tetratelabs/wazero"

	"example.com/hostapi"
)

func main() {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)

	h := hostapi.New(hostapi.NewSet(hostapi.CapLog, hostapi.CapKVRead, hostapi.CapKVWrite))
	mod, err := h.Instantiate(ctx, rt)
	if err != nil {
		log.Fatal(err)
	}

	names := make([]string, 0)
	for n := range mod.ExportedFunctionDefinitions() {
		names = append(names, n)
	}
	slices.Sort(names)
	fmt.Println("granted host surface:", names)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
granted host surface: [kv_get kv_set log]
```

### Tests

`TestScopedExports` is the boundary proof: for each grant set it asserts the
exported function names equal exactly the expected list — a `{cap.log}` grant must
export `log` and must *not* export `kv_get`. `TestGuardDenies` invokes a guard
closure directly with an empty grant and asserts it wrote the deny errno without
running the body (no memory needed, since deny returns first).
`TestHostFuncBoundsRejected` passes an out-of-range `(ptr, len)` to `log` against a
real one-page memory and asserts the fault errno, then a valid region and asserts
success. `TestKVRoundTrip` writes bytes into guest memory and drives `kv_set`/
`kv_get` to cover the store, hit, and miss paths. The your-turn test adds
`cap.kv.write` and asserts `kv_set` appears while `log` does not.

Create `hostapi_test.go`:

```go
package hostapi

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// memOnlyWasm is (module (memory 1)): one 64 KiB page, no functions. Tests use it
// to obtain a real api.Module with memory to hand to host-function bodies.
var memOnlyWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // magic + version
	0x05, 0x03, 0x01, 0x00, 0x01, // memory section: 1 memory, min 1 page
}

func newMemModule(t *testing.T, rt wazero.Runtime) api.Module {
	t.Helper()
	c, err := rt.CompileModule(t.Context(), memOnlyWasm)
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	mem, err := rt.InstantiateModule(t.Context(), c, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	return mem
}

func TestScopedExports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		caps []Capability
		want []string
	}{
		{"log only", []Capability{CapLog}, []string{"log"}},
		{"kv read and write", []Capability{CapKVRead, CapKVWrite}, []string{"kv_get", "kv_set"}},
		{"all", []Capability{CapLog, CapKVRead, CapKVWrite, CapHTTPFetch}, []string{"http_fetch", "kv_get", "kv_set", "log"}},
		{"none", nil, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := wazero.NewRuntime(t.Context())
			t.Cleanup(func() { rt.Close(context.Background()) })

			h := New(NewSet(tc.caps...))
			mod, err := h.Instantiate(t.Context(), rt)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			got := make([]string, 0)
			for n := range mod.ExportedFunctionDefinitions() {
				got = append(got, n)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("exports = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGuardDenies(t *testing.T) {
	t.Parallel()
	h := New(NewSet()) // empty grant
	called := false
	body := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		called = true
	})
	guarded := h.guard(CapKVRead, body)

	stack := []uint64{0, 0}
	guarded.Call(t.Context(), nil, stack) // deny returns before touching mod
	if called {
		t.Fatal("body ran despite denied capability")
	}
	if got := api.DecodeI32(stack[0]); got != errnoDenied {
		t.Errorf("errno = %d, want errnoDenied (%d)", got, errnoDenied)
	}
}

func TestHostFuncBoundsRejected(t *testing.T) {
	t.Parallel()
	rt := wazero.NewRuntime(t.Context())
	t.Cleanup(func() { rt.Close(context.Background()) })
	mem := newMemModule(t, rt)
	h := New(NewSet(CapLog))

	// One page is 65536 bytes; a region at 70000 is out of range.
	bad := []uint64{api.EncodeU32(70000), api.EncodeU32(10)}
	h.log(t.Context(), mem, bad)
	if got := api.DecodeI32(bad[0]); got != errnoFault {
		t.Errorf("out-of-bounds errno = %d, want errnoFault (%d)", got, errnoFault)
	}

	good := []uint64{api.EncodeU32(0), api.EncodeU32(5)}
	h.log(t.Context(), mem, good)
	if got := api.DecodeI32(good[0]); got != errnoOK {
		t.Errorf("in-bounds errno = %d, want errnoOK (%d)", got, errnoOK)
	}
	if len(h.Logs()) != 1 {
		t.Errorf("logged messages = %d, want 1", len(h.Logs()))
	}
}

func TestKVRoundTrip(t *testing.T) {
	t.Parallel()
	rt := wazero.NewRuntime(t.Context())
	t.Cleanup(func() { rt.Close(context.Background()) })
	mem := newMemModule(t, rt)
	h := New(NewSet(CapKVRead, CapKVWrite))

	pair := []byte("token=abc")
	if !mem.Memory().Write(0, pair) {
		t.Fatal("memory write failed")
	}
	set := []uint64{api.EncodeU32(0), api.EncodeU32(uint32(len(pair)))}
	h.kvSet(t.Context(), mem, set)
	if got := api.DecodeI32(set[0]); got != errnoOK {
		t.Fatalf("kv_set errno = %d, want OK", got)
	}

	key := []byte("token")
	mem.Memory().Write(100, key)
	hit := []uint64{api.EncodeU32(100), api.EncodeU32(uint32(len(key)))}
	h.kvGet(t.Context(), mem, hit)
	if got := api.DecodeI32(hit[0]); got != errnoOK {
		t.Errorf("kv_get hit errno = %d, want OK", got)
	}

	miss := []byte("absent")
	mem.Memory().Write(200, miss)
	missStack := []uint64{api.EncodeU32(200), api.EncodeU32(uint32(len(miss)))}
	h.kvGet(t.Context(), mem, missStack)
	if got := api.DecodeI32(missStack[0]); got != errnoNotFound {
		t.Errorf("kv_get miss errno = %d, want NotFound", got)
	}
}

func TestHTTPFetchReadsURL(t *testing.T) {
	t.Parallel()
	rt := wazero.NewRuntime(t.Context())
	t.Cleanup(func() { rt.Close(context.Background()) })
	mem := newMemModule(t, rt)
	h := New(NewSet(CapHTTPFetch))

	url := []byte("https://example.com")
	mem.Memory().Write(0, url)
	stack := []uint64{api.EncodeU32(0), api.EncodeU32(uint32(len(url)))}
	h.httpFetch(t.Context(), mem, stack)
	if got := api.DecodeI32(stack[0]); got != errnoOK {
		t.Errorf("http_fetch errno = %d, want OK", got)
	}
}

func ExampleHostAPI_Instantiate() {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)

	h := New(NewSet(CapLog, CapKVRead))
	mod, err := h.Instantiate(ctx, rt)
	if err != nil {
		panic(err)
	}
	names := make([]string, 0)
	for n := range mod.ExportedFunctionDefinitions() {
		names = append(names, n)
	}
	slices.Sort(names)
	fmt.Println(names)
	// Output: [kv_get log]
}

// Your turn: granting cap.kv.write must add kv_set to the exported surface while
// leaving un-granted functions off. Prove kv_set appears and log does not.
func TestGrantingWriteExportsKVSet(t *testing.T) {
	t.Parallel()
	rt := wazero.NewRuntime(t.Context())
	t.Cleanup(func() { rt.Close(context.Background()) })

	h := New(NewSet(CapKVRead, CapKVWrite))
	mod, err := h.Instantiate(t.Context(), rt)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defs := mod.ExportedFunctionDefinitions()
	if _, ok := defs["kv_set"]; !ok {
		t.Error("cap.kv.write should export kv_set")
	}
	if _, ok := defs["log"]; ok {
		t.Error("log must not be exported without cap.log")
	}
}
```

## Review

The module is correct when the exported surface equals the grant and nothing
leaks past it. The tell for a broken boundary is `TestScopedExports` failing on
the deny-by-default rows: if a `{cap.log}` grant exports `kv_get`, the `if
h.granted.Has(c)` gate is missing or inverted. The guard is correct when a denied
call writes the deny errno and never runs the body — the direct-invocation test
proves that without a guest, which is the point: the boundary is verifiable from
the host alone.

The mistakes to avoid are the memory ones. Never index the slice from
`Memory().Read` without checking the `ok` bool; the fault-errno path is what turns
a hostile `(ptr, len)` into a controlled error instead of a panic that becomes a
guest-triggered DoS. Read the parameters out of `stack` before you overwrite
`stack[0]` with the result, since they share the slice. And build a fresh
`HostAPI` per plugin so its granted set and its KV state stay per plugin — sharing
one across plugins is exactly the boundary violation the guard exists to catch.
Run `go test -race` to confirm the mutex guards the shared state.

## Resources

- [wazero package reference](https://pkg.go.dev/github.com/tetratelabs/wazero) — `NewHostModuleBuilder`, `HostFunctionBuilder`, `WithGoModuleFunction`, `Export`, `Instantiate`.
- [wazero/api package](https://pkg.go.dev/github.com/tetratelabs/wazero/api) — `GoModuleFunc`, `Module.Memory`, `Memory.Read`/`Write`, `EncodeI32`/`DecodeU32`, `ExportedFunctionDefinitions`.
- [wazero host functions example](https://github.com/tetratelabs/wazero/tree/main/examples/import-go) — importing Go functions into a guest namespace.
- [WebAssembly imports and exports](https://webassembly.github.io/spec/core/syntax/modules.html#imports) — why a guest can reach only what the host imports.

---

Back to [01-plugin-manifest-and-capabilities.md](01-plugin-manifest-and-capabilities.md) | Next: [03-sandboxed-plugin-runner.md](03-sandboxed-plugin-runner.md)
