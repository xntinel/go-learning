# Exercise 5: Debug — A Value Receiver Silently Drops a Config Update

This is the single most common receiver bug in production Go, and it produces no
error at all: a config builder whose `Set` has a value receiver. The config
loads, the code compiles, the tests you forgot to write would pass — and every
override silently vanishes. This module ships the bug, explains exactly why the
write disappears, and fixes it by moving the whole type onto pointer receivers.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
serverconfig/              independent module: example.com/serverconfig
  go.mod
  config.go                type ServerConfig; New() *ServerConfig; Set, Get, Len (pointer receivers)
  cmd/
    demo/
      main.go              load defaults, override a value, read it back
  config_test.go           Set persists; method set is consistent
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `ServerConfig` backed by a `map[string]string`, with `New() *ServerConfig` that initializes the map, and `Set(key, value string)`, `Get(key string) string`, `Len() int` — all pointer receivers.
Test: `Set("port", "8080")` then `Get("port") == "8080"` persists; a reflection test asserts the value method set is empty (every method is a pointer receiver, so the type is consistent).
Verify: `go test -count=1 -race ./...`

### The bug: a value receiver drops the write

Here is the shape the bug usually takes. A `ServerConfig` holds a map, the map is
created lazily on first `Set`, and `Set` has a *value* receiver:

```go
// BUG: value receiver + lazily-initialized map => every Set is lost.
type ServerConfig struct {
	data map[string]string
}

func New() ServerConfig { return ServerConfig{} } // data is nil

func (c ServerConfig) Set(key, value string) {
	if c.data == nil {
		c.data = make(map[string]string) // written to a COPY of the struct
	}
	c.data[key] = value // written to the COPY's map
}

func (c ServerConfig) Get(key string) string { return c.data[key] }
```

Trace one call. `cfg.Set("port", "8080")` copies `cfg` into the receiver `c`.
Because `New` left `data` nil, the guard fires and `c.data = make(...)` assigns a
fresh map — *to the copy*. The key goes into that copy's map. Then `Set` returns
and the copy is discarded. The caller's `cfg.data` is still nil. Every `Set` is
lost the same way, so `Get` always sees `nil` and returns `""`. The program runs,
loads its defaults, and silently ignores every override — the classic "config
loads but overrides never stick" incident.

A subtlety worth naming, because it is where the intuition trips: if the map had
already been initialized (non-nil) and `Set` only did `c.data[key] = value`
without the reassignment, the write *would* persist — a map is a reference, and
the copied struct shares the same backing store. That is exactly why the bug is so
slippery: it depends on whether the map was already allocated. Relying on that is
a trap. The robust fix is not "pre-allocate the map"; it is to make the whole type
mutate through pointer receivers, so *any* field — a map, a scalar, a nested
struct — persists, and the method set is uniform.

### The fix: pointer receivers, map initialized in the constructor

Give every method a pointer receiver so `Set` mutates the original, and return
`*ServerConfig` from `New` with the map already allocated. Now `Set` writes
through the pointer to the caller's own struct, and reads see it.

Create `config.go`:

```go
package serverconfig

// ServerConfig is a mutable key/value configuration. Every method uses a pointer
// receiver so mutations persist and the method set is consistent.
type ServerConfig struct {
	data map[string]string
}

// New returns a ready ServerConfig with its map allocated. It returns
// *ServerConfig because the type is mutable.
func New() *ServerConfig {
	return &ServerConfig{data: make(map[string]string)}
}

// Set stores value under key. Pointer receiver: the write persists.
func (c *ServerConfig) Set(key, value string) {
	c.data[key] = value
}

// Get returns the value for key, or "" if unset.
func (c *ServerConfig) Get(key string) string {
	return c.data[key]
}

// Len reports how many keys are configured.
func (c *ServerConfig) Len() int {
	return len(c.data)
}
```

### The runnable demo

The demo loads two defaults, overrides the port at runtime, and reads the
overridden value back — proving the write stuck.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/serverconfig"
)

func main() {
	cfg := serverconfig.New()
	cfg.Set("host", "0.0.0.0")
	cfg.Set("port", "80")

	// A runtime override that MUST persist.
	cfg.Set("port", "8080")

	fmt.Printf("port=%s\n", cfg.Get("port"))
	fmt.Printf("host=%s\n", cfg.Get("host"))
	fmt.Printf("keys=%d\n", cfg.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
port=8080
host=0.0.0.0
keys=2
```

### Tests

`TestSetPersistsValue` is the test that fails against the buggy value-receiver
version and passes once the receiver is a pointer — the before/after this
exercise is built around. `TestMethodSetIsConsistent` uses reflection to prove the
whole type is uniform: the *value* method set is empty (no method leaked onto
`ServerConfig`), and both methods live on `*ServerConfig`. That is the machine
check for "all methods share one receiver kind".

Create `config_test.go`:

```go
package serverconfig

import (
	"reflect"
	"testing"
)

func TestSetPersistsValue(t *testing.T) {
	t.Parallel()

	cfg := New()
	cfg.Set("port", "8080")

	if got := cfg.Get("port"); got != "8080" {
		t.Fatalf("Get(port) = %q after Set; want %q (a value receiver would drop the write)", got, "8080")
	}
}

func TestOverridePersists(t *testing.T) {
	t.Parallel()

	cfg := New()
	cfg.Set("port", "80")
	cfg.Set("port", "8080")

	if got := cfg.Get("port"); got != "8080" {
		t.Fatalf("Get(port) = %q, want %q", got, "8080")
	}
	if got := cfg.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}

func TestGetMissingReturnsEmpty(t *testing.T) {
	t.Parallel()

	cfg := New()
	if got := cfg.Get("nope"); got != "" {
		t.Fatalf("Get(missing) = %q, want empty string", got)
	}
}

func TestMethodSetIsConsistent(t *testing.T) {
	t.Parallel()

	// Every method uses a pointer receiver, so the VALUE method set is empty.
	if vt := reflect.TypeOf(ServerConfig{}); vt.NumMethod() != 0 {
		t.Fatalf("value method set has %d methods; want 0 (mixing receivers)", vt.NumMethod())
	}
	// And the pointer method set holds the mutators and readers alike.
	pt := reflect.TypeOf(&ServerConfig{})
	for _, name := range []string{"Set", "Get", "Len"} {
		if _, ok := pt.MethodByName(name); !ok {
			t.Fatalf("*ServerConfig is missing method %s", name)
		}
	}
}
```

## Review

The fix is correct when `Set` writes are visible to a later `Get` — the assertion
in `TestSetPersistsValue`, which would have failed against the value-receiver
version. Notice that the real cure was not "allocate the map in `New`" alone; it
was moving the entire type onto pointer receivers so the method set is consistent
and every field, not just a pre-initialized map, mutates reliably.
`TestMethodSetIsConsistent` encodes that as a check you can keep: if someone later
adds a value-receiver method, the value method set stops being empty and the test
fails. The failure mode this exercise inoculates against — code that compiles,
runs, and silently loses writes — is the hardest kind to catch in review, which is
exactly why a persistence test exists.

## Resources

- [Go Code Review Comments: Receiver Type](https://go.dev/wiki/CodeReviewComments#receiver-type) — if any method mutates, all methods take a pointer receiver.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — why a value receiver cannot persist a mutation.
- [`reflect.Type.NumMethod`](https://pkg.go.dev/reflect#Type) — inspecting the method set of `T` versus `*T`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-immutable-money-value-object.md](04-immutable-money-value-object.md) | Next: [06-nil-receiver-noop-methods.md](06-nil-receiver-noop-methods.md)
