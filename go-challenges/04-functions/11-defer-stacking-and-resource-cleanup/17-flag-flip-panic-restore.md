# Exercise 17: Feature Flag Activation — Restore State on Panic

**Nivel: Intermedio** — validacion rapida (un test corto).

Flipping a feature flag on for the duration of a request is a deliberate act
that should outlive the request, whether it succeeds or returns an ordinary
error. But if the request handler panics, the flag's meaning is undefined —
nobody downstream knows whether "on" reflects an intentional decision or a
half-crashed one. This module snapshots the flag's prior value before
flipping it and restores that snapshot only if a panic unwinds the request,
leaving normal completions (success or error) alone.

## What you'll build

```text
flagflip/                    independent module: example.com/flagflip
  go.mod
  flagflip/flagflip.go        FlagStore (mutex-guarded); ActivateDuringRequest (defer restore-on-panic)
  flagflip/flagflip_test.go   flag stays on after success/error; flag restored (false and true) after panic
  cmd/demo/main.go            runnable demo: a clean request vs. a panicking one
```

- Files: `flagflip/flagflip.go`, `flagflip/flagflip_test.go`, `cmd/demo/main.go`.
- Implement: a mutex-guarded `FlagStore` with `Get(name string) bool`; `ActivateDuringRequest(store *FlagStore, name string, work func() error) (err error)` that locks once to snapshot the flag's prior value and set it to `true` in one critical section, then defers a closure that `recover()`s, and only on a non-nil recovered value restores the prior value before re-`panic`king.
- Test: a request that returns `nil` — flag stays on; a request that returns a non-nil error — flag still stays on (an error is not a panic); a request that panics with the flag previously `false` — flag is restored to `false` and the panic still propagates; a request that panics with the flag previously `true` — flag is restored to `true`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/17-flag-flip-panic-restore/flagflip go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/17-flag-flip-panic-restore/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/17-flag-flip-panic-restore
go mod edit -go=1.24
```

### Restore-on-panic only, not restore-on-any-exit

This is a narrower guard than the snapshot/restore in
`11-config-snapshot-restore-defer.md`, which restores on *any* non-success
exit (error or panic). Here, only a panic triggers the restore. An error
return from `work` is a normal, expected outcome of activating the flag —
the flag flip itself succeeded; whatever failed downstream is unrelated, and
undoing the flip would be surprising to every other in-flight request that
also expects the flag to be on. A panic is different in kind: it means the
handler could not even finish running to produce a normal return value, so
its actions — including "I turned this flag on" — cannot be trusted, and
undoing them is the safer default. The `if r := recover(); r != nil` check is
exactly the fork between these two cases: `recover()` returns `nil` on the
normal paths (both `nil` and non-nil `error` returns take that branch)
and skips the restore.

### One locked critical section for the whole check-then-act

`snapshotAndActivate` reads the flag's current value and sets it to `true`
under a *single* `Lock()`/`Unlock()` pair. Splitting that into a `Get` call
followed by a separate `Set` call would open a window where a concurrent
request could flip the same flag between the read and the write, and
whichever request's restore ran last would clobber the other's intended
prior value. Locking once around both the read and the write makes
"snapshot, then activate" atomic from every other goroutine's point of view.

Create `flagflip/flagflip.go`:

```go
package flagflip

import "sync"

// FlagStore is a tiny, concurrency-safe feature-flag store. Real deployments
// back this with a remote config service; the locking discipline is the
// same regardless of the backend.
type FlagStore struct {
	mu    sync.Mutex
	flags map[string]bool
}

// NewFlagStore returns an empty store; every flag defaults to false.
func NewFlagStore() *FlagStore {
	return &FlagStore{flags: make(map[string]bool)}
}

// Get reports the current value of name.
func (s *FlagStore) Get(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flags[name]
}

func (s *FlagStore) set(name string, val bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flags[name] = val
}

// snapshotAndActivate locks once, reads the flag's current value, flips it
// to true, and returns the prior value -- a single locked check-then-act so
// no other goroutine can observe or change the flag between the read and
// the write.
func (s *FlagStore) snapshotAndActivate(name string) (prior bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prior = s.flags[name]
	s.flags[name] = true
	return prior
}

// ActivateDuringRequest flips name on for the duration of work. If work
// returns normally -- with or without an error -- the flag is left on: a
// feature flag flip is a deliberate, request-scoped decision that should
// stick even if the request itself failed for an unrelated reason. Only an
// unhandled panic, which leaves the flag's meaning undefined for whatever
// caller catches the panic further up, causes the deferred closure to
// restore the flag to its pre-request value before re-raising.
func ActivateDuringRequest(store *FlagStore, name string, work func() error) (err error) {
	prior := store.snapshotAndActivate(name)

	defer func() {
		if r := recover(); r != nil {
			store.set(name, prior)
			panic(r)
		}
	}()

	return work()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/flagflip/flagflip"
)

func main() {
	store := flagflip.NewFlagStore()

	// A request that succeeds: the flag stays on afterward.
	_ = flagflip.ActivateDuringRequest(store, "new-checkout", func() error {
		return nil
	})
	fmt.Println("after successful request:", store.Get("new-checkout"))

	// A request that panics: the flag is restored to its prior value (false).
	func() {
		defer func() { recover() }()
		_ = flagflip.ActivateDuringRequest(store, "beta-search", func() error {
			panic("unexpected nil pointer")
		})
	}()
	fmt.Println("after panicking request:", store.Get("beta-search"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after successful request: true
after panicking request: false
```

### Tests

Create `flagflip/flagflip_test.go`:

```go
package flagflip

import (
	"errors"
	"testing"
)

func TestActivateDuringRequestKeepsFlagOnAfterNormalReturn(t *testing.T) {
	t.Parallel()

	store := NewFlagStore()
	err := ActivateDuringRequest(store, "new-checkout", func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !store.Get("new-checkout") {
		t.Fatal("flag should remain on after a normal return")
	}
}

func TestActivateDuringRequestKeepsFlagOnAfterErrorReturn(t *testing.T) {
	t.Parallel()

	store := NewFlagStore()
	wantErr := errors.New("downstream failed")
	err := ActivateDuringRequest(store, "new-checkout", func() error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	// An ordinary error is not a panic: the flag flip was a deliberate
	// decision and stays in effect even though the request failed.
	if !store.Get("new-checkout") {
		t.Fatal("flag should remain on after an error return (not a panic)")
	}
}

func TestActivateDuringRequestRestoresFlagOnPanic(t *testing.T) {
	t.Parallel()

	store := NewFlagStore()
	store.set("new-checkout", false)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = ActivateDuringRequest(store, "new-checkout", func() error {
			panic("handler blew up")
		})
	}()

	if store.Get("new-checkout") {
		t.Fatal("flag should be restored to its prior value (false) after a panic")
	}
}

func TestActivateDuringRequestRestoresPriorTrueValueOnPanic(t *testing.T) {
	t.Parallel()

	store := NewFlagStore()
	store.set("new-checkout", true) // already on before this request started

	func() {
		defer func() {
			_ = recover()
		}()
		_ = ActivateDuringRequest(store, "new-checkout", func() error {
			panic("handler blew up")
		})
	}()

	if !store.Get("new-checkout") {
		t.Fatal("flag should be restored to its prior value (true) after a panic")
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

The four tests separate the exit paths that must NOT restore (`nil` return,
non-nil `error` return) from the one that must (`panic`), and the last two
tests check both directions of the panic restore — the flag going back to
`false` and going back to `true` — so a bug like "always reset to `false`"
cannot slip through. The mutex matters even though this exercise has no
explicit concurrent test: `snapshotAndActivate`'s read-then-write happens
under one `Lock`, and `set`'s restore happens under its own `Lock`, so any
real deployment that does call `ActivateDuringRequest` from multiple
goroutines gets a correct check-then-act rather than a race between two
requests' snapshot-and-restore pairs. `go vet` (part of `go test`) would flag
copying the `FlagStore`'s `sync.Mutex` by value; this module always passes
`*FlagStore`, never a copy, which is why that trap does not appear here.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [16-savepoint-depth-nested-rollback.md](16-savepoint-depth-nested-rollback.md) | Next: [18-pending-webhook-drain-timeout.md](18-pending-webhook-drain-timeout.md)
