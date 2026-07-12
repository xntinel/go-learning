# Exercise 3: Idempotent Writes with IfNotExist and Uniform Error Codes

Publishing a build artifact, claiming a lock file, or deduplicating an upload all
need write-once semantics: the first write wins and later writes must not clobber
it. This exercise builds an exactly-once publisher on `WriterOptions.IfNotExist`
and a classifier that turns any driver's failure into a portable `gcerrors` code,
so callers behave the same on every cloud.

## What you'll build

```text
publisher/                     independent module: example.com/publisher
  go.mod                       go 1.26; requires gocloud.dev
  publish.go                   Publish (write-once), PublishIdempotent (retry-safe), Fetch, Classify
  cmd/
    demo/
      main.go                  publishes once, retries safely, shows the bytes are untouched
  publish_test.go              conflict->FailedPrecondition, missing->NotFound, deadline classification
```

Files: `publish.go`, `cmd/demo/main.go`, `publish_test.go`.
Implement: `Publish` (write-once via `IfNotExist`, mapping a conflict to `ErrAlreadyPublished`), `PublishIdempotent` (retry-safe: an existing identical publish is success), `Fetch` (mapping absence to `ErrNotFound`), and `Classify` (a thin wrapper over `gcerrors.Code`).
Test: a second `Publish` conflicts (the raw driver conflict classifies as `gcerrors.FailedPrecondition`) and leaves the original bytes intact; a missing key classifies as `gcerrors.NotFound`; `Classify(context.DeadlineExceeded)` is `gcerrors.DeadlineExceeded`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/06-multi-cloud-blob-storage-gocloud/03-conditional-writes-and-idempotency/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/06-multi-cloud-blob-storage-gocloud/03-conditional-writes-and-idempotency
go get gocloud.dev/blob@latest
go mod edit -go=1.26
```

### Write-once with IfNotExist

`WriterOptions.IfNotExist = true` tells the driver to fail the write if the key
already exists. The conflict is not silent and it is not a generic error: it
surfaces with the portable code `gcerrors.FailedPrecondition`, at `Write` or at
`Close` depending on the driver. That single flag gives you exactly-once publish
without a separate lock service: the first `Publish` of an artifact key wins, and
any concurrent or later `Publish` of the same key fails cleanly with the original
bytes untouched. `Publish` maps that conflict to the package sentinel
`ErrAlreadyPublished` so callers can distinguish "someone already published this"
from a real I/O failure with `errors.Is`.

This is a genuine production pattern. A CI job that publishes
`artifacts/v1.4.2/service.tar.gz` must be safe to retry after a network blip:
re-running it must not overwrite a good artifact with a partial one, and must not
error out the pipeline just because the artifact is already there. That is what
`PublishIdempotent` encodes — it calls `Publish` and treats `ErrAlreadyPublished`
as success, so retrying is a no-op, while any other error still propagates.

Note the honest caveat from the concepts file: `IfNotExist` support and the exact
moment the conflict is reported vary by driver, which is precisely why the code
branches on the portable `gcerrors` code rather than a driver-specific type.

### One taxonomy for every provider

`gcerrors.Code(err)` collapses every driver's native error into a fixed
`ErrorCode` set — `NotFound`, `AlreadyExists`, `FailedPrecondition`,
`PermissionDenied`, `Unimplemented`, `DeadlineExceeded`, `Canceled`, and more. It
also understands the standard library: a `context.DeadlineExceeded` (even wrapped)
classifies as `gcerrors.DeadlineExceeded`, and `context.Canceled` as
`gcerrors.Canceled`. `Classify` is a one-line wrapper that makes this the domain's
error vocabulary. The payoff is that a retry policy, a metric label, or an HTTP
status mapping can be written once against these codes and work unchanged whether
the backend is S3, GCS, Azure, or a memblob fake — instead of a fragile
`strings.Contains(err.Error(), "NoSuchKey")` that breaks the moment you change
provider.

Create `publish.go`:

```go
// publish.go
package publisher

import (
	"context"
	"errors"
	"fmt"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

var (
	// ErrAlreadyPublished means the key already holds a published artifact.
	ErrAlreadyPublished = errors.New("artifact already published")
	// ErrNotFound means the key does not exist.
	ErrNotFound = errors.New("artifact not found")
)

// Publish writes data to key exactly once. A key that already exists is a
// conflict reported as ErrAlreadyPublished; the stored bytes are left untouched.
func Publish(ctx context.Context, b *blob.Bucket, key string, data []byte) error {
	err := b.WriteAll(ctx, key, data, &blob.WriterOptions{IfNotExist: true})
	if gcerrors.Code(err) == gcerrors.FailedPrecondition {
		return fmt.Errorf("publish %q: %w", key, ErrAlreadyPublished)
	}
	if err != nil {
		return fmt.Errorf("publish %q: %w", key, err)
	}
	return nil
}

// PublishIdempotent is safe to retry: if the key is already published it reports
// success, so a re-run after a transient failure is a no-op. Any other error
// still propagates.
func PublishIdempotent(ctx context.Context, b *blob.Bucket, key string, data []byte) error {
	err := Publish(ctx, b, key, data)
	if errors.Is(err, ErrAlreadyPublished) {
		return nil
	}
	return err
}

// Fetch reads a published artifact, mapping absence to ErrNotFound.
func Fetch(ctx context.Context, b *blob.Bucket, key string) ([]byte, error) {
	data, err := b.ReadAll(ctx, key)
	if gcerrors.Code(err) == gcerrors.NotFound {
		return nil, fmt.Errorf("fetch %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch %q: %w", key, err)
	}
	return data, nil
}

// Classify returns the portable error code for err, so callers branch on a
// provider-independent taxonomy instead of provider-specific error types.
func Classify(err error) gcerrors.ErrorCode {
	return gcerrors.Code(err)
}
```

### The runnable demo

The demo publishes an artifact, retries the same publish (which is a safe no-op),
attempts a raw overwrite with different bytes (which is rejected), and reads the
key back to prove the original bytes survived.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"example.com/publisher"
	"gocloud.dev/blob/memblob"
)

func main() {
	ctx := context.Background()
	b := memblob.OpenBucket(nil)
	defer b.Close()

	key := "artifacts/v1.4.2/service.tar.gz"
	original := []byte("ORIGINAL-BUILD")

	if err := publisher.Publish(ctx, b, key, original); err != nil {
		log.Fatal(err)
	}
	fmt.Println("first publish: ok")

	if err := publisher.PublishIdempotent(ctx, b, key, original); err != nil {
		log.Fatal(err)
	}
	fmt.Println("retry (idempotent): ok")

	err := publisher.Publish(ctx, b, key, []byte("TAMPERED"))
	if errors.Is(err, publisher.ErrAlreadyPublished) {
		fmt.Println("overwrite attempt: rejected")
	}

	got, err := publisher.Fetch(ctx, b, key)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stored bytes: %s\n", got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first publish: ok
retry (idempotent): ok
overwrite attempt: rejected
stored bytes: ORIGINAL-BUILD
```

### Tests

`TestPublishConflict` publishes once, then publishes again with different bytes
and asserts the error is `ErrAlreadyPublished`, that a raw `IfNotExist` conflict
classifies as `gcerrors.FailedPrecondition`, and that a `Fetch` still returns the
first bytes — proving the conflict did not corrupt the object.
`TestPublishIdempotentRetry` asserts a second `PublishIdempotent` returns `nil`.
`TestFetchMissing` asserts a missing key maps to `ErrNotFound` and that a raw read
of the missing key classifies as `gcerrors.NotFound`.
`TestClassifyContext` asserts `Classify` maps `context.DeadlineExceeded` and
`context.Canceled` to their portable codes. An `Example` shows a safe retry.

Create `publish_test.go`:

```go
// publish_test.go
package publisher

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"
	"gocloud.dev/gcerrors"
)

func newBucket(t *testing.T) *blob.Bucket {
	t.Helper()
	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	return b
}

func TestPublishConflict(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	b := newBucket(t)
	key := "art/1"

	if err := Publish(ctx, b, key, []byte("first")); err != nil {
		t.Fatalf("first Publish: %v", err)
	}

	err := Publish(ctx, b, key, []byte("second"))
	if !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("second Publish = %v, want ErrAlreadyPublished", err)
	}

	// The raw driver conflict classifies as the portable FailedPrecondition code;
	// that is what Publish maps to ErrAlreadyPublished.
	rawErr := b.WriteAll(ctx, key, []byte("third"), &blob.WriterOptions{IfNotExist: true})
	if code := Classify(rawErr); code != gcerrors.FailedPrecondition {
		t.Errorf("raw conflict code = %v, want FailedPrecondition", code)
	}

	got, err := Fetch(ctx, b, key)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("stored bytes = %q, want %q (conflict corrupted the object)", got, "first")
	}
}

func TestPublishIdempotentRetry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	b := newBucket(t)
	key := "art/2"

	if err := PublishIdempotent(ctx, b, key, []byte("v")); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := PublishIdempotent(ctx, b, key, []byte("v")); err != nil {
		t.Fatalf("retry should be a no-op, got %v", err)
	}
}

func TestFetchMissing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	b := newBucket(t)

	_, err := Fetch(ctx, b, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Fetch missing = %v, want ErrNotFound", err)
	}

	// The raw driver read of a missing key classifies as NotFound; that is what
	// Fetch maps to ErrNotFound.
	_, rawErr := b.ReadAll(ctx, "nope")
	if code := Classify(rawErr); code != gcerrors.NotFound {
		t.Errorf("raw missing code = %v, want NotFound", code)
	}
}

func TestClassifyContext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want gcerrors.ErrorCode
	}{
		{"deadline", context.DeadlineExceeded, gcerrors.DeadlineExceeded},
		{"canceled", context.Canceled, gcerrors.Canceled},
		{"wrapped deadline", fmt.Errorf("op: %w", context.DeadlineExceeded), gcerrors.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func Example() {
	ctx := context.Background()
	b := memblob.OpenBucket(nil)
	defer b.Close()

	_ = PublishIdempotent(ctx, b, "k", []byte("data"))
	err := PublishIdempotent(ctx, b, "k", []byte("data"))
	fmt.Println(err == nil)
	// Output: true
}
```

## Review

The publisher is correct when `Publish` refuses to overwrite (mapping the
`gcerrors.FailedPrecondition` conflict to `ErrAlreadyPublished`),
`PublishIdempotent` swallows only that sentinel and nothing else, and `Classify`
routes both driver errors and context errors through the one portable taxonomy.
The subtle mistake is having `PublishIdempotent` swallow every error rather than
only `ErrAlreadyPublished` — that would hide real I/O failures and report a
publish that never happened as success; the test asserts a fresh publish returns
`nil` but the conflict test proves the original bytes are never replaced, so a
sloppy overwrite would fail. Do not classify by string matching on
`err.Error()`; `gcerrors.Code` is the portable answer and is what makes the same
retry logic work across providers. Because `IfNotExist` timing varies by driver,
always branch on the code, never on where the error surfaced. Run
`go test -count=1 -race ./...`.

## Resources

- [gocloud.dev/blob WriterOptions](https://pkg.go.dev/gocloud.dev/blob#WriterOptions) — `IfNotExist` and the write-once conflict.
- [gocloud.dev/gcerrors](https://pkg.go.dev/gocloud.dev/gcerrors) — `Code`, the `ErrorCode` taxonomy, and context-error mapping.
- [google/go-cloud blob.go source](https://github.com/google/go-cloud/blob/master/blob/blob.go) — the `IfNotExist` and `Writer.Close` semantics in the implementation.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-list-and-paginate.md](02-list-and-paginate.md) | Next: [04-presigned-urls.md](04-presigned-urls.md)
