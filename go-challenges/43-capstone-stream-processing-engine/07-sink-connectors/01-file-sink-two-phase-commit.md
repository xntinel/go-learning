# Exercise 1: File Sink with Two-Phase Commit

The file sink is the workhorse output connector: it writes records as newline-delimited JSON to a file, and it earns exactly-once delivery by implementing two-phase commit on top of the engine's checkpoints. This exercise also introduces the `Sink` and `CheckpointableSink` interfaces and the `Metrics` counters that every connector in this chapter shares.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
sink.go                Record, Sink, CheckpointableSink, Metrics, sentinel errors
file_sink.go           FileSink: buffered append, rotation, PrepareCommit + Commit
cmd/
  demo/
    main.go            write five records, two-phase commit, read the committed file
file_sink_test.go      flush, 2PC, stale-temp cleanup, rotation, implicit-close-flush
```

- Files: `sink.go`, `file_sink.go`, `cmd/demo/main.go`, `file_sink_test.go`.
- Implement: `FileSink` with `Open`, `Write`, `Flush`, `Close`, and the `CheckpointableSink` methods `PrepareCommit` and `Commit`, plus the shared `Record`, `Sink`, and `Metrics` types.
- Test: `file_sink_test.go` round-trips records to disk, drives prepare/commit through an atomic rename, proves `Open` removes stale `.tmp` files, exercises size-based rotation, and pins that `Close` is an implicit flush.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p file-sink-two-phase-commit/cmd/demo && cd file-sink-two-phase-commit
go mod init example.com/file-sink-two-phase-commit
go mod edit -go=1.26
```

### The shared types: interface, errors, and atomic metrics

Every connector in this chapter implements the same `Sink` interface — `Open`, `Write`, `Flush`, `Close` — so the engine can drive any of them through one code path. The file sink additionally implements `CheckpointableSink`, which extends `Sink` with `PrepareCommit` and `Commit`: the two phases of the commit protocol. Separating the two interfaces lets the engine ask, with a type assertion, whether a given sink can participate in exactly-once checkpointing or is only capable of at-least-once delivery.

The `Metrics` struct is built from `atomic.Int64`, a struct type with `Add`, `Load`, `Store`, and `CompareAndSwap` methods (added in Go 1.19). It replaces the older package-level `sync/atomic` functions, needs no manual alignment, and is safe to read from any goroutine at any instant — so a monitoring goroutine can sample a sink's counters while it is actively writing without a data race.

Create `sink.go`:

```go
// Package sink provides output connectors for a stream processing pipeline.
// FileSink additionally implements CheckpointableSink for exactly-once delivery
// via two-phase commit.
package sink

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Record is the unit of data flowing through the pipeline.
type Record struct {
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Metadata  map[string]string
}

// Sentinel errors. Wrap with fmt.Errorf("%w", ...) for context; check with errors.Is.
var (
	ErrNotOpen     = errors.New("sink: not open")
	ErrAlreadyOpen = errors.New("sink: already open")
	ErrEmptyPath   = errors.New("sink: path must not be empty")
)

// Sink is the write side of the stream pipeline.
// Open must be called before Write. Close must be called exactly once.
type Sink interface {
	Open(ctx context.Context) error
	Write(ctx context.Context, records []Record) error
	Flush(ctx context.Context) error
	Close() error
}

// CheckpointableSink extends Sink with a two-phase commit protocol for
// exactly-once delivery. PrepareCommit stages pending data durably;
// Commit makes it permanent. On recovery, Open discards any uncommitted
// staged data and the engine replays records from the last checkpoint.
type CheckpointableSink interface {
	Sink
	PrepareCommit(ctx context.Context, checkpointID uint64) error
	Commit(ctx context.Context, checkpointID uint64) error
}

// Metrics tracks per-sink activity counters. All fields are atomic and safe
// to read from any goroutine at any time.
type Metrics struct {
	RecordsWritten atomic.Int64
	BytesWritten   atomic.Int64
	BatchesFlushed atomic.Int64
	FlushErrors    atomic.Int64
	Retries        atomic.Int64
}
```

### How the file sink reaches exactly-once

Records do not go straight to disk. `Write` serializes each record to a JSON line and appends it to an in-memory `pending` buffer; nothing touches the filesystem yet. From there, two paths diverge. `Flush` is the at-least-once path: it appends the pending lines to the primary output file and clears the buffer. If a crash interrupts a `Flush` and the engine replays the batch, the file ends up with duplicates — acceptable only when the downstream is idempotent.

`PrepareCommit` and `Commit` are the exactly-once path, and they are the heart of the exercise. `PrepareCommit(id)` writes the pending lines to a staging file named `{path}.ckpt-{id}.tmp`, flushes the buffered writer, and calls `f.Sync()` to force the bytes through the kernel and the drive's cache so they survive a power loss. Crucially, it does *not* clear `pending` — the records stay buffered until the commit succeeds, so a crash before commit loses nothing. `Commit(id)` then performs the single atomic step that makes the data visible: `os.Rename` of the `.tmp` file to its final `{path}.ckpt-{id}` name. On a POSIX filesystem this rename is atomic, so a reader sees the committed file in full or not at all — never a half-written prefix. Only after the rename succeeds does `Commit` clear the pending buffer.

The recovery half lives in `Open`, which globs `{path}.ckpt-*.tmp` and deletes any matches. A leftover `.tmp` file means a previous run pre-committed but crashed before committing; the engine will replay those records from the last checkpoint, so the stale staging file must go. This three-part design — stage-and-fsync, atomic-rename, clean-stale-on-open — is the same protocol Flink's file sink uses in streaming mode.

`Commit` deliberately treats a missing `.tmp` file as a no-op rather than an error: if `PrepareCommit` had nothing pending it never created a staging file, and committing that checkpoint is simply nothing to do.

Create `file_sink.go`:

```go
package sink

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileSinkConfig holds configuration for FileSink.
type FileSinkConfig struct {
	// Path is the base output file path. Required.
	Path string
	// MaxSizeBytes triggers file rotation when the active file reaches this
	// size. Zero disables rotation.
	MaxSizeBytes int64
	// BufSize is the bufio.Writer buffer size in bytes. Defaults to 64 KiB.
	BufSize int
}

// FileSink writes records as newline-delimited JSON. It implements
// CheckpointableSink: PrepareCommit stages pending records in a temp file;
// Commit atomically renames it to the final checkpoint path.
type FileSink struct {
	cfg     FileSinkConfig
	mu      sync.Mutex
	pending [][]byte // serialized JSON lines awaiting commit
	open    bool
	metrics Metrics
}

// NewFileSink constructs a FileSink. Returns ErrEmptyPath if cfg.Path is empty.
func NewFileSink(cfg FileSinkConfig) (*FileSink, error) {
	if cfg.Path == "" {
		return nil, ErrEmptyPath
	}
	if cfg.BufSize <= 0 {
		cfg.BufSize = 64 * 1024
	}
	return &FileSink{cfg: cfg}, nil
}

// Metrics returns the live activity counters.
func (s *FileSink) Metrics() *Metrics { return &s.metrics }

// Open prepares the sink for writing. It removes leftover .tmp checkpoint
// files from any previous crashed run.
func (s *FileSink) Open(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open {
		return ErrAlreadyOpen
	}
	if err := s.removeStaleTempFiles(); err != nil {
		return fmt.Errorf("file sink open: %w", err)
	}
	s.pending = s.pending[:0]
	s.open = true
	return nil
}

// removeStaleTempFiles deletes uncommitted .tmp checkpoint files.
// Callers must hold s.mu.
func (s *FileSink) removeStaleTempFiles() error {
	dir := filepath.Dir(s.cfg.Path)
	base := filepath.Base(s.cfg.Path)
	matches, err := filepath.Glob(filepath.Join(dir, base+".ckpt-*.tmp"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Write serializes records to JSON and appends them to the pending buffer.
func (s *FileSink) Write(_ context.Context, records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return ErrNotOpen
	}
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("file sink write: %w", err)
		}
		line := make([]byte, len(b)+1)
		copy(line, b)
		line[len(b)] = '\n'
		s.pending = append(s.pending, line)
		s.metrics.RecordsWritten.Add(1)
		s.metrics.BytesWritten.Add(int64(len(line)))
	}
	return nil
}

// Flush writes all pending records to the primary output file (at-least-once).
// For exactly-once delivery, use PrepareCommit + Commit instead.
func (s *FileSink) Flush(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return ErrNotOpen
	}
	if len(s.pending) == 0 {
		return nil
	}
	if err := s.appendPendingTo(s.cfg.Path, true); err != nil {
		s.metrics.FlushErrors.Add(1)
		return fmt.Errorf("file sink flush: %w", err)
	}
	s.pending = s.pending[:0]
	s.metrics.BatchesFlushed.Add(1)
	return nil
}

// appendPendingTo writes all pending lines to the named file. When rotate is
// true and MaxSizeBytes is set, it rotates the primary file before writing.
// Callers must hold s.mu.
func (s *FileSink) appendPendingTo(path string, rotate bool) error {
	if rotate && s.cfg.MaxSizeBytes > 0 && path == s.cfg.Path {
		info, err := os.Stat(path)
		if err == nil && info.Size() >= s.cfg.MaxSizeBytes {
			suffix := time.Now().UTC().Format("20060102T150405.000")
			rotated := s.cfg.Path + "." + suffix
			if err := os.Rename(s.cfg.Path, rotated); err != nil {
				return fmt.Errorf("rotate: %w", err)
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, s.cfg.BufSize)
	for _, line := range s.pending {
		if _, err := bw.Write(line); err != nil {
			f.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Close flushes pending records to the primary output file and marks the sink
// closed. Subsequent calls are no-ops.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return nil
	}
	s.open = false
	if len(s.pending) == 0 {
		return nil
	}
	if err := s.appendPendingTo(s.cfg.Path, true); err != nil {
		return fmt.Errorf("file sink close: %w", err)
	}
	s.pending = s.pending[:0]
	return nil
}

// PrepareCommit writes all pending records to a staging file
// {Path}.ckpt-{checkpointID}.tmp and fsyncs it so the data survives a crash.
// Pending records remain in the buffer until Commit clears them.
func (s *FileSink) PrepareCommit(_ context.Context, checkpointID uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return ErrNotOpen
	}
	if len(s.pending) == 0 {
		return nil
	}
	tmpPath := fmt.Sprintf("%s.ckpt-%d.tmp", s.cfg.Path, checkpointID)
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("file sink prepare: create: %w", err)
	}
	bw := bufio.NewWriterSize(f, s.cfg.BufSize)
	for _, line := range s.pending {
		if _, werr := bw.Write(line); werr != nil {
			f.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("file sink prepare: write: %w", werr)
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("file sink prepare: flush: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("file sink prepare: sync: %w", err)
	}
	return f.Close()
}

// Commit atomically renames the staging file to {Path}.ckpt-{checkpointID}
// and clears the pending buffer. If no staging file exists (nothing was
// pending), Commit is a no-op.
func (s *FileSink) Commit(_ context.Context, checkpointID uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tmpPath := fmt.Sprintf("%s.ckpt-%d.tmp", s.cfg.Path, checkpointID)
	finalPath := fmt.Sprintf("%s.ckpt-%d", s.cfg.Path, checkpointID)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("file sink commit: %w", err)
	}
	s.pending = s.pending[:0]
	return nil
}
```

Read `PrepareCommit` and `Commit` as the two halves of one durable handoff. `PrepareCommit` is allowed to fail at any line — create, write, flush, or sync — and every failure path removes the half-written `.tmp` file before returning, so a failed prepare never leaves a staging file that `Commit` might later promote. `Commit` does the one thing that cannot be half-done: the rename. Because the rename is atomic and idempotent-on-absence, the protocol survives a crash at any point.

### The runnable demo

The demo writes five records, runs a full two-phase commit for checkpoint 1, then reads the committed file back to prove the records landed. It avoids timestamps so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"example.com/file-sink-two-phase-commit"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "sink-demo")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "events.ndjson")
	fs, err := sink.NewFileSink(sink.FileSinkConfig{Path: path})
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := fs.Open(ctx); err != nil {
		return err
	}
	for i := 0; i < 5; i++ {
		if err := fs.Write(ctx, []sink.Record{{
			Key:   []byte(fmt.Sprintf("key-%d", i)),
			Value: []byte(fmt.Sprintf("event-%d", i)),
		}}); err != nil {
			return err
		}
	}

	// Two-phase commit for checkpoint 1.
	if err := fs.PrepareCommit(ctx, 1); err != nil {
		return err
	}
	if err := fs.Commit(ctx, 1); err != nil {
		return err
	}
	if err := fs.Close(); err != nil {
		return err
	}

	committed := path + ".ckpt-1"
	data, err := os.ReadFile(committed)
	if err != nil {
		return err
	}
	m := fs.Metrics()
	fmt.Printf("committed %s with %d lines\n", filepath.Base(committed), countLines(data))
	fmt.Printf("records=%d batches=%d flushErrors=%d\n",
		m.RecordsWritten.Load(), m.BatchesFlushed.Load(), m.FlushErrors.Load())
	return nil
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
committed events.ndjson.ckpt-1 with 5 lines
records=5 batches=0 flushErrors=0
```

`batches=0` because this run committed through the 2PC path, which does not increment `BatchesFlushed` — that counter tracks the at-least-once `Flush` path, which the demo never calls.

### Tests

The tests pin the file sink's contracts. `TestFileSinkWriteAndFlush` round-trips records through the at-least-once path and confirms each line is valid JSON. `TestFileSinkTwoPhaseCommit` drives prepare and commit and asserts the staging file exists after prepare and is renamed away after commit. `TestFileSinkOpenRemovesStaleTempFiles` plants a leftover `.tmp` and proves `Open` removes it. `TestFileSinkRotatesOnSizeLimit` forces rotation with a one-byte limit. And `TestFileSinkCloseFlushesWithoutExplicitFlush` pins the implicit-flush contract: records written but never explicitly flushed must still reach the file on `Close`.

Create `file_sink_test.go`:

```go
package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestFileSinkWriteAndFlush(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.ndjson")

	fs, err := NewFileSink(FileSinkConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := fs.Open(ctx); err != nil {
		t.Fatal(err)
	}
	records := []Record{
		{Key: []byte("k1"), Value: []byte("v1")},
		{Key: []byte("k2"), Value: []byte("v2")},
	}
	if err := fs.Write(ctx, records); err != nil {
		t.Fatal(err)
	}
	if err := fs.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), data)
	}
	for i, line := range lines {
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("line %d: %v: %s", i, err, line)
		}
	}
}

func TestFileSinkTwoPhaseCommit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.ndjson")

	fs, _ := NewFileSink(FileSinkConfig{Path: path})
	ctx := context.Background()
	_ = fs.Open(ctx)
	_ = fs.Write(ctx, []Record{{Key: []byte("k"), Value: []byte("v")}})

	if err := fs.PrepareCommit(ctx, 42); err != nil {
		t.Fatalf("PrepareCommit: %v", err)
	}
	tmpPath := fmt.Sprintf("%s.ckpt-42.tmp", path)
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("temp file not created: %v", err)
	}

	if err := fs.Commit(ctx, 42); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	finalPath := fmt.Sprintf("%s.ckpt-42", path)
	if _, err := os.Stat(finalPath); err != nil {
		t.Fatalf("committed file not found: %v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("temp file should be gone after commit")
	}

	if got := fs.metrics.RecordsWritten.Load(); got != 1 {
		t.Fatalf("RecordsWritten = %d, want 1", got)
	}
	_ = fs.Close()
}

func TestFileSinkOpenRemovesStaleTempFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.ndjson")

	stale := fmt.Sprintf("%s.ckpt-7.tmp", path)
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs, _ := NewFileSink(FileSinkConfig{Path: path})
	ctx := context.Background()
	if err := fs.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale .tmp file was not removed by Open")
	}
	_ = fs.Close()
}

func TestFileSinkRotatesOnSizeLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.ndjson")

	fs, _ := NewFileSink(FileSinkConfig{Path: path, MaxSizeBytes: 1})
	ctx := context.Background()
	_ = fs.Open(ctx)

	_ = fs.Write(ctx, []Record{{Key: []byte("a")}})
	_ = fs.Flush(ctx)

	_ = fs.Write(ctx, []Record{{Key: []byte("b")}})
	_ = fs.Flush(ctx)
	_ = fs.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 files in %s, got %d", dir, len(entries))
	}
}

func TestFileSinkRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := NewFileSink(FileSinkConfig{})
	if !errors.Is(err, ErrEmptyPath) {
		t.Fatalf("err = %v, want ErrEmptyPath", err)
	}
}

func TestFileSinkWriteBeforeOpenFails(t *testing.T) {
	t.Parallel()

	fs, _ := NewFileSink(FileSinkConfig{Path: "/tmp/x"})
	err := fs.Write(context.Background(), []Record{{Key: []byte("k")}})
	if !errors.Is(err, ErrNotOpen) {
		t.Fatalf("err = %v, want ErrNotOpen", err)
	}
}

// TestFileSinkCloseFlushesWithoutExplicitFlush pins the contract that Close is
// an implicit flush: records written but never explicitly flushed must still
// reach the primary output file when Close is called.
func TestFileSinkCloseFlushesWithoutExplicitFlush(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.ndjson")

	fs, _ := NewFileSink(FileSinkConfig{Path: path})
	ctx := context.Background()
	if err := fs.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fs.Write(ctx, []Record{{Key: []byte("k"), Value: []byte("v")}}); err != nil {
		t.Fatal(err)
	}
	// No Flush call here on purpose.
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("primary file missing: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("primary file is empty: Close did not flush pending records")
	}
}

func ExampleNewFileSink() {
	dir, _ := os.MkdirTemp("", "sinke")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "out.ndjson")

	fs, _ := NewFileSink(FileSinkConfig{Path: path})
	ctx := context.Background()
	_ = fs.Open(ctx)
	_ = fs.Write(ctx, []Record{{Key: []byte("k"), Value: []byte("hello")}})
	_ = fs.Flush(ctx)
	_ = fs.Close()

	data, _ := os.ReadFile(path)
	fmt.Printf("nonempty=%v lines=%d\n", len(data) > 0, bytes.Count(data, []byte("\n")))
	// Output: nonempty=true lines=1
}

func ExampleFileSink_PrepareCommit() {
	dir, _ := os.MkdirTemp("", "sink2pc")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "events.ndjson")

	fs, _ := NewFileSink(FileSinkConfig{Path: path})
	ctx := context.Background()
	_ = fs.Open(ctx)
	_ = fs.Write(ctx, []Record{{Key: []byte("k")}})
	_ = fs.PrepareCommit(ctx, 1)
	_ = fs.Commit(ctx, 1)
	_ = fs.Close()

	_, err := os.Stat(path + ".ckpt-1")
	fmt.Printf("committed=%v\n", err == nil)
	// Output: committed=true
}
```

## Review

The sink is correct when the two commit phases are clean mirrors of stage and promote. `PrepareCommit` must fsync the staging file and keep the pending buffer intact, so a crash before `Commit` is fully recoverable; `Commit` must make the data visible with a single atomic `os.Rename` and only then clear pending. Confirm `Open` removes stale `.ckpt-*.tmp` files so a crashed prepare cannot strand a staging file, and that `Close` flushes pending records even when the caller never called `Flush`. The common mistakes are skipping the `f.Sync()` in prepare (the data is in the page cache but not on the platter, so a power loss loses an acknowledged checkpoint), clearing pending in `PrepareCommit` instead of `Commit` (a crash between the two then loses the records), and treating a missing `.tmp` in `Commit` as an error instead of a no-op. The full test suite passing under `go test -race ./...` establishes these properties.

## Resources

- [`os.Rename`: atomic rename on POSIX](https://pkg.go.dev/os#Rename) — the atomicity guarantee that makes the commit phase exactly-once.
- [`os.File.Sync`](https://pkg.go.dev/os#File.Sync) — forces staged data through the kernel and drive cache so a pre-committed checkpoint survives a crash.
- [`bufio.Writer`](https://pkg.go.dev/bufio#Writer) — buffered I/O that amortizes syscall overhead on the write path.
- [Apache Flink FileSink streaming mode](https://nightlies.apache.org/flink/flink-docs-stable/docs/connectors/datastream/filesystem/) — the production two-phase commit design this exercise mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-tcp-sink-reconnection.md](02-tcp-sink-reconnection.md)
