# Exercise 4: Self-Registering Plugins via Blank Import (database/sql Pattern)

This is the one production-legitimate use of `init()` for registration: a plugin
package registers itself into a shared registry from its `init()`, and the
application activates it purely by blank import — `import _ "app/plugins/json"` —
with no direct symbol dependency. It is exactly how `database/sql` drivers, image
decoders, and `net/http/pprof` are wired.

This module is fully self-contained: a base `codec` registry plus two real
stdlib-backed plugins (`encoding/json` and `encoding/gob`).

## What you'll build

```text
codeckit/                    independent module: example.com/codeckit
  go.mod                     module example.com/codeckit
  codec/codec.go             package codec: Register (panics on dup), Get, Names + global registry
  codec/codec_test.go        package codec_test: blank-imports both plugins, asserts discovery + dup panic
  plugins/json/json.go       package json: init() registers a json codec
  plugins/gob/gob.go         package gob: init() registers a gob codec
  cmd/demo/main.go           blank-imports both plugins and marshals through them
```

Files: `codec/codec.go`, `codec/codec_test.go`, `plugins/json/json.go`, `plugins/gob/gob.go`, `cmd/demo/main.go`.
Implement: a package-global codec registry whose `Register` panics on a duplicate name (the `sql.Register` contract), and two plugins that self-register in `init()`.
Test: blank-importing both plugins makes both discoverable via `codec.Names()`; a duplicate `Register` panics.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/04-blank-import-side-effect-registration/codec go-solutions/04-functions/08-init-functions-and-package-initialization/04-blank-import-side-effect-registration/plugins/json go-solutions/04-functions/08-init-functions-and-package-initialization/04-blank-import-side-effect-registration/plugins/gob go-solutions/04-functions/08-init-functions-and-package-initialization/04-blank-import-side-effect-registration/cmd/demo
cd go-solutions/04-functions/08-init-functions-and-package-initialization/04-blank-import-side-effect-registration
```

### Why the blank import is the whole mechanism

Blank-import registration deliberately trades one property for another. In
Exercise 1 the registry was a value the caller owned, and registration was an
explicit `Register` call in `main`. Here the registry is a package-level global,
and each plugin registers itself in `init()`. The application never names the
plugin's types; it only writes `import _ "example.com/codeckit/plugins/json"`. The
blank `_` says: "I do not use any symbol from this package, but link it in and run
its `init()`." That `init()` calls `codec.Register(jsonCodec{})`, and after
package initialization completes the codec is discoverable by name.

This is precisely the `database/sql` pattern. `sql.Register("postgres", drv)` runs
in a driver package's `init()`; your program writes `import _ ".../lib/pq"` and then
`sql.Open("postgres", dsn)`. The image decoders (`image/png`, `image/jpeg`) and
`net/http/pprof` all self-register the same way. The design lets a build select its
active plugins by import path alone, which is ideal for a set of interchangeable
codecs chosen at compile time.

The critical contract is what happens on a duplicate name. `sql.Register` *panics*
if the same name is registered twice, and this exercise copies that behavior for a
reason: a blank import is invisible at the call site, so if two imports both claim
`"json"`, silently overwriting one would hide a real wiring bug that only manifests
much later as the wrong codec being used. Panicking at initialization turns a
mis-wired import graph into an immediate, loud startup failure — the correct
trade-off for a fail-fast binary.

One structural note: because each plugin imports `codec`, the plugins cannot be
imported by the `codec` package (that would cycle). The test therefore lives in an
external test package (`package codec_test`) so it may blank-import the plugins
without closing a cycle.

Create `codec/codec.go`:

```go
// codec/codec.go
package codec

import (
	"fmt"
	"maps"
	"slices"
	"sync"
)

// Codec is a named serialization strategy.
type Codec interface {
	Name() string
	Marshal(v any) ([]byte, error)
}

var (
	mu     sync.Mutex
	codecs = make(map[string]Codec)
)

// Register adds c under its Name. Like database/sql's Register, it PANICS on a
// duplicate name: a blank import is invisible at the call site, so a double
// registration is a wiring bug that must fail loudly at startup rather than
// silently shadow a codec later.
func Register(c Codec) {
	mu.Lock()
	defer mu.Unlock()
	name := c.Name()
	if _, dup := codecs[name]; dup {
		panic(fmt.Sprintf("codec: Register called twice for %q", name))
	}
	codecs[name] = c
}

// Get returns the codec registered under name.
func Get(name string) (Codec, bool) {
	mu.Lock()
	defer mu.Unlock()
	c, ok := codecs[name]
	return c, ok
}

// Names returns the registered codec names in sorted order.
func Names() []string {
	mu.Lock()
	defer mu.Unlock()
	return slices.Sorted(maps.Keys(codecs))
}
```

Create `plugins/json/json.go`:

```go
// plugins/json/json.go
package json

import (
	stdjson "encoding/json"

	"example.com/codeckit/codec"
)

type jsonCodec struct{}

func (jsonCodec) Name() string { return "json" }

func (jsonCodec) Marshal(v any) ([]byte, error) { return stdjson.Marshal(v) }

// init self-registers the json codec. A blank import of this package triggers it.
func init() { codec.Register(jsonCodec{}) }
```

Create `plugins/gob/gob.go`:

```go
// plugins/gob/gob.go
package gob

import (
	"bytes"
	"encoding/gob"

	"example.com/codeckit/codec"
)

type gobCodec struct{}

func (gobCodec) Name() string { return "gob" }

func (gobCodec) Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// init self-registers the gob codec via a blank import.
func init() { codec.Register(gobCodec{}) }
```

### The runnable demo

`main` selects its codecs purely by blank import. It never mentions the `jsonCodec`
or `gobCodec` types; it just imports the plugin packages for their side effect and
then looks them up by name.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/codeckit/codec"

	_ "example.com/codeckit/plugins/gob"
	_ "example.com/codeckit/plugins/json"
)

func main() {
	fmt.Println("codecs:", codec.Names())

	value := map[string]int{"width": 3}

	if c, ok := codec.Get("json"); ok {
		out, _ := c.Marshal(value)
		fmt.Printf("json: %s\n", out)
	}
	if c, ok := codec.Get("gob"); ok {
		out, _ := c.Marshal(value)
		fmt.Printf("gob: %d bytes\n", len(out))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
codecs: [gob json]
json: {"width":3}
gob: 26 bytes
```

### Tests

The test is the unit under test itself: it blank-imports both plugins, so their
`init()` functions run before any test body, and then asserts both codecs are
discoverable. No `TestMain` is needed — the `init` side effects are exactly what is
being verified.

Create `codec/codec_test.go`:

```go
// codec/codec_test.go
package codec_test

import (
	"slices"
	"testing"

	"example.com/codeckit/codec"

	_ "example.com/codeckit/plugins/gob"
	_ "example.com/codeckit/plugins/json"
)

func TestBlankImportsRegisterCodecs(t *testing.T) {
	t.Parallel()

	names := codec.Names()
	for _, want := range []string{"gob", "json"} {
		if !slices.Contains(names, want) {
			t.Fatalf("codec %q not registered by blank import; Names() = %v", want, names)
		}
	}
}

func TestRegisteredCodecMarshals(t *testing.T) {
	t.Parallel()

	c, ok := codec.Get("json")
	if !ok {
		t.Fatal("json codec not found")
	}
	out, err := c.Marshal(map[string]int{"n": 7})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"n":7}` {
		t.Fatalf("json Marshal = %s, want {\"n\":7}", out)
	}
}

// stubCodec lets the test drive Register directly to observe the duplicate panic.
type stubCodec struct{ name string }

func (s stubCodec) Name() string                { return s.name }
func (s stubCodec) Marshal(any) ([]byte, error) { return nil, nil }

func TestDuplicateRegisterPanics(t *testing.T) {
	t.Parallel()

	codec.Register(stubCodec{name: "dup-under-test"})

	defer func() {
		if recover() == nil {
			t.Fatal("second Register of same name did not panic")
		}
	}()
	codec.Register(stubCodec{name: "dup-under-test"}) // must panic
}
```

## Review

The registration is correct when a blank import alone makes a codec discoverable —
`TestBlankImportsRegisterCodecs` passes only because the `init()` functions ran as
a side effect of importing the plugin packages, with no explicit `Register` call
anywhere in the test. That is the property `database/sql` relies on. If you removed
one of the blank imports, the corresponding codec would silently vanish from
`Names()`, which is the exact test/prod divergence to keep in mind: an `init()`
only runs if its package is actually imported.

The duplicate-panic contract is the other half. `TestDuplicateRegisterPanics`
registers a fresh name twice and recovers the panic; this mirrors `sql.Register`
and ensures a mis-wired double import fails at startup, loudly, rather than
shadowing a codec. The trap to avoid is "helpfully" making `Register` overwrite or
no-op on a duplicate: that hides the wiring bug the panic is designed to expose.
Use a distinct name in this test (not `"json"`/`"gob"`) so it does not collide with
the plugins the same test binary already registered.

## Resources

- [database/sql: Register](https://pkg.go.dev/database/sql#Register) — the panic-on-duplicate self-registration contract this exercise mirrors.
- [Effective Go: blank import for side effects](https://go.dev/doc/effective_go#blank_import) — why `import _ "path"` runs a package's `init`.
- [image package: registering decoders](https://pkg.go.dev/image#RegisterFormat) — another canonical blank-import registration in the standard library.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-package-var-init-order-precompiled-validators.md](05-package-var-init-order-precompiled-validators.md)
