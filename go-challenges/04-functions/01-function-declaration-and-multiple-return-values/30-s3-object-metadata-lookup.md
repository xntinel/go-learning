# Exercise 30: S3 Object Metadata With ETag and Size

**Nivel: Intermedio** — validacion rapida (un test corto).

A cache that wants to know "is my copy of this object still current?" needs
exactly what S3's `HeadObject` call gives back: an ETag to compare, and a
size for progress reporting during a multipart upload — without pulling the
object body. This exercise builds `Store.HeadObject(key) (etag string, size
int64, found bool, error)`, keeping "the object was deleted" (a normal,
`nil`-error outcome) distinct from "the request itself failed".

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
s3meta/                    independent module: example.com/s3-object-metadata-lookup
  go.mod                   go 1.24
  s3meta.go                package s3meta; Store; HeadObject(key) (etag,size,found,error)
  cmd/
    demo/
      main.go              cache-validity check, object overwritten, missing key, forced access-denied failure
  s3meta_test.go            found; absent; forced failure is not absence; etag changes after overwrite
```

- Files: `s3meta.go`, `cmd/demo/main.go`, `s3meta_test.go`.
- Implement: `(*Store).HeadObject(key string) (etag string, size int64, found bool, err error)` returning `(etag, size, true, nil)` for a present object, `("", 0, false, nil)` for an absent one, and `("", 0, false, wrapped error)` when the request is forced to fail.
- Test: a present object returns its etag and size; a missing key gives `found == false, err == nil`; a forced failure gives `err != nil` and `found == false` in the same call, distinct from ordinary absence; overwriting an object changes its etag on the next lookup.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why "not found" and "request failed" cannot share one `error`

A cache invalidation check calls `HeadObject` before serving a cached
response: if the etag matches, serve from cache; if it does not (or the
object is gone), refetch. Collapsing "gone" and "request failed" into the
same non-nil `error` makes the cache do the wrong thing on a transient
network blip — treating a timeout as "the object was deleted" would evict
a perfectly good cache entry and force a full refetch storm the moment S3
has one slow response. Conversely, if a genuinely deleted object were
reported as `err != nil`, the cache would keep retrying a HEAD call for an
object that will never come back, instead of correctly treating `found ==
false` as "stop serving this, it's gone".

`size` earns its own return slot because it is useful *before* the object
finishes uploading in a multipart flow: a client polling `HeadObject` while
assembling parts can show upload progress purely from the size S3 already
knows about, without waiting for the full body to be readable.

Create `s3meta.go`:

```go
package s3meta

import "fmt"

type object struct {
	etag string
	size int64
}

// Store is an in-memory stand-in for an S3 bucket's HEAD-able metadata.
// failNext, when set, simulates a transport or permissions failure on the
// next HeadObject call.
type Store struct {
	objects  map[string]object
	failNext error
}

func NewStore() *Store {
	return &Store{objects: make(map[string]object)}
}

// Put seeds an object's metadata (stands in for a real PUT/upload).
func (s *Store) Put(key, etag string, size int64) {
	s.objects[key] = object{etag: etag, size: size}
}

// FailNextWith forces the next HeadObject call to return a wrapped copy of
// err, simulating a network failure or an access-denied response.
func (s *Store) FailNextWith(err error) {
	s.failNext = err
}

// HeadObject looks up an object's metadata without fetching its body,
// mirroring S3's HEAD Object call. It distinguishes three outcomes:
//   - request failure: (empty, 0, false, wrapped error) -- the bucket could
//     not be reached, or access was denied. This is operationally
//     different from "the object is not there".
//   - object absent:   ("", 0, false, nil) -- a normal outcome for a cache
//     invalidation check ("has this been deleted since I last saw it?").
//   - object present:  (etag, size, true, nil)
func (s *Store) HeadObject(key string) (etag string, size int64, found bool, err error) {
	if s.failNext != nil {
		failure := s.failNext
		s.failNext = nil
		return "", 0, false, fmt.Errorf("head object %q: %w", key, failure)
	}

	obj, ok := s.objects[key]
	if !ok {
		return "", 0, false, nil
	}
	return obj.etag, obj.size, true, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	s3meta "example.com/s3-object-metadata-lookup"
)

func main() {
	store := s3meta.NewStore()
	store.Put("reports/2026-07.csv", "\"abc123\"", 40960)

	cachedETag := "\"abc123\""
	etag, size, found, err := store.HeadObject("reports/2026-07.csv")
	fmt.Printf("cache check: found=%t etag=%s size=%d cacheValid=%t err=%v\n",
		found, etag, size, found && etag == cachedETag, err)

	// The object was overwritten since the client's cache was populated.
	store.Put("reports/2026-07.csv", "\"def456\"", 45056)
	etag, size, found, err = store.HeadObject("reports/2026-07.csv")
	fmt.Printf("cache check: found=%t etag=%s size=%d cacheValid=%t err=%v\n",
		found, etag, size, found && etag == cachedETag, err)

	_, _, found, err = store.HeadObject("reports/2026-08.csv")
	fmt.Printf("missing key: found=%t err=%v\n", found, err)

	store.FailNextWith(errors.New("403 Forbidden"))
	_, _, found, err = store.HeadObject("reports/2026-07.csv")
	fmt.Printf("access denied: found=%t err=%v\n", found, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cache check: found=true etag="abc123" size=40960 cacheValid=true err=<nil>
cache check: found=true etag="def456" size=45056 cacheValid=false err=<nil>
missing key: found=false err=<nil>
access denied: found=false err=head object "reports/2026-07.csv": 403 Forbidden
```

### Tests

Create `s3meta_test.go`:

```go
package s3meta

import (
	"errors"
	"testing"
)

func TestHeadObjectFound(t *testing.T) {
	t.Parallel()
	store := NewStore()
	store.Put("k1", "\"etag1\"", 100)

	etag, size, found, err := store.HeadObject("k1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if etag != "\"etag1\"" || size != 100 {
		t.Fatalf("etag=%q size=%d, want %q/100", etag, size, "\"etag1\"")
	}
}

func TestHeadObjectAbsent(t *testing.T) {
	t.Parallel()
	store := NewStore()

	etag, size, found, err := store.HeadObject("missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("found = true, want false")
	}
	if etag != "" || size != 0 {
		t.Fatalf("etag=%q size=%d, want zero values", etag, size)
	}
}

func TestHeadObjectFailureIsNotAbsence(t *testing.T) {
	t.Parallel()
	store := NewStore()
	store.Put("k1", "\"etag1\"", 100)
	store.FailNextWith(errors.New("403 Forbidden"))

	_, _, found, err := store.HeadObject("k1")
	if err == nil {
		t.Fatal("want a non-nil error on a forced failure")
	}
	if found {
		t.Fatal("found = true despite a forced failure")
	}
}

func TestHeadObjectETagChangesAfterOverwrite(t *testing.T) {
	t.Parallel()
	store := NewStore()
	store.Put("k1", "\"v1\"", 100)

	etag1, _, _, _ := store.HeadObject("k1")

	store.Put("k1", "\"v2\"", 200)
	etag2, size2, found, err := store.HeadObject("k1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if etag1 == etag2 {
		t.Fatal("etag must change after the object is overwritten")
	}
	if size2 != 200 {
		t.Fatalf("size = %d, want 200", size2)
	}
}
```

## Review

`HeadObject` is correct when "absent" and "failed" never blur: an absent
object is a quiet `(zero values, false, nil)` that a cache should act on
immediately (evict, stop serving), while a failed request is a loud
non-nil `error` that should trigger a retry, not an eviction.
`TestHeadObjectFailureIsNotAbsence` is the load-bearing test — a design
that reported both as `found == false, err == nil` would make a transient
403 look identical to a real deletion, which is exactly the confusion that
causes a cache to evict good data on a blip.

The mistake to avoid is reusing the object's zero value as a sentinel for
absence instead of a dedicated `found bool` — an object legitimately can
have an empty-string etag from certain storage backends, or (less commonly)
a `0`-byte size, and conflating "zero value" with "not found" would
misreport those as missing.

## Resources

- [Amazon S3: HeadObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html) — the real API this exercise's `HeadObject` models, including its ETag and ContentLength fields.
- [Amazon S3: Uploading and copying objects using multipart upload](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html) — the size-reporting use case for `HeadObject` during an in-progress upload.
- [aws-sdk-go-v2 s3.Client.HeadObject](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/s3#Client.HeadObject) — the production Go client call this exercise mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-rate-limit-quota-display.md](29-rate-limit-quota-display.md) | Next: [31-graceful-shutdown-coordinated.md](31-graceful-shutdown-coordinated.md)
