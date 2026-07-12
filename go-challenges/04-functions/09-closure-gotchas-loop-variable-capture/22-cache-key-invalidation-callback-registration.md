# Exercise 22: Cache Invalidation: Registering Callbacks that Capture Cache Keys in a Loop

**Nivel: Intermedio** — validacion rapida (un test corto).

A cache builds one invalidation callback per key so an eviction hook, a TTL
tick, or an upstream change notice can invalidate the right entry later. The
trap: if every callback closes over a single shared "current key" pointer
instead of its own key value, every callback ends up invalidating the SAME
(last) key, because a callback is registered once but always fires later.

## What you'll build

```text
cacheinval/                  independent module: example.com/cacheinval
  go.mod                     go 1.24
  cacheinval.go                Callback, RegisterInvalidationCallbacks, RegisterInvalidationCallbacksBuggy
  cacheinval_test.go           table test: own key vs. shared key, single-key edge case
```

- Files: `cacheinval.go`, `cacheinval_test.go`.
- Implement: `RegisterInvalidationCallbacks(keys) []Callback` closing over each key's own value; `RegisterInvalidationCallbacksBuggy` closing over a pointer to a single shared "current key" variable instead.
- Test: one table test that registers callbacks for both variants, fires every callback, and asserts which key each one reports.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/22-cache-key-invalidation-callback-registration
cd go-solutions/04-functions/09-closure-gotchas-loop-variable-capture/22-cache-key-invalidation-callback-registration
go mod edit -go=1.24
```

### Why the callback needs its own key, not a shared pointer

`RegisterInvalidationCallbacksBuggy` allocates `currentKey` once with `new
(string)`, outside the loop, and every callback closes over that same
pointer instead of the loop's `key` value. A callback is meant to fire later
— on eviction, on a TTL tick — always after registration finishes. By the
time any callback actually runs, `*currentKey` holds whatever the loop last
assigned it: the last key registered. Every callback, regardless of which key
it was "registered for," invalidates that same last key.

`RegisterInvalidationCallbacks` fixes it by closing over `key` from `for i,
key := range keys` directly — on a `go 1.24` module the range variable is
already a fresh binding per iteration, so callback `i` always reports
`keys[i]`, no matter how much later it fires.

Create `cacheinval.go`:

```go
package cacheinval

// Callback invalidates one cache entry and reports which key it invalidated,
// so a test can tell which entry a registered callback actually targets.
type Callback func() string

// RegisterInvalidationCallbacksBuggy registers one callback per key, but
// every callback closes over a POINTER to a SINGLE shared "current key"
// variable declared outside the loop instead of that key's own value. A
// callback is meant to fire later -- on eviction, on a TTL tick, on an
// upstream change notice -- long after registration finishes. By the time any
// callback actually runs, the shared variable holds whatever key the loop
// last assigned, so every callback invalidates the SAME (last) key.
func RegisterInvalidationCallbacksBuggy(keys []string) []Callback {
	callbacks := make([]Callback, len(keys))
	currentKey := new(string)
	for i, key := range keys {
		*currentKey = key // BUG: every callback shares this same pointer
		callbacks[i] = func() string {
			return *currentKey
		}
	}
	return callbacks
}

// RegisterInvalidationCallbacks registers one callback per key, each closing
// over its OWN key value captured at registration time, so firing callback i
// always invalidates keys[i] no matter how much later it fires.
func RegisterInvalidationCallbacks(keys []string) []Callback {
	callbacks := make([]Callback, len(keys))
	for i, key := range keys {
		callbacks[i] = func() string {
			return key
		}
	}
	return callbacks
}
```

### Test

One table test registers callbacks for three keys with both variants, fires
every callback, and asserts each one's reported key — every callback for its
own key in the correct version, every callback the last key in the buggy
version.

Create `cacheinval_test.go`:

```go
package cacheinval

import "testing"

func TestRegisterInvalidationCallbacks(t *testing.T) {
	keys := []string{"user:1", "user:2", "user:3"}

	tests := []struct {
		name    string
		build   func([]string) []Callback
		wantAll bool // true: callback i targets keys[i]; false: every callback targets keys[len-1]
	}{
		{name: "own key per callback", build: RegisterInvalidationCallbacks, wantAll: true},
		{name: "shared key pointer leaks to the last key", build: RegisterInvalidationCallbacksBuggy, wantAll: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callbacks := tt.build(keys)
			for i, cb := range callbacks {
				got := cb()
				want := keys[len(keys)-1]
				if tt.wantAll {
					want = keys[i]
				}
				if got != want {
					t.Fatalf("callbacks[%d]() = %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestRegisterInvalidationCallbacksSingleKeyEdgeCase(t *testing.T) {
	callbacks := RegisterInvalidationCallbacksBuggy([]string{"only"})
	if got := callbacks[0](); got != "only" {
		t.Fatalf("callbacks[0]() = %q, want %q", got, "only")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

This is the loop-capture family in its purest form: `currentKey` is not the
range variable, it is an ordinary pointer declared outside the loop that
every callback happens to share. A callback runs long after registration, so
anything it reads through a shared pointer reflects whatever that pointer
points to AT CALL TIME, not at registration time. `RegisterInvalidationCallbacks`
shows the fix costs nothing extra here — the range variable itself is already
per-iteration on Go 1.22+, so simply closing over `key` directly instead of
routing through a shared pointer is enough.

## Resources

- [Go spec: For statements with range clause](https://go.dev/ref/spec#For_range) — per-iteration variable semantics since Go 1.22.
- [Go blog: Closures](https://go.dev/tour/moretypes/25) — closures capturing variables, not values.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-metric-aggregator-per-key-buffer-write-race.md](21-metric-aggregator-per-key-buffer-write-race.md) | Next: [23-grpc-listener-address-handler-binding-closure.md](23-grpc-listener-address-handler-binding-closure.md)
