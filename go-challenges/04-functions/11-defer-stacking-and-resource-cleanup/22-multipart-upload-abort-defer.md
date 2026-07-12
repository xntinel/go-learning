# Exercise 22: Multi-Part Cloud Upload — Abort and Capture Error

**Nivel: Intermedio** — validacion rapida (un test corto).

A multi-part cloud upload (S3, GCS, Azure Blob) is a three-act resource:
initiate it, stage parts, then either complete it or abort it — and if you
don't explicitly do one of the last two, the provider keeps billing you for
orphaned part storage indefinitely. This module defers the abort, gated by
a commit flag exactly like the staged-write pattern from
`14-staged-write-discard-unless-committed.md`, and — the new piece — folds a
failed abort call itself into the named return error instead of swallowing it.

## What you'll build

```text
multipart/                   independent module: example.com/multipart
  go.mod
  multipart/multipart.go      Client interface; UploadFile (defer abort-unless-committed)
  multipart/multipart_test.go commits without aborting; aborts on part failure; aborts on Complete failure; abort-failure is joined in
  cmd/demo/main.go            runnable demo: a clean upload, then one where part 2 fails
```

- Files: `multipart/multipart.go`, `multipart/multipart_test.go`, `cmd/demo/main.go`.
- Implement: a `Client` interface (`Initiate`, `UploadPart`, `Complete`, `Abort`) modeled on a real cloud SDK; `UploadFile(client Client, parts [][]byte) (err error)` that initiates, defers an abort-unless-committed closure, uploads every part, and completes — setting `committed = true` only after `Complete` succeeds.
- Test: an in-memory fake `Client`; a full success (no `Abort` call); a part upload failure (mid-batch, `Abort` called, later parts never run); a `Complete` failure (`Abort` called even though every part succeeded); and a case where `Abort` itself fails, asserting the returned error mentions both the original failure and the abort failure.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/22-multipart-upload-abort-defer/multipart go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/22-multipart-upload-abort-defer/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/22-multipart-upload-abort-defer
go mod edit -go=1.24
```

### Abort is exactly staged-write-discard, applied to a network resource

`committed := false; defer func() { if !committed { ... } }()` is the same
shape as exercise 14's `WriteBatch`, transplanted from an in-memory `pending`
slice to a live cloud API call. The reason it still fits: an in-progress
multi-part upload behaves like a staging area from the provider's point of
view too — nothing staged is visible or billed as a finished object until
`Complete` runs, and everything staged needs disposing of if it never gets
there. `committed` only flips to `true` on the very last line, after
`Complete` itself returns `nil` — a `Complete` failure leaves `committed`
false just like a part failure does, so both paths defer to the same abort.

### Don't let a failed cleanup call erase the original failure

The one genuinely new idea here: what happens when the *cleanup* itself
fails? A less careful version would either ignore `Abort`'s return value
(silently leaving the orphaned parts on record, undetected) or overwrite
`err` with the abort error (losing the original failure that's usually the
more actionable one — a part failed because of a bad checksum, say, and now
the caller only sees "abort also failed"). `errors.Join(err,
fmt.Errorf("abort upload %s: %w", uploadID, aerr))` keeps both: the returned
error, once formatted, mentions the part or `Complete` failure *and* the
abort failure, and `errors.Is` still matches either one if they were wrapped
sentinels. This is the same principle as the `errors.Join` used for
rollback failures in `21-write-ahead-log-rollback.md`, applied to a single
named-return `err` instead of a slice of rollback errors.

Create `multipart/multipart.go`:

```go
package multipart

import (
	"errors"
	"fmt"
)

// Client is the subset of a cloud multi-part upload API this module needs
// (modeled on S3's CreateMultipartUpload / UploadPart / CompleteMultipartUpload
// / AbortMultipartUpload). A real implementation talks to the network; the
// test in this module uses an in-memory fake instead.
type Client interface {
	Initiate() (uploadID string, err error)
	UploadPart(uploadID string, partNum int, data []byte) error
	Complete(uploadID string) error
	Abort(uploadID string) error
}

// UploadFile initiates a multi-part upload, stages every part, and completes
// it. The deferred closure aborts the upload unless it was actually
// committed by Complete -- an early return from a failed part, or a failed
// Complete call itself, both leave committed false, so the abort always
// runs on any path that did not fully finish the upload. If the abort call
// itself fails, that failure is joined into the named return err instead of
// silently discarding either error: the caller sees both what went wrong
// with the upload AND that the cloud provider may still be holding
// unreleased storage for the aborted parts.
func UploadFile(client Client, parts [][]byte) (err error) {
	uploadID, err := client.Initiate()
	if err != nil {
		return fmt.Errorf("initiate upload: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			if aerr := client.Abort(uploadID); aerr != nil {
				err = errors.Join(err, fmt.Errorf("abort upload %s: %w", uploadID, aerr))
			}
		}
	}()

	for i, p := range parts {
		partNum := i + 1
		if uerr := client.UploadPart(uploadID, partNum, p); uerr != nil {
			return fmt.Errorf("upload part %d: %w", partNum, uerr)
		}
	}

	if cerr := client.Complete(uploadID); cerr != nil {
		return fmt.Errorf("complete upload %s: %w", uploadID, cerr)
	}

	committed = true
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/multipart/multipart"
)

// demoClient is a tiny fake standing in for a real cloud SDK client, just
// for this runnable demo.
type demoClient struct {
	failOnPart int
}

func (c *demoClient) Initiate() (string, error) { return "upload-42", nil }

func (c *demoClient) UploadPart(uploadID string, partNum int, data []byte) error {
	if partNum == c.failOnPart {
		return errors.New("simulated network error")
	}
	fmt.Printf("uploaded part %d\n", partNum)
	return nil
}

func (c *demoClient) Complete(uploadID string) error {
	fmt.Println("completed:", uploadID)
	return nil
}

func (c *demoClient) Abort(uploadID string) error {
	fmt.Println("aborted:", uploadID)
	return nil
}

func main() {
	parts := [][]byte{[]byte("part-1"), []byte("part-2"), []byte("part-3")}

	fmt.Println("-- successful upload --")
	err := multipart.UploadFile(&demoClient{}, parts)
	fmt.Println("error:", err)

	fmt.Println("-- part 2 fails --")
	err = multipart.UploadFile(&demoClient{failOnPart: 2}, parts)
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
-- successful upload --
uploaded part 1
uploaded part 2
uploaded part 3
completed: upload-42
error: <nil>
-- part 2 fails --
uploaded part 1
aborted: upload-42
error: upload part 2: simulated network error
```

### Tests

Create `multipart/multipart_test.go`:

```go
package multipart

import (
	"errors"
	"strings"
	"testing"
)

// fakeClient is an in-memory stand-in for a cloud multi-part upload API.
type fakeClient struct {
	failOnPart     int // 0 means never fail
	failOnComplete bool
	failOnAbort    bool

	uploadedParts []int
	completed     bool
	aborted       bool
}

func (c *fakeClient) Initiate() (string, error) {
	return "upload-1", nil
}

func (c *fakeClient) UploadPart(uploadID string, partNum int, data []byte) error {
	if c.failOnPart != 0 && partNum == c.failOnPart {
		return errors.New("network error on part")
	}
	c.uploadedParts = append(c.uploadedParts, partNum)
	return nil
}

func (c *fakeClient) Complete(uploadID string) error {
	if c.failOnComplete {
		return errors.New("complete rejected: checksum mismatch")
	}
	c.completed = true
	return nil
}

func (c *fakeClient) Abort(uploadID string) error {
	c.aborted = true
	if c.failOnAbort {
		return errors.New("abort rejected: upload already expired")
	}
	return nil
}

func parts(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte{byte(i)}
	}
	return out
}

func TestUploadFileCommitsWithoutAbortingOnSuccess(t *testing.T) {
	t.Parallel()

	c := &fakeClient{}
	if err := UploadFile(c, parts(3)); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !c.completed {
		t.Fatal("expected Complete to have been called")
	}
	if c.aborted {
		t.Fatal("Abort must not be called after a successful commit")
	}
	if len(c.uploadedParts) != 3 {
		t.Fatalf("uploadedParts = %v, want 3 parts", c.uploadedParts)
	}
}

func TestUploadFileAbortsWhenAPartFails(t *testing.T) {
	t.Parallel()

	c := &fakeClient{failOnPart: 2}
	err := UploadFile(c, parts(3))

	if err == nil {
		t.Fatal("expected an error")
	}
	if !c.aborted {
		t.Fatal("expected Abort to have been called after part 2 failed")
	}
	if c.completed {
		t.Fatal("Complete must never be called after a part failed")
	}
	// Only part 1 succeeded before part 2 failed; part 3 never ran.
	if len(c.uploadedParts) != 1 || c.uploadedParts[0] != 1 {
		t.Fatalf("uploadedParts = %v, want [1]", c.uploadedParts)
	}
}

func TestUploadFileAbortsWhenCompleteFails(t *testing.T) {
	t.Parallel()

	c := &fakeClient{failOnComplete: true}
	err := UploadFile(c, parts(2))

	if err == nil {
		t.Fatal("expected an error")
	}
	if !c.aborted {
		t.Fatal("expected Abort to have been called after Complete failed")
	}
	if c.completed {
		t.Fatal("completed must be false: Complete itself failed")
	}
}

func TestUploadFileJoinsAbortFailureIntoReturnedError(t *testing.T) {
	t.Parallel()

	c := &fakeClient{failOnPart: 1, failOnAbort: true}
	err := UploadFile(c, parts(2))

	if err == nil {
		t.Fatal("expected an error")
	}
	got := err.Error()
	if !strings.Contains(got, "network error on part") {
		t.Fatalf("err = %v, want it to mention the original part failure", got)
	}
	if !strings.Contains(got, "abort rejected") {
		t.Fatalf("err = %v, want it to also mention the abort failure", got)
	}
	if !c.aborted {
		t.Fatal("expected Abort to have been attempted")
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

`committed` is set on exactly one line, the last one, after `Complete`
returns `nil` — every other return in the function, whether from a part
failure or a `Complete` failure, leaves it `false` and lets the deferred
`Abort` run. The fourth test is the one that would catch a regression where
someone "simplifies" the defer to `_ = client.Abort(uploadID)`, discarding
whatever `Abort` itself reports: without `errors.Join`, a failed abort would
be invisible, and an operator debugging orphaned multi-part uploads in their
cloud bill would have no signal from the application logs that the abort
call ever failed. Wrapping `err` — not replacing it — is what keeps both
failures visible in the one value the caller actually inspects.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [errors.Join](https://pkg.go.dev/errors#Join)
- [Amazon S3: Multipart upload overview](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html) — the real API this `Client` interface is modeled on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [21-write-ahead-log-rollback.md](21-write-ahead-log-rollback.md) | Next: [23-circuit-breaker-state-unwinding.md](23-circuit-breaker-state-unwinding.md)
