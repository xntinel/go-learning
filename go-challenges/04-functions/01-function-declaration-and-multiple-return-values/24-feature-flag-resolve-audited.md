# Exercise 24: Feature Flag Resolution With Decision Audit

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature flag rollout is only as trustworthy as its audit trail — a
compliance review asking "who decided this flag was on for this user" needs
a concrete answer, not "check the logs and guess". This exercise builds
`Resolve(flag, userID) (enabled bool, source string, auditID string)`,
resolving through a per-user override, an org default, and a global
default in priority order, while recording every decision under a fresh,
deterministic audit ID.

This module is fully self-contained: its own `go mod init`, all code
inline, one quick test file.

## What you'll build

```text
flagresolve/                  independent module: example.com/feature-flag-resolve-audited
  go.mod                      go 1.24
  flagresolve.go                package flagresolve; Resolver; NewSequentialIDGen; Resolve(flag,userID) (enabled,source,auditID)
  cmd/
    demo/
      main.go                   user override, org default, an unconfigured flag, and the resulting audit log
  flagresolve_test.go           priority order plus deterministic audit IDs; global-default and unknown-flag fallback
```

- Files: `flagresolve.go`, `cmd/demo/main.go`, `flagresolve_test.go`.
- Implement: `(*Resolver).Resolve(flag, userID string) (enabled bool, source string, auditID string)` checking a user override, then an org default, then a global default, then falling back to `("unknown", false)`, appending an `AuditEntry` on every call using an injected `nextAuditID func() string`.
- Test: a user override outranks the org default; the org default outranks the global default; an unconfigured flag resolves to `enabled == false, source == "unknown"`; audit IDs are assigned in call order and deterministic under `NewSequentialIDGen`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/flagresolve/cmd/demo
cd ~/go-exercises/flagresolve
go mod init example.com/feature-flag-resolve-audited
go mod edit -go=1.24
```

### The audit ID is not an error, and it is not random

`Resolve` never fails — an unconfigured flag is exactly the same kind of
ordinary absence this lesson has treated as a non-error result throughout,
resolved to `("unknown", false)` rather than wrapped in an `error`. What it
adds beyond a plain `(enabled, source)` pair is a third value whose only
job is traceability: every resolution gets its own `auditID`, appended to
an internal log a compliance reviewer can later pull with `AuditLog()`.

Generating that ID with `crypto/rand` or a real UUID library would work in
production, but it would make this package's own tests non-reproducible —
asserting "the second call gets audit ID X" only works if ID generation is
injected and deterministic, the same reasoning this lesson has applied to
clocks and environment lookups:

```go
func NewSequentialIDGen(prefix string) func() string {
	var counter int64
	return func() string {
		n := atomic.AddInt64(&counter, 1)
		return fmt.Sprintf("%s-%d", prefix, n)
	}
}
```

`atomic.AddInt64` makes the generator itself safe to share across
concurrent `Resolve` calls without its own mutex, while `Resolver`'s own
`sync.Mutex` still protects the three flag maps and the audit log —
resolution and audit-logging happen as one atomic step per call, so two
concurrent `Resolve` calls for the same flag can never interleave their
log entries.

Create `flagresolve.go`:

```go
package flagresolve

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// AuditEntry records one resolution decision, for compliance review of
// exactly which flags were on for which user and why.
type AuditEntry struct {
	AuditID string
	Flag    string
	UserID  string
	Enabled bool
	Source  string
}

// Resolver resolves feature flags in priority order: a per-user override,
// then a per-org default, then a global default, then "unknown" (false).
// Every resolution is appended to an internal audit log with a fresh
// audit ID, so a compliance question ("who decided this flag was on for
// this user, on this date") has a concrete answer.
type Resolver struct {
	mu             sync.Mutex
	userOverrides  map[string]bool // key: flag + "|" + userID
	orgDefaults    map[string]bool // key: flag
	globalDefaults map[string]bool // key: flag
	nextAuditID    func() string
	log            []AuditEntry
}

// NewResolver builds a Resolver. nextAuditID is the injectable ID
// generator (pass NewSequentialIDGen in production or tests for
// deterministic, inspectable IDs — never a raw random/UUID call, which
// would make audit-log assertions non-reproducible).
func NewResolver(nextAuditID func() string) *Resolver {
	return &Resolver{
		userOverrides:  make(map[string]bool),
		orgDefaults:    make(map[string]bool),
		globalDefaults: make(map[string]bool),
		nextAuditID:    nextAuditID,
	}
}

// NewSequentialIDGen returns a concurrency-safe generator producing
// "prefix-1", "prefix-2", ... in call order.
func NewSequentialIDGen(prefix string) func() string {
	var counter int64
	return func() string {
		n := atomic.AddInt64(&counter, 1)
		return fmt.Sprintf("%s-%d", prefix, n)
	}
}

func userKey(flag, userID string) string { return flag + "|" + userID }

// SetUserOverride pins flag to enabled for one specific user, overriding
// org and global defaults.
func (r *Resolver) SetUserOverride(flag, userID string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userOverrides[userKey(flag, userID)] = enabled
}

// SetOrgDefault sets the org-wide default for flag.
func (r *Resolver) SetOrgDefault(flag string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.orgDefaults[flag] = enabled
}

// SetGlobalDefault sets the system-wide default for flag.
func (r *Resolver) SetGlobalDefault(flag string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.globalDefaults[flag] = enabled
}

// Resolve decides whether flag is enabled for userID, returning which
// tier made the decision and a fresh audit ID for that decision. An
// unrecognized flag (present in no tier) resolves to disabled with
// source "unknown" — it is not an error, since a flag that was never
// configured for this environment is an entirely ordinary state.
func (r *Resolver) Resolve(flag, userID string) (enabled bool, source string, auditID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if v, ok := r.userOverrides[userKey(flag, userID)]; ok {
		enabled, source = v, "user_override"
	} else if v, ok := r.orgDefaults[flag]; ok {
		enabled, source = v, "org_default"
	} else if v, ok := r.globalDefaults[flag]; ok {
		enabled, source = v, "global_default"
	} else {
		enabled, source = false, "unknown"
	}

	auditID = r.nextAuditID()
	r.log = append(r.log, AuditEntry{
		AuditID: auditID,
		Flag:    flag,
		UserID:  userID,
		Enabled: enabled,
		Source:  source,
	})
	return enabled, source, auditID
}

// AuditLog returns a copy of every recorded resolution, oldest first.
func (r *Resolver) AuditLog() []AuditEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AuditEntry, len(r.log))
	copy(out, r.log)
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/feature-flag-resolve-audited"
)

func main() {
	resolver := flagresolve.NewResolver(flagresolve.NewSequentialIDGen("audit"))
	resolver.SetGlobalDefault("new-checkout", false)
	resolver.SetOrgDefault("new-checkout", true)
	resolver.SetUserOverride("new-checkout", "user-42", false)

	enabled, source, auditID := resolver.Resolve("new-checkout", "user-42")
	fmt.Printf("user-42:  enabled=%t source=%s auditID=%s\n", enabled, source, auditID)

	enabled, source, auditID = resolver.Resolve("new-checkout", "user-99")
	fmt.Printf("user-99:  enabled=%t source=%s auditID=%s\n", enabled, source, auditID)

	enabled, source, auditID = resolver.Resolve("unconfigured-flag", "user-99")
	fmt.Printf("unknown:  enabled=%t source=%s auditID=%s\n", enabled, source, auditID)

	fmt.Println("audit log:")
	for _, entry := range resolver.AuditLog() {
		fmt.Printf("  %+v\n", entry)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user-42:  enabled=false source=user_override auditID=audit-1
user-99:  enabled=true source=org_default auditID=audit-2
unknown:  enabled=false source=unknown auditID=audit-3
audit log:
  {AuditID:audit-1 Flag:new-checkout UserID:user-42 Enabled:false Source:user_override}
  {AuditID:audit-2 Flag:new-checkout UserID:user-99 Enabled:true Source:org_default}
  {AuditID:audit-3 Flag:unconfigured-flag UserID:user-99 Enabled:false Source:unknown}
```

### Test

Create `flagresolve_test.go`:

```go
package flagresolve

import (
	"fmt"
	"testing"
)

func TestResolvePriorityAndAudit(t *testing.T) {
	t.Parallel()
	resolver := NewResolver(NewSequentialIDGen("audit"))
	resolver.SetGlobalDefault("flag", false)
	resolver.SetOrgDefault("flag", true)
	resolver.SetUserOverride("flag", "user-1", false)

	tests := []struct {
		name       string
		userID     string
		wantEnable bool
		wantSource string
	}{
		{"user override wins", "user-1", false, "user_override"},
		{"falls through to org default", "user-2", true, "org_default"},
	}

	for i, tc := range tests {
		enabled, source, auditID := resolver.Resolve("flag", tc.userID)
		if enabled != tc.wantEnable || source != tc.wantSource {
			t.Fatalf("%s: Resolve = (%t, %q), want (%t, %q)", tc.name, enabled, source, tc.wantEnable, tc.wantSource)
		}
		wantAuditID := fmt.Sprintf("audit-%d", i+1)
		if auditID != wantAuditID {
			t.Fatalf("%s: auditID = %q, want %q", tc.name, auditID, wantAuditID)
		}
	}

	log := resolver.AuditLog()
	if len(log) != len(tests) {
		t.Fatalf("AuditLog has %d entries, want %d", len(log), len(tests))
	}
}

func TestResolveFallsBackToGlobalThenUnknown(t *testing.T) {
	t.Parallel()
	resolver := NewResolver(NewSequentialIDGen("audit"))
	resolver.SetGlobalDefault("flag", true)

	enabled, source, auditID := resolver.Resolve("flag", "any-user")
	if !enabled || source != "global_default" {
		t.Fatalf("Resolve = (%t, %q), want (true, global_default)", enabled, source)
	}
	if auditID == "" {
		t.Fatal("auditID is empty")
	}

	enabled, source, _ = resolver.Resolve("never-configured", "any-user")
	if enabled || source != "unknown" {
		t.Fatalf("Resolve for unconfigured flag = (%t, %q), want (false, unknown)", enabled, source)
	}
}
```

## Review

`Resolve` is correct when the priority order never inverts — a user
override always wins when present, regardless of what the org or global
tiers say — and when every call, including ones that fall all the way
through to `"unknown"`, gets a distinct audit ID appended to the log.
`TestResolvePriorityAndAudit`'s exact-match assertion on `audit-1` then
`audit-2` is the test that would catch a resolver built with a real UUID
or `time.Now()`-seeded ID generator instead of an injected one — those
can't be asserted on precisely, which is itself a sign the dependency
should have been injected in the first place.

The mistake to avoid is checking tiers with three independent `if`
statements instead of `if / else if / else` — three plain `if`s with no
`else` would let a later, lower-priority tier overwrite `enabled` and
`source` after a higher-priority tier already set them, silently inverting
the priority order the moment a flag exists in more than one tier at once.

## Resources

- [sync/atomic package docs](https://pkg.go.dev/sync/atomic) — `atomic.AddInt64`, making `NewSequentialIDGen` safe to share across goroutines without its own mutex.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the three flag maps and the audit log as one critical section per `Resolve` call.
- [Martin Fowler: Feature Toggles](https://martinfowler.com/articles/feature-toggles.html) — the layered-override rollout model (user, then cohort/org, then global) this resolver implements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-webhook-validator-collect-errors.md](23-webhook-validator-collect-errors.md) | Next: [25-kafka-offset-commit-verify.md](25-kafka-offset-commit-verify.md)
