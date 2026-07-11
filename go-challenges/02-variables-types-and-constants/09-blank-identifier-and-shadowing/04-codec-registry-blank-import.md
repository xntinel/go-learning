# Exercise 4: Format Registry: Side-Effect Blank Imports at the Composition Root

A pluggable codec registry, in the shape of `image.RegisterFormat` and
`database/sql` driver registration: a core package exposes `Register`, each codec
package registers itself in `init()`, and a consumer activates a codec with a
blank import. This teaches why side-effect imports belong at the composition root
and how a missing one yields a runtime "unknown codec" error, not a compile
failure.

This module is fully self-contained: its own `go mod init`, all code inline
across three packages, its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
codecreg/                       module: example.com/codecreg
  go.mod
  codec.go                      package codec: Codec, Register, Lookup, List, ErrUnknownCodec
  gzipcodec/
    gzip.go                     package gzipcodec: init() registers a real gzip codec
  cmd/
    demo/
      main.go                   blank-imports gzipcodec, round-trips, shows an absent codec
  codec_test.go                 package codec: registration, unknown-error, List sorted, -race
  codec_ext_test.go             package codec_test: blank import proves activation
```

- Files: `codec.go`, `gzipcodec/gzip.go`, `cmd/demo/main.go`, `codec_test.go`, `codec_ext_test.go`.
- Implement: `Register(name, Codec)` (panics on duplicate), `Lookup(name) (Codec, error)` returning a wrapped `ErrUnknownCodec`, and a sorted `List()`, all guarded by a `sync.RWMutex`.
- Test: a test-only codec self-registering in `init()`; `Lookup` finds registered codecs and errors on unknown names (`errors.Is`); `List` is sorted; `Register`/`Lookup` are race-safe; a blank import activates the gzip codec.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/codecreg/gzipcodec ~/go-exercises/codecreg/cmd/demo
cd ~/go-exercises/codecreg
go mod init example.com/codecreg
```

### Why the blank import lives at the composition root

The registry is a package-level map guarded by an `RWMutex`. A codec makes itself
available by calling `Register` from its `init()` function; that `init` runs only
if the package is imported. A codec package exports nothing the consumer needs to
name, so the consumer imports it for its side effect alone:

```go
import _ "example.com/codecreg/gzipcodec"
```

That is a blank import. It runs `gzipcodec.init()` — which registers the gzip
codec — and imports no identifier. The critical property: activation is a runtime
fact, established by init ordering, not a compile-time one. If you *forget* the
blank import, the code still compiles; it fails later, at `Lookup("gzip")`, with a
typed `ErrUnknownCodec`. That is exactly why the import belongs at the composition
root (`main`, or a wiring package): the set of active codecs is a deployment
decision, and burying `import _ "…/gzipcodec"` inside a leaf business package
would couple that package's behavior to hidden init ordering and hide the real
dependency. `Lookup` uses the comma-ok map read (`c, ok := registry[name]`) so a
missing codec is a clean error, never a nil returned as if it were valid.

Create `codec.go`:

```go
package codec

import (
	"fmt"
	"sort"
	"sync"
)

// Codec encodes and decodes a payload under a registered name.
type Codec interface {
	Name() string
	Encode([]byte) []byte
	Decode([]byte) ([]byte, error)
}

// ErrUnknownCodec is the sentinel Lookup wraps for an unregistered name.
var ErrUnknownCodec = fmt.Errorf("codec: unknown codec")

var (
	mu       sync.RWMutex
	registry = map[string]Codec{}
)

// Register adds c under name; a codec package calls it from init(). It panics on
// a duplicate, matching database/sql and image registration conventions.
func Register(name string, c Codec) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[name]; dup {
		panic("codec: Register called twice for " + name)
	}
	registry[name] = c
}

// Lookup returns the codec registered under name, or a wrapped ErrUnknownCodec.
func Lookup(name string) (Codec, error) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("%q: %w", name, ErrUnknownCodec)
	}
	return c, nil
}

// List returns a sorted snapshot of registered codec names.
func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
```

The gzip codec is a real one built on `compress/gzip`. It registers itself in
`init()`.

Create `gzipcodec/gzip.go`:

```go
package gzipcodec

import (
	"bytes"
	"compress/gzip"
	"io"

	"example.com/codecreg"
)

type gzipCodec struct{}

func (gzipCodec) Name() string { return "gzip" }

func (gzipCodec) Encode(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func (gzipCodec) Decode(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func init() {
	codec.Register("gzip", gzipCodec{})
}
```

### The runnable demo

The demo blank-imports `gzipcodec` (its only reason to touch that package),
round-trips a payload, and shows that an un-imported codec is an "unknown codec"
error at runtime.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/codecreg"
	_ "example.com/codecreg/gzipcodec" // side-effect import activates the gzip codec
)

func main() {
	fmt.Println("registered:", codec.List())

	c, err := codec.Lookup("gzip")
	if err != nil {
		fmt.Println("lookup gzip:", err)
		return
	}

	payload := []byte("log line log line log line")
	roundTripped, _ := c.Decode(c.Encode(payload))
	fmt.Println("gzip round-trip ok:", string(roundTripped) == string(payload))

	if _, err := codec.Lookup("brotli"); err != nil {
		fmt.Println("lookup brotli:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
registered: [gzip]
gzip round-trip ok: true
lookup brotli: "brotli": codec: unknown codec
```

### Tests

The internal test registers a test-only codec in `init()` and checks lookup,
sorting, and race safety. The external test blank-imports `gzipcodec` and proves
the import alone made the codec available.

Create `codec_test.go`:

```go
package codec

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

type fakeCodec struct{ name string }

func (f fakeCodec) Name() string { return f.name }

func (fakeCodec) Encode(b []byte) []byte { return b }

func (fakeCodec) Decode(b []byte) ([]byte, error) { return b, nil }

func init() {
	Register("fake", fakeCodec{name: "fake"})
}

func TestLookupFindsRegistered(t *testing.T) {
	c, err := Lookup("fake")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "fake" {
		t.Fatalf("Name() = %q, want fake", c.Name())
	}
}

func TestLookupUnknown(t *testing.T) {
	_, err := Lookup("nope")
	if !errors.Is(err, ErrUnknownCodec) {
		t.Fatalf("error = %v, want ErrUnknownCodec", err)
	}
}

func TestListSorted(t *testing.T) {
	names := List()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("List() not sorted at %d: %v", i, names)
		}
	}
}

func TestRegistryRace(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("race-%d", i)
			Register(name, fakeCodec{name: name})
			_, _ = Lookup(name)
			_ = List()
		}()
	}
	wg.Wait()
}

func ExampleLookup() {
	_, err := Lookup("does-not-exist")
	fmt.Println(err)
	// Output: "does-not-exist": codec: unknown codec
}
```

Create `codec_ext_test.go`:

```go
package codec_test

import (
	"testing"

	"example.com/codecreg"
	_ "example.com/codecreg/gzipcodec" // blank import must activate the gzip codec
)

func TestBlankImportActivatesGzip(t *testing.T) {
	c, err := codec.Lookup("gzip")
	if err != nil {
		t.Fatalf("gzip not registered after blank import: %v", err)
	}

	payload := []byte("hello registry")
	got, err := c.Decode(c.Encode(payload))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("round-trip = %q, want %q", got, payload)
	}
}
```

## Review

The registry is correct when activation is decoupled from compilation: a codec is
present only if its package was imported, and a missing one is a typed runtime
error, never a nil masquerading as a value. `TestLookupUnknown` proves the error
path via `errors.Is`; `TestBlankImportActivatesGzip` proves the blank import is
what makes gzip appear — remove the `import _` line and that test fails at
`Lookup`. `TestRegistryRace` under `-race` proves the `RWMutex` actually guards
the map, and `List` returning a sorted snapshot proves reads never expose the
live map. The discipline: keep side-effect imports at `main` or a wiring package,
and never at a leaf.

## Resources

- [`image.RegisterFormat`](https://pkg.go.dev/image#RegisterFormat) — the canonical registration-via-init pattern this mirrors.
- [`database/sql.Register`](https://pkg.go.dev/database/sql#Register) — driver registration and the blank-import convention.
- [Effective Go: The blank identifier for side effect](https://go.dev/doc/effective_go#blank_import) — why and where blank imports belong.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-interface-compliance-guards.md](05-interface-compliance-guards.md)
