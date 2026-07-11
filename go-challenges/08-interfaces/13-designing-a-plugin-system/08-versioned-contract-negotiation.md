# Exercise 8: API-Version Negotiation Between Host and Plugin

Long-lived hosts and independently-shipped plugins drift. Add an explicit
`APIVersion()` to the contract and have the registry reject plugins outside the
host's supported range at registration — with a typed error carrying the got and
wanted numbers — instead of crashing deep inside `Process` at runtime.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
versioned/                independent module: example.com/versioned
  go.mod                  go 1.25
  registry.go             HostAPIVersion range; VersionError (Unwrap); Register version-gates
  cmd/
    demo/
      main.go             register plugins below/inside/above the range
  registry_test.go        boundary table test; errors.As extracts VersionError
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: an `APIVersion() int` on the plugin; host constants for the supported min/max; a typed `VersionError{Got, Min, Max}` implementing `error` and `Unwrap`; `Register` rejects out-of-range plugins with it.
- Test: register plugins below, inside, and above the range; assert only in-range registers and out-of-range returns an error from which `errors.As` extracts a `*VersionError` with correct `Got`/`Min`/`Max`, and `errors.Is` matches a sentinel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/versioned/cmd/demo
cd ~/go-exercises/versioned
go mod init example.com/versioned
go mod edit -go=1.25
```

### Fail closed at registration, not open at Process time

A host that loads third-party plugins cannot assume every plugin was built against
the current contract. A plugin compiled against an older `Process` signature or an
older set of guarantees will, if loaded blind, misbehave or panic deep inside
`Process` — at request time, in production, far from the cause. The fix is to make
the contract version explicit and check it at the boundary where loading happens:
registration.

Add `APIVersion() int` to the plugin (a semver struct works the same way; an int
keeps this exercise focused). The host declares the range it supports —
`MinAPIVersion` and `MaxAPIVersion` constants — and `Register` compares the
plugin's version against that range. Anything outside is rejected *before* the
plugin is stored, so an incompatible plugin never reaches `Process`. This is
failing closed: an unrecognized version is refused, not tolerated.

The rejection carries structured detail so the operator can act on it. A typed
`VersionError` holds `Got` (the plugin's version) and the host's `Min`/`Max`, and
implements `error`. It also implements `Unwrap() error` returning a sentinel
`ErrIncompatibleVersion`, which gives callers both matching styles: `errors.Is(err,
ErrIncompatibleVersion)` for a coarse "was this a version problem," and
`errors.As(err, &verr)` to recover the exact numbers for a log line or an admin
response. Wrapping errors are the reason to prefer `errors.Is`/`errors.As` over
`==` and type-switch — the version error might itself be wrapped by a higher layer.

Create `registry.go`:

```go
package versioned

import (
	"errors"
	"fmt"
)

// Host-supported API version range. A plugin must report APIVersion in
// [MinAPIVersion, MaxAPIVersion] to register.
const (
	MinAPIVersion = 2
	MaxAPIVersion = 4
)

// ErrIncompatibleVersion is the sentinel a VersionError unwraps to, so callers
// can errors.Is against it without knowing the concrete type.
var ErrIncompatibleVersion = errors.New("incompatible plugin API version")

// VersionError reports a plugin whose APIVersion is outside the host's range.
type VersionError struct {
	Plugin string
	Got    int
	Min    int
	Max    int
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("plugin %q reports API version %d, host supports [%d,%d]",
		e.Plugin, e.Got, e.Min, e.Max)
}

// Unwrap lets errors.Is(err, ErrIncompatibleVersion) match a *VersionError.
func (e *VersionError) Unwrap() error { return ErrIncompatibleVersion }

// Plugin adds an explicit contract version to the base methods.
type Plugin interface {
	Name() string
	APIVersion() int
	Process(input string) (string, error)
}

// Registry version-gates plugins at registration.
type Registry struct {
	plugins map[string]Plugin
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{plugins: make(map[string]Plugin)}
}

// Register stores p only if p.APIVersion() is within the host's supported range;
// otherwise it returns a *VersionError (which unwraps to ErrIncompatibleVersion)
// and does not store the plugin.
func (r *Registry) Register(p Plugin) error {
	if v := p.APIVersion(); v < MinAPIVersion || v > MaxAPIVersion {
		return &VersionError{Plugin: p.Name(), Got: v, Min: MinAPIVersion, Max: MaxAPIVersion}
	}
	r.plugins[p.Name()] = p
	return nil
}

// Has reports whether a plugin with name is registered.
func (r *Registry) Has(name string) bool {
	_, ok := r.plugins[name]
	return ok
}
```

### The runnable demo

The demo tries to register three plugins: one built against version 1 (too old),
one against version 3 (in range), one against version 5 (too new). Only the middle
one registers; the other two are rejected with a `VersionError` the demo unwraps
for its numbers.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/versioned"
)

type plugin struct {
	name    string
	version int
}

func (p plugin) Name() string                      { return p.name }
func (p plugin) APIVersion() int                   { return p.version }
func (p plugin) Process(in string) (string, error) { return in, nil }

func main() {
	r := versioned.NewRegistry()
	for _, p := range []plugin{
		{name: "legacy", version: 1},
		{name: "current", version: 3},
		{name: "future", version: 5},
	} {
		err := r.Register(p)
		switch {
		case err == nil:
			fmt.Printf("%s: registered\n", p.name)
		default:
			var ve *versioned.VersionError
			if errors.As(err, &ve) {
				fmt.Printf("%s: rejected (got %d, want [%d,%d])\n",
					p.name, ve.Got, ve.Min, ve.Max)
			}
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
legacy: rejected (got 1, want [2,4])
current: registered
future: rejected (got 5, want [2,4])
```

### Tests

The table test walks the boundary values — one below the min, both endpoints, one
above the max — and asserts each registers exactly when it should.
`TestRejectionCarriesVersionError` proves the rejection is both `errors.Is`-matchable
against the sentinel and `errors.As`-extractable to a `*VersionError` with correct
`Got`/`Min`/`Max`.

Create `registry_test.go`:

```go
package versioned

import (
	"errors"
	"testing"
)

type plugin struct {
	name    string
	version int
}

func (p plugin) Name() string                      { return p.name }
func (p plugin) APIVersion() int                   { return p.version }
func (p plugin) Process(in string) (string, error) { return in, nil }

func TestRegisterVersionBoundaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		version    int
		wantRegist bool
	}{
		{"below-min", MinAPIVersion - 1, false},
		{"at-min", MinAPIVersion, true},
		{"in-range", MinAPIVersion + 1, true},
		{"at-max", MaxAPIVersion, true},
		{"above-max", MaxAPIVersion + 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewRegistry()
			err := r.Register(plugin{name: tc.name, version: tc.version})
			registered := err == nil
			if registered != tc.wantRegist {
				t.Fatalf("Register(v=%d) registered=%v, want %v (err=%v)",
					tc.version, registered, tc.wantRegist, err)
			}
			if r.Has(tc.name) != tc.wantRegist {
				t.Fatalf("Has(%q)=%v, want %v", tc.name, r.Has(tc.name), tc.wantRegist)
			}
		})
	}
}

func TestRejectionCarriesVersionError(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	err := r.Register(plugin{name: "future", version: MaxAPIVersion + 3})
	if err == nil {
		t.Fatal("expected rejection for out-of-range version")
	}

	if !errors.Is(err, ErrIncompatibleVersion) {
		t.Fatalf("err %v does not match ErrIncompatibleVersion", err)
	}

	var ve *VersionError
	if !errors.As(err, &ve) {
		t.Fatalf("err %v is not a *VersionError", err)
	}
	if ve.Got != MaxAPIVersion+3 || ve.Min != MinAPIVersion || ve.Max != MaxAPIVersion {
		t.Fatalf("VersionError = %+v, want Got=%d Min=%d Max=%d",
			ve, MaxAPIVersion+3, MinAPIVersion, MaxAPIVersion)
	}
}
```

## Review

Version negotiation is correct when an out-of-range plugin is refused at
registration and never stored, so `Process` only ever runs against a
contract-compatible plugin. The typed-error design gives callers two handles: the
sentinel via `Unwrap`/`errors.Is` for a coarse category check, and the struct via
`errors.As` for the exact numbers — which is why the rejection must be a
`*VersionError` and not a bare `errors.New`. The boundary table is the important
test: off-by-one range checks (`<` versus `<=`) are the classic bug, and asserting
both endpoints register while their neighbors do not pins the comparison exactly.
Failing closed here — refusing an unknown version rather than tolerating it — is
what converts a latent `Process`-time panic into a clean startup rejection.

## Resources

- [errors.As](https://pkg.go.dev/errors#As) — extracting the concrete `*VersionError` from a possibly-wrapped error.
- [errors package: Unwrap](https://pkg.go.dev/errors#pkg-overview) — how `Unwrap` chains a typed error to a sentinel.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `Is`/`As`/`Unwrap` and when to use each.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-decorator-middleware-plugins.md](07-decorator-middleware-plugins.md) | Next: [09-config-driven-loading.md](09-config-driven-loading.md)
