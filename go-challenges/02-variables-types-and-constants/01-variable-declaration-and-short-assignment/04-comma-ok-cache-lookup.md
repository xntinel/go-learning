# Exercise 4: Comma-Ok Lookup in a Cache and Feature-Flag Store

A feature-flag store is the canonical place the comma-ok form earns its keep: an
unset flag and a flag explicitly set to `false` are both `false`, and only the
second boolean from `value, ok := m[key]` can tell them apart. Drop the `ok` and
every "default on unless overridden" flag silently reads as off.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
flagstore/                     independent module: example.com/flagstore
  go.mod                       module example.com/flagstore
  flagstore.go                 FlagStore: Set, Lookup(name) (bool, bool), Enabled(name, def)
  attrs.go                     AttrStore: interface{} values with type-assertion comma-ok
  cmd/
    demo/
      main.go                  shows unset vs explicit-false, and default fallbacks
  flagstore_test.go            present-zero vs absent, default fallback, type-assert comma-ok
```

- Files: `flagstore.go`, `attrs.go`, `cmd/demo/main.go`, `flagstore_test.go`.
- Implement: `Lookup(name) (value, ok bool)` distinguishing absent from present-false, `Enabled(name, def)` applying a default only when unset, and a typed attribute lookup using type-assertion comma-ok.
- Test: present-with-zero returns `ok=true`; missing returns `ok=false` and the zero value; an unset flag never reads as explicit `false`; a type-assertion comma-ok case.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/flagstore/cmd/demo
cd ~/go-exercises/flagstore
go mod init example.com/flagstore
```

### Why the single-result map form is a bug here

A map indexed with one result returns the zero value for a missing key:
`m["ship-new-checkout"]` is `false` whether the flag was set to `false` or never
set at all. For a flag store that is catastrophic: a flag defined as "enabled
unless an operator disables it" must treat *unset* as its default (perhaps `true`)
and *explicitly false* as off. The only way to make that distinction is the
two-result form, `value, ok := m[name]`, where `ok` reports presence independently
of the value. `Lookup` returns exactly that pair, and `Enabled` uses it to apply a
default only when `ok` is false.

This is not academic. A `map[string]bool` read with a single result is one of the
most common latent bugs in feature-flag and cache code precisely because it
compiles, passes the happy-path test where the flag is set to `true`, and fails
silently the day a flag is meant to default on.

### Type-assertion comma-ok is the same idea for `any`

An attribute store keyed to `any` (per-request metadata, decoded JSON) has the
same trap in a different guise: `v.(int)` panics on a type mismatch and gives you
no way to distinguish "absent" from "present but wrong type". The comma-ok
assertion `n, ok := v.(int)` never panics and reports whether the assertion held,
so a typed getter can fall back safely.

Create `flagstore.go`:

```go
package flagstore

import "sync"

// FlagStore holds boolean feature flags. It distinguishes an unset flag from a
// flag explicitly set to false, which single-result map indexing cannot.
type FlagStore struct {
	mu    sync.RWMutex
	flags map[string]bool
}

func NewFlagStore() *FlagStore {
	return &FlagStore{flags: make(map[string]bool)}
}

// Set records an explicit value for name.
func (s *FlagStore) Set(name string, value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flags[name] = value
}

// Lookup reports the stored value and whether the flag was set at all. The second
// result is the only way to tell an explicit false from an absent flag.
func (s *FlagStore) Lookup(name string) (value bool, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok = s.flags[name]
	return value, ok
}

// Enabled returns the flag's value if it was set, otherwise def. An unset flag
// falls back to def; an explicit false stays false.
func (s *FlagStore) Enabled(name string, def bool) bool {
	if value, ok := s.Lookup(name); ok {
		return value
	}
	return def
}
```

Create `attrs.go`:

```go
package flagstore

import "sync"

// AttrStore holds arbitrary per-key attributes. Typed getters use comma-ok type
// assertions so a missing or wrongly-typed value never panics.
type AttrStore struct {
	mu    sync.RWMutex
	attrs map[string]any
}

func NewAttrStore() *AttrStore {
	return &AttrStore{attrs: make(map[string]any)}
}

func (s *AttrStore) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs[key] = value
}

// Int returns the int stored at key. ok is false if the key is absent or the
// stored value is not an int.
func (s *AttrStore) Int(key string) (n int, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, present := s.attrs[key]
	if !present {
		return 0, false
	}
	n, ok = v.(int) // comma-ok assertion: never panics on a type mismatch
	return n, ok
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/flagstore"
)

func main() {
	fs := flagstore.NewFlagStore()
	fs.Set("beta-search", false) // explicitly disabled

	// "beta-search" is set to false; "new-dashboard" is unset.
	fmt.Printf("beta-search enabled (default true): %t\n", fs.Enabled("beta-search", true))
	fmt.Printf("new-dashboard enabled (default true): %t\n", fs.Enabled("new-dashboard", true))

	as := flagstore.NewAttrStore()
	as.Set("retries", 3)
	as.Set("region", "eu-west-1")

	n, ok := as.Int("retries")
	fmt.Printf("retries=%d ok=%t\n", n, ok)
	m, ok := as.Int("region") // wrong type
	fmt.Printf("region-as-int=%d ok=%t\n", m, ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
beta-search enabled (default true): false
new-dashboard enabled (default true): true
retries=3 ok=true
region-as-int=0 ok=false
```

The explicit `false` survives its `true` default, while the unset flag falls back
to `true` — the exact distinction a single-result lookup would erase. The
attribute typed as a string reports `ok=false` from the comma-ok assertion instead
of panicking.

### Tests

Create `flagstore_test.go`:

```go
package flagstore

import (
	"fmt"
	"testing"
)

func TestLookupDistinguishesAbsentFromFalse(t *testing.T) {
	t.Parallel()
	fs := NewFlagStore()
	fs.Set("explicit-false", false)

	if v, ok := fs.Lookup("explicit-false"); !ok || v != false {
		t.Fatalf("Lookup(explicit-false) = %v,%v; want false,true", v, ok)
	}
	if v, ok := fs.Lookup("never-set"); ok || v != false {
		t.Fatalf("Lookup(never-set) = %v,%v; want false,false", v, ok)
	}
}

func TestEnabledAppliesDefaultOnlyWhenUnset(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		set   bool // whether to Set it
		value bool
		def   bool
		want  bool
	}{
		{"unset falls to default true", false, false, true, true},
		{"unset falls to default false", false, false, false, false},
		{"explicit false beats default true", true, false, true, false},
		{"explicit true beats default false", true, true, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := NewFlagStore()
			if tc.set {
				fs.Set("flag", tc.value)
			}
			if got := fs.Enabled("flag", tc.def); got != tc.want {
				t.Fatalf("Enabled = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestAttrIntCommaOk(t *testing.T) {
	t.Parallel()
	as := NewAttrStore()
	as.Set("retries", 5)
	as.Set("name", "svc")

	if n, ok := as.Int("retries"); !ok || n != 5 {
		t.Fatalf("Int(retries) = %d,%v; want 5,true", n, ok)
	}
	if n, ok := as.Int("name"); ok || n != 0 {
		t.Fatalf("Int(name) = %d,%v; want 0,false (wrong type)", n, ok)
	}
	if n, ok := as.Int("missing"); ok || n != 0 {
		t.Fatalf("Int(missing) = %d,%v; want 0,false (absent)", n, ok)
	}
}

func ExampleFlagStore_Enabled() {
	fs := NewFlagStore()
	fs.Set("kill-switch", false)
	fmt.Println(fs.Enabled("kill-switch", true), fs.Enabled("unset", true))
	// Output: false true
}
```

`TestEnabledAppliesDefaultOnlyWhenUnset` is the behavioral proof that the comma-ok
distinction is preserved through `Enabled`: the two "explicit" rows must beat their
defaults, which is impossible with a single-result read.

## Review

The store is correct when `ok` — not the value — decides presence. `Lookup`
returns `(value, ok)` and `Enabled` branches on `ok`, so an explicit `false`
overrides a `true` default while an unset flag falls back. The typed attribute
getter uses a comma-ok assertion so a wrong type or absent key returns
`(zero, false)` instead of panicking.

The mistakes to avoid: reading `m[name]` with one result (an unset flag becomes an
explicit `false`), and asserting `v.(int)` without the `ok` (a panic on any
mismatch). Run `go test -race`; the `sync.RWMutex` must guard both maps under
concurrent readers and writers.

## Resources

- [Go Specification: Index expressions (comma-ok map form)](https://go.dev/ref/spec#Index_expressions)
- [Go Specification: Type assertions](https://go.dev/ref/spec#Type_assertions)
- [Effective Go: Maps (the comma-ok idiom)](https://go.dev/doc/effective_go#maps)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-shadowing-in-transaction-commit.md](03-shadowing-in-transaction-commit.md) | Next: [05-grouped-var-build-metadata.md](05-grouped-var-build-metadata.md)
