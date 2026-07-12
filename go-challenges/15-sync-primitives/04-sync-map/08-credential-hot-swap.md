# Exercise 8: Atomic credential/secret hot-swap returning the previous value

Rotating a live API key or database credential is a zero-downtime operation: the
new secret must become visible atomically, readers on the request path must never
observe a torn or empty value, and the rotation must hand back the *previous*
secret so it can be audited or zeroized. `sync.Map`'s `Swap` — atomic
replace-and-return — is the primitive for exactly this. This module builds the
secret store and proves that continuous concurrent readers only ever see whole
values while a stream of rotations runs.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
secretstore/                  independent module: example.com/secretstore
  go.mod                      go 1.26
  store.go                    type Secret, type Store; Rotate, Get, Ensure
  cmd/
    demo/
      main.go                 runnable demo: seed, rotate returning previous, read
  store_test.go               first-rotate-loaded-false, concurrent read-during-rotate, Example
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `Store` with `Rotate(name string, s Secret) (Secret, bool)` (Swap), `Get(name string) (Secret, bool)`, `Ensure(name string, s Secret) (Secret, bool)` (LoadOrStore).
- Test: first `Rotate` on an absent key reports `loaded=false`; concurrent `Get` during a stream of `Rotate`s only ever sees whole values from the known set; the final value equals the last write.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir go-solutions/15-sync-primitives/04-sync-map/08-credential-hot-swap && cd go-solutions/15-sync-primitives/04-sync-map/08-credential-hot-swap
```

### Why Swap, and why readers need no lock

`Swap(name, new) (previous, loaded)` installs `new` and returns whatever was there
before, atomically. That single operation is the whole rotation: the new secret
becomes visible to every reader at one instant, and the caller gets the old secret
back for audit or cleanup. There is no window in which the key is absent or holds a
half-written value, because `sync.Map` stores the value as one `any` word — a
reader either sees the complete old secret or the complete new one, never a mix.
That is why `Get` needs no lock and no coordination with the rotator: the atomicity
of `Swap` on the writer side is exactly what makes lock-free reads safe on the
reader side.

The `loaded` bool distinguishes the first install from a real rotation. The first
`Rotate` of a name that was never set returns `(zero, false)` — there was no
previous secret to return. Every later `Rotate` returns `(previousSecret, true)`,
which is what an audit log or a zeroization routine consumes. `Ensure` uses
`LoadOrStore` to seed a secret only if absent, for the common "install the initial
credential, do not disturb a rotation already in flight" case.

The reason this matters over a `map`+`RWMutex` version is not correctness — both
are correct — but that the read path, which every authenticated request hits, never
takes a lock or blocks behind a rotation. For a credential read on the hot path
that lock-free property is the point.

Create `store.go`:

```go
package secretstore

import "sync"

// Secret is a named credential value. It is a comparable struct so tests can use
// it as a set key and so it is safe to store by value.
type Secret struct {
	Name  string
	Value string
}

// Store holds one Secret per name with atomic, zero-downtime rotation. Reads are
// lock-free; rotation is a single atomic Swap.
type Store struct {
	secrets sync.Map // map[string]Secret
}

// NewStore returns an empty store ready for concurrent use.
func NewStore() *Store {
	return &Store{}
}

// Rotate installs s for name and returns the previous secret. loaded is false on
// the first install (no previous secret), true on a real rotation.
func (s *Store) Rotate(name string, secret Secret) (Secret, bool) {
	prev, loaded := s.secrets.Swap(name, secret)
	if !loaded {
		return Secret{}, false
	}
	return prev.(Secret), true
}

// Get returns the current secret for name and whether it is present. It takes no
// lock and never observes a torn value.
func (s *Store) Get(name string) (Secret, bool) {
	v, ok := s.secrets.Load(name)
	if !ok {
		return Secret{}, false
	}
	return v.(Secret), true
}

// Ensure installs secret only if name is absent, returning the secret now in
// effect and whether it was already present.
func (s *Store) Ensure(name string, secret Secret) (Secret, bool) {
	actual, loaded := s.secrets.LoadOrStore(name, secret)
	return actual.(Secret), loaded
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/secretstore"
)

func main() {
	s := secretstore.NewStore()

	// First rotation: no previous secret.
	_, loaded := s.Rotate("db", secretstore.Secret{Name: "db", Value: "pw-v1"})
	fmt.Println("first rotate had previous:", loaded)

	// Second rotation: returns the previous value for audit.
	prev, loaded := s.Rotate("db", secretstore.Secret{Name: "db", Value: "pw-v2"})
	fmt.Printf("rotated, previous was: %q (had previous: %v)\n", prev.Value, loaded)

	cur, _ := s.Get("db")
	fmt.Printf("current: %q\n", cur.Value)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first rotate had previous: false
rotated, previous was: "pw-v1" (had previous: true)
current: "pw-v2"
```

### Tests

`TestFirstRotateHasNoPrevious` pins that the first `Rotate` reports `loaded=false`
and a later one returns the exact previous secret. `TestConcurrentReadDuringRotate`
is the contract: one rotator walks a known sequence of secrets while many readers
call `Get` in a tight loop; every value a reader observes must be a member of the
known set (never empty, never torn), and after the rotator finishes the final value
must equal the last one written. `-race` proves the lock-free reads are clean
against the concurrent writes.

Create `store_test.go`:

```go
package secretstore

import (
	"fmt"
	"sync"
	"testing"
)

func TestFirstRotateHasNoPrevious(t *testing.T) {
	t.Parallel()

	s := NewStore()
	v1 := Secret{Name: "api", Value: "k1"}
	if prev, loaded := s.Rotate("api", v1); loaded {
		t.Fatalf("first Rotate loaded=true prev=%+v, want loaded=false", prev)
	}

	v2 := Secret{Name: "api", Value: "k2"}
	prev, loaded := s.Rotate("api", v2)
	if !loaded || prev != v1 {
		t.Fatalf("second Rotate = (%+v,%v), want (%+v,true)", prev, loaded, v1)
	}
	if cur, _ := s.Get("api"); cur != v2 {
		t.Fatalf("current = %+v, want %+v", cur, v2)
	}
}

func TestConcurrentReadDuringRotate(t *testing.T) {
	t.Parallel()

	s := NewStore()
	const versions = 200
	known := make(map[Secret]bool, versions)
	seq := make([]Secret, versions)
	for i := range versions {
		sec := Secret{Name: "db", Value: fmt.Sprintf("pw-v%d", i)}
		seq[i] = sec
		known[sec] = true
	}
	s.Rotate("db", seq[0]) // seed

	var wg sync.WaitGroup

	// Rotator: walk the known sequence.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i < versions; i++ {
			s.Rotate("db", seq[i])
		}
	}()

	// Readers: every observed value must be a whole member of the known set.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2000 {
				if cur, ok := s.Get("db"); ok && !known[cur] {
					t.Errorf("reader saw unknown/torn secret %+v", cur)
					return
				}
			}
		}()
	}
	wg.Wait()

	if cur, _ := s.Get("db"); cur != seq[versions-1] {
		t.Fatalf("final = %+v, want last write %+v", cur, seq[versions-1])
	}
}

func ExampleStore() {
	s := NewStore()
	s.Rotate("token", Secret{Name: "token", Value: "t1"})
	prev, loaded := s.Rotate("token", Secret{Name: "token", Value: "t2"})
	fmt.Println(prev.Value, loaded)
	cur, _ := s.Get("token")
	fmt.Println(cur.Value)
	// Output:
	// t1 true
	// t2
}
```

## Review

The store is correct when a rotation is a single atomic `Swap` that returns the
previous secret, and readers see only whole values with no lock.
`TestFirstRotateHasNoPrevious` pins the `loaded` semantics; the concurrent
read-during-rotate test proves readers never observe an empty or torn value while
rotations stream through, and that the store settles on the last write. The design
point is that `Swap`'s atomicity on the writer side is what licenses lock-free reads
on the hot path — you get zero-downtime rotation and an audit trail (the returned
previous value) in one operation. The trap to avoid is a read-modify-write rotation
(`Load` then `Store`) that both loses the previous value's atomicity and opens a
window where a reader could see the old value after you meant to replace it; `Swap`
collapses that into one step. Run `go test -race` to confirm the reads are clean
against concurrent rotation.

## Resources

- [sync.Map.Swap](https://pkg.go.dev/sync#Map.Swap) — atomic replace-and-return, added in Go 1.20.
- [sync.Map.LoadOrStore](https://pkg.go.dev/sync#Map.LoadOrStore) — seed-if-absent for `Ensure`.
- [The Go Memory Model](https://go.dev/ref/mem) — `Swap` is both a read and a write operation.

---

Back to [07-versioned-config-cas.md](07-versioned-config-cas.md) | Next: [09-idempotency-guard.md](09-idempotency-guard.md)
