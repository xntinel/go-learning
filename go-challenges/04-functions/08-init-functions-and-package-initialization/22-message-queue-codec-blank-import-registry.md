# Exercise 22: Message Codecs That Self-Register via Blank Import and init()

**Nivel: Intermedio** — validacion rapida (un test corto).

`database/sql` lets a driver register itself just by being blank-imported —
`import _ "some/driver"` — because the driver's own `init()` calls
`sql.Register`. This exercise builds the same pattern for message
serialization codecs: a `registry` package holds a name-keyed map of
`Codec`s, and separate `jsoncodec` and `gobcodec` packages register
themselves into it purely as a side effect of being imported, with no
explicit wiring call anywhere in application code.

## What you'll build

```text
codecs/                    independent module: example.com/codecs
  go.mod                    module example.com/codecs
  registry/
    registry.go              Codec interface, Register, Get, Names, sentinels
    registry_test.go          register/get/duplicate-panics/sorted-names, own stub codecs
  jsoncodec/
    jsoncodec.go             registers "json" via init()
    jsoncodec_test.go         proves self-registration by importing only this package
  gobcodec/
    gobcodec.go               registers "gob" via init() (stands in for a binary format)
    gobcodec_test.go           proves self-registration by importing only this package
  cmd/
    demo/
      main.go                 blank-imports both codecs, roundtrips a message through each
```

Files: `registry/registry.go`, `jsoncodec/jsoncodec.go`, `gobcodec/gobcodec.go`, `cmd/demo/main.go`, `registry/registry_test.go`, `jsoncodec/jsoncodec_test.go`, `gobcodec/gobcodec_test.go`.
Implement: `registry.Codec` interface with `Register`/`Get`/`Names`; `jsoncodec` and `gobcodec`, each calling `registry.Register` from its own `init()`.
Test: `registry` alone with stub codecs (register, get, duplicate-name panic, sorted names); each codec package's own test proves it self-registers merely by being imported, with no explicit call into it.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/codecs/registry
mkdir -p ~/go-exercises/codecs/jsoncodec
mkdir -p ~/go-exercises/codecs/gobcodec
mkdir -p ~/go-exercises/codecs/cmd/demo
cd ~/go-exercises/codecs
go mod init example.com/codecs
go mod edit -go=1.24
```

### Why blank-import self-registration, and its real cost

`registry.Register` is called from `jsoncodec`'s and `gobcodec`'s own
`init()` functions, which run the instant those packages are imported —
including with the blank identifier, `_ "example.com/codecs/jsoncodec"`,
which imports a package purely for its side effects and refuses to let
anything reference its exported names directly. This is precisely how
`database/sql` drivers, `image` format decoders, and `net/http/pprof`
register themselves: the application chooses which codecs it supports simply
by choosing which packages it imports, with zero wiring code anywhere in
`main`.

Like `database/sql.Register`, `registry.Register` panics on a duplicate
name rather than returning an error — a second package trying to claim
"json" is a build-time programming mistake, not a runtime condition any
caller is in a position to recover from.

The cost, and the reason an earlier exercise in this chapter built a
registry the opposite way — returned fresh by `New()`, with no global at all
— is real: `registry`'s own tests below mutate one shared package-level map,
so they cannot use `t.Parallel()` the way the `New()`-based registry's tests
could. Each test calls an unexported `reset()` between runs to get a clean
slate. That tradeoff is the whole point of this exercise: blank-import
self-registration buys convenient wiring at the cost of the tests around it
losing the independence a caller-owned value would have given them for free.

Create `registry/registry.go`:

```go
// registry/registry.go
// Package registry is a name-keyed registry of message codecs, mirroring
// the database/sql driver pattern: codec implementations self-register from
// their own init() behind a blank import, so the set of wire formats a
// binary supports is decided by which codec packages it imports, not by
// this package's source code.
package registry

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
)

var (
	// ErrCodecExists mirrors database/sql's "Register called twice"
	// panic: a duplicate name is a programming error, not a runtime
	// condition to recover from.
	ErrCodecExists = errors.New("codec already registered")
	// ErrCodecNotFound is returned by Get for an unregistered name.
	ErrCodecNotFound = errors.New("codec not found")
)

// Codec marshals and unmarshals values for one wire format.
type Codec interface {
	Name() string
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

var (
	mu     sync.RWMutex
	codecs = map[string]Codec{}
)

// Register adds c under its Name, called from a codec package's init().
// Like database/sql.Register, a duplicate name is a programming error and
// Register panics rather than returning an error nobody at init time is
// positioned to handle.
func Register(c Codec) {
	mu.Lock()
	defer mu.Unlock()
	name := c.Name()
	if _, ok := codecs[name]; ok {
		panic(fmt.Errorf("%w: %s", ErrCodecExists, name))
	}
	codecs[name] = c
}

// Get returns the codec registered under name.
func Get(name string) (Codec, error) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := codecs[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrCodecNotFound, name)
	}
	return c, nil
}

// Names returns every registered codec name in sorted order.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	return slices.Sorted(maps.Keys(codecs))
}

// reset clears the registry. It is unexported and exists only so this
// package's own tests can start from a known-empty registry instead of
// depending on init side effects left over from another test — unlike a
// caller of the public API, a test in this package can reach into that
// state directly.
func reset() {
	mu.Lock()
	defer mu.Unlock()
	codecs = map[string]Codec{}
}
```

Create `jsoncodec/jsoncodec.go`:

```go
// jsoncodec/jsoncodec.go
// Package jsoncodec registers a JSON registry.Codec purely as a side effect
// of being imported. A caller never calls anything in this package directly
// — it is imported with the blank identifier for its init() alone.
package jsoncodec

import (
	"encoding/json"

	"example.com/codecs/registry"
)

type codec struct{}

func (codec) Name() string { return "json" }

func (codec) Marshal(v any) ([]byte, error) { return json.Marshal(v) }

func (codec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func init() {
	registry.Register(codec{})
}
```

Create `gobcodec/gobcodec.go`:

```go
// gobcodec/gobcodec.go
// Package gobcodec registers a gob-based registry.Codec purely as a side
// effect of being imported. It stands in for a binary wire format like
// Protobuf or MessagePack — the self-registration pattern is identical
// regardless of which format actually does the encoding.
package gobcodec

import (
	"bytes"
	"encoding/gob"

	"example.com/codecs/registry"
)

type codec struct{}

func (codec) Name() string { return "gob" }

func (codec) Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (codec) Unmarshal(data []byte, v any) error {
	return gob.NewDecoder(bytes.NewReader(data)).Decode(v)
}

func init() {
	registry.Register(codec{})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/codecs/registry"

	_ "example.com/codecs/gobcodec"
	_ "example.com/codecs/jsoncodec"
)

// Message is roundtripped through every registered codec below.
type Message struct {
	ID   int
	Body string
}

func main() {
	fmt.Println("registered codecs:", registry.Names())

	for _, name := range registry.Names() {
		c, err := registry.Get(name)
		if err != nil {
			fmt.Println("get:", err)
			continue
		}
		data, err := c.Marshal(Message{ID: 1, Body: "hello"})
		if err != nil {
			fmt.Println("marshal:", err)
			continue
		}
		var out Message
		if err := c.Unmarshal(data, &out); err != nil {
			fmt.Println("unmarshal:", err)
			continue
		}
		fmt.Printf("%s roundtrip: %+v\n", name, out)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
registered codecs: [gob json]
gob roundtrip: {ID:1 Body:hello}
json roundtrip: {ID:1 Body:hello}
```

Note that `cmd/demo` never calls anything named `jsoncodec.` or `gobcodec.`
— both blank imports exist purely to trigger their `init()`, and `main`
interacts only with `registry`.

### Tests

Create `registry/registry_test.go`:

```go
// registry/registry_test.go
package registry

import (
	"errors"
	"testing"
)

// stubCodec is an in-test Codec; it never depends on any real wire format.
type stubCodec struct{ name string }

func (s stubCodec) Name() string { return s.name }

func (s stubCodec) Marshal(v any) ([]byte, error) { return []byte(s.name), nil }

func (s stubCodec) Unmarshal(data []byte, v any) error { return nil }

// These tests mutate the single package-level registry directly, so unlike
// the New()-per-call registries elsewhere in this chapter, they cannot run
// with t.Parallel(): the whole point of the blank-import pattern is a
// shared, global set of codecs, and that convenience is exactly what
// prevents test isolation here. Each test calls reset() to start clean.

func TestRegisterAndGet(t *testing.T) {
	t.Cleanup(reset)
	Register(stubCodec{name: "stub-a"})

	c, err := Get("stub-a")
	if err != nil {
		t.Fatalf("Get(stub-a) error = %v", err)
	}
	if c.Name() != "stub-a" {
		t.Fatalf("Get returned codec named %q, want stub-a", c.Name())
	}
}

func TestGetUnknownIsNotFound(t *testing.T) {
	t.Cleanup(reset)
	_, err := Get("does-not-exist")
	if !errors.Is(err, ErrCodecNotFound) {
		t.Fatalf("Get(unknown) err = %v, want ErrCodecNotFound", err)
	}
}

func TestRegisterTwicePanics(t *testing.T) {
	t.Cleanup(reset)
	Register(stubCodec{name: "dup"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate Register, got none")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, ErrCodecExists) {
			t.Fatalf("recovered value = %v, want error wrapping ErrCodecExists", r)
		}
	}()
	Register(stubCodec{name: "dup"})
}

func TestNamesSorted(t *testing.T) {
	t.Cleanup(reset)
	Register(stubCodec{name: "zeta"})
	Register(stubCodec{name: "alpha"})

	got := Names()
	want := []string{"alpha", "zeta"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}
```

Create `jsoncodec/jsoncodec_test.go`:

```go
// jsoncodec/jsoncodec_test.go
package jsoncodec

import (
	"testing"

	"example.com/codecs/registry"
)

// TestSelfRegisters proves the pattern this exercise is about: merely
// importing this package (jsoncodec's own test binary always does, since
// the package under test is itself) is enough for its codec to be
// registered — no explicit call into jsoncodec is needed.
func TestSelfRegisters(t *testing.T) {
	c, err := registry.Get("json")
	if err != nil {
		t.Fatalf("json codec did not self-register: %v", err)
	}
	data, err := c.Marshal(map[string]int{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]int
	if err := c.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out["a"] != 1 {
		t.Fatalf("roundtrip mismatch: %v", out)
	}
}
```

Create `gobcodec/gobcodec_test.go`:

```go
// gobcodec/gobcodec_test.go
package gobcodec

import (
	"testing"

	"example.com/codecs/registry"
)

func TestSelfRegisters(t *testing.T) {
	type payload struct{ A int }

	c, err := registry.Get("gob")
	if err != nil {
		t.Fatalf("gob codec did not self-register: %v", err)
	}
	data, err := c.Marshal(payload{A: 7})
	if err != nil {
		t.Fatal(err)
	}
	var out payload
	if err := c.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.A != 7 {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
}
```

## Review

`jsoncodec_test.go` and `gobcodec_test.go` each prove self-registration in
the strongest available way: their test binary imports only the package
under test (plus `registry`), yet `registry.Get` still finds the codec —
proof that `init()` alone, triggered by the import, did the registering.
`registry`'s own tests, using `stubCodec`, prove the registry mechanism
independently of any real wire format, including the duplicate-name panic
that mirrors `database/sql.Register`'s real behavior.

The tradeoff to sit with is the one called out above: this registry cannot
give its own tests the parallel independence the `New()`-based registry
exercise achieved, because a shared global is the entire mechanism blank-import
registration depends on. Reach for this pattern when a binary's supported
codecs really should be decided by its import list; reach for a caller-owned
`New()`-returned registry when tests (or callers) need to control the active
set directly instead.

## Resources

- [database/sql: Register](https://pkg.go.dev/database/sql#Register) — the canonical self-registering driver pattern this exercise mirrors for codecs.
- [encoding/gob](https://pkg.go.dev/encoding/gob) — the stdlib binary codec standing in for Protobuf or MessagePack here.
- [Go spec — Import declarations](https://go.dev/ref/spec#Import_declarations) — the blank identifier import (`_ "pkg"`) that runs a package's `init()` for its side effects alone.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-oauth2-provider-explicit-constructor.md](21-oauth2-provider-explicit-constructor.md) | Next: [23-graceful-shutdown-handler-collection-stack.md](23-graceful-shutdown-handler-collection-stack.md)
