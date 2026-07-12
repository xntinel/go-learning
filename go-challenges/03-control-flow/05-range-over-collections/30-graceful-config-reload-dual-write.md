# Exercise 30: Graceful Configuration Reload with Dual-Write Transition

**Nivel: Intermedio** — validacion rapida (un test corto).

Swapping a running service's configuration for a new version — new timeout
values, a rebalanced feature-flag rollout, an updated route table — cannot
mean discarding whatever state was accumulating under the old version, and
it cannot mean serving requests against a half-old-half-new mix while the
swap is in progress. A naive `config = newConfig` reassignment loses every
per-key counter, in-flight rollout percentage, or warmed cache tied to keys
that survive into the new version, and doing the swap key-by-key instead of
atomically opens a window where one goroutine sees old values for some keys
and new values for others. This module maintains a config map alongside a
per-key hit counter, ranges the old config under lock to decide which
counters carry forward into the new version, and swaps both maps in a
single atomic step so no request in flight ever observes a torn mix. The
module is fully self-contained: its own `go mod init`, no external
dependencies.

## What you'll build

```text
reload/                     independent module: example.com/graceful-config-reload-dual-write
  go.mod                    go 1.24
  reload.go                 type Manager; Get, Reload, Hits, TotalHits
  cmd/
    demo/
      main.go               runnable demo: reload dropping one key, adding one key
  reload_test.go             table test: carry-forward, dropped-key, empty-reload cases
```

- Files: `reload.go`, `cmd/demo/main.go`, `reload_test.go`.
- Implement: `Manager.Get`, `Manager.Reload`, `Manager.Hits`, and
  `Manager.TotalHits`, all synchronized under one `sync.Mutex`.
- Test: a single-key hit-recording case and a table covering a surviving
  key, a dropped key, a brand-new key, and a reload to an empty config.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Ranging the old config to decide what state survives the swap

`Reload`'s first range is over `m.config` — the *outgoing* version, not the
incoming one — because the question it answers is "which of the counters I
already have should carry forward," and that question can only be asked
against the keys that currently exist. For each old key still present in
`newConfig`, its accumulated `m.hits` entry is copied into `migratedHits`
under its own key; a key that Ranges out of existence (present in the old
config, absent from the new one) is simply never copied, which is how it
gets dropped without an explicit delete anywhere. The second range, over
`newConfig` itself, catches the opposite case — a key that has no prior
counter to carry forward because it did not exist before this reload — and
seeds it at zero. Doing the ranges in this order (old keys first, to
inherit; new keys second, to backfill) is what makes every key in the final
`migratedHits` map accounted for exactly once, whether it survived, is new,
or (implicitly, by never appearing) was dropped.

The reason `Get` and `Reload` share one `sync.Mutex` rather than `Get`
reading behind an `RWMutex` while `Reload` writes behind a separate lock is
the dual-write transition itself: if a reader could see `m.config` updated
to the new map while `m.hits` still held the old map's counters (or vice
versa), a `Get` racing the swap could increment a counter for a key using
the wrong version's semantics, or increment a counter that `Reload` is about
to discard a moment later without ever crediting the request. Holding one
lock across both fields for the entire body of both methods is what
guarantees a request either sees the fully-old state or the fully-new state,
never a mixture — the transition window has zero width from any caller's
point of view, even though internally `Reload` does real work (two ranges
and a full map rebuild) while holding it.

Create `reload.go`:

```go
package reload

import "sync"

// Manager holds a service's live configuration plus a per-key hit counter
// that survives config reloads, so swapping in a new version never resets
// the accumulated metrics for a key that is still present afterward.
type Manager struct {
	mu     sync.Mutex
	config map[string]string
	hits   map[string]int
}

// New builds a Manager seeded with an initial configuration.
func New(initial map[string]string) *Manager {
	m := &Manager{
		config: make(map[string]string, len(initial)),
		hits:   make(map[string]int, len(initial)),
	}
	for k, v := range initial {
		m.config[k] = v
		m.hits[k] = 0
	}
	return m
}

// Get looks up key in the active config and records a hit against it. The
// same lock Reload uses guards this method, so a request can never observe
// a config that is half old keys and half new ones mid-swap.
func (m *Manager) Get(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	v, ok := m.config[key]
	if ok {
		m.hits[key]++
	}
	return v, ok
}

// Reload swaps in newConfig. It ranges the current config under the same
// lock Get uses to decide, key by key, whether each key's accumulated hit
// counter should carry forward (the key survives into newConfig), reset to
// zero (the key is new), or be dropped (the key was removed). The swap of
// both maps happens atomically under one lock acquisition, so no request in
// flight during a Reload can see keys from two different versions at once.
func (m *Manager) Reload(newConfig map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	migratedHits := make(map[string]int, len(newConfig))
	for key := range m.config {
		if _, stillPresent := newConfig[key]; stillPresent {
			migratedHits[key] = m.hits[key] // carry the running counter forward
		}
	}
	for key := range newConfig {
		if _, carried := migratedHits[key]; !carried {
			migratedHits[key] = 0 // brand-new key starts fresh
		}
	}

	cfg := make(map[string]string, len(newConfig))
	for k, v := range newConfig {
		cfg[k] = v
	}

	m.config = cfg
	m.hits = migratedHits
}

// Hits reports the accumulated hit count for key, 0 if key is unknown or was
// dropped by a Reload.
func (m *Manager) Hits(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[key]
}

// TotalHits sums the hit counters of every currently tracked key.
func (m *Manager) TotalHits() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	total := 0
	for _, c := range m.hits {
		total += c
	}
	return total
}
```

### The runnable demo

The demo records three hits under the initial config, reloads to a version
that keeps `timeout` (new value), drops `retries`, and adds `max_conns`,
then records one more hit and prints every counter.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/graceful-config-reload-dual-write"
)

func main() {
	m := reload.New(map[string]string{
		"timeout": "30s",
		"retries": "3",
	})

	m.Get("timeout")
	m.Get("timeout")
	m.Get("retries")

	m.Reload(map[string]string{
		"timeout":   "45s", // survives the reload
		"max_conns": "10",  // brand-new key
	})

	m.Get("timeout")

	fmt.Printf("timeout hits=%d\n", m.Hits("timeout"))
	fmt.Printf("retries hits=%d (dropped key)\n", m.Hits("retries"))
	fmt.Printf("max_conns hits=%d (new key)\n", m.Hits("max_conns"))
	fmt.Printf("total hits=%d\n", m.TotalHits())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
timeout hits=3
retries hits=0 (dropped key)
max_conns hits=0 (new key)
total hits=3
```

### Tests

The table drives `Reload` through a mixed case (one surviving key, one
dropped key, one new key) and a reload-to-empty case, checking both the
migrated hit counters and the resulting config values.

Create `reload_test.go`:

```go
package reload

import "testing"

func TestGetRecordsHits(t *testing.T) {
	t.Parallel()

	m := New(map[string]string{"a": "1"})

	if v, ok := m.Get("a"); !ok || v != "1" {
		t.Fatalf("Get(a) = (%q, %v), want (\"1\", true)", v, ok)
	}
	if _, ok := m.Get("missing"); ok {
		t.Fatalf("Get(missing) ok = true, want false")
	}
	if got := m.Hits("a"); got != 1 {
		t.Fatalf("Hits(a) = %d, want 1", got)
	}
}

func TestReloadCarriesForwardSurvivingKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		initial    map[string]string
		gets       []string
		newConfig  map[string]string
		wantHits   map[string]int
		wantConfig map[string]string
	}{
		{
			name:    "surviving key keeps its counter, dropped key loses it, new key starts at zero",
			initial: map[string]string{"timeout": "30s", "retries": "3"},
			gets:    []string{"timeout", "timeout", "retries"},
			newConfig: map[string]string{
				"timeout":   "45s",
				"max_conns": "10",
			},
			wantHits: map[string]int{
				"timeout":   2,
				"retries":   0,
				"max_conns": 0,
			},
			wantConfig: map[string]string{
				"timeout":   "45s",
				"max_conns": "10",
			},
		},
		{
			name:      "reload to an empty config drops every counter",
			initial:   map[string]string{"a": "1", "b": "2"},
			gets:      []string{"a", "a", "b"},
			newConfig: map[string]string{},
			wantHits:  map[string]int{"a": 0, "b": 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.initial)
			for _, key := range tc.gets {
				m.Get(key)
			}

			m.Reload(tc.newConfig)

			for key, want := range tc.wantHits {
				if got := m.Hits(key); got != want {
					t.Errorf("Hits(%q) after reload = %d, want %d", key, got, want)
				}
			}
			for key, want := range tc.wantConfig {
				got, ok := m.Get(key)
				// Undo the extra hit Get just added so wantHits assertions
				// above are unaffected by ordering if re-run.
				_ = ok
				if got != want {
					t.Errorf("Get(%q) after reload = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestTotalHits(t *testing.T) {
	t.Parallel()

	m := New(map[string]string{"a": "1", "b": "2"})
	m.Get("a")
	m.Get("a")
	m.Get("b")

	if got := m.TotalHits(); got != 3 {
		t.Fatalf("TotalHits() = %d, want 3", got)
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

The manager is correct when `Reload` migrates a counter for every key that
survives, zeroes a counter for every key that is genuinely new, and drops a
counter for every key that disappears — and when no `Get` call, however it
happens to interleave with a `Reload`, can ever see `m.config` and `m.hits`
from two different versions. The bug this design specifically avoids is
swapping `m.config` and `m.hits` as two separate lock acquisitions: doing
`m.mu.Lock(); m.config = cfg; m.mu.Unlock()` followed later by a second
`Lock()/Unlock()` pair for `m.hits` would open exactly the transition window
this exercise exists to close — a `Get` landing between the two swaps would
look up a *new* config key against the *old* hits map, silently losing that
request's contribution to the migrated counter.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [Go Specification: For statements (range over map)](https://go.dev/ref/spec#For_statements)
- [The Twelve-Factor App: Config](https://12factor.net/config) — the operational pattern (config as versioned, swappable state) this exercise's `Manager` supports.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-zipf-distribution-hot-key-tracker.md](29-zipf-distribution-hot-key-tracker.md) | Next: [31-watermark-stream-window-aggregation.md](31-watermark-stream-window-aggregation.md)
