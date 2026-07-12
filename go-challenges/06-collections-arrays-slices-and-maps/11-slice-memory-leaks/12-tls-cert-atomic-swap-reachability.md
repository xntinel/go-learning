# Exercise 12: TLS Certificate Hot-Reload That Leaks Nothing Between Renewals

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A long-running edge server cannot restart every time its TLS certificate
renews -- ACME clients like cert-manager rotate certificates every 60 to 90
days, and a production listener that has been up for two years has been
through a dozen generations of key material. The standard fix is to hold the
active certificate behind an atomic pointer and swap it in on renewal: Envoy's
SDS (Secret Discovery Service) does this, and so does any Go server that backs
`tls.Config.GetCertificate` with an `atomic.Pointer`. Readers -- here, every
in-flight TLS handshake -- load the pointer once and use whatever generation
they got, with no lock and no risk of seeing half of one certificate and half
of the next.

The design temptation this module is built to head off is not a bug in the
swap itself; it is what to do with the certificate the swap just replaced.
It is tempting to keep it: "what if the new certificate is bad and we need
to roll back," so every `Reload` appends the outgoing generation to a
`history` slice instead of letting the atomic pointer's old value fall out
of reach. That instinct is understandable and it is also a leak: a
certificate carries its private key, and a server that renews every quarter
for two years accumulates generations of key material sitting in memory for
no operational reason, because nothing ever reads `history[0]` again. The
correct behavior falls directly out of reachability, not an explicit cleanup
step: once `Reload` installs generation N+1 and `Store` points nowhere at
generation N, generation N is garbage the instant the last in-flight
handshake that captured it finishes -- no cache to evict, nothing to
remember to do.

This module builds `Store`: an atomically-swapped certificate holder with a
validating constructor, sentinel errors for a bad reload, and a documented
copy-on-write contract for what `Current` hands back. The rollback-history
design is not part of that API; it exists only in the test file, as the thing
the tests measure and reject.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
certstore/                module example.com/certstore
  go.mod                   go 1.24
  certstore.go             Certificate, Store; New, Reload, Current, Name; four sentinel errors
  certstore_test.go        validation table, generation increments, aliasing, the history-leak
                           contrast via MemStats, concurrency, ExampleStore_Reload
```

- Files: `certstore.go`, `certstore_test.go`.
- Implement: `New(name string) (*Store, error)` rejecting an empty name with `ErrInvalidName`; `(*Store).Reload(chain, key []byte) (*Certificate, error)` rejecting an empty chain or key with `ErrEmptyChain`/`ErrEmptyKey`, cloning both inputs, assigning the next generation number, and atomically installing the result; `(*Store).Current() (*Certificate, error)` returning `ErrNoCertificateLoaded` before the first successful `Reload`; `(*Store).Name() string`.
- Test: rejected construction and rejected reloads; generation numbers incrementing from 1; a captured handle to an old generation still reading its own fields after a later `Reload`; `Reload` not aliasing the caller's input buffers; the history-leak contrast -- a naive store that keeps every generation retains private key material proportional to renewal count, `Store` does not; safe concurrent `Reload`/`Current`; and `ExampleStore_Reload` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/11-slice-memory-leaks/12-tls-cert-atomic-swap-reachability
cd go-solutions/06-collections-arrays-slices-and-maps/11-slice-memory-leaks/12-tls-cert-atomic-swap-reachability
go mod edit -go=1.24
```

### Reachability decides what a generation costs, not a cleanup step

`atomic.Pointer[Certificate]` gives `Store` two properties for free that a
mutex-protected plain field would need explicit code to get right: `Load`
never observes a torn write (you get generation N in full or generation N+1
in full, never a struct with half of each field), and `Store` never blocks a
concurrent `Load`. Neither of those is this module's subject. What this
module is about is what happens to the *old* `*Certificate` the moment
`Reload` overwrites the pointer:

```go
// The trap: keep every generation "in case we need to roll back."
func (s *leakyStore) Reload(chain, key []byte) {
    cert := &Certificate{Chain: chain, PrivateKey: key, Generation: s.next()}
    s.history = append(s.history, cert)   // every past generation stays reachable
    s.current = cert
}
```

`s.history` is a completely ordinary Go slice, keeping every element
reachable for as long as the slice itself is -- nothing here is a
memory-safety bug, it is a slice retaining data on purpose, the same
operation that makes a cache useful. The problem is that nothing ever reads
`history[0]` again once `history[1]` exists, so retaining it costs real
memory (an RSA or ECDSA private key plus its chain, easily several
kilobytes) for no operational value, multiplied by every renewal over the
server's uptime. `Store`'s fix is the absence of that slice:

```go
func (s *Store) Reload(chain, key []byte) (*Certificate, error) {
    cert := &Certificate{ /* ... */ }
    s.current.Store(cert)   // the old *Certificate is simply overwritten
    return cert, nil
}
```

`s.current` is a single atomic slot, not a growing collection. The old
`*Certificate` that `Store` just replaced has, at that instant, exactly one
path keeping it alive: whatever caller still holds the `*Certificate` a
previous `Current` or `Reload` returned -- typically an in-flight TLS
handshake that loaded the pointer before the swap. Once that handshake
finishes and drops its reference, the old generation is ordinary garbage.
No history to prune: the fix is not writing the retaining code at all.

Create `certstore.go`:

```go
// Package certstore hot-reloads a TLS certificate behind an atomic pointer,
// the pattern a long-running server uses so a certificate renewal (an
// ACME/cert-manager rotation) never blocks or corrupts an in-flight
// handshake -- compare Envoy's SDS (Secret Discovery Service) or a Go
// server's tls.Config.GetCertificate callback backed by an atomic value.
//
// The design choice this package makes on purpose: Store retains only the
// current generation, never a rollback history. Once Reload installs
// generation N+1, generation N has no path back to the Store -- it stays
// reachable only through whatever in-flight handshake goroutine captured
// it before the swap, and once that handshake finishes, its private key
// material is ordinary garbage. See the tests for what a history costs.
package certstore

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"
)

var (
	// ErrInvalidName means New was called with an empty name.
	ErrInvalidName = errors.New("certstore: name must not be empty")
	// ErrEmptyChain means Reload was called with an empty certificate chain.
	ErrEmptyChain = errors.New("certstore: certificate chain must not be empty")
	// ErrEmptyKey means Reload was called with an empty private key.
	ErrEmptyKey = errors.New("certstore: private key must not be empty")
	// ErrNoCertificateLoaded means Current was called before any Reload
	// succeeded.
	ErrNoCertificateLoaded = errors.New("certstore: no certificate loaded yet")
)

// Certificate is one generation of TLS certificate material.
type Certificate struct {
	// Chain is the PEM-encoded certificate chain.
	Chain []byte
	// PrivateKey is the PEM-encoded private key for Chain's leaf certificate.
	PrivateKey []byte
	// Generation is a monotonically increasing sequence number assigned by
	// the Store that produced this Certificate, starting at 1.
	Generation int
}

// Store holds the currently active certificate for one listener and swaps
// it atomically on Reload.
//
// Store is safe for concurrent use: any number of goroutines may call
// Current while Reload runs concurrently, typically from one file-watcher
// or renewal goroutine.
type Store struct {
	name    string
	current atomic.Pointer[Certificate]
	genSeq  atomic.Int64
}

// New returns an empty Store identified by name (typically the listener or
// SNI hostname it serves). It returns ErrInvalidName if name is empty.
// Current returns ErrNoCertificateLoaded until the first successful Reload.
func New(name string) (*Store, error) {
	if name == "" {
		return nil, ErrInvalidName
	}
	return &Store{name: name}, nil
}

// Name reports the identifier this Store was constructed with.
func (s *Store) Name() string { return s.name }

// Reload validates and installs a new certificate generation, atomically
// replacing whatever Store currently holds. It returns ErrEmptyChain or
// ErrEmptyKey if either input is empty; on success it returns the installed
// Certificate.
//
// Reload copies chain and key into the stored Certificate (via
// bytes.Clone), so the caller may reuse or overwrite the slices it passed
// in immediately after Reload returns -- Store never aliases them.
//
// A concurrent Current call during Reload sees either the previous
// generation in full or the new one in full, never a partial mix of
// fields: the swap is a single atomic pointer store.
func (s *Store) Reload(chain, key []byte) (*Certificate, error) {
	if len(chain) == 0 {
		return nil, ErrEmptyChain
	}
	if len(key) == 0 {
		return nil, ErrEmptyKey
	}
	cert := &Certificate{
		Chain:      bytes.Clone(chain),
		PrivateKey: bytes.Clone(key),
		Generation: int(s.genSeq.Add(1)),
	}
	s.current.Store(cert)
	return cert, nil
}

// Current returns the active certificate, or ErrNoCertificateLoaded if
// Reload has never succeeded.
//
// The returned *Certificate is shared by every concurrent caller and must
// be treated as read-only; do not mutate its fields. Once a later Reload
// installs a new generation, this Store holds no reference back to the
// Certificate returned here -- it stays alive only as long as this caller
// (or another that captured it before the swap) keeps it reachable, the
// same reachability rule that governs every other pin in this lesson,
// applied here to a certificate generation instead of a byte slice.
func (s *Store) Current() (*Certificate, error) {
	cert := s.current.Load()
	if cert == nil {
		return nil, fmt.Errorf("%w: store %q", ErrNoCertificateLoaded, s.name)
	}
	return cert, nil
}
```

### Using it

Construct one `Store` per listener at startup, call `Reload` once with the
initial certificate before serving traffic, and wire a file-watcher or ACME
renewal callback to call `Reload` again whenever new material arrives. Every
handshake calls `Current` once and uses the `*Certificate` it got for that
handshake's whole duration -- that single load is what makes the swap
race-free without a lock on the read path.

Two contracts cross the package boundary. `Reload` never aliases the
`chain`/`key` slices the caller passed in, so the caller is free to reuse or
zero those buffers immediately after the call returns. And the
`*Certificate` `Current` hands back is shared and must be treated as
read-only -- copy-on-write only works if readers never mutate what they got.

`ExampleStore_Reload` is the runnable demonstration of this module: `go
test` runs it and compares its standard output against the `// Output:`
comment, so the usage shown below cannot drift from the code.

```go
func ExampleStore_Reload() {
	s, err := New("edge-gateway")
	if err != nil {
		panic(err)
	}

	first, err := s.Reload([]byte("chain-gen1"), []byte("key-gen1"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("installed generation %d\n", first.Generation)

	second, err := s.Reload([]byte("chain-gen2"), []byte("key-gen2"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("installed generation %d\n", second.Generation)

	cur, err := s.Current()
	if err != nil {
		panic(err)
	}
	fmt.Printf("current generation: %d, chain: %s\n", cur.Generation, cur.Chain)

	// The handle captured at the first Reload is still generation 1, even
	// though the Store itself has moved on to generation 2.
	fmt.Printf("captured handle still reads generation %d\n", first.Generation)

	// Output:
	// installed generation 1
	// installed generation 2
	// current generation: 2, chain: chain-gen2
	// captured handle still reads generation 1
}
```

### Tests

`TestNewRejectsEmptyNameAndCurrentBeforeReload` and
`TestReloadRejectsEmptyInputs` pin the four sentinel errors against
`errors.Is`. `TestReloadInstallsAndIncrementsGeneration` checks generation
numbering, that a captured old handle keeps reading its own fields after a
later `Reload`, and that `Reload` does not alias the caller's buffers.

`TestLeakyStoreRetainsEveryGeneration` and
`TestStoreRetainsOnlyCurrentGeneration` are the heart of the module, reusing
the `runtime.ReadMemStats` discipline from Exercise 2: force GC twice, read
`HeapAlloc`, compare a 3.2 MiB allocation's delta (100 renewals at 16 KiB
chain plus 16 KiB key each) against a 1.6 MiB threshold. `leakyStore` is an
unexported test type appending every reloaded certificate to a `history`
slice -- never exported, never reachable from the package API -- and its
test shows the heap growing with every renewal; `Store`'s test performs the
same 100 renewals and stays far below the threshold, since only the current
generation is reachable. Neither test calls `t.Parallel`: `HeapAlloc` is
process-global, so a concurrently allocating test would perturb the reading.

`TestStoreIsSafeForConcurrentUse` runs 50 concurrent `Reload`/`Current` pairs
under `-race`.

Create `certstore_test.go`:

```go
package certstore

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// readHeap returns HeapAlloc after two full GC cycles. The second GC
// completes the sweep started by the first, so the reading is stable.
func readHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// fillBuf allocates an n-byte slice and writes a pattern across it so the
// pages are committed and the object is a genuine, individually reclaimable
// allocation.
func fillBuf(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) + seed
	}
	return b
}

func TestNewRejectsEmptyNameAndCurrentBeforeReload(t *testing.T) {
	t.Parallel()

	if _, err := New(""); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("New(\"\") error = %v, want ErrInvalidName", err)
	}
	s, err := New("edge-gateway")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Current(); !errors.Is(err, ErrNoCertificateLoaded) {
		t.Fatalf("Current() error = %v, want ErrNoCertificateLoaded", err)
	}
}

func TestReloadRejectsEmptyInputs(t *testing.T) {
	t.Parallel()

	s, err := New("edge-gateway")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Reload(nil, []byte("key")); !errors.Is(err, ErrEmptyChain) {
		t.Fatalf("Reload(nil chain) error = %v, want ErrEmptyChain", err)
	}
	if _, err := s.Reload([]byte("chain"), nil); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("Reload(nil key) error = %v, want ErrEmptyKey", err)
	}
	// A failed Reload must not install anything.
	if _, err := s.Current(); !errors.Is(err, ErrNoCertificateLoaded) {
		t.Fatalf("Current() after failed Reloads: err = %v, want ErrNoCertificateLoaded", err)
	}
}

func TestReloadInstallsAndIncrementsGeneration(t *testing.T) {
	t.Parallel()

	s, err := New("edge-gateway")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c1, err := s.Reload([]byte("chain-1"), []byte("key-1"))
	if err != nil || c1.Generation != 1 {
		t.Fatalf("Reload 1: cert=%+v err=%v, want generation 1", c1, err)
	}
	c2, err := s.Reload([]byte("chain-2"), []byte("key-2"))
	if err != nil || c2.Generation != 2 {
		t.Fatalf("Reload 2: cert=%+v err=%v, want generation 2", c2, err)
	}

	if cur, err := s.Current(); err != nil || cur.Generation != 2 || string(cur.Chain) != "chain-2" {
		t.Fatalf("Current() = %+v err=%v, want generation 2 chain-2", cur, err)
	}

	// The generation-1 handle is still valid for whoever captured it, even
	// though the Store has moved on -- the copy-on-write contract at work.
	if c1.Generation != 1 || string(c1.Chain) != "chain-1" {
		t.Fatalf("captured generation 1 changed: %+v", c1)
	}

	// Reload does not alias the caller's buffers: mutating them afterward
	// must not change the stored certificate.
	chain := []byte("original-chain")
	key := []byte("original-key")
	cert, err := s.Reload(chain, key)
	if err != nil {
		t.Fatalf("Reload 3: %v", err)
	}
	chain[0], key[0] = 'X', 'X'
	if cert.Chain[0] == 'X' || cert.PrivateKey[0] == 'X' {
		t.Fatal("mutating the caller's buffers changed the stored certificate")
	}
}

// leakyStore mimics a "keep every generation for rollback" design: every
// reloaded certificate is appended to an ever-growing history instead of
// only the current one being retained. It is never exported and never
// reachable from the package API; it exists so the tests can measure what
// it costs.
type leakyStore struct {
	history []*Certificate
}

func (l *leakyStore) reload(chain, key []byte, gen int) {
	l.history = append(l.history, &Certificate{
		Chain:      bytes.Clone(chain),
		PrivateKey: bytes.Clone(key),
		Generation: gen,
	})
}

// TestLeakyStoreRetainsEveryGeneration is the core of this module: keeping
// a rollback history means every past certificate's private key material
// stays reachable forever, across however many renewals the server lives
// through.
//
// This test deliberately does not call t.Parallel: it forces GC and reads
// process-global heap stats, which a concurrently allocating goroutine
// would perturb.
func TestLeakyStoreRetainsEveryGeneration(t *testing.T) {
	const n = 100
	const size = 16 << 10 // 16 KiB chain + 16 KiB key per generation
	total := int64(n * size * 2)
	half := total / 2

	base := readHeap()

	var ls leakyStore
	for i := range n {
		chain := fillBuf(size, byte(i))
		key := fillBuf(size, byte(i+1))
		ls.reload(chain, key, i+1)
	}

	after := readHeap()
	if delta := int64(after) - int64(base); delta < half {
		t.Fatalf("leaky store did not retain every generation: delta %d bytes, want >= %d", delta, half)
	}
	runtime.KeepAlive(&ls)
}

// TestStoreRetainsOnlyCurrentGeneration is the fix, measured the same way:
// after the same number of reloads, only the most recent generation is
// reachable, so the heap stays far below the naive history's footprint.
func TestStoreRetainsOnlyCurrentGeneration(t *testing.T) {
	const n = 100
	const size = 16 << 10
	total := int64(n * size * 2)
	half := total / 2

	base := readHeap()

	s, err := New("edge-gateway")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range n {
		chain := fillBuf(size, byte(i))
		key := fillBuf(size, byte(i+1))
		if _, err := s.Reload(chain, key); err != nil {
			t.Fatalf("Reload %d: %v", i, err)
		}
	}

	after := readHeap()
	if delta := int64(after) - int64(base); delta >= half {
		t.Fatalf("Store retained more than one generation: delta %d bytes, want < %d", delta, half)
	}
	cur, err := s.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur.Generation != n {
		t.Fatalf("Current().Generation = %d, want %d", cur.Generation, n)
	}
}

func TestStoreIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	s, err := New("edge-gateway")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Reload([]byte("chain-0"), []byte("key-0")); err != nil {
		t.Fatalf("initial Reload: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 50 {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			chain := fmt.Appendf(nil, "chain-%d", i+1)
			key := fmt.Appendf(nil, "key-%d", i+1)
			if _, err := s.Reload(chain, key); err != nil {
				t.Errorf("Reload %d: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.Current(); err != nil {
				t.Errorf("Current: %v", err)
			}
		}()
	}
	wg.Wait()

	cur, err := s.Current()
	if err != nil {
		t.Fatalf("final Current: %v", err)
	}
	if cur.Generation < 1 {
		t.Fatalf("final Generation = %d, want >= 1", cur.Generation)
	}
}

// ExampleStore_Reload is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment.
func ExampleStore_Reload() {
	s, err := New("edge-gateway")
	if err != nil {
		panic(err)
	}

	first, err := s.Reload([]byte("chain-gen1"), []byte("key-gen1"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("installed generation %d\n", first.Generation)

	second, err := s.Reload([]byte("chain-gen2"), []byte("key-gen2"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("installed generation %d\n", second.Generation)

	cur, err := s.Current()
	if err != nil {
		panic(err)
	}
	fmt.Printf("current generation: %d, chain: %s\n", cur.Generation, cur.Chain)

	// The handle captured at the first Reload is still generation 1, even
	// though the Store itself has moved on to generation 2.
	fmt.Printf("captured handle still reads generation %d\n", first.Generation)

	// Output:
	// installed generation 1
	// installed generation 2
	// current generation: 2, chain: chain-gen2
	// captured handle still reads generation 1
}
```

## Review

`Store` is correct when `Reload` installs a new generation atomically --
`TestReloadInstallsAndIncrementsGeneration` pins the numbering, the
copy-on-write read of an old handle, and the no-aliasing contract in one
pass. The lesson the module teaches is what `Reload` does *not* do: it
never appends the outgoing generation anywhere. `TestLeakyStoreRetainsEveryGeneration`
and `TestStoreRetainsOnlyCurrentGeneration` measure the consequence directly
with a `runtime.ReadMemStats` delta -- a naive "keep everything for rollback"
design retains key material proportional to renewal count, while `Store`
retains exactly one generation, because reachability, not an explicit
eviction step, releases the rest. `ErrInvalidName`, `ErrEmptyChain`,
`ErrEmptyKey`, and `ErrNoCertificateLoaded` are all checkable with
`errors.Is`, and `Store` is safe for concurrent `Reload`/`Current` because
the swap is a single `atomic.Pointer.Store`. Run `go test -count=1 -race ./...`.

## Resources

- [`sync/atomic.Pointer`](https://pkg.go.dev/sync/atomic#Pointer) — the lock-free swap primitive `Store` is built on.
- [`tls.Config.GetCertificate`](https://pkg.go.dev/crypto/tls#Config) — the standard library hook a real server wires to a `Store` like this one.
- [Envoy documentation: Secret Discovery Service (SDS)](https://www.envoyproxy.io/docs/envoy/latest/configuration/security/secret) — a real system that hot-swaps TLS material without dropping connections.
- [`runtime.MemStats`](https://pkg.go.dev/runtime#MemStats) — the leak-detection technique this module's core test reuses from Exercise 2.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-ct-merkle-leaf-array-dedup.md](11-ct-merkle-leaf-array-dedup.md) | Next: [13-wal-lazy-iter-seq-reader.md](13-wal-lazy-iter-seq-reader.md)
