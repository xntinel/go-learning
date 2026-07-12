# Exercise 10: Audit a Fat ObjectStore Interface Against Its Call Sites and Split It Cleanly

This is the deliberate, diagnostic version of everything the chapter has built
toward: given a fat `ObjectStore` and three consumers, map which methods each
consumer actually calls, derive the minimal role interfaces those call sites
justify, and migrate to them without a breaking change by keeping the concrete
type and narrowing only parameter types.

## What you'll build

```text
objectstore/                   independent module: example.com/objectstore
  go.mod                       go 1.24
  store.go                     fat ObjectStore + derived roles Uploader, Downloader, Lifecycler
  consumers.go                 upload(Uploader), download(Downloader), enforceRetention(Lifecycler)
  fake.go                      fakeObjectStore implements the full set
  cmd/
    demo/
      main.go                  upload, download, and enforce retention through the same fake
  store_test.go                var _ role checks; each consumer works with the same fake
```

Files: `store.go`, `consumers.go`, `fake.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: fat `ObjectStore` (Put, Get, Delete, List, Presign, Copy, HeadBucket, SetLifecycle), derive `Uploader`, `Downloader`, `Lifecycler`, and consumers that take only their role.
Test: compile-time `var _ Uploader = (*fakeObjectStore)(nil)` etc.; `upload`, `download`, `enforceRetention` each work with the same fake; no consumer's parameter exposes a method it does not call.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The audit: map call sites, then derive roles

Start from the symptom: `ObjectStore` has eight methods and three consumers each
depend on all eight even though each calls only a few. The audit is mechanical.
Read each consumer and record the methods it invokes:

```text
consumer            methods actually called
uploader            Put, Presign
downloader          Get, HeadBucket
retention job       List, Delete, SetLifecycle
```

The minimal roles fall straight out of that table. `Uploader` is `Put` +
`Presign`. `Downloader` is `Get` + `HeadBucket`. `Lifecycler` is `List` +
`Delete` + `SetLifecycle`. `Copy` is called by nobody, so it belongs in no role
(a real audit would flag it as a candidate for deletion). This is the discipline
from the concepts file made concrete: derive the interface from the call site,
not from the domain noun. There is no `ObjectStore` role because no single
consumer uses the whole surface.

The migration is non-breaking precisely because the concrete `fakeObjectStore`
(standing in for an S3 client wrapper) is untouched — it still has all eight
methods. Only the *parameter types* of `upload`, `download`, and
`enforceRetention` change from `ObjectStore` to the derived role. The same fake
value flows into all three functions with no adapter, because structural
satisfaction means it satisfies every role it has the methods for. A reviewer
reading `func enforceRetention(l Lifecycler)` knows at the signature that
retention cannot upload — the method is not in scope.

Every method takes `context.Context` first; the upload path takes an `io.Reader`
(not a concrete body type) so it composes with the streaming pipeline from
Exercise 4; errors are wrapped with `%w` over a sentinel `ErrNoSuchKey`.

Create `store.go`:

```go
package objectstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNoSuchKey is returned when an object is absent.
var ErrNoSuchKey = errors.New("no such key")

// ObjectStore is the FAT interface: the full surface of the storage client.
// Kept intact so callers that genuinely need everything still compile.
type ObjectStore interface {
	Put(ctx context.Context, key string, body io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
	Presign(ctx context.Context, key string, ttl time.Duration) (string, error)
	Copy(ctx context.Context, src, dst string) error
	HeadBucket(ctx context.Context) error
	SetLifecycle(ctx context.Context, key string, ttl time.Duration) error
}

// Uploader is the role the upload path uses: Put + Presign.
type Uploader interface {
	Put(ctx context.Context, key string, body io.Reader) error
	Presign(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// Downloader is the role the download path uses: Get + HeadBucket.
type Downloader interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	HeadBucket(ctx context.Context) error
}

// Lifecycler is the role the retention job uses: List + Delete + SetLifecycle.
type Lifecycler interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
	SetLifecycle(ctx context.Context, key string, ttl time.Duration) error
}
```

Create `consumers.go`. Each parameter type is the exact derived role:

```go
package objectstore

import (
	"context"
	"fmt"
	"io"
	"time"
)

// upload stores an object and returns a presigned URL. It takes Uploader, so it
// cannot Get, Delete, or List.
func upload(ctx context.Context, u Uploader, key string, body io.Reader) (string, error) {
	if err := u.Put(ctx, key, body); err != nil {
		return "", fmt.Errorf("put %s: %w", key, err)
	}
	url, err := u.Presign(ctx, key, time.Hour)
	if err != nil {
		return "", fmt.Errorf("presign %s: %w", key, err)
	}
	return url, nil
}

// download fetches an object after verifying the bucket. It takes Downloader.
func download(ctx context.Context, d Downloader, key string) ([]byte, error) {
	if err := d.HeadBucket(ctx); err != nil {
		return nil, fmt.Errorf("head bucket: %w", err)
	}
	rc, err := d.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// enforceRetention lists a prefix and deletes objects past their retention,
// setting a lifecycle rule on the rest. It takes Lifecycler; it cannot upload.
func enforceRetention(ctx context.Context, l Lifecycler, prefix string, keep time.Duration) (int, error) {
	keys, err := l.List(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", prefix, err)
	}
	deleted := 0
	for _, k := range keys {
		if err := l.Delete(ctx, k); err != nil {
			return deleted, fmt.Errorf("delete %s: %w", k, err)
		}
		deleted++
	}
	// Apply a lifecycle rule to the prefix marker for future objects.
	if err := l.SetLifecycle(ctx, prefix, keep); err != nil {
		return deleted, fmt.Errorf("set lifecycle %s: %w", prefix, err)
	}
	return deleted, nil
}
```

Create `fake.go`. The concrete type implements the full fat surface:

```go
package objectstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// fakeObjectStore is an in-memory stand-in for a real storage client. It
// implements the full ObjectStore; each consumer narrows to a role.
type fakeObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: make(map[string][]byte)}
}

func (s *fakeObjectStore) Put(_ context.Context, key string, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
	return nil
}

func (s *fakeObjectStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("get: %w", ErrNoSuchKey)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeObjectStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *fakeObjectStore) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *fakeObjectStore) Presign(_ context.Context, key string, ttl time.Duration) (string, error) {
	return fmt.Sprintf("https://store.example/%s?ttl=%d", key, int(ttl.Seconds())), nil
}

func (s *fakeObjectStore) Copy(_ context.Context, src, dst string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[src]
	if !ok {
		return fmt.Errorf("copy: %w", ErrNoSuchKey)
	}
	s.objects[dst] = data
	return nil
}

func (s *fakeObjectStore) HeadBucket(_ context.Context) error { return nil }

func (s *fakeObjectStore) SetLifecycle(_ context.Context, _ string, _ time.Duration) error {
	return nil
}

// Compile-time proof the one concrete type satisfies the fat interface and every
// derived role. A dropped method fails here, not at a distant call site.
var (
	_ ObjectStore = (*fakeObjectStore)(nil)
	_ Uploader    = (*fakeObjectStore)(nil)
	_ Downloader  = (*fakeObjectStore)(nil)
	_ Lifecycler  = (*fakeObjectStore)(nil)
)
```

### The runnable demo

Create `cmd/demo/main.go`. Expose exported wrappers over the unexported
consumers and fake so the demo can drive them.

Add to `consumers.go`:

```go
// Upload, Download, and EnforceRetention are exported entry points over the
// role-narrowed consumers, for demos and external callers.
func Upload(ctx context.Context, u Uploader, key string, body io.Reader) (string, error) {
	return upload(ctx, u, key, body)
}

func Download(ctx context.Context, d Downloader, key string) ([]byte, error) {
	return download(ctx, d, key)
}

func EnforceRetention(ctx context.Context, l Lifecycler, prefix string, keep time.Duration) (int, error) {
	return enforceRetention(ctx, l, prefix, keep)
}
```

Add to `fake.go`:

```go
// NewStore returns a fresh in-memory store as the concrete type; each caller
// narrows to the role it needs.
func NewStore() *fakeObjectStore {
	return newFakeObjectStore()
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"example.com/objectstore"
)

func main() {
	ctx := context.Background()
	store := objectstore.NewStore()

	// upload path receives the store as an Uploader.
	url, _ := objectstore.Upload(ctx, store, "logs/1.txt", strings.NewReader("hello"))
	fmt.Printf("uploaded: %s\n", url)

	// download path receives it as a Downloader.
	data, _ := objectstore.Download(ctx, store, "logs/1.txt")
	fmt.Printf("downloaded: %s\n", data)

	// retention job receives it as a Lifecycler.
	_, _ = objectstore.Upload(ctx, store, "logs/2.txt", strings.NewReader("old"))
	deleted, _ := objectstore.EnforceRetention(ctx, store, "logs/", 24*time.Hour)
	fmt.Printf("retention deleted: %d\n", deleted)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
uploaded: https://store.example/logs/1.txt?ttl=3600
downloaded: hello
retention deleted: 2
```

### Tests

Create `store_test.go`:

```go
package objectstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUploadUsesUploaderRole(t *testing.T) {
	t.Parallel()

	store := newFakeObjectStore()
	// Pass the fake as an Uploader; it cannot reach Get/Delete/List here.
	var u Uploader = store
	url, err := upload(context.Background(), u, "k1", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "k1") {
		t.Fatalf("url = %q, want it to contain k1", url)
	}
}

func TestDownloadUsesDownloaderRole(t *testing.T) {
	t.Parallel()

	store := newFakeObjectStore()
	ctx := context.Background()
	_ = store.Put(ctx, "k1", strings.NewReader("hello world"))

	var d Downloader = store
	data, err := download(ctx, d, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("data = %q, want hello world", data)
	}
}

func TestDownloadMissingKey(t *testing.T) {
	t.Parallel()

	store := newFakeObjectStore()
	_, err := download(context.Background(), store, "ghost")
	if !errors.Is(err, ErrNoSuchKey) {
		t.Fatalf("err = %v, want ErrNoSuchKey", err)
	}
}

func TestEnforceRetentionUsesLifecyclerRole(t *testing.T) {
	t.Parallel()

	store := newFakeObjectStore()
	ctx := context.Background()
	_ = store.Put(ctx, "logs/a", strings.NewReader("1"))
	_ = store.Put(ctx, "logs/b", strings.NewReader("2"))
	_ = store.Put(ctx, "other/c", strings.NewReader("3"))

	var l Lifecycler = store
	deleted, err := enforceRetention(ctx, l, "logs/", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	// The non-matching prefix survived.
	if _, err := store.Get(ctx, "other/c"); err != nil {
		t.Fatalf("other/c should survive: %v", err)
	}
}

func TestSameFakeSatisfiesEveryRole(t *testing.T) {
	t.Parallel()

	store := newFakeObjectStore()
	var (
		_ Uploader    = store
		_ Downloader  = store
		_ Lifecycler  = store
		_ ObjectStore = store
	)
}
```

## Review

The audit is done correctly when each derived role contains exactly the methods
its consumer calls — no more — and the unused `Copy` method ends up in no role,
flagged for review. The migration is non-breaking because the concrete
`fakeObjectStore` never changes: it keeps all eight methods, and only the
consumers' parameter types narrow from `ObjectStore` to a role, which structural
satisfaction makes seamless. The compile-time `var _ Uploader = (*fakeObjectStore)(nil)`
block is the safety net that turns a dropped method into a failure at the type,
not at a call site three packages away. The structural guarantee the tests pin is
that `func enforceRetention(l Lifecycler)` physically cannot upload, so the
retention job's blast radius excludes the write path. Run `go test -race` because
the fake's object map is shared across the concurrent access a real store sees.

## Resources

- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)
- [io package (Reader, ReadCloser, NopCloser, ReadAll)](https://pkg.go.dev/io)
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces)
- [Interface Segregation Principle](https://en.wikipedia.org/wiki/Interface_segregation_principle)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-segregate-pubsub-port.md](09-segregate-pubsub-port.md) | Next: [../07-nil-interface-values/00-concepts.md](../07-nil-interface-values/00-concepts.md)
