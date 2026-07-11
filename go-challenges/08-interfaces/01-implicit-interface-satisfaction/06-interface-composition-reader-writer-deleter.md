# Exercise 6: Splitting a Fat Store Into Reader, Writer, and Deleter

A twelve-method repository forces every fake and every decorator to implement
twelve methods. The fix is interface segregation: declare single-purpose
interfaces and let each consumer depend on exactly the one it uses. This module
splits the three-method store from Exercise 1 into `KeyReader`, `KeyWriter`,
`KeyDeleter`, composes a `ReadWriter`, and writes three consumers each typed to
the narrowest interface it needs.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests.

## What you'll build

```text
segregate/                    independent module: example.com/segregate
  go.mod                      go 1.26
  store.go                    KeyReader/KeyWriter/KeyDeleter; ReadWriter composition; *MemoryStore
  consumers.go                WarmCache(KeyReader); BackupKeys(KeyReader); PurgeKey(KeyDeleter)
  cmd/
    demo/
      main.go                 runnable demo: one *MemoryStore through three narrow views
  store_test.go               each consumer via its narrow interface; single-method fake; composition guard
```

- Files: `store.go`, `consumers.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: single-method `KeyReader`/`KeyWriter`/`KeyDeleter`, a composed `ReadWriter`, a `*MemoryStore` satisfying all, and three consumers each accepting only what it uses.
- Test: pass the same `*MemoryStore` to a read-only consumer, a backup consumer, and a purge consumer; fake `KeyReader` with a single method; assert `*MemoryStore` satisfies the composition.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/segregate/cmd/demo
cd ~/go-exercises/segregate
go mod init example.com/segregate
```

### Segregation: the call site drives the interface

The canonical example is the standard library itself: `io.Reader` is one method,
`io.Writer` is one method, and `io.ReadWriter` is their composition
(`interface { Reader; Writer }`). A function that only reads takes an `io.Reader`;
`io.Copy(dst Writer, src Reader)` takes exactly the two capabilities it uses, so
you can pass it anything readable and anything writable, independently.

Applying that here: instead of one `Store` with three methods, declare
`KeyReader` (`Get`), `KeyWriter` (`Set`), `KeyDeleter` (`Delete`). Compose
`ReadWriter` where a consumer needs both read and write. Then each consumer's
signature names the narrowest interface it actually uses:

- A cache-warmer only reads, so it takes `KeyReader`. Its fake needs one method.
- A backup job only reads, so it takes `KeyReader`.
- An admin purge only deletes, so it takes `KeyDeleter`.

The payoff is twofold. First, testability: a fake for the cache-warmer implements
one method, not three. Second, the type system documents and enforces intent — a
function typed to `KeyReader` *cannot* call `Set`, because `Set` is not in its
interface. The read-only guarantee is compile-checked, so a refactor cannot
accidentally make the backup job start writing. `*MemoryStore` implements all three
single-method interfaces (and therefore the composition) because implicit
satisfaction is per-method-set: one concrete type can satisfy many narrow
interfaces at once.

Create `store.go`:

```go
package segregate

import (
	"errors"
	"sync"
)

// ErrNotFound is returned by Get when the key is absent.
var ErrNotFound = errors.New("key not found")

// The store split into single-purpose interfaces.
type KeyReader interface {
	Get(key string) (string, error)
}

type KeyWriter interface {
	Set(key, value string) error
}

type KeyDeleter interface {
	Delete(key string) error
}

// ReadWriter composes the two capabilities a read-write consumer needs, the way
// io.ReadWriter composes io.Reader and io.Writer.
type ReadWriter interface {
	KeyReader
	KeyWriter
}

// MemoryStore satisfies all three single-method interfaces at once.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]string)}
}

func (m *MemoryStore) Get(key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (m *MemoryStore) Set(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *MemoryStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

// Compile-time proof that one concrete type satisfies every narrow interface
// and the composition.
var (
	_ KeyReader  = (*MemoryStore)(nil)
	_ KeyWriter  = (*MemoryStore)(nil)
	_ KeyDeleter = (*MemoryStore)(nil)
	_ ReadWriter = (*MemoryStore)(nil)
)
```

Create `consumers.go` — each takes the narrowest interface it uses:

```go
package segregate

import "errors"

// WarmCache reads a set of keys to prime downstream caches. It depends on
// KeyReader only: its signature cannot call Set or Delete. It returns the keys
// that were present.
func WarmCache(r KeyReader, keys []string) []string {
	var present []string
	for _, k := range keys {
		if _, err := r.Get(k); err == nil {
			present = append(present, k)
		}
	}
	return present
}

// BackupKeys copies the given keys into a plain map. Read-only: KeyReader.
func BackupKeys(r KeyReader, keys []string) map[string]string {
	out := make(map[string]string)
	for _, k := range keys {
		v, err := r.Get(k)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err == nil {
			out[k] = v
		}
	}
	return out
}

// PurgeKey removes a key. Delete-only: KeyDeleter. It cannot read or write.
func PurgeKey(d KeyDeleter, key string) error {
	return d.Delete(key)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/segregate"
)

func main() {
	store := segregate.NewMemoryStore()
	_ = store.Set("user:1", "alice")
	_ = store.Set("user:2", "bob")

	// The same *MemoryStore is viewed through three different narrow interfaces.
	warm := segregate.WarmCache(store, []string{"user:1", "user:2", "user:3"})
	sort.Strings(warm)
	fmt.Printf("warmed: %v\n", warm)

	backup := segregate.BackupKeys(store, []string{"user:1", "user:2"})
	fmt.Printf("backup user:1=%s user:2=%s\n", backup["user:1"], backup["user:2"])

	_ = segregate.PurgeKey(store, "user:1")
	warmAfter := segregate.WarmCache(store, []string{"user:1", "user:2"})
	sort.Strings(warmAfter)
	fmt.Printf("after purge: %v\n", warmAfter)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
warmed: [user:1 user:2]
backup user:1=alice user:2=bob
after purge: [user:2]
```

### Tests

The tests pass the same `*MemoryStore` to consumers typed to different narrow
interfaces, and use `roReader` — a fake that implements only `Get` — to prove the
read-only consumer needs nothing more than one method.

Create `store_test.go`:

```go
package segregate

import (
	"sort"
	"testing"
)

// roReader implements only KeyReader: a one-method fake, which is all a
// read-only consumer requires.
type roReader struct {
	rows map[string]string
}

func (r roReader) Get(key string) (string, error) {
	v, ok := r.rows[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func TestWarmCacheWithOneMethodFake(t *testing.T) {
	t.Parallel()

	r := roReader{rows: map[string]string{"a": "1", "b": "2"}}
	got := WarmCache(r, []string{"a", "b", "missing"})
	sort.Strings(got)
	want := []string{"a", "b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("WarmCache = %v, want %v", got, want)
	}
}

func TestConsumersShareOneStore(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	_ = store.Set("k", "v")

	if got := BackupKeys(store, []string{"k"}); got["k"] != "v" {
		t.Fatalf("backup[k] = %q, want v", got["k"])
	}
	if err := PurgeKey(store, "k"); err != nil {
		t.Fatal(err)
	}
	if got := WarmCache(store, []string{"k"}); len(got) != 0 {
		t.Fatalf("WarmCache after purge = %v, want empty", got)
	}
}

func TestMemoryStoreSatisfiesComposition(t *testing.T) {
	t.Parallel()

	var _ ReadWriter = (*MemoryStore)(nil)
	var rw ReadWriter = NewMemoryStore()
	if err := rw.Set("x", "1"); err != nil {
		t.Fatal(err)
	}
	if v, err := rw.Get("x"); err != nil || v != "1" {
		t.Fatalf("Get = %q,%v, want 1,nil", v, err)
	}
}
```

## Review

Interface segregation is correct when each consumer's signature names only the
methods it calls, single-method interfaces compose into wider ones where needed,
and one concrete type satisfies all of them at once. The tell is the fake: a read-
only consumer's fake implements one method, which is exactly what
`TestWarmCacheWithOneMethodFake` demonstrates. The read-only guarantee is not a
convention — a function typed to `KeyReader` physically cannot call `Set`, because
`Set` is not in the interface, so the compiler enforces the intent. The common
mistake is the fat `Store` with every operation, which forces `WarmCache`'s fake to
stub `Set` and `Delete` it never uses and lets a careless edit make the backup job
start writing. Compose narrow interfaces at the call site instead. Run
`go test -race` to confirm the shared store is concurrency-clean across consumers.

## Resources

- [`io.Reader` / `io.Writer` / `io.ReadWriter`](https://pkg.go.dev/io#ReadWriter) — the canonical single-method interfaces and their composition.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — composing interfaces from smaller ones.
- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — keep interfaces small, at the consumer.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-satisfy-io-writer-sink.md](07-satisfy-io-writer-sink.md)
