# Exercise 1: The Manifest and Capability Model

Before any Wasm runs, the host needs a vocabulary for trust: what a plugin *asks*
for, what the operator *permits*, and what it therefore *gets*. This exercise
builds that vocabulary as pure Go — a closed set of capabilities, a manifest
parsed from JSON, and a policy that intersects the two — so the deny-by-default
core every later exercise consumes is nailed down and fully tested before a
runtime enters the picture.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
capability/                 independent module: example.com/capability
  go.mod                    go 1.26
  capability.go             Capability set; Manifest; ParseManifest; CapabilitySet;
                            Policy.Grant; sentinels ErrUnknownCapability,
                            ErrCapabilityDenied, ErrInvalidManifest
  cmd/
    demo/
      main.go               parses an embedded manifest, prints granted vs denied
  capability_test.go        table-driven grant/parse tests, an Example, a your-turn test
```

- Files: `capability.go`, `cmd/demo/main.go`, `capability_test.go`.
- Implement: a closed `Capability` set, `ParseManifest([]byte) (Manifest, error)`, a `CapabilitySet` with `Has`/`Sorted`, and `Policy.Grant(Manifest) (CapabilitySet, error)` returning `granted = requested ∩ allowed` or a wrapped sentinel.
- Test: table-driven cases for unknown capability (`ErrUnknownCapability`), denied capability (`ErrCapabilityDenied` with the name surfaced), and a valid subset (exact sorted grant); an `Example` printing a granted set; a your-turn test asserting an empty request yields an empty non-nil grant.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/capability/cmd/demo
cd ~/go-exercises/capability
go mod init example.com/capability
```

### Why a closed set, and why the grant is an intersection

A capability is a named power the host can hand out: write to a log, read the KV
store, write it, fetch a URL. The set is *closed* — there is no such thing as an
open-ended capability, because a capability the host does not recognize is a
capability the host cannot possibly enforce. So `ParseCapability` rejects any
string outside the known universe with `ErrUnknownCapability`. This is the
difference between "I do not know what `cap.filesystem` means, so I will ignore
it" (a silent escalation waiting to happen) and "I do not know what
`cap.filesystem` means, so this manifest is invalid."

The manifest is untrusted input: the plugin author wrote it, and it states
*intent*, not permission. The operator's `Policy` carries the other half — the
allow-list of capabilities the operator is willing to grant to anyone. `Grant`
computes the intersection: a capability is granted only if the plugin requested
it *and* the operator allows it. Crucially, a request for something the operator
does not allow is not silently dropped; it is an error (`ErrCapabilityDenied`),
because a plugin that asks for `cap.http.fetch` on a host that forbids outbound
fetches is misconfigured, and failing loudly at load time beats discovering it at
runtime. This two-sided model — plugin declares, operator authorizes — is exactly
how you avoid a confused-deputy escalation where a plugin talks a privileged host
into acting for it.

### Deterministic sets, and why sentinels are wrapped

A `CapabilitySet` is a `map[Capability]struct{}`, which has no order. Any code
that prints or compares a grant needs determinism, so `Sorted` returns the
capabilities via `slices.Sorted(maps.Keys(s))`. Because `Capability` is a
string-based type it satisfies `cmp.Ordered`, so `slices.Sorted` orders it
lexically with no custom comparator. Every error the package returns wraps a
package-level sentinel with `%w`, so a caller writes `errors.Is(err,
ErrCapabilityDenied)` rather than string-matching a message — and the wrap still
carries the offending capability name and plugin label in the message for logs.

Create `capability.go`:

```go
// Package capability defines the deny-by-default trust vocabulary a Wasm plugin
// host uses to decide what an untrusted plugin may do: a closed set of named
// capabilities, a manifest a plugin declares, and a policy that intersects the
// requested capabilities with an operator allow-list.
package capability

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// Capability is one entry in a closed set of powers the host can grant. Its
// underlying string type keeps it ordered, so a set of them sorts deterministically.
type Capability string

const (
	CapLog       Capability = "cap.log"
	CapKVRead    Capability = "cap.kv.read"
	CapKVWrite   Capability = "cap.kv.write"
	CapHTTPFetch Capability = "cap.http.fetch"
)

// known is the closed universe of capabilities. A string outside it is a
// manifest error, not an unknown-but-harmless extension.
var known = []Capability{CapLog, CapKVRead, CapKVWrite, CapHTTPFetch}

// Sentinel errors, wrapped with %w so callers match them via errors.Is.
var (
	ErrUnknownCapability = errors.New("unknown capability")
	ErrCapabilityDenied  = errors.New("capability denied by policy")
	ErrInvalidManifest   = errors.New("invalid manifest")
)

// ParseCapability validates a raw string against the closed set.
func ParseCapability(s string) (Capability, error) {
	c := Capability(s)
	if !slices.Contains(known, c) {
		return "", fmt.Errorf("%q: %w", s, ErrUnknownCapability)
	}
	return c, nil
}

// Manifest is the plugin author's declaration: identity plus requested
// capabilities. Config is opaque plugin-specific settings the host does not
// interpret. It is untrusted input parsed from JSON.
type Manifest struct {
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	Capabilities []string        `json:"capabilities"`
	Config       json.RawMessage `json:"config,omitempty"`
}

// ParseManifest decodes and structurally validates a manifest. It does not
// authorize anything; capability authorization is Policy.Grant's job.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if m.Name == "" {
		return Manifest{}, fmt.Errorf("%w: missing name", ErrInvalidManifest)
	}
	return m, nil
}

// Label is a human-readable identifier that defaults a missing version.
func (m Manifest) Label() string {
	return m.Name + "@" + cmp.Or(m.Version, "unversioned")
}

// CapabilitySet is a set of granted capabilities.
type CapabilitySet map[Capability]struct{}

// Has reports whether c is in the set.
func (s CapabilitySet) Has(c Capability) bool {
	_, ok := s[c]
	return ok
}

// Sorted returns the capabilities in deterministic (lexical) order.
func (s CapabilitySet) Sorted() []Capability {
	return slices.Sorted(maps.Keys(s))
}

// Policy is the operator's side: the allow-list of capabilities it is willing to
// grant to any plugin.
type Policy struct {
	allowed []Capability
}

// NewPolicy builds a policy from the operator's allow-list.
func NewPolicy(allowed ...Capability) Policy {
	return Policy{allowed: allowed}
}

// Grant intersects the manifest's requested capabilities with the allow-list.
// An unknown requested capability is ErrUnknownCapability; a known one the
// operator does not permit is ErrCapabilityDenied. On success it returns the
// granted set, which is a subset of the allow-list and non-nil even when empty.
func (p Policy) Grant(m Manifest) (CapabilitySet, error) {
	granted := make(CapabilitySet, len(m.Capabilities))
	for _, raw := range m.Capabilities {
		c, err := ParseCapability(raw)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", m.Label(), err)
		}
		if !slices.Contains(p.allowed, c) {
			return nil, fmt.Errorf("plugin %s requested %q: %w", m.Label(), c, ErrCapabilityDenied)
		}
		granted[c] = struct{}{}
	}
	return granted, nil
}
```

### The runnable demo

The demo carries the manifest as a JSON string (a real deployment would read it
from the plugin bundle), grants it against an operator policy that permits
logging and KV reads, and then shows a second, overreaching plugin being denied.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log"

	"example.com/capability"
)

// manifestJSON stands in for a manifest shipped inside a plugin bundle.
const manifestJSON = `{
	"name": "event-enricher",
	"version": "1.4.0",
	"capabilities": ["cap.log", "cap.kv.read"]
}`

func main() {
	m, err := capability.ParseManifest([]byte(manifestJSON))
	if err != nil {
		log.Fatal(err)
	}

	// The operator permits logging and KV reads, but not writes or fetches.
	policy := capability.NewPolicy(capability.CapLog, capability.CapKVRead)

	granted, err := policy.Grant(m)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s granted: %v\n", m.Label(), granted.Sorted())

	// A second plugin overreaches by requesting an un-permitted capability.
	overreach := capability.Manifest{
		Name:         "exfiltrator",
		Version:      "0.1.0",
		Capabilities: []string{"cap.http.fetch"},
	}
	if _, err := policy.Grant(overreach); errors.Is(err, capability.ErrCapabilityDenied) {
		fmt.Printf("%s denied: %v\n", overreach.Label(), err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
event-enricher@1.4.0 granted: [cap.kv.read cap.log]
exfiltrator@0.1.0 denied: plugin exfiltrator@0.1.0 requested "cap.http.fetch": capability denied by policy
```

### Tests

`TestParseManifest` covers a valid manifest, a missing name, and malformed JSON,
each rejection asserted against `ErrInvalidManifest`. `TestGrant` is the core
table: a valid subset that returns the exact sorted grant, an empty request, an
unknown capability, and a denied one — the last two matched with `errors.Is`
against their sentinels. `TestDeniedCapabilityNameSurfaced` proves the denied
capability's name reaches the message, since an audit log that says only "denied"
is useless. The `Example` pins a sorted grant, and the your-turn test proves an
empty request yields an empty but non-nil set.

Create `capability_test.go`:

```go
package capability

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestParseManifest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{"valid", `{"name":"p","version":"1.0.0","capabilities":["cap.log"]}`, false},
		{"missing name", `{"version":"1.0.0"}`, true},
		{"malformed json", `{`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseManifest([]byte(tc.json))
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidManifest) {
					t.Fatalf("ParseManifest error = %v, want ErrInvalidManifest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseManifest unexpected error: %v", err)
			}
		})
	}
}

func TestGrant(t *testing.T) {
	t.Parallel()
	policy := NewPolicy(CapLog, CapKVRead, CapKVWrite)
	tests := []struct {
		name    string
		req     []string
		want    []Capability
		wantErr error
	}{
		{"valid subset", []string{"cap.kv.read", "cap.log"}, []Capability{CapKVRead, CapLog}, nil},
		{"empty request", nil, []Capability{}, nil},
		{"unknown capability", []string{"cap.bogus"}, nil, ErrUnknownCapability},
		{"denied capability", []string{"cap.http.fetch"}, nil, ErrCapabilityDenied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := Manifest{Name: "p", Version: "1.0.0", Capabilities: tc.req}
			granted, err := policy.Grant(m)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Grant error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Grant unexpected error: %v", err)
			}
			if got := granted.Sorted(); !slices.Equal(got, tc.want) {
				t.Errorf("granted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeniedCapabilityNameSurfaced(t *testing.T) {
	t.Parallel()
	policy := NewPolicy(CapLog)
	m := Manifest{Name: "grabby", Capabilities: []string{"cap.http.fetch"}}
	_, err := policy.Grant(m)
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("want ErrCapabilityDenied, got %v", err)
	}
	if !strings.Contains(err.Error(), "cap.http.fetch") {
		t.Errorf("denied error should name the capability, got %q", err.Error())
	}
}

func ExamplePolicy_Grant() {
	m, _ := ParseManifest([]byte(`{"name":"enricher","version":"2.0.0","capabilities":["cap.log","cap.kv.read"]}`))
	policy := NewPolicy(CapLog, CapKVRead)
	granted, _ := policy.Grant(m)
	fmt.Println(granted.Sorted())
	// Output: [cap.kv.read cap.log]
}

// Your turn: a plugin may legitimately request nothing at all. Prove that an
// empty requested list yields an empty but non-nil grant, so callers can range
// over it without a nil check.
func TestEmptyGrantIsNonNil(t *testing.T) {
	t.Parallel()
	granted, err := NewPolicy(CapLog).Grant(Manifest{Name: "noop"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if granted == nil {
		t.Fatal("grant must be non-nil even when empty")
	}
	if len(granted) != 0 {
		t.Errorf("grant length = %d, want 0", len(granted))
	}
}
```

## Review

The model is correct when a grant is exactly `requested ∩ allowed` and every
rejection is a typed sentinel. The two rejections are not interchangeable: an
unknown capability means the manifest names a power the host cannot enforce
(`ErrUnknownCapability`), while a denied capability means a known power the
operator withholds (`ErrCapabilityDenied`). Collapsing them into one error, or
into a bare string, loses the distinction an operator needs to tell a
misconfigured plugin from a malicious one. The most common mistake is treating
the manifest as authorization — granting whatever it asks for; the whole point of
`Policy` is that the manifest is a request the operator still has to approve.

Keep the grant non-nil even when empty (a plugin that requests nothing is valid,
not an error), keep `Sorted` deterministic so audit output and tests are stable,
and keep the wrap so `err.Error()` still names the offending capability. Run
`go test -race` to confirm the whole thing under the race detector.

## Resources

- [`encoding/json`](https://pkg.go.dev/encoding/json) — `Unmarshal`, struct tags, and `json.RawMessage` for opaque plugin config.
- [`slices`](https://pkg.go.dev/slices) — `slices.Contains`, `slices.Sorted`, and `slices.Equal`.
- [`maps`](https://pkg.go.dev/maps) — `maps.Keys`, the iterator `slices.Sorted` consumes.
- [`cmp`](https://pkg.go.dev/cmp) — `cmp.Or` for defaulting and `cmp.Ordered` for the sortable key type.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-capability-scoped-host-module.md](02-capability-scoped-host-module.md)
