# Exercise 1: File Source

A source connector hides one specific origin behind the common `Source` contract. This first one tails append-only files the way `tail -f` does: seek to the end on open, then poll for new lines and emit each as a `Record`, shutting down cleanly when the context is cancelled.

Every module in this lesson is fully self-contained: it begins with its own `go mod init`, bundles the shared `Record`, `Metrics`, and `Source` definitions it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
file-source/
  go.mod
  source.go              Record, Metrics, Source, ErrSourceClosed
  file_source.go         FileSource: tail loop, SeekEnd, atomic byte counter
  file_source_test.go    new-line emission, graceful shutdown, missing-path error
  cmd/demo/main.go       tail a temp file and print one emitted line
```

- Files: `source.go`, `file_source.go`, `file_source_test.go`, `cmd/demo/main.go`.
- Implement: `FileSource` with `Open(ctx) (<-chan Record, <-chan error)`, `Close() error`, and `Metrics() Metrics`, plus the bundled `Record`/`Metrics`/`Source` types.
- Test: lines appended after `Open` are emitted in order; `Close` returns quickly; a non-existent path surfaces an error on the error channel.
- Verify: `go test -race ./...`

### The lifecycle contract, in one source

`source.go` holds the vocabulary every source in this lesson shares. `Record` is the atomic unit that flows downstream: `Key` and `Value` are raw bytes, `Source` identifies the origin, and `Metadata` carries optional per-source annotations. `Metrics` is a point-in-time counter snapshot. `Source` is the interface the rest of the pipeline programs against — it never names `FileSource`, only `Open`, `Close`, and `Metrics`. `ErrSourceClosed` is the sentinel `Close` returns when the source was never opened, so a double `Close` or a `Close` before `Open` is a typed error rather than a nil-pointer panic.

Create `source.go`:

```go
package filesource

import (
	"context"
	"errors"
	"time"
)

// Record is the atomic unit flowing through the pipeline.
type Record struct {
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Source    string
	Metadata  map[string]string
}

// Metrics is a point-in-time snapshot of a source's counters.
type Metrics struct {
	RecordsEmitted int64
	BytesRead      int64
	ErrorsTotal    int64
	BacklogSize    int64
}

// ErrSourceClosed is returned by Close when the source was never opened.
var ErrSourceClosed = errors.New("filesource: source not open")

// Source is the common interface for all data origins.
type Source interface {
	Open(ctx context.Context) (<-chan Record, <-chan error)
	Close() error
	Metrics() Metrics
}
```

### Why seek to the end, and why a poll loop

`Open` creates an internal context derived from the caller's so that `Close` (which cancels the internal context) and an upstream cancellation both funnel into the same shutdown path. It launches one `tail` goroutine per path under the `WaitGroup`, and a single closer goroutine that `Wait`s and then closes both channels exactly once — the only place a channel is ever closed.

Each `tail` goroutine opens its file and immediately `Seek(0, io.SeekEnd)`. This is the decision that makes it a *tailer* and not a *reader*: a tailer emits only content written after it started watching, exactly like `tail -f`, so replaying a service's entire historical log on every restart is avoided. It then drives a `bufio.Scanner`. The inner `for scanner.Scan()` loop drains every currently-available line; when `Scan` returns false the loop checks `scanner.Err()` (a real read error is fatal for that file) and otherwise treats the false as EOF — it sleeps for `pollEvery` and resets the scanner to pick up bytes appended since. A POSIX read past end-of-file returns zero bytes rather than an error, which is precisely why this poll-on-EOF design works.

The send is a context-guarded blocking send: `select { case fs.records <- r: case <-ctx.Done(): return }`. A file is a durable log, so dropping a line silently would be invisible data loss; blocking applies backpressure to the tailer instead, and the `ctx.Done()` arm guarantees a cancelled source never wedges behind a full buffer. Byte and record counts live in `atomic.Int64` fields so `Metrics` reads them without a lock from any goroutine.

Create `file_source.go`:

```go
package filesource

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// FileSource tails one or more files, emitting each new line as a Record.
// It starts reading from the end of the file on Open, then polls for new content.
type FileSource struct {
	paths      []string
	bufferSize int
	pollEvery  time.Duration

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	records chan Record
	errs    chan error

	emitted atomic.Int64
	bytes   atomic.Int64
	errCnt  atomic.Int64
}

// NewFileSource creates a FileSource that tails the given paths.
func NewFileSource(paths []string, bufferSize int, pollEvery time.Duration) *FileSource {
	return &FileSource{paths: paths, bufferSize: bufferSize, pollEvery: pollEvery}
}

func (fs *FileSource) Open(ctx context.Context) (<-chan Record, <-chan error) {
	inner, cancel := context.WithCancel(ctx)
	fs.cancel = cancel
	fs.records = make(chan Record, fs.bufferSize)
	fs.errs = make(chan error, 16)

	for _, p := range fs.paths {
		fs.wg.Add(1)
		go fs.tail(inner, p)
	}

	go func() {
		fs.wg.Wait()
		close(fs.records)
		close(fs.errs)
	}()

	return fs.records, fs.errs
}

func (fs *FileSource) tail(ctx context.Context, path string) {
	defer fs.wg.Done()

	f, err := os.Open(path)
	if err != nil {
		fs.errCnt.Add(1)
		select {
		case fs.errs <- fmt.Errorf("filesource: open %s: %w", path, err):
		default:
		}
		return
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		fs.errCnt.Add(1)
		return
	}

	scanner := bufio.NewScanner(f)
	for {
		for scanner.Scan() {
			line := scanner.Bytes()
			fs.bytes.Add(int64(len(line)))

			r := Record{
				Value:     append([]byte(nil), line...),
				Timestamp: time.Now().UTC(),
				Source:    path,
			}
			select {
			case fs.records <- r:
				fs.emitted.Add(1)
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			fs.errCnt.Add(1)
			select {
			case fs.errs <- fmt.Errorf("filesource: scan %s: %w", path, err):
			default:
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(fs.pollEvery):
			scanner = bufio.NewScanner(f)
		}
	}
}

func (fs *FileSource) Close() error {
	if fs.cancel == nil {
		return ErrSourceClosed
	}
	fs.cancel()
	fs.wg.Wait()
	return nil
}

func (fs *FileSource) Metrics() Metrics {
	return Metrics{
		RecordsEmitted: fs.emitted.Load(),
		BytesRead:      fs.bytes.Load(),
		ErrorsTotal:    fs.errCnt.Load(),
	}
}

var _ Source = (*FileSource)(nil)
```

### The runnable demo

The demo creates a temp file, opens a `FileSource` on it, waits briefly so the tail goroutine reaches its `SeekEnd` before anything is written, then appends one line and prints the record that comes back along with the byte count.

The deliberate `time.Sleep(50ms)` before the write is not cosmetic: without it the goroutine may seek to end *after* the write lands, positioning the file pointer past the new content and missing the line entirely. This is the classic tail race, and the demo makes it visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	fs "example.com/file-source"
)

func main() {
	f, err := os.CreateTemp("", "demo-*.log")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.Remove(f.Name())

	src := fs.NewFileSource([]string{f.Name()}, 16, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	recs, errs := src.Open(ctx)
	go func() {
		for range errs {
		}
	}()

	time.Sleep(50 * time.Millisecond)
	fmt.Fprintln(f, "line-from-file")

	r := <-recs
	fmt.Printf("FileSource emitted: %s\n", r.Value)

	src.Close()
	m := src.Metrics()
	fmt.Printf("records=%d bytes=%d\n", m.RecordsEmitted, m.BytesRead)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
FileSource emitted: line-from-file
records=1 bytes=14
```

The line `line-from-file` is 14 bytes (the scanner strips the trailing newline), which is exactly what `BytesRead` reports.

### Tests

The tests pin three properties. `TestFileSourceEmitsNewLines` writes three lines after `Open` and asserts all three arrive in order and that `RecordsEmitted` matches. `TestFileSourceGracefulShutdown` opens a source on an idle file and asserts `Close` returns in well under a second — proving the poll loop's `ctx.Done()` arm actually unblocks the sleep. `TestFileSourceNonExistentPath` (the property that an unreadable origin must surface, not swallow, its error) opens a path that does not exist and asserts an error lands on the error channel within 500 ms and `ErrorsTotal` is non-zero.

Create `file_source_test.go`:

```go
package filesource

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func drain(ch <-chan Record, max int, timeout time.Duration) []Record {
	var out []Record
	deadline := time.After(timeout)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, r)
			if len(out) >= max {
				return out
			}
		case <-deadline:
			return out
		}
	}
}

func TestFileSourceEmitsNewLines(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp(t.TempDir(), "tail-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	fs := NewFileSource([]string{f.Name()}, 32, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	recs, errs := fs.Open(ctx)
	go func() {
		for e := range errs {
			t.Logf("filesource error: %v", e)
		}
	}()

	time.Sleep(50 * time.Millisecond)

	lines := []string{"hello", "world", "stream"}
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			t.Fatal(err)
		}
	}

	got := drain(recs, len(lines), 2*time.Second)
	if len(got) != len(lines) {
		t.Fatalf("got %d records, want %d", len(got), len(lines))
	}
	for i, r := range got {
		if string(r.Value) != lines[i] {
			t.Errorf("record[%d] = %q, want %q", i, r.Value, lines[i])
		}
	}

	fs.Close()
	if m := fs.Metrics(); m.RecordsEmitted != int64(len(lines)) {
		t.Errorf("RecordsEmitted = %d, want %d", m.RecordsEmitted, len(lines))
	}
}

func TestFileSourceGracefulShutdown(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp(t.TempDir(), "tail-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	fs := NewFileSource([]string{f.Name()}, 8, 10*time.Millisecond)
	_, _ = fs.Open(context.Background())

	start := time.Now()
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close took %v, want < 1s", elapsed)
	}
}

func TestFileSourceNonExistentPath(t *testing.T) {
	t.Parallel()

	fs := NewFileSource([]string{"/nonexistent/path.log"}, 8, 10*time.Millisecond)
	_, errs := fs.Open(context.Background())

	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no error received within 500ms")
	}
	fs.Close()
	if m := fs.Metrics(); m.ErrorsTotal == 0 {
		t.Error("ErrorsTotal = 0, want >= 1")
	}
}
```

## Review

The source is correct when `Open` and `Close` compose without a deadlock and the record channel closes exactly once. Confirm the tail goroutine seeks to end before emitting, so only post-`Open` content appears; confirm the EOF branch sleeps and retries rather than returning, so an idle file stays a live source; confirm every send is guarded by `ctx.Done()`, so `Close` is fast even with a full buffer. The common mistakes here are treating `Scan`-returns-false as a fatal end (it is usually just EOF), using a non-blocking send and silently dropping lines from what is supposed to be a durable log, and closing the channels from `Close` itself instead of from the single post-`Wait` closer goroutine. Running the suite under `-race` is what proves the atomic counters and the channel-close ordering are actually safe.

## Resources

- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner) — the `Scan`/`Err`/`Bytes` contract the tail loop is built on, including the EOF behaviour.
- [`os.File.Seek` and `io.SeekEnd`](https://pkg.go.dev/io#pkg-constants) — seeking to the end of a file, the operation that makes this a tailer.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `Int64.Add`/`Load`, the lock-free counters behind `Metrics`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-tcp-source.md](02-tcp-source.md)
