# Exercise 2: A WASI Reactor with go:wasmexport — Exported Functions That Persist

A command guest runs once and is spent. When the same guest is on a hot path —
called thousands of times per second to evaluate a feature flag or a rule — you
want a *reactor*: initialize the module once, then call its exported functions
repeatedly on the one live instance. This exercise builds a feature-flag reactor
with `//go:wasmexport` functions and a `wazero` host that drives them.

This module is fully self-contained. The rollout algorithm lives in a plain
package that both the reactor guest and the host tests use, so the numeric logic is
verifiable with an ordinary `go test`.

## What you'll build

```text
rollflags/                  independent module: example.com/rollflags
  go.mod                    go 1.24; requires github.com/tetratelabs/wazero
  rollout.go                pure: Bucket, InRollout (no wazero, no build tag)
  reactor.go                Reactor over wazero; NewReactor (_initialize), Bucket, InRollout, call
  guest/
    main.go                 //go:build wasip1 — package main; //go:wasmexport bucket, in_rollout
  cmd/
    demo/
      main.go               host: init reactor once, call exports repeatedly
  rollout_test.go           table tests for Bucket/InRollout; Example
```

- Files: `rollout.go`, `reactor.go`, `guest/main.go`, `cmd/demo/main.go`, `rollout_test.go`.
- Implement: pure `Bucket(userID)` and `InRollout(userID, pct)`, two `//go:wasmexport` wrappers, and a `Reactor` that instantiates with `WithStartFunctions("_initialize")` and calls exports via `ExportedFunction`/`Call` with `api.Encode*`/`api.Decode*`.
- Test: table-driven determinism and range checks on `Bucket`; edge cases on `InRollout`; an `Example` with `// Output:`.
- Verify: build the reactor with `-buildmode=c-shared`, then `go run ./cmd/demo`.

Set up the module. `//go:wasmexport` requires Go 1.24+ (TinyGo 0.34+):

```bash
go mod edit -go=1.24
go get github.com/tetratelabs/wazero@latest
```

### Command versus reactor, made concrete

The guest in Exercise 1 was a command: its `_start` ran `main` to completion and
the instance was done. A reactor inverts this. It is built with
`-buildmode=c-shared`, which tells the linker to emit `_initialize` instead of
`_start` and to export the `//go:wasmexport` functions. The host calls
`_initialize` exactly once — through `WithStartFunctions("_initialize")` — to run
the Go runtime bring-up and package `init`s, and then the instance *persists*. You
call `bucket` and `in_rollout` on it as many times as you like, and the expensive
initialization is paid once, not per call. A `func main()` still has to exist for
the package to compile, but under the reactor build it is never invoked.

That amortization is the entire reason to reach for a reactor. For a flag check on
the request path, re-instantiating a command per request would rebuild the runtime
every time; a reactor turns each check into a bare function call into already-warm
linear memory.

### Keep the exported logic pure

Exactly as in Exercise 1, the algorithm is ordinary Go and lives outside the
guest. `Bucket` maps a user ID deterministically into one of 100 buckets with a
splitmix64 finalizer — a small, fast integer hash with good dispersion, which is
what you want so a 10% rollout actually reaches a well-spread 10% of users rather
than a clustered slice of the ID space. `InRollout` reports whether a user falls
in the first `pct` percent of buckets, with the boundaries pinned:
`pct <= 0` is always false, `pct >= 100` is always true. Because this is pure and
tag-free, the host tests call it directly and prove the algorithm without touching
Wasm; the guest merely re-exposes it.

Create `rollout.go`:

```go
package rollflags

// Bucket maps a user ID deterministically into one of 100 buckets, [0, 100).
// It uses the splitmix64 finalizer, a fast integer hash with good dispersion, so
// the same logic runs in the guest (compiled to Wasm) and in host-side tests.
func Bucket(userID int64) int32 {
	x := uint64(userID)
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	x ^= x >> 31
	return int32(x % 100)
}

// InRollout reports whether userID falls in the first pct percent of buckets.
// pct <= 0 is never in; pct >= 100 is always in.
func InRollout(userID int64, pct int32) bool {
	if pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	return Bucket(userID) < pct
}
```

### The reactor guest and the numeric ABI

The guest is `package main` with two `//go:wasmexport` functions. The directive's
argument is the *export name* the host looks up; the Go function name can differ,
which is why `in_rollout` (a conventional Wasm export name) maps to the Go function
`inRollout`. Both wrappers take and return scalars only — `int64` and `int32` —
because that is the whole ABI across the raw boundary. There is no `bool` on the
wire: `inRollout` returns `int32` (1 or 0), and the host decides what that means.
Anything richer than scalars — a string, a struct — would require the
linear-memory allocator dance from lesson 2; a flag check needs none of it, which
is exactly why a reactor fits here.

Create `guest/main.go`:

```go
//go:build wasip1

// Command rollflags is a WASI reactor. Build it with
// GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o flags.wasm ./guest
// or tinygo build -buildmode=c-shared -target=wasip1 -o flags.wasm ./guest
package main

import "example.com/rollflags"

// main exists so the package compiles; under -buildmode=c-shared it is not run.
func main() {}

//go:wasmexport bucket
func bucket(userID int64) int32 {
	return rollflags.Bucket(userID)
}

//go:wasmexport in_rollout
func inRollout(userID int64, pct int32) int32 {
	if rollflags.InRollout(userID, pct) {
		return 1
	}
	return 0
}
```

### The host: initialize once, call many

`NewReactor` builds the runtime, instantiates WASI, and then instantiates the
guest with `WithStartFunctions("_initialize")` — the line that makes this a
reactor rather than a command. It holds onto the returned `api.Module`; that live
instance is what the repeated calls run against. `Bucket` and `InRollout` encode
their scalar arguments with `api.EncodeI64`/`api.EncodeI32`, call the export
through the shared `call` helper, and decode the single result with
`api.DecodeI32`. `call` looks the function up by its export name and returns a
wrapped `ErrExportMissing` if it is absent — the failure you get when the guest
was built as a command (no exports) or the name is misspelled.

Create `reactor.go`:

```go
package rollflags

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// ErrExportMissing is returned when a named export is absent — typically because
// the guest was built as a command, not a reactor.
var ErrExportMissing = errors.New("reactor export not found")

// Reactor is a live reactor instance: _initialize has run, and its exported
// functions can be called repeatedly on this one instance.
type Reactor struct {
	rt  wazero.Runtime
	mod api.Module
}

// NewReactor instantiates the reactor guest once, running _initialize (not
// _start), and keeps the instance live for repeated calls.
func NewReactor(ctx context.Context, wasm []byte) (*Reactor, error) {
	rt := wazero.NewRuntime(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	cfg := wazero.NewModuleConfig().WithStartFunctions("_initialize")
	mod, err := rt.InstantiateWithConfig(ctx, wasm, cfg)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("instantiate reactor: %w", err)
	}
	return &Reactor{rt: rt, mod: mod}, nil
}

// Close releases the instance and the runtime.
func (r *Reactor) Close(ctx context.Context) error { return r.rt.Close(ctx) }

// Bucket calls the guest's exported bucket function.
func (r *Reactor) Bucket(ctx context.Context, userID int64) (int32, error) {
	res, err := r.call(ctx, "bucket", api.EncodeI64(userID))
	if err != nil {
		return 0, err
	}
	return api.DecodeI32(res[0]), nil
}

// InRollout calls the guest's exported in_rollout function, decoding its 1/0
// result into a bool.
func (r *Reactor) InRollout(ctx context.Context, userID int64, pct int32) (bool, error) {
	res, err := r.call(ctx, "in_rollout", api.EncodeI64(userID), api.EncodeI32(pct))
	if err != nil {
		return false, err
	}
	return api.DecodeI32(res[0]) == 1, nil
}

func (r *Reactor) call(ctx context.Context, name string, args ...uint64) ([]uint64, error) {
	fn := r.mod.ExportedFunction(name)
	if fn == nil {
		return nil, fmt.Errorf("%w: %q", ErrExportMissing, name)
	}
	res, err := fn.Call(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("call %q: %w", name, err)
	}
	return res, nil
}
```

### The runnable demo

The demo initializes the reactor once and then loops over five user IDs, calling
both exports on the same live instance — the reactor property made visible. Each
call is a bare function call into the warm module.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"example.com/rollflags"
)

func main() {
	ctx := context.Background()
	wasm, err := os.ReadFile("flags.wasm")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read flags.wasm (build the reactor first):", err)
		os.Exit(1)
	}

	r, err := rollflags.NewReactor(ctx, wasm)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer r.Close(ctx)

	for _, id := range []int64{1, 2, 3, 4, 5} {
		b, err := r.Bucket(ctx, id)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		in, err := r.InRollout(ctx, id, 50)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("user %d: bucket=%d in_50pct=%v\n", id, b, in)
	}
}
```

Build the reactor, then run the host:

```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o flags.wasm ./guest
# or, for a smaller, faster-loading artifact:
tinygo build -buildmode=c-shared -target=wasip1 -o flags.wasm ./guest
go run ./cmd/demo
```

Expected output:

```
user 1: bucket=65 in_50pct=false
user 2: bucket=10 in_50pct=true
user 3: bucket=53 in_50pct=false
user 4: bucket=78 in_50pct=false
user 5: bucket=18 in_50pct=true
```

### Tests

The exported functions are pure numeric logic, so the tests prove the algorithm
directly. `TestBucketDeterministicInRange` asserts `Bucket` is stable across calls
and always lands in `[0, 100)`. `TestInRollout` covers the pinned boundaries
(`pct <= 0`, `pct >= 100`) and monotonicity: a user in the rollout at some
percentage is still in it at a higher one. The `Example` uses the boundary values,
so its output does not depend on the hash.

Create `rollout_test.go`:

```go
package rollflags

import (
	"fmt"
	"testing"
)

func TestBucketDeterministicInRange(t *testing.T) {
	t.Parallel()
	for _, id := range []int64{0, 1, -1, 42, 1 << 40, -(1 << 40)} {
		first := Bucket(id)
		if got := Bucket(id); got != first {
			t.Fatalf("Bucket(%d) not deterministic: %d then %d", id, first, got)
		}
		if first < 0 || first >= 100 {
			t.Fatalf("Bucket(%d) = %d, want [0,100)", id, first)
		}
	}
}

func TestInRollout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		userID int64
		pct    int32
		want   bool
	}{
		{"zero pct never in", 42, 0, false},
		{"negative pct never in", 42, -10, false},
		{"full pct always in", 42, 100, true},
		{"over 100 always in", 42, 200, true},
		// Bucket(1) == 65, so 65 is out (65 < 65 is false) and 66 is in.
		{"below own bucket out", 1, 65, false},
		{"at own bucket in", 1, 66, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := InRollout(tc.userID, tc.pct); got != tc.want {
				t.Fatalf("InRollout(%d, %d) = %v, want %v", tc.userID, tc.pct, got, tc.want)
			}
		})
	}
}

func TestInRolloutMonotonic(t *testing.T) {
	t.Parallel()
	const id int64 = 7
	prev := false
	for pct := int32(0); pct <= 100; pct++ {
		got := InRollout(id, pct)
		if prev && !got {
			t.Fatalf("InRollout(%d, %d) fell out of rollout after being in", id, pct)
		}
		prev = got
	}
}

func ExampleInRollout() {
	fmt.Println(InRollout(42, 0), InRollout(42, 100))
	// Output: false true
}
```

## Review

The reactor is correct when initialization happens once and the exports persist.
The defining mistake is treating the guest like a command: instantiating without
`WithStartFunctions("_initialize")` (so the host tries to run `_start`, which a
c-shared build does not emit) or building the guest without `-buildmode=c-shared`
(so no exports exist and `ExportedFunction("bucket")` returns nil, surfacing as
`ErrExportMissing`). If `NewReactor` fails at instantiation, the reactor build
flags are the first suspect; if the calls fail with `ErrExportMissing`, the export
name or the build mode is wrong.

The ABI is correct when everything on the wire is a scalar. `Bucket` and
`InRollout` encode with `api.EncodeI64`/`api.EncodeI32` and decode with
`api.DecodeI32`; there is deliberately no string or struct crossing the boundary,
because the `//go:wasmexport` convention is `[]uint64` in and `[]uint64` out.
Confirm the numeric logic with the pure tests — `Bucket` deterministic and in
range, `InRollout` respecting its boundaries — and note that `Bucket(1) == 65` is
what makes the demo's `user 1 ... in_50pct=false` and the "below own bucket" test
row agree.

## Resources

- [Extensible Wasm Applications with Go](https://go.dev/blog/wasmexport) — `//go:wasmexport`, reactors, and `-buildmode=c-shared`, from the Go blog.
- [Go 1.24 Release Notes - WebAssembly](https://go.dev/doc/go1.24#wasm) — the `go:wasmexport` directive, the c-shared reactor build, and the relaxed `wasmimport` types.
- [wazero/api package](https://pkg.go.dev/github.com/tetratelabs/wazero/api) — `Module.ExportedFunction`, `Function.Call`, and `EncodeI32`/`EncodeI64`/`DecodeI32`.
- [wazero ModuleConfig.WithStartFunctions](https://pkg.go.dev/github.com/tetratelabs/wazero#ModuleConfig) — selecting `_initialize` for a reactor instead of the default `_start`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-wasi-command-filter.md](01-wasi-command-filter.md) | Next: [03-wasi-capabilities-sandbox.md](03-wasi-capabilities-sandbox.md)
