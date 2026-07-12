# Exercise 13: S3 Multipart Upload: A Bounded In-Flight Part Buffer That Caps Memory and Aborts the Reader

**Level: Advanced**

A large object is split into parts and uploaded concurrently to an object store. The object can be arbitrarily large, so a naive uploader that reads every part into memory before uploading OOMs on a big file, and one that keeps reading after a part has already failed burns bandwidth on gigabytes it will only discard. This exercise builds an uploader whose buffered part channel is a hard memory bound and whose first failure cancels the reader upstream so it stops pulling from the source.

This module is self-contained: its own module, a `multipart` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
multipart/                   independent module: example.com/multipart
  go.mod                     go 1.26, require go.uber.org/goleak
  multipart.go               New + Uploader.Upload: bounded-memory, cancel-on-first-failure multipart upload
  cmd/demo/main.go           runnable demo: a clean run and an aborting run
  multipart_test.go          exactly-once + sorted, memory-bound gauge, bounded-overshoot abort, goleak
```

- Files: `multipart.go`, `cmd/demo/main.go`, `multipart_test.go`.
- Implement: `type Part struct { Num int; Data []byte }`, `type ETag struct { Num int; Tag string }`, `func New(maxInFlight, workers int, put func(context.Context, Part) (string, error)) *Uploader`, and `func (u *Uploader) Upload(ctx context.Context, parts iter.Seq[Part]) ([]ETag, error)`.
- Test: a clean run uploads every part exactly once and returns ETags sorted by Num; the resident-parts gauge never exceeds `maxInFlight+workers`; a put failure aborts after only a bounded overshoot and returns the injected cause; goleak confirms the reader and all workers exit on success and abort.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/03-buffered-vs-unbuffered-channels/13-multipart-upload-bounded-inflight-parts/cmd/demo
cd go-solutions/13-goroutines-and-channels/03-buffered-vs-unbuffered-channels/13-multipart-upload-bounded-inflight-parts
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### The bounded queue and the upstream cancellation

Two decisions define this uploader, and both are the buffered-vs-unbuffered decision from a different angle.

First, the buffer is the memory bound. The reader is the sole producer; it pulls parts from an `iter.Seq[Part]` and sends them into `make(chan Part, maxInFlight)`. Because the channel has capacity `maxInFlight`, the reader blocks on the send the instant the buffer is full and every worker is busy. That backpressure is the whole point: it turns memory from `O(fileSize)` into `O(maxInFlight*partSize + workers*partSize)`. No matter whether the object is 10 MB or 10 TB, at most `maxInFlight` parts sit queued plus at most `workers` are in flight. An unbounded channel (or reading the whole file first) would let the reader race ahead and pin the entire object in RAM; the fixed capacity is what forbids that.

Second, the failure path cancels upstream. `Upload` derives a context with `context.WithCancelCause`. The moment any worker's `put` returns an error, it calls `cancel(err)` — the first such call wins and records the cause. The reader's send is written as a `select` on both `ctx.Done()` and the channel send, so a cancelled context unblocks the reader immediately and it stops pulling from the source. This is the fix for the classic leaked-producer bug: without the `ctx.Done()` arm, a reader blocked on a full buffer whose workers have all given up would hang forever, and even a reader that kept going would drain the entire multi-gigabyte stream into a channel nobody reads. The abort is prompt and the overshoot is bounded to roughly the buffer plus the worker count.

The ownership rules keep the channel safe. Exactly one goroutine — the reader — sends, so exactly one goroutine closes (`defer close(ch)`), never a receiver and never twice. The workers only receive; they `range` the channel so they drain whatever is buffered and then exit when it closes, on both the success and the abort path. That is why closing the buffered channel is a clean shutdown signal rather than a source of "send on closed channel" panics.

The resident gauge that proves the bound is incremented *after* the send succeeds, not before. A part the reader is still holding while blocked on a full buffer is not yet counted; it becomes resident only once it is actually queued. That ordering is what pins the high-water mark at exactly `maxInFlight+workers` (buffered plus in flight) instead of one more.

Create `multipart.go`:

```go
// Package multipart uploads a large object split into parts, keeping at most a
// fixed number of parts resident in memory regardless of the object's size and
// aborting the whole upload promptly on the first part failure.
package multipart

import (
	"cmp"
	"context"
	"iter"
	"slices"
	"sync"
	"sync/atomic"
)

// Part is one slice of the object. Data is the bytes to upload for part Num.
type Part struct {
	Num  int
	Data []byte
}

// ETag is the store's receipt for an uploaded part, keyed by part number.
type ETag struct {
	Num int
	Tag string
}

// Uploader runs a bounded-memory multipart upload. The buffered part channel of
// capacity maxInFlight is the memory bound: at most maxInFlight parts sit queued
// plus at most `workers` in flight, so peak resident bytes are
// O(maxInFlight+workers) rather than O(fileSize).
type Uploader struct {
	maxInFlight int
	workers     int
	put         func(context.Context, Part) (string, error)

	// resident counts parts that have entered the bounded channel and not yet
	// finished uploading (buffered + in flight). peakResident is its high-water
	// mark; the test reads it to prove the memory bound. Both are reset per Upload.
	resident     atomic.Int64
	peakResident atomic.Int64
}

// New builds an Uploader that keeps at most maxInFlight parts buffered, uploads
// with a pool of `workers` goroutines, and calls put to store each part.
func New(maxInFlight, workers int, put func(context.Context, Part) (string, error)) *Uploader {
	if maxInFlight < 1 {
		maxInFlight = 1
	}
	if workers < 1 {
		workers = 1
	}
	return &Uploader{maxInFlight: maxInFlight, workers: workers, put: put}
}

// Upload reads parts from the iterator into a bounded channel of capacity
// maxInFlight, uploads them with a pool of `workers` (each launched via wg.Go),
// and on the first put error cancels via context.WithCancelCause so the reader
// stops pulling further parts from the source. On success it returns the ETags
// sorted by Num; on failure it returns context.Cause(ctx) (the injected cause).
func (u *Uploader) Upload(ctx context.Context, parts iter.Seq[Part]) ([]ETag, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	u.resident.Store(0)
	u.peakResident.Store(0)

	// Buffered to maxInFlight: this capacity is the memory bound and the decoupling
	// queue between the single reader and the worker pool.
	ch := make(chan Part, u.maxInFlight)

	// Reader: the sole sender, so it owns the close. It pulls from the source only
	// as fast as the buffer drains, and stops the moment the context is cancelled
	// instead of reading the rest of the stream it would only discard.
	go func() {
		defer close(ch)
		for p := range parts {
			select {
			case <-ctx.Done():
				return
			case ch <- p:
				u.markResident()
			}
		}
	}()

	var (
		mu    sync.Mutex
		etags []ETag
		wg    sync.WaitGroup
	)
	for range u.workers {
		wg.Go(func() {
			// Range drains the channel until the reader closes it, so every worker
			// exits cleanly on both success and abort (no leaked goroutine).
			for p := range ch {
				if ctx.Err() != nil {
					u.resident.Add(-1) // released without uploading; an abort is in progress
					continue
				}
				tag, err := u.put(ctx, p)
				u.resident.Add(-1)
				if err != nil {
					cancel(err) // first error sets the cause and makes the reader stop pulling
					continue
				}
				mu.Lock()
				etags = append(etags, ETag{Num: p.Num, Tag: tag})
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	slices.SortFunc(etags, func(a, b ETag) int { return cmp.Compare(a.Num, b.Num) })
	return etags, nil
}

// PeakResident reports the high-water mark of resident parts (buffered plus in
// flight) observed during the most recent Upload. It never exceeds
// maxInFlight+workers, which is the memory bound this Uploader guarantees.
func (u *Uploader) PeakResident() int { return int(u.peakResident.Load()) }

// markResident bumps the resident gauge after a part enters the buffer and keeps
// the high-water mark. Incrementing after the send (not before) is what pins the
// peak at maxInFlight+workers: a part held by the reader while blocked on a full
// buffer is not counted until it is actually queued.
func (u *Uploader) markResident() {
	n := u.resident.Add(1)
	for {
		old := u.peakResident.Load()
		if n <= old || u.peakResident.CompareAndSwap(old, n) {
			return
		}
	}
}
```

### The runnable demo

The demo runs a clean upload of six parts (printing the sorted ETags and confirming the memory bound held) and an aborting upload whose source would yield a thousand parts but whose part 2 fails.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"example.com/multipart"
)

// genParts yields n parts numbered 0..n-1 with placeholder bytes.
func genParts(n int) iter.Seq[multipart.Part] {
	return func(yield func(multipart.Part) bool) {
		for i := range n {
			if !yield(multipart.Part{Num: i, Data: []byte(fmt.Sprintf("part-%d-bytes", i))}) {
				return
			}
		}
	}
}

func main() {
	ctx := context.Background()

	// Clean run: every part uploads, ETags come back sorted by Num.
	store := func(_ context.Context, p multipart.Part) (string, error) {
		return fmt.Sprintf("etag-%04x", p.Num), nil
	}
	const maxInFlight, workers = 4, 3
	u := multipart.New(maxInFlight, workers, store)
	etags, err := u.Upload(ctx, genParts(6))
	// The exact peak is scheduling-dependent; the memory bound is not, so print the
	// invariant (peak <= maxInFlight+workers) rather than the raw high-water mark.
	fmt.Printf("clean: %d parts uploaded, err=%v, withinMemoryBound=%v\n",
		len(etags), err, u.PeakResident() <= maxInFlight+workers)
	for _, e := range etags {
		fmt.Printf("  part %d -> %s\n", e.Num, e.Tag)
	}

	// Failing run: part 2 fails; the whole upload aborts and returns the cause.
	errBoom := errors.New("store rejected part")
	storeFail := func(_ context.Context, p multipart.Part) (string, error) {
		if p.Num == 2 {
			return "", errBoom
		}
		return fmt.Sprintf("etag-%04x", p.Num), nil
	}
	uf := multipart.New(4, 3, storeFail)
	etags, err = uf.Upload(ctx, genParts(1_000))
	fmt.Printf("abort: parts=%v, err=%v, isBoom=%v\n", etags, err, errors.Is(err, errBoom))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the clean run returns six sorted ETags within the bound; the aborting run returns an empty slice and the injected cause):

```
clean: 6 parts uploaded, err=<nil>, withinMemoryBound=true
  part 0 -> etag-0000
  part 1 -> etag-0001
  part 2 -> etag-0002
  part 3 -> etag-0003
  part 4 -> etag-0004
  part 5 -> etag-0005
abort: parts=[], err=store rejected part, isBoom=true
```

### Tests

`TestCleanRunUploadsEachPartOnceSorted` pins the happy path: an atomic per-Num guard catches any double upload, and the returned ETags must be sorted by Num with the tag the store handed back. `TestResidentGaugeNeverExceedsBound` is the memory-bound invariant: 5000 parts flow through a `maxInFlight=8, workers=4` uploader and the peak resident gauge must stay within `maxInFlight+workers` — if the reader ignored backpressure and drained the stream into the channel, the peak would scale with the part count and blow the bound. `TestFirstFailureAbortsReaderEarly` pins the abort path: with a source that would yield a million parts, part 2 fails, `Upload` must return the injected cause via `errors.Is`, and a pull counter inside the `iter.Seq` must show the reader stopped after only a bounded overshoot rather than draining the whole stream. `TestNoGoroutineLeakOnSuccess` and `TestNoGoroutineLeakOnAbort` use `goleak.VerifyNone` to prove the reader and every worker exit on both paths.

Create `multipart_test.go`:

```go
package multipart

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"
)

// countingSeq yields n parts and records, via pulls, how many the reader actually
// requested from the source. A caller that aborts early leaves pulls far below n.
func countingSeq(n int, pulls *atomic.Int64) iter.Seq[Part] {
	return func(yield func(Part) bool) {
		for i := range n {
			pulls.Add(1)
			if !yield(Part{Num: i, Data: []byte{byte(i)}}) {
				return
			}
		}
	}
}

// TestCleanRunUploadsEachPartOnceSorted pins down the happy path: every part is
// uploaded exactly once (an atomic per-Num guard catches any double upload) and
// the returned ETags are sorted by Num with the tag the store handed back.
func TestCleanRunUploadsEachPartOnceSorted(t *testing.T) {
	const n = 200
	var seen [n]atomic.Int32
	store := func(_ context.Context, p Part) (string, error) {
		seen[p.Num].Add(1)
		return fmt.Sprintf("tag-%d", p.Num), nil
	}

	u := New(8, 4, store)
	etags, err := u.Upload(context.Background(), countingSeq(n, new(atomic.Int64)))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(etags) != n {
		t.Fatalf("len(etags) = %d, want %d", len(etags), n)
	}
	for i := range n {
		if got := seen[i].Load(); got != 1 {
			t.Fatalf("part %d uploaded %d times, want exactly 1", i, got)
		}
	}
	if !slices.IsSortedFunc(etags, func(a, b ETag) int { return a.Num - b.Num }) {
		t.Fatalf("etags not sorted by Num: %v", etags)
	}
	for i, e := range etags {
		if e.Num != i || e.Tag != fmt.Sprintf("tag-%d", i) {
			t.Fatalf("etags[%d] = %+v, want {Num:%d Tag:tag-%d}", i, e, i, i)
		}
	}
}

// TestResidentGaugeNeverExceedsBound is the memory-bound invariant: no matter how
// many parts flow through, the peak of buffered-plus-in-flight parts stays within
// maxInFlight+workers. If the reader ignored backpressure and drained the whole
// stream into the channel, this peak would scale with n and blow the bound.
func TestResidentGaugeNeverExceedsBound(t *testing.T) {
	const (
		maxInFlight = 8
		workers     = 4
		n           = 5_000
	)
	var uploaded atomic.Int64
	store := func(_ context.Context, p Part) (string, error) {
		uploaded.Add(1)
		return "tag", nil
	}

	u := New(maxInFlight, workers, store)
	_, err := u.Upload(context.Background(), countingSeq(n, new(atomic.Int64)))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := uploaded.Load(); got != n {
		t.Fatalf("uploaded %d parts, want %d", got, n)
	}
	if peak := u.PeakResident(); peak > maxInFlight+workers {
		t.Fatalf("peak resident = %d, exceeds bound maxInFlight+workers = %d", peak, maxInFlight+workers)
	}
}

// TestFirstFailureAbortsReaderEarly pins down the abort path: when a put fails,
// Upload returns the injected cause (via context.Cause / errors.Is) and the reader
// stops pulling from the source after only a bounded overshoot -- not the whole
// stream. The source can yield a million parts; the reader must touch a tiny
// prefix before the cancellation reaches it.
func TestFirstFailureAbortsReaderEarly(t *testing.T) {
	const (
		maxInFlight = 4
		workers     = 3
		total       = 1_000_000
		failAt      = 2
	)
	errBoom := errors.New("store rejected part")
	store := func(_ context.Context, p Part) (string, error) {
		if p.Num == failAt {
			return "", errBoom
		}
		return "tag", nil
	}

	var pulls atomic.Int64
	u := New(maxInFlight, workers, store)
	etags, err := u.Upload(context.Background(), countingSeq(total, &pulls))

	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom via context.Cause", err)
	}
	if etags != nil {
		t.Fatalf("etags = %v, want nil on abort", etags)
	}
	// Bounded overshoot: at most the buffer plus the in-flight workers plus a small
	// slack can have been pulled before the reader observed the cancellation.
	if got := pulls.Load(); got >= total {
		t.Fatalf("reader pulled %d parts; it drained the whole stream instead of aborting", got)
	}
	if bound := int64(maxInFlight + workers + 16); pulls.Load() > bound {
		t.Fatalf("reader pulled %d parts, want <= %d (bounded overshoot)", pulls.Load(), bound)
	}
}

// TestNoGoroutineLeakOnSuccess proves the reader and every worker exit after a
// clean upload.
func TestNoGoroutineLeakOnSuccess(t *testing.T) {
	defer goleak.VerifyNone(t)

	store := func(_ context.Context, p Part) (string, error) { return "tag", nil }
	u := New(4, 3, store)
	if _, err := u.Upload(context.Background(), countingSeq(500, new(atomic.Int64))); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// TestNoGoroutineLeakOnAbort proves the reader and every worker exit after an
// aborted upload -- the classic leaked-producer bug would leave the reader blocked
// on a full channel forever.
func TestNoGoroutineLeakOnAbort(t *testing.T) {
	defer goleak.VerifyNone(t)

	errBoom := errors.New("boom")
	store := func(_ context.Context, p Part) (string, error) {
		if p.Num == 1 {
			return "", errBoom
		}
		return "tag", nil
	}
	u := New(4, 3, store)
	if _, err := u.Upload(context.Background(), countingSeq(1_000_000, new(atomic.Int64))); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}
```

## Review

"Correct" here means three properties hold at once. Every part uploads exactly once and the receipts return sorted by Num — proven by the per-Num atomic guard (any double upload trips it) and the `slices.IsSortedFunc` check against `Upload`'s final `slices.SortFunc`. Peak memory is bounded independent of file size — proven by the resident gauge, incremented after a part is queued and decremented when its put finishes, never exceeding `maxInFlight+workers` even for 5000 parts, because the buffered channel's fixed capacity blocks the reader the moment the queue fills. And a single failure aborts promptly — proven by `errors.Is` against the injected cause plus the source's pull counter, which shows the reader stopped after a bounded overshoot rather than draining a million parts, because `cancel(err)` fires the derived context and the reader's `select` on `ctx.Done()` breaks its send loop. goleak on both paths guarantees no goroutine survives the call. The production bug this prevents is the leaked producer: an uploader that keeps reading a giant object into an unbounded queue after the upload has already failed, pinning memory and wasting bandwidth on bytes it will throw away — the exact failure mode the bounded buffer and upstream cancellation together eliminate.

## Resources

- [pkg.go.dev: context.WithCancelCause](https://pkg.go.dev/context#WithCancelCause) -- deriving a cancelable context that carries the failure cause returned by `context.Cause`.
- [pkg.go.dev: iter](https://pkg.go.dev/iter) -- the `iter.Seq` pull model the reader ranges over and stops early on abort.
- [The Go Blog: Go Concurrency Patterns -- Pipelines and cancellation](https://go.dev/blog/pipelines) -- the leaked-producer problem and the cancellation discipline that fixes it.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- asserting the reader and worker goroutines exit on both success and abort.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-hedged-request-buffered-reply-noleak.md](12-hedged-request-buffered-reply-noleak.md) | Next: [../04-channel-direction/00-concepts.md](../04-channel-direction/00-concepts.md)
