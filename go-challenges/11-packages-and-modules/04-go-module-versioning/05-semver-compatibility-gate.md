# Exercise 5: Negotiate a minimum supported API version between client and server

A versioned API surface — gRPC, HTTP, an internal SDK — must decide whether a
client that reports version X is allowed to talk to it. The rule is semver: same
major, at or above a configured minimum. Here you build that gate on top of
`golang.org/x/mod/semver`, the same comparison logic the Go toolchain uses.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
compat/                     independent module: example.com/billing/compat
  go.mod
  compat.go                 Gate with Check(clientVersion); three sentinel errors
  cmd/
    demo/
      main.go               runnable: gate min v1.2.0, classify several clients
  compat_test.go            table-driven allow/reject + semver.Compare sign contract
```

- Files: `compat.go`, `cmd/demo/main.go`, `compat_test.go`.
- Implement: `NewGate(min string)` and `(*Gate).Check(clientVersion string) error`, returning `ErrInvalidVersion`, `ErrIncompatibleMajor`, or `ErrBelowMinimum`.
- Test: same major & `>=` min allows; lower minor rejects; different major is incompatible; a version without the leading `v` is invalid; and `semver.Compare` returns exactly `-1/0/+1`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/04-go-module-versioning/05-semver-compatibility-gate/cmd/demo
cd go-solutions/11-packages-and-modules/04-go-module-versioning/05-semver-compatibility-gate
go mod edit -go=1.26
```

### The three decisions, in order

A compatibility check is three questions asked in a fixed order, and the order
matters. First: is the reported version even valid semver? `golang.org/x/mod/semver`
requires the leading `v` — `semver.IsValid("v1.2.0")` is true, `semver.IsValid("1.2.0")`
is false — and *every* other function in the package returns the empty/false/zero
result for an invalid input. So an unvalidated `"1.2.0"` does not compare "less
than" anything; it silently no-ops through `Compare` (which returns 0 against an
invalid operand) and would slip past a naive check. Validate first, reject with
`ErrInvalidVersion`, and nothing downstream can be fooled.

Second: same major? A different major is *incompatible* by definition of Semantic
Import Versioning — v2 is a different contract, not "a newer v1" — so the gate rejects
a major mismatch outright with `ErrIncompatibleMajor`, before any ordering question.
`semver.Major("v2.3.1")` returns `"v2"`; compare it to the server's major. Only if
the majors match does the third question make sense.

Third: is the client at or above the minimum within that major? `semver.Compare`
returns exactly `-1`, `0`, or `+1` — a total order over valid semver, with
pre-release and build metadata handled per the spec — so `Compare(client, min) < 0`
means below minimum, rejected with `ErrBelowMinimum`. Canonicalizing the configured
minimum with `semver.Canonical` up front means `"v1.2"` and `"v1.2.0"` gate
identically.

Create `compat.go`:

```go
package compat

import (
	"errors"
	"fmt"

	"golang.org/x/mod/semver"
)

var (
	// ErrInvalidVersion is returned for a version that is not valid semver
	// (most commonly a missing leading "v").
	ErrInvalidVersion = errors.New("compat: version is not valid semver")
	// ErrIncompatibleMajor is returned when the client and server majors differ.
	ErrIncompatibleMajor = errors.New("compat: incompatible major version")
	// ErrBelowMinimum is returned when the client is older than the minimum.
	ErrBelowMinimum = errors.New("compat: version below minimum supported")
)

// Gate accepts clients at or above a minimum version within the same major.
type Gate struct {
	min string // canonical, e.g. "v1.2.0"
}

// NewGate builds a Gate from a minimum supported version. It rejects a min that
// is not valid semver.
func NewGate(min string) (*Gate, error) {
	if !semver.IsValid(min) {
		return nil, fmt.Errorf("compat: min %q: %w", min, ErrInvalidVersion)
	}
	return &Gate{min: semver.Canonical(min)}, nil
}

// Check reports whether a client at clientVersion may talk to this server. It
// returns nil when the client is valid, shares the server's major, and is at or
// above the minimum; otherwise a sentinel-wrapped error.
func (g *Gate) Check(clientVersion string) error {
	if !semver.IsValid(clientVersion) {
		return fmt.Errorf("compat: client %q: %w", clientVersion, ErrInvalidVersion)
	}
	if semver.Major(clientVersion) != semver.Major(g.min) {
		return fmt.Errorf("compat: client %s vs server %s: %w",
			semver.Major(clientVersion), semver.Major(g.min), ErrIncompatibleMajor)
	}
	if semver.Compare(clientVersion, g.min) < 0 {
		return fmt.Errorf("compat: client %s < min %s: %w", clientVersion, g.min, ErrBelowMinimum)
	}
	return nil
}

// Min returns the canonical minimum this gate enforces.
func (g *Gate) Min() string { return g.min }
```

### The runnable demo

The demo gates on `v1.2.0` and classifies four clients: a newer patch line, an
older minor, a different major, and a malformed version.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/billing/compat"
)

func main() {
	gate, err := compat.NewGate("v1.2.0")
	if err != nil {
		fmt.Println("bad gate:", err)
		return
	}
	fmt.Printf("server minimum: %s\n", gate.Min())

	for _, v := range []string{"v1.5.0", "v1.1.0", "v2.0.0", "1.5.0"} {
		if err := gate.Check(v); err != nil {
			fmt.Printf("%-7s reject: %v\n", v, err)
		} else {
			fmt.Printf("%-7s allow\n", v)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
server minimum: v1.2.0
v1.5.0  allow
v1.1.0  reject: compat: client v1.1.0 < min v1.2.0: compat: version below minimum supported
v2.0.0  reject: compat: client v2 vs server v1: compat: incompatible major version
1.5.0   reject: compat: client "1.5.0": compat: version is not valid semver
```

### Tests

The table drives every branch and asserts the *cause* with `errors.Is`. A separate
test pins the `semver.Compare` sign contract the gate relies on.

Create `compat_test.go`:

```go
package compat

import (
	"errors"
	"testing"

	"golang.org/x/mod/semver"
)

func TestGateCheck(t *testing.T) {
	t.Parallel()
	gate, err := NewGate("v1.2.0")
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	cases := []struct {
		name    string
		client  string
		wantErr error // nil means allow
	}{
		{"newer-minor-allowed", "v1.5.0", nil},
		{"exact-min-allowed", "v1.2.0", nil},
		{"newer-major-line-patch", "v1.2.1", nil},
		{"below-min-rejected", "v1.1.0", ErrBelowMinimum},
		{"older-major-line", "v1.0.9", ErrBelowMinimum},
		{"different-major-incompatible", "v2.0.0", ErrIncompatibleMajor},
		{"missing-v-invalid", "1.5.0", ErrInvalidVersion},
		{"garbage-invalid", "not-a-version", ErrInvalidVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := gate.Check(tc.client)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Check(%q) = %v, want allow", tc.client, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Check(%q) = %v, want %v", tc.client, err, tc.wantErr)
			}
		})
	}
}

func TestNewGateRejectsInvalidMin(t *testing.T) {
	t.Parallel()
	if _, err := NewGate("1.0.0"); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("NewGate(1.0.0) err = %v, want ErrInvalidVersion", err)
	}
}

func TestCompareSignContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.0", "v1.5.0", -1},
		{"v1.5.0", "v1.5.0", 0},
		{"v2.0.0", "v1.5.0", 1},
	}
	for _, tc := range cases {
		if got := semver.Compare(tc.a, tc.b); got != tc.want {
			t.Fatalf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
```

## Review

The gate is correct when it asks its three questions in order — valid, then same
major, then at-or-above minimum — because each guards the next. Validating first is
not pedantry: `golang.org/x/mod/semver` treats any version without the leading `v` as
invalid and no-ops, so an unvalidated `"1.5.0"` would compare as `0` and slip
through; `TestGateCheck` proves the malformed cases are rejected as
`ErrInvalidVersion` instead. A different major is incompatible by SIV, not merely
"newer," which is why that check precedes the ordering one. `TestCompareSignContract`
pins the `-1/0/+1` contract the whole design leans on. The trap to avoid is skipping
`IsValid` and trusting `Compare` alone — it cannot tell you an operand was garbage.

## Resources

- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver) — `IsValid`, `Compare`, `Major`, `Canonical`, and the leading-`v` requirement.
- [Semantic Versioning 2.0.0](https://semver.org/) — the precedence rules `Compare` implements.
- [Go Modules Reference: versions](https://go.dev/ref/mod#versions) — how Go maps semver onto module compatibility.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-gomod-supply-chain-linter.md](06-gomod-supply-chain-linter.md)
