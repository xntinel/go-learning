# Exercise 11: Feature Flag Rollout Gate: Override Beats Percentage

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature flag service almost never ships as a plain on/off switch: support
needs to force it on for one debugging customer regardless of the rollout
percentage, and everyone else needs a stable, repeatable answer to "am I in
the 20% getting the new checkout flow." This module builds that decision as
three ordered `if`s: a disabled flag wins outright, a per-user override wins
next, and only then does a deterministic percentage bucket decide.

## What you'll build

```text
flaggate/                   independent module: example.com/flag-rollout-gate
  go.mod                    go 1.24
  flag.go                   Config, Enabled(userID, cfg), bucket(userID)
  flag_test.go              table: disabled, overrides, percentage 0/100, mid bucket
```

- Files: `flag.go`, `flag_test.go`.
- Implement: `Enabled(userID string, cfg Config) bool` where `Config` holds `Disabled bool`, `Overrides map[string]bool`, and `Percentage int`; the comma-ok read `if enabled, ok := cfg.Overrides[userID]; ok { return enabled }` lets an absent override fall through to the percentage check instead of being treated as "override to false."
- Test: a table covering a disabled flag beating a 100% rollout, an override forcing on and off, percentage 0 and 100 for everyone else, and two known user IDs whose deterministic bucket lands inside and outside a 50% rollout.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/flaggate
cd ~/go-exercises/flaggate
go mod init example.com/flag-rollout-gate
go mod edit -go=1.24
```

### Why the order of the three `if`s matters

Each guard answers a narrower question than the one before it. `Disabled`
is an operator kill switch — it must win regardless of anything else, so it
is checked first and returns immediately. The override map is checked next
with comma-ok, because "was there an override at all" is a different
question from "what does the map return for this key": a missing entry
must fall through to the percentage, not be misread as `false`. Only once
both of those are cleared does the percentage bucket — the everyone-else
case — get to decide. Reordering these three checks changes the product
behavior, not just the code style.

Create `flag.go`:

```go
// Package flaggate decides whether a feature flag is enabled for a user.
package flaggate

import "hash/fnv"

// Config is one flag's rollout configuration. Overrides takes precedence
// over Percentage, and Disabled takes precedence over both.
type Config struct {
	Key        string          // unique feature key, used with userID for deterministic bucketing
	Disabled   bool
	Overrides  map[string]bool // per-user forced on/off, checked with comma-ok
	Percentage int             // 0-100, gradual rollout for everyone else
}

// Enabled decides whether the flag is on for userID. The order is: a
// disabled flag always loses; an explicit override always wins over the
// percentage; otherwise feature key + userID is bucketed deterministically
// into 0-99 and compared against Percentage.
func Enabled(userID string, cfg Config) bool {
	if cfg.Disabled {
		return false
	}

	if enabled, ok := cfg.Overrides[userID]; ok {
		return enabled
	}

	if cfg.Percentage <= 0 {
		return false
	}
	if cfg.Percentage >= 100 {
		return true
	}

	return bucket(cfg.Key, userID) < cfg.Percentage
}

// bucket maps feature key + userID to a stable integer in [0, 100) using
// FNV-1a, so the same userID always lands in the same bucket for the same
// feature key across process restarts.
func bucket(featureKey, userID string) int {
	h := fnv.New32a()
	h.Write([]byte(featureKey))
	h.Write([]byte(":"))
	h.Write([]byte(userID))
	return int(h.Sum32() % 100)
}
```

### Tests

The table checks the three guards in isolation — disabled beats a 100%
rollout, an override beats a 100% rollout in the other direction — then
checks the percentage boundaries at 0 and 100, then locks in the
deterministic bucket for two specific user IDs (`bucket("user-1")` is 0,
`bucket("user-2")` is 57), proving a 50% rollout includes one and excludes
the other with no randomness involved.

Create `flag_test.go`:

```go
package flaggate

import "testing"

func TestEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		user string
		cfg  Config
		want bool
	}{
		{
			name: "disabled flag always loses, even with a 100% rollout",
			user: "user-1",
			cfg: Config{
				Key:        "feature-a",
				Disabled:   true,
				Percentage: 100,
			},
			want: false,
		},
		{
			name: "override forces the flag on for one user",
			user: "user-2",
			cfg: Config{
				Key:        "feature-a",
				Overrides:  map[string]bool{"user-2": true},
				Percentage: 0,
			},
			want: true,
		},
		{
			name: "override forces the flag off even at 100%",
			user: "user-2",
			cfg: Config{
				Key:        "feature-a",
				Overrides:  map[string]bool{"user-2": false},
				Percentage: 100,
			},
			want: false,
		},
		{
			name: "percentage 0 disables everyone not overridden",
			user: "user-3",
			cfg: Config{
				Key:        "feature-a",
				Percentage: 0,
			},
			want: false,
		},
		{
			name: "percentage 100 enables everyone not overridden",
			user: "user-3",
			cfg: Config{
				Key:        "feature-a",
				Percentage: 100,
			},
			want: true,
		},
		{
			// bucket("feature-a", "user-1") == 10, which is < 50.
			name: "user-1 falls inside a 50% rollout bucket",
			user: "user-1",
			cfg: Config{
				Key:        "feature-a",
				Percentage: 50,
			},
			want: true,
		},
		{
			// bucket("feature-a", "user-2") == 91, which is not < 50.
			name: "user-2 falls outside a 50% rollout bucket",
			user: "user-2",
			cfg: Config{
				Key:        "feature-a",
				Percentage: 50,
			},
			want: false,
		},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Enabled(tc.user, tc.cfg); got != tc.want {
				t.Errorf("Enabled(%q, %+v) = %v, want %v", tc.user, tc.cfg, got, tc.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The comma-ok override lookup is what makes "no opinion" a distinct case
from "opinion is false" — a map read alone cannot tell them apart, and
conflating them would mean nobody could ever fall through to the
percentage. Carry this forward: whenever a decision has a "specific
override beats a general rule" shape — feature flags, per-customer
pricing, per-tenant limits — comma-ok on the override map, checked before
the general rule, is the correct init-statement shape.

## Resources

- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — the two-result form of a map index and what `ok` means.
- [hash/fnv package docs](https://pkg.go.dev/hash/fnv) — the non-cryptographic hash used for deterministic bucketing.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-idempotency-key-gate.md](10-idempotency-key-gate.md) | Next: [12-tiered-cache-fallback.md](12-tiered-cache-fallback.md)
