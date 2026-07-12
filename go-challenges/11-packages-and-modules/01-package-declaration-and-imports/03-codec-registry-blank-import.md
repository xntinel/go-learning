# Exercise 3: A Driver-Style Codec Registry Wired via init() and Blank Imports

Every Go backend uses this pattern even if the team never named it: a global
registry that providers populate from `init()`, and a consumer that pulls the
providers in with `import _ "path"` purely for that side effect. It is exactly how
`database/sql` finds its drivers, how `image` finds PNG/JPEG decoders, and how
`net/http/pprof` mounts its routes. This exercise reproduces the whole mechanism
from scratch so the "magic" of blank imports stops being magic.

This module is self-contained: one module, several packages, its own tests and
demo. Nothing here imports another exercise.

## What you'll build

```text
codecreg/                          module: example.com/codecreg
  go.mod
  registry/registry.go             package registry: Codec, Register, Get, List, ErrNotFound
  codec/jsoncodec/jsoncodec.go     package jsoncodec: registers "json" in init()
  codec/gobcodec/gobcodec.go       package gobcodec: registers "gob" in init()
  registry/registry_test.go        package registry_test: blank-imports both codecs, exercises the registry
  cmd/demo/main.go                 blank-imports both codecs, lists and uses them
```

- Files: the five above.
- Implement: a `sync.RWMutex`-guarded registry that panics on duplicate registration (like `database/sql.Register`), plus two codec packages that self-register in `init()`.
- Test: after blank-importing both codecs, `Get("json")`/`Get("gob")` return non-nil; `Get("missing")` returns `ErrNotFound`; a duplicate `Register` panics; `List()` is sorted.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/01-package-declaration-and-imports/03-codec-registry-blank-import/registry go-solutions/11-packages-and-modules/01-package-declaration-and-imports/03-codec-registry-blank-import/codec/jsoncodec \
  go-solutions/11-packages-and-modules/01-package-declaration-and-imports/03-codec-registry-blank-import/codec/gobcodec go-solutions/11-packages-and-modules/01-package-declaration-and-imports/03-codec-registry-blank-import/cmd/demo
cd go-solutions/11-packages-and-modules/01-package-declaration-and-imports/03-codec-registry-blank-import
go mod edit -go=1.26
```

### The registry: a global mutable map, guarded and panic-on-dup

The registry is package-level global state: a `map[string]Codec` behind a
`sync.RWMutex`. Reads (`Get`, `List`) take the read lock; `Register` takes the
write lock. `Register` *panics* on a duplicate name, matching `database/sql`'s
behavior precisely — the reasoning is that registration happens at program start
from `init()`, so a duplicate key is a programming error that should fail loudly at
startup rather than silently shadow one provider with another. `Get` returns a
wrapped `ErrNotFound` so callers can branch with `errors.Is`. This is genuinely
hidden global state, which is the trade-off: it makes wiring trivial (`import _`)
but means the set of available codecs depends on which packages happened to be
imported into the final binary.

Create `registry/registry.go`:

```go
package registry

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrNotFound is returned by Get for an unregistered name.
var ErrNotFound = errors.New("registry: codec not found")

// Codec is the behavior a provider must supply to register itself.
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

var (
	mu     sync.RWMutex
	codecs = map[string]Codec{}
)

// Register adds a codec under name. It panics on a duplicate name or a nil
// codec, mirroring database/sql.Register: registration runs from init(), so a
// clash is a startup-time programming error, not a runtime condition.
func Register(name string, c Codec) {
	mu.Lock()
	defer mu.Unlock()
	if c == nil {
		panic("registry: Register codec is nil")
	}
	if _, dup := codecs[name]; dup {
		panic(fmt.Sprintf("registry: Register called twice for %q", name))
	}
	codecs[name] = c
}

// Get returns the codec registered under name, or a wrapped ErrNotFound.
func Get(name string) (Codec, error) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := codecs[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return c, nil
}

// List returns the registered names in sorted order.
func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(codecs))
	for n := range codecs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
```

### The providers self-register in init()

Each codec package imports the registry and calls `registry.Register` from its
`init()`. The concrete `codec` type is *unexported* — a consumer never names it,
it only ever reaches the codec through `registry.Get`. This is the key to the
whole pattern: the provider package exports nothing a consumer must reference, so
the only way to "use" it is the blank import that runs its `init()`.

Create `codec/jsoncodec/jsoncodec.go`:

```go
package jsoncodec

import (
	"encoding/json"

	"example.com/codecreg/registry"
)

type codec struct{}

func (codec) Marshal(v any) ([]byte, error)   { return json.Marshal(v) }
func (codec) Unmarshal(d []byte, v any) error { return json.Unmarshal(d, v) }

func init() { registry.Register("json", codec{}) }
```

Create `codec/gobcodec/gobcodec.go`:

```go
package gobcodec

import (
	"bytes"
	"encoding/gob"

	"example.com/codecreg/registry"
)

type codec struct{}

func (codec) Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (codec) Unmarshal(d []byte, v any) error {
	return gob.NewDecoder(bytes.NewReader(d)).Decode(v)
}

func init() { registry.Register("gob", codec{}) }
```

### The demo consumes them via blank imports

Create `cmd/demo/main.go`. Note the blank imports: the demo never names
`jsoncodec` or `gobcodec` in code, yet they must be imported so their `init()`
runs and registers the codecs. Delete either blank import and that codec vanishes
from `registry.List()` — the imports are load-bearing, not decorative.

```go
package main

import (
	"fmt"

	"example.com/codecreg/registry"

	_ "example.com/codecreg/codec/gobcodec"
	_ "example.com/codecreg/codec/jsoncodec"
)

func main() {
	fmt.Println("registered:", registry.List())

	c, err := registry.Get("json")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	data, _ := c.Marshal(map[string]int{"answer": 42})
	fmt.Println("json:", string(data))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
registered: [gob json]
json: {"answer":42}
```

### Tests

The test lives in the *external test package* `registry_test`, in the same
directory as `registry`. That matters: an in-package test (`package registry`)
could not import `jsoncodec`, because `jsoncodec` imports `registry` — that would
be an import cycle. The external test package is allowed to import packages that
depend on `registry`, so it can blank-import the providers and observe their
registration effect. Those blank imports are what make `Get("json")` succeed;
without them the test would see an empty registry.

`TestDuplicateRegisterPanics` needs a non-nil `Codec` to trigger the duplicate
path, so the test file defines a tiny stand-in type `dupCodec`. The registry's
duplicate check fires before the codec value is ever stored, so this test does not
corrupt the registry state the other tests observe.

Create `registry/registry_test.go`:

```go
package registry_test

import (
	"errors"
	"reflect"
	"testing"

	"example.com/codecreg/registry"

	_ "example.com/codecreg/codec/gobcodec"
	_ "example.com/codecreg/codec/jsoncodec"
)

// dupCodec is a non-nil Codec used only to hit the duplicate-name branch.
type dupCodec struct{}

func (dupCodec) Marshal(v any) ([]byte, error)   { return nil, nil }
func (dupCodec) Unmarshal(d []byte, v any) error { return nil }

func TestBlankImportsRegisterCodecs(t *testing.T) {
	for _, name := range []string{"json", "gob"} {
		c, err := registry.Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		if c == nil {
			t.Fatalf("Get(%q) returned nil codec", name)
		}
	}
}

func TestGetMissing(t *testing.T) {
	_, err := registry.Get("msgpack") // no package registers this
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestListSorted(t *testing.T) {
	got := registry.List()
	if !reflect.DeepEqual(got, []string{"gob", "json"}) {
		t.Fatalf("List() = %v, want [gob json]", got)
	}
}

func TestDuplicateRegisterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	registry.Register("json", dupCodec{}) // "json" already registered
}

func TestRoundTrip(t *testing.T) {
	c, err := registry.Get("json")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	data, err := c.Marshal(map[string]int{"n": 7})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out map[string]int
	if err := c.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out["n"] != 7 {
		t.Fatalf("round-trip = %v, want n:7", out)
	}
}
```

## Review

The registry is correct when registration is write-locked, lookups are
read-locked, a duplicate name panics at registration time, and `Get` reports a
wrapped `ErrNotFound` for anything unregistered. The design lesson is that the
blank imports are the API: the providers export nothing you reference, so the only
way to activate them is `import _`, and removing one silently drops a codec at
runtime — a compile-clean change that breaks production. That invisibility is the
cost of the convenience, which is why real registries (`database/sql`) panic
loudly on the one thing that is unambiguously a bug: registering the same name
twice. Keep the concrete codec types unexported so consumers cannot bypass the
registry, and always run `go test -race` — a registry is shared mutable state and
the lock must actually hold under concurrent access.

## Resources

- [`database/sql.Register`](https://pkg.go.dev/database/sql#Register) — the canonical panic-on-duplicate driver registry this mirrors.
- [`image.RegisterFormat`](https://pkg.go.dev/image#RegisterFormat) — the decoder registry behind `_ "image/png"`.
- [Go Blog: Package names](https://go.dev/blog/package-names) — why provider packages keep a tiny exported surface.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-grep-cli-executable.md](02-grep-cli-executable.md) | Next: [04-import-alias-versioned-apis.md](04-import-alias-versioned-apis.md)
