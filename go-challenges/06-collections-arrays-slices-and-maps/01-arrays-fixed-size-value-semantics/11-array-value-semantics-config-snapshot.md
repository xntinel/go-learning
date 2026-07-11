# Exercise 11: A Config Snapshot That Cannot Be Corrupted By Its Readers

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A config-reload pipeline (the kind that watches a file, an etcd key, or a
Consul KV prefix and republishes on change) has one non-negotiable
requirement: a subscriber that grabs "the current config" must hold a
picture that cannot be mutated later, by the publisher's next reload or by
another subscriber's careless edit. Whether that requirement holds for free
depends entirely on how the settings list is typed inside the snapshot
struct. Systems like Consul's KV watch API and etcd's watch stream hand
every subscriber a value, not a reference, for exactly this reason — but a
Go struct can look like a value while secretly still being a reference, if
one field inside it is a slice.

This module builds a `Publisher` whose `Snapshot` embeds a fixed
`[MaxSettings]Setting` array and proves the returned value is truly
independent of the publisher's own state, even when readers and a reloading
publisher are hammering it concurrently. The broken slice-field twin that
looks identical but aliases is not part of the package: it lives in the test
file, where a test pins the exact corruption it causes, so the mistake is
demonstrated rather than merely described.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
configsnap/                  module example.com/configsnap
  go.mod                     go 1.24
  config.go                  Setting; Snapshot{Count, Settings [MaxSettings]Setting}; Publisher
  config_test.go             publish table, array isolation, leaky-snapshot contrast,
                              concurrent publish/read, ExamplePublisher_Current
```

- Files: `config.go`, `config_test.go`.
- Implement: `Setting{Key, Value string}`; `Snapshot{Count int; Settings [MaxSettings]Setting}`; `Publisher` with `NewPublisher() *Publisher`, `(*Publisher).Publish(settings []Setting) error` returning `ErrTooManySettings` for more than `MaxSettings` entries, and `(*Publisher).Current() Snapshot` returning the live config by value under an `RWMutex`.
- Test: a table over empty, one setting, exactly `MaxSettings`, and one over `MaxSettings`; a mutation of a returned `Snapshot` never reaching the `Publisher`'s own state; the unexported `leakyPublisher`/`leakySnapshot` twin proving the identical mutation *does* corrupt a slice-backed snapshot; many goroutines publishing and reading at once under `-race`, asserting no torn snapshot is ever observed; and `ExamplePublisher_Current` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/configsnap
cd ~/go-exercises/configsnap
go mod init example.com/configsnap
go mod edit -go=1.24
```

### Why the array field is the whole mechanism

A `Publisher` holds one live `Snapshot` and hands it out to every caller
that asks for the current configuration. Whether that handout is safe comes
down to one line: `return p.current`. Returning a struct by value copies
every field. When `Settings` is a `[MaxSettings]Setting` array, that copy
duplicates every setting into the caller's own memory — the caller's
`Snapshot` and the `Publisher`'s internal one share no storage from the
instant `Current` returns. The caller can mutate `snap.Settings[0].Value`
all day; the `Publisher`'s live config is untouched, and the next call to
`Current` still reflects what was actually published.

The broken alternative looks identical at a glance but stores `Settings` as
a `[]Setting`. Copying that struct copies the slice header — pointer,
length, capacity — not the backing array the pointer refers to:

```go
type leakySnapshot struct {
    Count    int
    Settings []Setting   // header only; copying it does not copy the bytes
}
```

A `currentSnapshot()` method that returns a `leakySnapshot` by value
produces a copy whose `Settings` header still points at the exact same
backing array as the publisher's own state. A caller who mutates an entry in
what they believe is their own snapshot is actually mutating the live
configuration out from under it, and every other subscriber that reads
afterward sees the corruption too. This is the same trap as a byte-slice
"defensive copy" that isn't one, just with struct-valued elements instead of
bytes: the type of the field, not the `return` statement, decides whether
the copy is real.

Create `config.go`:

```go
// Package configsnap publishes point-in-time configuration snapshots to many
// concurrent readers.
//
// A Publisher exists to answer one question a config-reload pipeline (the
// kind that watches a file, an etcd key, or a Consul KV prefix and
// republishes on change) cannot get wrong: once a subscriber holds a
// Snapshot, the next reload must never be able to reach back and mutate it.
// Whether that holds is entirely a function of how the Settings field is
// typed. See the package tests for a broken, slice-field twin that looks
// identical but fails this exact guarantee.
package configsnap

import (
	"errors"
	"sync"
)

// MaxSettings is the fixed capacity of a Snapshot: at most this many
// key/value pairs can be published at once.
const MaxSettings = 8

// ErrTooManySettings means the caller tried to publish more settings than
// MaxSettings, which cannot fit in the fixed-size Snapshot.
var ErrTooManySettings = errors.New("configsnap: too many settings")

// Setting is one key/value configuration entry.
type Setting struct {
	Key   string
	Value string
}

// Snapshot is a fixed-size, value-typed view of a published configuration.
//
// Because Settings is a [MaxSettings]Setting array rather than a slice,
// copying a Snapshot -- by assignment, by return, by parameter -- deep-copies
// every entry: the copy shares no storage with the original. That is what
// makes Publisher.Current a true immutable snapshot instead of an aliased
// view of live state.
type Snapshot struct {
	// Count is the number of valid entries in Settings; entries at index
	// >= Count are the zero Setting and unused.
	Count    int
	Settings [MaxSettings]Setting
}

// Publisher holds the live configuration and hands out point-in-time
// snapshots to subscribers. It models a config-reload pipeline where many
// readers must see a consistent picture even while a reload is replacing the
// configuration underneath them.
//
// Publisher is safe for concurrent use by multiple goroutines.
type Publisher struct {
	mu      sync.RWMutex
	current Snapshot
}

// NewPublisher returns a Publisher with an empty snapshot.
func NewPublisher() *Publisher {
	return &Publisher{}
}

// Publish replaces the current configuration with settings. It returns
// ErrTooManySettings, leaving the previous configuration in place, if
// settings does not fit in the fixed-size Snapshot.
func (p *Publisher) Publish(settings []Setting) error {
	if len(settings) > MaxSettings {
		return ErrTooManySettings
	}
	var next Snapshot
	next.Count = len(settings)
	for i, s := range settings {
		next.Settings[i] = s
	}

	p.mu.Lock()
	p.current = next
	p.mu.Unlock()
	return nil
}

// Current returns the published configuration by value.
//
// Because Snapshot.Settings is an array, this return copies every setting:
// the caller receives an independent picture that cannot be mutated back
// into the Publisher, and a concurrent Publish cannot retroactively change a
// Snapshot a caller is still holding.
func (p *Publisher) Current() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.current
}
```

### Using it

`Publisher` is the whole surface: construct it once where the reload loop
lives, call `Publish` every time a new configuration arrives, and let every
handler goroutine call `Current` whenever it needs the live settings. The
`RWMutex` means any number of readers can call `Current` concurrently with
each other and with a `Publish`, and `TestConcurrentPublishAndCurrentNeverObservesATornSnapshot`
holds that promise to the strongest thing a reader could observe: a
`Snapshot` whose `Count` disagrees with how many of its entries are actually
filled in.

The contract that crosses the package boundary is documented on `Current`:
the returned `Snapshot` is a full, independent copy, so a caller may retain
it, mutate it, or hand it to unrelated code without ever risking the
`Publisher`'s own state — `TestSnapshotArrayIsolatesMutation` pins exactly
that. The module has no `main.go`, because a config publisher is a library,
not a tool. Its executable demonstration is `ExamplePublisher_Current`: `go
test` runs it and compares its standard output against the `// Output:`
comment, so the usage shown below cannot drift away from the code.

### Tests

`TestPublish` is the table: empty settings, one setting, exactly
`MaxSettings`, and one over `MaxSettings` rejected with `ErrTooManySettings`.
`TestSnapshotArrayIsolatesMutation` grabs a `Snapshot`, mutates one setting
in it, and asserts a fresh `Current()` still shows the original value — the
array field held the line.

`TestLeakySnapshotAliasesPublisherState` is the antipattern contrast. It
performs the identical mutation against an unexported `leakyPublisher`, kept
only in the test file and unreachable from the package API, and asserts the
opposite: the leaky publisher's live config *did* change. Pinning a bug as
an explicit, named expectation documents the trap instead of silently
avoiding it — if `Publisher` itself ever regressed to a slice field, this
test's twin of it would already show what breaks.

`TestConcurrentPublishAndCurrentNeverObservesATornSnapshot` runs eight
publishing goroutines against eight reading goroutines and, under `-race`,
asserts every `Snapshot` a reader observes is internally consistent — no
`Count` greater than the number of non-zero entries actually present. That
is the property the `RWMutex` exists to guarantee; the array field alone is
not enough, because without a lock two goroutines could still race on
`p.current`'s multi-word assignment.

Create `config_test.go`:

```go
package configsnap

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// leakySnapshot and leakyPublisher are the WRONG design: the same shape as
// Snapshot and Publisher, but Settings is a []Setting instead of an array.
// They are unexported and unreachable from the package API; they exist only
// so a test can pin the aliasing bug numerically instead of describing it in
// prose.
type leakySnapshot struct {
	Count    int
	Settings []Setting
}

type leakyPublisher struct {
	current leakySnapshot
}

func newLeakyPublisher() *leakyPublisher {
	return &leakyPublisher{current: leakySnapshot{Settings: make([]Setting, 0, MaxSettings)}}
}

// publish keeps the backing slice the leakyPublisher itself will keep
// referencing after this call returns.
func (p *leakyPublisher) publish(settings []Setting) {
	buf := make([]Setting, len(settings))
	copy(buf, settings)
	p.current = leakySnapshot{Count: len(settings), Settings: buf}
}

// currentSnapshot returns the published configuration by value, but the
// struct copy is shallow: leakySnapshot.Settings is a slice header, so the
// returned copy's Settings and p.current.Settings point at the same backing
// array.
func (p *leakyPublisher) currentSnapshot() leakySnapshot {
	return p.current
}

func TestPublish(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings []Setting
		wantErr  error
	}{
		{name: "empty", settings: nil},
		{name: "one setting", settings: []Setting{{Key: "timeout", Value: "30s"}}},
		{
			name: "exactly MaxSettings",
			settings: func() []Setting {
				s := make([]Setting, MaxSettings)
				for i := range s {
					s[i] = Setting{Key: fmt.Sprintf("k%d", i), Value: "v"}
				}
				return s
			}(),
		},
		{
			name: "one over MaxSettings",
			settings: func() []Setting {
				s := make([]Setting, MaxSettings+1)
				for i := range s {
					s[i] = Setting{Key: fmt.Sprintf("k%d", i), Value: "v"}
				}
				return s
			}(),
			wantErr: ErrTooManySettings,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := NewPublisher()
			err := p.Publish(tc.settings)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Publish() err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			snap := p.Current()
			if snap.Count != len(tc.settings) {
				t.Fatalf("Count = %d, want %d", snap.Count, len(tc.settings))
			}
			for i, s := range tc.settings {
				if snap.Settings[i] != s {
					t.Fatalf("Settings[%d] = %+v, want %+v", i, snap.Settings[i], s)
				}
			}
		})
	}
}

func TestSnapshotArrayIsolatesMutation(t *testing.T) {
	t.Parallel()

	p := NewPublisher()
	if err := p.Publish([]Setting{{Key: "timeout", Value: "30s"}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	snap := p.Current()
	snap.Settings[0].Value = "999s" // mutate the caller's copy

	fresh := p.Current()
	if fresh.Settings[0].Value != "30s" {
		t.Fatalf("array snapshot leaked into publisher: got %q, want %q",
			fresh.Settings[0].Value, "30s")
	}
}

// TestLeakySnapshotAliasesPublisherState is the antipattern contrast: the
// identical mutation against leakyPublisher must corrupt the publisher's own
// state, because leakySnapshot.Settings is a slice header shared with
// p.current. Asserting the corruption happens documents the trap instead of
// silently avoiding it.
func TestLeakySnapshotAliasesPublisherState(t *testing.T) {
	t.Parallel()

	p := newLeakyPublisher()
	p.publish([]Setting{{Key: "timeout", Value: "30s"}})

	snap := p.currentSnapshot()
	snap.Settings[0].Value = "999s" // mutate the caller's "copy"

	fresh := p.currentSnapshot()
	if fresh.Settings[0].Value != "999s" {
		t.Fatalf("expected leaky snapshot to alias publisher state: got %q, want %q",
			fresh.Settings[0].Value, "999s")
	}
}

// TestConcurrentPublishAndCurrentNeverObservesATornSnapshot runs many
// Publish and Current calls at once. The RWMutex serializes every whole
// Snapshot assignment and read, so Current must never observe a Snapshot
// whose Count disagrees with how many of its Settings entries are non-zero.
func TestConcurrentPublishAndCurrentNeverObservesATornSnapshot(t *testing.T) {
	t.Parallel()

	p := NewPublisher()
	full := make([]Setting, MaxSettings)
	for i := range full {
		full[i] = Setting{Key: fmt.Sprintf("k%d", i), Value: "v"}
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if err := p.Publish(full); err != nil {
					t.Errorf("Publish: %v", err)
				}
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				snap := p.Current()
				for i := 0; i < snap.Count; i++ {
					if snap.Settings[i] == (Setting{}) {
						t.Errorf("Current() returned a torn snapshot: Count=%d but Settings[%d] is zero", snap.Count, i)
					}
				}
			}
		}()
	}
	wg.Wait()
}

// ExamplePublisher_Current is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment below.
func ExamplePublisher_Current() {
	p := NewPublisher()
	if err := p.Publish([]Setting{
		{Key: "timeout", Value: "30s"},
		{Key: "retries", Value: "3"},
	}); err != nil {
		panic(err)
	}

	snap := p.Current()
	fmt.Printf("count=%d first=%s=%s\n", snap.Count, snap.Settings[0].Key, snap.Settings[0].Value)

	snap.Settings[0].Value = "999s" // mutate the caller's own copy

	fresh := p.Current()
	fmt.Printf("after caller mutation, publisher still has %s=%s\n",
		fresh.Settings[0].Key, fresh.Settings[0].Value)

	tooMany := make([]Setting, MaxSettings+1)
	if err := p.Publish(tooMany); errors.Is(err, ErrTooManySettings) {
		fmt.Println("publish rejected:", err)
	}

	// Output:
	// count=2 first=timeout=30s
	// after caller mutation, publisher still has timeout=30s
	// publish rejected: configsnap: too many settings
}
```

## Review

The array-field `Snapshot` is correct when a mutation of a returned value
never reaches the `Publisher`'s own state, and
`TestSnapshotArrayIsolatesMutation` is the direct proof of that.
`leakySnapshot` is the cautionary twin, isolated in the test file where it
can never be imported by a real caller: its test deliberately asserts the
corruption happens, because pinning a bug as an explicit, named expectation
is how you document a trap instead of silently avoiding it. The rule this
exercise hands you for real config pipelines: any struct you hand out as an
"immutable snapshot" is only immutable to the extent that every field inside
it copies by value on struct assignment — arrays and other structs of arrays
qualify, slices and maps do not. The `RWMutex` around `p.current` is what
extends that guarantee to concurrent readers and a concurrently reloading
publisher, which `TestConcurrentPublishAndCurrentNeverObservesATornSnapshot`
confirms under `-race`. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Assignability and struct copies](https://go.dev/ref/spec#Assignments) — field-by-field copy semantics that make an array field a real defensive copy.
- [Go blog: Arrays, slices (and strings)](https://go.dev/blog/slices) — why a slice header copy still shares the backing array.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — the lock that lets many concurrent readers share one writer-guarded `Snapshot`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-binary-header-slice-to-array.md](10-binary-header-slice-to-array.md) | Next: [12-session-id-array-key-idempotency.md](12-session-id-array-key-idempotency.md)
