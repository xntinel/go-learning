# Exercise 7: Nil Maps in the Wild: safe reads, write panics, and zero-value struct fields

A struct's map field starts life as `nil`, silently, whenever a constructor did
not initialize it or a config section was absent. This exercise builds a config
loader around that reality: reading and ranging a nil map is safe and `len` is
zero, but the first write panics — and a lazy-init guard, a constructor, or even
`json.Unmarshal` fixes it.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
niloader/                  independent module: example.com/niloader
  go.mod
  loader.go                Config with a possibly-nil Settings map; Lookup, Count, Set (lazy-init guard)
  cmd/
    demo/
      main.go              runnable demo: read nil map, lazy-init on write, JSON-decode into nil map
  loader_test.go           nil read, nil range, len(nil), write-panic, lazy Set, json.Unmarshal populates
```

- Files: `loader.go`, `cmd/demo/main.go`, `loader_test.go`.
- Implement: `Config` whose `Settings map[string]string` may be nil; `Lookup` (safe read), `Count` (`len` of possibly-nil map), and `Set` with a lazy-init guard before the first write.
- Test: reading a nil map returns zero+false, ranging runs zero iterations, `len(nil)==0`, writing to a raw nil map panics, `Set` initializes and accepts writes, and `json.Unmarshal` populates a previously-nil field.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/07-nil-map-failure-modes/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/07-nil-map-failure-modes
```

### What is and isn't safe on a nil map

The zero value of a map type is `nil`, and a struct field of map type that no
constructor touched is exactly that. The asymmetry to internalize: **reads are
safe, the first write panics.** On a nil map, `v, ok := m[k]` returns the value
type's zero and `ok == false`; `len(m)` is `0`; `for range m` runs zero
iterations. None of that panics — it is all defined behavior, which is precisely
why a nil-map bug can hide: every read-only code path works, and only the first
write (`m[k] = v`) panics with `assignment to entry in nil map`.

So `Lookup` and `Count` need no guard — they read a possibly-nil `Settings`
directly and behave correctly. Only `Set` needs the lazy-init guard:

```go
if c.Settings == nil {
	c.Settings = make(map[string]string)
}
c.Settings[key] = value
```

The alternative fixes are a constructor that always `make`s the map, or accepting
input through `json.Unmarshal`, which allocates the map for you when it decodes a
JSON object into a nil map field. That last one is a double-edged sword: it means
a loader that only ever receives config via JSON can look correct forever, because
the decoder hides the missing initialization — until someone adds a code path that
writes to the field directly and hits the panic in production. Knowing that reads
and JSON-decoding both paper over the nil is what lets you spot the one write that
does not.

Create `loader.go`:

```go
package niloader

// Config models a config section whose Settings map may be nil when the section
// was absent or no constructor initialized it.
type Config struct {
	Name     string            `json:"name"`
	Settings map[string]string `json:"settings"`
}

// Lookup reads a setting. It is safe on a nil Settings map: a nil map reads as
// empty, returning ("", false).
func (c *Config) Lookup(key string) (string, bool) {
	v, ok := c.Settings[key]
	return v, ok
}

// Count reports the number of settings. len of a nil map is 0.
func (c *Config) Count() int {
	return len(c.Settings)
}

// Set writes a setting, lazily initializing the map on first write so the nil
// zero value does not panic.
func (c *Config) Set(key, value string) {
	if c.Settings == nil {
		c.Settings = make(map[string]string)
	}
	c.Settings[key] = value
}
```

### The runnable demo

The demo starts with a `Config` whose `Settings` is nil, reads it safely, writes
through the lazy-init `Set`, and then decodes JSON into a separate nil-mapped
config to show the decoder allocates.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/niloader"
)

func main() {
	c := &niloader.Config{Name: "svc"} // Settings is nil

	fmt.Println("count on nil:", c.Count())
	_, ok := c.Lookup("timeout")
	fmt.Println("lookup missing ok:", ok)

	c.Set("timeout", "30s") // lazy-init on first write
	v, _ := c.Lookup("timeout")
	fmt.Println("after Set timeout:", v)

	var decoded niloader.Config // Settings starts nil
	_ = json.Unmarshal([]byte(`{"name":"svc2","settings":{"region":"eu"}}`), &decoded)
	fmt.Println("decoded region:", decoded.Settings["region"])
	fmt.Println("decoded count:", decoded.Count())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
count on nil: 0
lookup missing ok: false
after Set timeout: 30s
decoded region: eu
decoded count: 1
```

The first two lines prove reads on a nil map are safe. `Set` initializes the map on
the first write. The JSON decode populates a previously-nil `Settings` because the
decoder allocated it.

### Tests

Each test documents one nil-map behavior as an assertion. `TestReadNilReturnsZero`
and `TestRangeNilZeroIterations` and `TestLenNilIsZero` pin the safe operations.
`TestWriteToNilPanics` reproduces the panic on a raw nil map and asserts the
message. `TestSetLazyInits` proves the guard makes writes work. `TestJSONPopulatesNil`
proves the decoder allocates.

Create `loader_test.go`:

```go
package niloader

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestReadNilReturnsZero(t *testing.T) {
	t.Parallel()

	c := &Config{} // Settings is nil
	if v, ok := c.Lookup("x"); ok || v != "" {
		t.Fatalf("Lookup on nil map = %q,%v; want \"\",false", v, ok)
	}
}

func TestRangeNilZeroIterations(t *testing.T) {
	t.Parallel()

	var m map[string]string // nil
	n := 0
	for range m {
		n++
	}
	if n != 0 {
		t.Fatalf("ranged nil map %d times, want 0", n)
	}
}

func TestLenNilIsZero(t *testing.T) {
	t.Parallel()

	c := &Config{}
	if got := c.Count(); got != 0 {
		t.Fatalf("Count on nil map = %d, want 0", got)
	}
}

func TestWriteToNilPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic writing to a nil map")
		}
		if !strings.Contains(fmt.Sprint(r), "nil map") {
			t.Fatalf("panic = %v, want a nil-map message", r)
		}
	}()

	var m map[string]string // nil
	// The next line panics: assignment to entry in nil map.
	m["k"] = "v"
}

func TestSetLazyInits(t *testing.T) {
	t.Parallel()

	c := &Config{}  // Settings is nil
	c.Set("a", "1") // must not panic; lazily allocates
	if v, ok := c.Lookup("a"); !ok || v != "1" {
		t.Fatalf("after Set: Lookup = %q,%v; want 1,true", v, ok)
	}
}

func TestJSONPopulatesNil(t *testing.T) {
	t.Parallel()

	var c Config // Settings is nil
	if err := json.Unmarshal([]byte(`{"name":"s","settings":{"a":"1"}}`), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if v := c.Settings["a"]; v != "1" {
		t.Fatalf("decoded Settings[a] = %q, want 1", v)
	}
}

func ExampleConfig_Count() {
	c := &Config{} // nil Settings
	fmt.Println(c.Count())
	// Output: 0
}
```

## Review

The loader is correct because it treats the nil map honestly: `Lookup` and `Count`
read a possibly-nil `Settings` without a guard and return the right zero answers,
while `Set` is the only method that guards, because it is the only one that writes.
`TestWriteToNilPanics` is the cautionary twin — it shows the exact crash that the
lazy-init guard prevents. The subtle production lesson is `TestJSONPopulatesNil`:
`json.Unmarshal` allocates the map for you, so a JSON-fed loader can mask a missing
initialization indefinitely, and the bug only surfaces on the first direct write.
Initialize map fields in a constructor when you can; lazy-guard the write when you
cannot.

## Resources

- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — nil map read/write semantics and `len`.
- [Go blog: Go maps in action](https://go.dev/blog/maps) — "a nil map behaves like an empty map when reading".
- [encoding/json Unmarshal](https://pkg.go.dev/encoding/json#Unmarshal) — decoding into a nil map allocates it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-two-level-metrics-nested-maps.md](06-two-level-metrics-nested-maps.md) | Next: [08-struct-key-idempotency-guard.md](08-struct-key-idempotency-guard.md)
