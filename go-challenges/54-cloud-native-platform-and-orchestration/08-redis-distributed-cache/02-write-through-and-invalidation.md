# Exercise 2: Write-Through and Delete-on-Write Invalidation

The read path from Exercise 1 has a write-path counterpart: when the authoritative
store changes, the cache must be made coherent. This exercise implements both
strategies side by side — delete-on-write and write-through — so the trade-off is
concrete, batches the cache mutations with a pipeline, and includes a test that
documents the dual-write hazard honestly.

This module is fully self-contained: its own `go mod init`, its own demo, and its
own tests against an in-process `miniredis`.

## What you'll build

```text
writethrough/                 independent module: example.com/writethrough
  go.mod                      go 1.26; requires go-redis and miniredis
  store.go                    DB (origin), Repo{ReadUser, UpdateDeleteOnWrite,
                              UpdateWriteThrough, UpdateOriginOnly}
  cmd/
    demo/
      main.go                 before/after cache state for both strategies
  store_test.go               delete-vs-write-through, pipeline both keys, dual-write hazard
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `ReadUser` (cache-aside), `UpdateDeleteOnWrite` (origin write then pipelined `Del` of the entity key and the aggregate list key), `UpdateWriteThrough` (origin write then pipelined `SetArgs` of the entity value plus `Del` of the list), and `UpdateOriginOnly` (origin write only, to model the hazard).
- Test: delete-on-write removes the key so the next read re-loads; write-through leaves it present with the new value and the next read is a cache hit; the pipeline deletes both keys; the dual-write hazard serves stale data until the TTL expires.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get github.com/redis/go-redis/v9@latest
go get github.com/alicebob/miniredis/v2@latest
```

### Two invalidation strategies, one pipeline, one honest hazard

The origin here is a `DB` — a stand-in for Postgres that counts its reads, so a
test can prove that a cache hit did not touch it. We cache two kinds of key: the
entity key `u:v1:user:<id>` and an aggregate `u:v1:all` list key. The list key
matters because it is what forces a real design decision on the write path: you
cannot cheaply "write through" an aggregate, so it must be invalidated.

Delete-on-write is the simpler strategy: write the origin, then delete the cache
keys and let the next read re-populate from the origin. It is self-healing — the
cache is never wrong for longer than it takes the next reader to reload — and it
never caches a value nobody asked for. Its cost is a guaranteed miss on the next
read. `UpdateDeleteOnWrite` deletes both the entity key and the list key in one
`Pipelined` call, which batches both `Del` commands into a single round trip.

Write-through keeps the entity hot: write the origin, then overwrite the cached
value with the new one so the next read is a hit. `UpdateWriteThrough` uses
`SetArgs` for the overwrite, which lets you set the TTL explicitly (`redis.SetArgs{
TTL: r.ttl}`) rather than relying on the plain `Set` signature; the aggregate list,
which cannot be written through, is deleted in the same pipeline. The cost is a
cache write on every origin write and the risk — under concurrent writers — of
caching a stale value if two updates interleave.

Neither ordering is atomic across the two systems. `UpdateOriginOnly` exists to
make that visible: it writes the origin and deliberately skips the cache mutation,
modelling a process that crashed between the two writes. A subsequent `ReadUser`
then serves the *stale* cached value — and the test asserts exactly that, then
shows the entry self-correcting once the TTL expires. That is the dual-write window,
named and bounded rather than pretended away.

Create `store.go`:

```go
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// User is the entity we cache.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// DB stands in for the authoritative store (Postgres, etc.). It counts reads so a
// test can prove a cache hit did not touch the origin.
type DB struct {
	mu    sync.Mutex
	rows  map[string]User
	Reads int
}

func NewDB() *DB {
	return &DB{rows: make(map[string]User)}
}

func (d *DB) Get(id string) (User, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Reads++
	u, ok := d.rows[id]
	return u, ok
}

func (d *DB) Put(u User) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rows[u.ID] = u
}

const namespace = "u:v1"

func userKey(id string) string { return namespace + ":user:" + id }
func listKey() string          { return namespace + ":all" }

// Repo reads users cache-aside and keeps the cache coherent on writes.
type Repo struct {
	db  *DB
	rdb *redis.Client
	ttl time.Duration
}

func New(db *DB, rdb *redis.Client, ttl time.Duration) *Repo {
	return &Repo{db: db, rdb: rdb, ttl: ttl}
}

// ReadUser is a cache-aside read: hit -> decode; miss -> load and populate;
// Redis error -> fail open to the origin without caching.
func (r *Repo) ReadUser(ctx context.Context, id string) (User, bool, error) {
	key := userKey(id)
	raw, err := r.rdb.Get(ctx, key).Bytes()
	switch {
	case err == nil:
		var u User
		if json.Unmarshal(raw, &u) == nil {
			return u, true, nil
		}
		// decode failure: fall through and reload
	case errors.Is(err, redis.Nil):
		// miss: fall through and load
	default:
		// Redis unreachable: fail open, do not cache
		u, ok := r.db.Get(id)
		return u, ok, nil
	}

	u, ok := r.db.Get(id)
	if !ok {
		return User{}, false, nil
	}
	if b, merr := json.Marshal(u); merr == nil {
		_ = r.rdb.Set(ctx, key, b, r.ttl).Err()
	}
	return u, true, nil
}

// UpdateDeleteOnWrite persists to the origin, then invalidates the cache by
// deleting the entity key and the aggregate list key in one pipeline round trip.
// The next read re-populates from the origin (self-healing).
func (r *Repo) UpdateDeleteOnWrite(ctx context.Context, u User) error {
	r.db.Put(u)
	_, err := r.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, userKey(u.ID))
		p.Del(ctx, listKey())
		return nil
	})
	return err
}

// UpdateWriteThrough persists to the origin, then overwrites the cached entity
// value (with an explicit TTL via SetArgs) so the next read is a hit. The
// aggregate list cannot be written through, so it is invalidated instead.
func (r *Repo) UpdateWriteThrough(ctx context.Context, u User) error {
	r.db.Put(u)
	b, err := json.Marshal(u)
	if err != nil {
		return err
	}
	_, err = r.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.SetArgs(ctx, userKey(u.ID), b, redis.SetArgs{TTL: r.ttl})
		p.Del(ctx, listKey())
		return nil
	})
	return err
}

// UpdateOriginOnly writes the origin and deliberately skips the cache mutation,
// modelling the dual-write hazard: a crash between the two writes leaves the cache
// serving stale data until its TTL expires.
func (r *Repo) UpdateOriginOnly(u User) {
	r.db.Put(u)
}
```

### The runnable demo

The demo seeds the cache with a read, then shows the cache state before and after
each strategy: delete-on-write removes the key (and a re-read repopulates it), and
write-through leaves the key present holding the new serialized value.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/writethrough"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func main() {
	s, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer s.Close()

	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer rdb.Close()

	db := repo.NewDB()
	db.Put(repo.User{ID: "1", Name: "alice", Email: "alice@example.com"})
	r := repo.New(db, rdb, time.Minute)
	ctx := context.Background()

	r.ReadUser(ctx, "1") // populate cache
	fmt.Printf("seeded cache:          exists=%v\n", s.Exists("u:v1:user:1"))

	r.UpdateDeleteOnWrite(ctx, repo.User{ID: "1", Name: "alice2", Email: "a2@example.com"})
	fmt.Printf("after delete-on-write: exists=%v\n", s.Exists("u:v1:user:1"))

	u, _, _ := r.ReadUser(ctx, "1")
	fmt.Printf("re-read repopulates:   name=%s exists=%v\n", u.Name, s.Exists("u:v1:user:1"))

	r.UpdateWriteThrough(ctx, repo.User{ID: "1", Name: "alice3", Email: "a3@example.com"})
	got, _ := s.Get("u:v1:user:1")
	fmt.Printf("after write-through:   cached=%s\n", got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
seeded cache:          exists=true
after delete-on-write: exists=false
re-read repopulates:   name=alice2 exists=true
after write-through:   cached={"id":"1","name":"alice3","email":"a3@example.com"}
```

### Tests

The tests seed the cache via a read, then exercise each strategy. `s.Exists`
inspects the miniredis keyspace directly to assert presence or absence;
`db.Reads` proves that a write-through read is served from the cache and never
touches the origin. `TestDualWriteHazard` is the important one: it asserts the
cache serves *stale* data after an origin-only write, and that `FastForward` past
the TTL recovers the fresh value.

Create `store_test.go`:

```go
package repo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRepo(t *testing.T) (*Repo, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return New(NewDB(), rdb, time.Minute), s
}

func TestUpdateDeleteOnWrite(t *testing.T) {
	t.Parallel()
	r, s := newRepo(t)
	r.db.Put(User{ID: "1", Name: "alice"})
	ctx := context.Background()

	if _, _, err := r.ReadUser(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if !s.Exists(userKey("1")) {
		t.Fatal("cache not seeded by read")
	}
	if err := r.UpdateDeleteOnWrite(ctx, User{ID: "1", Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	if s.Exists(userKey("1")) {
		t.Fatal("delete-on-write should have removed the key")
	}
	u, _, _ := r.ReadUser(ctx, "1")
	if u.Name != "bob" {
		t.Fatalf("re-read name = %q, want bob", u.Name)
	}
}

func TestUpdateWriteThrough(t *testing.T) {
	t.Parallel()
	r, s := newRepo(t)
	r.db.Put(User{ID: "1", Name: "alice"})
	ctx := context.Background()
	r.ReadUser(ctx, "1")

	if err := r.UpdateWriteThrough(ctx, User{ID: "1", Name: "carol"}); err != nil {
		t.Fatal(err)
	}
	if !s.Exists(userKey("1")) {
		t.Fatal("write-through should keep the key present")
	}
	readsBefore := r.db.Reads
	u, found, _ := r.ReadUser(ctx, "1")
	if !found || u.Name != "carol" {
		t.Fatalf("read after write-through = %+v found=%v, want carol", u, found)
	}
	if r.db.Reads != readsBefore {
		t.Fatalf("origin reads = %d, want %d (served from cache)", r.db.Reads, readsBefore)
	}
}

func TestPipelineInvalidatesBothKeys(t *testing.T) {
	t.Parallel()
	r, s := newRepo(t)
	r.db.Put(User{ID: "1", Name: "alice"})
	ctx := context.Background()
	r.ReadUser(ctx, "1")
	if err := s.Set(listKey(), "[cached-list]"); err != nil {
		t.Fatal(err)
	}

	if err := r.UpdateDeleteOnWrite(ctx, User{ID: "1", Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	if s.Exists(userKey("1")) || s.Exists(listKey()) {
		t.Fatalf("pipeline should delete both keys: user=%v list=%v",
			s.Exists(userKey("1")), s.Exists(listKey()))
	}
}

func TestDualWriteHazard(t *testing.T) {
	t.Parallel()
	r, s := newRepo(t)
	r.db.Put(User{ID: "1", Name: "alice"})
	ctx := context.Background()
	r.ReadUser(ctx, "1") // cache now holds alice

	r.UpdateOriginOnly(User{ID: "1", Name: "dave"}) // origin updated, cache skipped

	u, _, _ := r.ReadUser(ctx, "1")
	if u.Name != "alice" {
		t.Fatalf("expected STALE alice from cache, got %q", u.Name)
	}

	s.FastForward(2 * time.Minute) // past TTL
	u, _, _ = r.ReadUser(ctx, "1")
	if u.Name != "dave" {
		t.Fatalf("after TTL expiry expected fresh dave, got %q", u.Name)
	}
}

func Example() {
	s, _ := miniredis.Run()
	defer s.Close()
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer rdb.Close()
	db := NewDB()
	db.Put(User{ID: "1", Name: "alice"})
	r := New(db, rdb, time.Minute)
	ctx := context.Background()

	r.ReadUser(ctx, "1")
	r.UpdateDeleteOnWrite(ctx, User{ID: "1", Name: "bob"})
	u, _, _ := r.ReadUser(ctx, "1")
	fmt.Println(u.Name, s.Exists(userKey("1")))
	// Output: bob true
}
```

## Review

The two strategies are correct when their post-conditions hold. After
delete-on-write the entity key is gone (`s.Exists` false) and the next read reloads
the updated value and repopulates; after write-through the entity key is present
holding the new value and the next read is a cache hit that leaves `db.Reads`
unchanged. If write-through's read still hits the origin, either the overwrite did
not happen or you read the wrong key. `TestPipelineInvalidatesBothKeys` confirms
the `Pipelined` closure actually issued both `Del`s in one batch — a common bug is
forgetting the aggregate key and leaving a stale list cached.

The mistakes to avoid: do not assume either ordering is atomic across the two
systems — `TestDualWriteHazard` exists precisely to show the stale window and its
TTL-bounded recovery, and pretending it away is the real error. Prefer
delete-on-write plus a bounded TTL as the default; reach for write-through only
when the key is hot enough that a guaranteed post-write miss hurts, and accept the
concurrent-writer staleness risk that comes with it. Run `go test -race` to confirm
the `DB` mutex holds under concurrent access.

## Resources

- [`github.com/redis/go-redis/v9`](https://pkg.go.dev/github.com/redis/go-redis/v9) — `Del`, `Pipelined`/`TxPipelined`, `Pipeliner`, and `SetArgs` with `redis.SetArgs`.
- [Cache-aside with Go (Redis docs)](https://redis.io/docs/latest/develop/use-cases/cache-aside/go/) — the read/write path and why invalidation is best-effort.
- [`github.com/alicebob/miniredis/v2`](https://pkg.go.dev/github.com/alicebob/miniredis/v2) — `Exists`, `Get`, `Set`, and `FastForward` for asserting cache state in tests.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-cache-aside-with-ttl.md](01-cache-aside-with-ttl.md) | Next: [03-stampede-protection.md](03-stampede-protection.md)
