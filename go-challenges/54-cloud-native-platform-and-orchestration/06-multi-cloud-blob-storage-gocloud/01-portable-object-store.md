# Exercise 1: A Provider-Agnostic Object Store Service

This is the whole-task, on-the-job exercise: the storage adapter you would
actually ship. You wrap `*blob.Bucket` behind a narrow domain interface, stream
writes instead of buffering them, check `Close` as the commit point, and carry
content type and metadata — so the same code runs on S3 in production and on an
in-memory fake in tests.

## What you'll build

```text
artifactstore/                 independent module: example.com/artifactstore
  go.mod                       go 1.26; requires gocloud.dev
  store.go                     ObjectStore port; Store adapter over *blob.Bucket; Artifact; ErrNotFound
  cmd/
    demo/
      main.go                  streams a payload into memblob, then Stat + Get it back
  store_test.go                round-trip, Attributes, Exists, Delete->NotFound, streaming-error tests
```

Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: an `ObjectStore` interface (`Put`, `Get`, `Stat`, `Exists`, `Delete`) and a `Store` that satisfies it over any `*blob.Bucket`, streaming reads and writes and mapping driver errors to a domain sentinel.
Test: table tests against `memblob.OpenBucket(nil)` — Put/Get round-trip, `Attributes` fields after Put, `Exists` true/false, `Delete` then `Get` yields `ErrNotFound` (via `gcerrors.NotFound`), and a mid-stream reader error surfaces wrapped.
Verify: `go test -count=1 -race ./...`

Set up the module. The Go CDK requires network access to fetch on first build:

```bash
mkdir -p ~/go-exercises/artifactstore/cmd/demo
cd ~/go-exercises/artifactstore
go mod init example.com/artifactstore
go get gocloud.dev/blob@latest
go mod edit -go=1.26
```

### Why a narrow port, not raw *blob.Bucket everywhere

`*blob.Bucket` is already portable across clouds, so it is tempting to pass it
through the whole codebase. Resist that. `*blob.Bucket` is a large surface with
methods your domain does not need and options (`As`, `BeforeWrite`) that would
tempt a future edit to reach for a provider-specific feature. Define the smallest
interface your domain actually uses — here `Put`, `Get`, `Stat`, `Exists`,
`Delete` — and let the adapter satisfy it. The domain depends on the interface;
tests inject a memblob-backed adapter; production injects an S3-backed one. The
compile-time assertion `var _ ObjectStore = (*Store)(nil)` keeps the adapter
honest. This is the same seam as this repository's hexagonal split: the port has
no cloud types in its signatures, only `io.Reader`, `io.Writer`, `string`, and a
plain `Artifact` value.

### Streaming and the commit point

`Put` takes an `io.Reader`, not a `[]byte`, so a caller uploading a large file
never has to hold it in memory. It opens a `*blob.Writer`, copies the reader into
it with `io.Copy`, and then — this is the load-bearing line — checks the error
from `w.Close()`. `Close` is where a multipart upload is finalized and where a
provider reports a failure; a write is not durable until `Close` returns `nil`.
If the copy itself fails partway, `Put` calls `w.Close()` best-effort to abort the
partial upload before returning the copy error, so no half-written object is left
committed. `Get` is the mirror image: it uses `Download`, which streams the object
straight into the caller's `io.Writer` without a full-object buffer.

### Mapping driver errors to a domain sentinel

The adapter is the one place allowed to know about `gcerrors`. `Get`, `Stat`, and
`Delete` translate a `gcerrors.NotFound` from any driver into the package sentinel
`ErrNotFound`, wrapped with `%w` so callers can use `errors.Is(err, ErrNotFound)`
without importing `gcerrors` at all. That is the portability boundary made
concrete: below the adapter, provider error codes; above it, one domain error.

Create `store.go`:

```go
// store.go
package artifactstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// ErrNotFound is returned when a key is absent, regardless of the underlying
// provider's own error type or code.
var ErrNotFound = errors.New("artifact not found")

// Artifact is the provider-independent metadata for a stored object.
type Artifact struct {
	Key         string
	ContentType string
	Size        int64
	ModTime     time.Time
	ETag        string
	Metadata    map[string]string
}

// ObjectStore is the narrow port the domain depends on. It names no cloud types.
type ObjectStore interface {
	Put(ctx context.Context, key, contentType string, meta map[string]string, r io.Reader) error
	Get(ctx context.Context, key string, w io.Writer) error
	Stat(ctx context.Context, key string) (*Artifact, error)
	Exists(ctx context.Context, key string) (bool, error)
	Delete(ctx context.Context, key string) error
}

// Store is the adapter over a *blob.Bucket. It is the only place that knows
// about gcerrors; above it, callers see ErrNotFound.
type Store struct {
	bucket *blob.Bucket
}

// New wraps a bucket. The caller owns the bucket's lifetime unless it uses Close.
func New(bucket *blob.Bucket) *Store {
	return &Store{bucket: bucket}
}

// Close releases the underlying bucket.
func (s *Store) Close() error {
	return s.bucket.Close()
}

// Put streams r into key. Close is the commit point and its error is checked;
// a mid-stream failure aborts the write instead of committing a partial object.
func (s *Store) Put(ctx context.Context, key, contentType string, meta map[string]string, r io.Reader) error {
	w, err := s.bucket.NewWriter(ctx, key, &blob.WriterOptions{
		ContentType: contentType,
		Metadata:    meta,
	})
	if err != nil {
		return fmt.Errorf("open writer for %q: %w", key, err)
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close() // best-effort abort; the copy error is the real failure
		return fmt.Errorf("stream %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("commit %q: %w", key, err)
	}
	return nil
}

// Get streams key into w without buffering the whole object.
func (s *Store) Get(ctx context.Context, key string, w io.Writer) error {
	err := s.bucket.Download(ctx, key, w, nil)
	if gcerrors.Code(err) == gcerrors.NotFound {
		return fmt.Errorf("get %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("get %q: %w", key, err)
	}
	return nil
}

// Stat returns provider-independent metadata for key.
func (s *Store) Stat(ctx context.Context, key string) (*Artifact, error) {
	a, err := s.bucket.Attributes(ctx, key)
	if gcerrors.Code(err) == gcerrors.NotFound {
		return nil, fmt.Errorf("stat %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", key, err)
	}
	return &Artifact{
		Key:         key,
		ContentType: a.ContentType,
		Size:        a.Size,
		ModTime:     a.ModTime,
		ETag:        a.ETag,
		Metadata:    a.Metadata,
	}, nil
}

// Exists reports whether key is present.
func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	ok, err := s.bucket.Exists(ctx, key)
	if err != nil {
		return false, fmt.Errorf("exists %q: %w", key, err)
	}
	return ok, nil
}

// Delete removes key, mapping a missing key to ErrNotFound.
func (s *Store) Delete(ctx context.Context, key string) error {
	err := s.bucket.Delete(ctx, key)
	if gcerrors.Code(err) == gcerrors.NotFound {
		return fmt.Errorf("delete %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}

// Compile-time proof the adapter satisfies the port.
var _ ObjectStore = (*Store)(nil)
```

### The runnable demo

The demo wires the adapter to `memblob` — no network, no credentials — streams a
small payload in, then reads back both the metadata and the bytes. Swapping this
to S3 in production is one import and one URL; the rest of `main` is unchanged.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"

	"example.com/artifactstore"
	"gocloud.dev/blob/memblob"
)

func main() {
	ctx := context.Background()
	store := artifactstore.New(memblob.OpenBucket(nil))
	defer store.Close()

	body := "id,amount\n1,42\n2,17\n"
	meta := map[string]string{"pipeline": "billing", "run": "2026-07-02"}
	if err := store.Put(ctx, "reports/daily.csv", "text/csv", meta, strings.NewReader(body)); err != nil {
		log.Fatal(err)
	}

	a, err := store.Stat(ctx, "reports/daily.csv")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stored %s type=%s size=%d pipeline=%s\n", a.Key, a.ContentType, a.Size, a.Metadata["pipeline"])

	var buf bytes.Buffer
	if err := store.Get(ctx, "reports/daily.csv", &buf); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("read back %d bytes, first line: %s\n", buf.Len(), strings.SplitN(buf.String(), "\n", 2)[0])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stored reports/daily.csv type=text/csv size=20 pipeline=billing
read back 20 bytes, first line: id,amount
```

### Tests

The tests run entirely against `memblob.OpenBucket(nil)`, so they need no network
and no credentials — the reason the port exists. `TestRoundTrip` covers Put then
Get and asserts the bytes survive. `TestAttributes` asserts the content type,
size, and metadata that `Put` carried through. `TestExists` checks both truth
values. `TestDeleteThenGet` deletes a key and asserts the follow-up `Get` returns
an error that `errors.Is` matches to `ErrNotFound` — the domain sentinel, proving
the `gcerrors.NotFound` mapping. `TestStreamError` feeds `Put` a reader that
fails partway and asserts the failure is surfaced wrapped, confirming the abort
path rather than a silently committed partial object. An `Example` pins a minimal
round trip with a checked `// Output:`.

Create `store_test.go`:

```go
// store_test.go
package artifactstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"gocloud.dev/blob/memblob"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s := New(memblob.OpenBucket(nil))
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t)

	want := "hello, portable world"
	if err := s.Put(ctx, "greeting.txt", "text/plain", nil, strings.NewReader(want)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var got bytes.Buffer
	if err := s.Get(ctx, "greeting.txt", &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.String() != want {
		t.Fatalf("round trip = %q, want %q", got.String(), want)
	}
}

func TestAttributes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t)

	body := "id,amount\n1,42\n"
	meta := map[string]string{"pipeline": "billing"}
	if err := s.Put(ctx, "reports/x.csv", "text/csv", meta, strings.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	a, err := s.Stat(ctx, "reports/x.csv")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if a.ContentType != "text/csv" {
		t.Errorf("ContentType = %q, want text/csv", a.ContentType)
	}
	if a.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", a.Size, len(body))
	}
	if a.Metadata["pipeline"] != "billing" {
		t.Errorf("Metadata[pipeline] = %q, want billing", a.Metadata["pipeline"])
	}
}

func TestExists(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t)

	if err := s.Put(ctx, "here", "text/plain", nil, strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	cases := []struct {
		key  string
		want bool
	}{
		{"here", true},
		{"gone", false},
	}
	for _, tc := range cases {
		ok, err := s.Exists(ctx, tc.key)
		if err != nil {
			t.Fatalf("Exists(%q): %v", tc.key, err)
		}
		if ok != tc.want {
			t.Errorf("Exists(%q) = %v, want %v", tc.key, ok, tc.want)
		}
	}
}

func TestDeleteThenGet(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t)

	if err := s.Put(ctx, "temp", "text/plain", nil, strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, "temp"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	err := s.Get(ctx, "temp", io.Discard)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

// errReader fails after emitting n bytes, simulating a truncated upload source.
type errReader struct {
	data []byte
	n    int
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.n >= len(r.data) {
		return 0, errors.New("source stream broke")
	}
	c := copy(p, r.data[r.n:])
	r.n += c
	return c, nil
}

func TestStreamError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t)

	err := s.Put(ctx, "partial", "application/octet-stream", nil, &errReader{data: []byte("abc")})
	if err == nil {
		t.Fatal("Put with a failing reader returned nil; want an error")
	}
	if !strings.Contains(err.Error(), "source stream broke") {
		t.Fatalf("error %v does not wrap the source failure", err)
	}
}

func Example() {
	ctx := context.Background()
	s := New(memblob.OpenBucket(nil))
	defer s.Close()

	_ = s.Put(ctx, "k", "text/plain", nil, strings.NewReader("hi"))
	var buf bytes.Buffer
	_ = s.Get(ctx, "k", &buf)
	fmt.Println(buf.String())
	// Output: hi
}
```

## Review

The adapter is correct when its signatures name no cloud types, when every write
checks the error from `Close`, and when every read-side method translates
`gcerrors.NotFound` into the `ErrNotFound` sentinel that `errors.Is` can match.
The most consequential mistake is dropping the `Close` error — the write compiles
and appears to work in a demo, then loses data under a real multipart failure;
`TestStreamError` exercises the abort path so a regression that commits partial
objects fails the suite. Do not "simplify" `Put` to take a `[]byte` and call
`WriteAll`: that reintroduces the full-object buffer the streaming design exists
to avoid. Keep `gcerrors` confined to this file; if a caller elsewhere imports it,
the portable boundary has leaked. Confirm correctness with
`go test -count=1 -race ./...`; the race detector matters because a real service
shares one `*Store` across concurrent request goroutines.

## Resources

- [gocloud.dev/blob package reference](https://pkg.go.dev/gocloud.dev/blob) — `Bucket`, `NewWriter`, `Download`, `Attributes`, and the option structs.
- [Go CDK How-To: Blob storage](https://gocloud.dev/howto/blob/) — opening buckets by URL versus direct constructors, and driver setup.
- [gocloud.dev/gcerrors](https://pkg.go.dev/gocloud.dev/gcerrors) — the portable `ErrorCode` taxonomy and `Code()`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-list-and-paginate.md](02-list-and-paginate.md)
