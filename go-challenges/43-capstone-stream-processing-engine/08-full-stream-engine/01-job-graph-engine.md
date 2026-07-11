# Exercise 1: The Job-Graph Engine

A stream engine has a control plane before it has a data plane: you declare a pipeline, validate it, compile it to a task graph, submit it for execution, and watch its lifecycle and backpressure. This module builds that control plane — a fluent `JobBuilder`, an immutable `JobDecl`, a `Compile` step that inserts shuffles automatically, a `JobManager` state machine, and a `BackpressureMonitor` — as one cohesive, self-contained package.

This module is fully self-contained. It defines its own record/message types, operator function types, source and sink interfaces, the builder, the compiler, the manager, and the monitor in package `streamengine`, plus its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
engine.go     Record, Message/MsgType, CheckpointBarrier, Watermark,
              WindowSpec + TumblingWindow/SlidingWindow,
              MapFn/FilterFn/KeyFn/ReduceFn, Source/Sink/CheckpointableSink
builder.go    StageKind, stage, sentinel errors, JobDecl, JobBuilder
              (accumulator-error fluent API), Build defensive copy
graph.go      NodeKind, TaskNode, TaskGraph, CompileOptions,
              Compile (auto-inserts NodeShuffle before NodeKeyBy)
manager.go    JobStatus state machine, JobManager: Submit/Cancel/Status
monitor.go    ChannelMetric, BackpressureMonitor: Register/Sample
cmd/
  demo/
    main.go   build -> compile -> submit -> sample -> cancel
engine_test.go builder validation, compile/shuffle, manager lifecycle,
              concurrent submit, backpressure sampling, examples
```

- Files: `engine.go`, `builder.go`, `graph.go`, `manager.go`, `monitor.go`, `cmd/demo/main.go`, `engine_test.go`.
- Implement: the fluent `JobBuilder` with the accumulator-error pattern, `Compile` with automatic shuffle insertion, the `JobManager` state machine, and the `BackpressureMonitor`.
- Test: builder validation (missing/duplicate/nil endpoints, ordering rules, first-error-wins), compilation (node count, auto-shuffle, upstream links, bad parallelism), manager (submit/status/cancel/double-cancel/unknown, concurrent submit), and backpressure sampling.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p streamengine/cmd/demo && cd streamengine
go mod init example.com/streamengine
go mod edit -go=1.26
```

### How the pieces fit

The package is split by responsibility but is one logical unit. `engine.go` is the vocabulary: a `Record` is one event; a `Message` is the tagged union (record, barrier, or watermark) that every inter-task channel carries, so barrier-alignment and watermark code handle one channel per upstream link rather than three; the operator function types and the `Source`/`Sink`/`CheckpointableSink` interfaces are the seams the runnable engine and real connectors plug into.

`builder.go` is the declarative front end. Each method appends a `stage` and latches the first error, and `Build()` checks the two mandatory endpoints (a source and a sink) and returns an immutable `*JobDecl` whose stage slice is a defensive copy — so further builder calls cannot mutate a declaration already handed out. The ordering rules (`Window` must follow `KeyBy`, `Reduce` must follow `Window`) are enforced here at declaration time, where the error names the exact misuse, rather than failing obscurely at execution.

`graph.go` is the compiler. It walks the stages in order, and the one piece of real logic is the shuffle insertion: when it reaches a `KeyBy` whose upstream is not already keyed, it emits a `NodeShuffle` first so the graph makes the network redistribution explicit and inspectable. Every node records its upstream pointers, so the resulting DAG is the authoritative data-flow view. `manager.go` is the lifecycle: a mutex serializes every status mutation, an `atomic.Uint64` mints job IDs without touching that mutex, and each job owns a derived cancel function so `Cancel` both fires cancellation and records the terminal state. `monitor.go` is operational telemetry: `Register` adds a channel, `Sample` reads `len`/`cap` of each without blocking any pipeline goroutine, so you can poll backpressure from a separate goroutine at any frequency.

Create `engine.go`:

```go
// Package streamengine assembles the source connectors, operators, windowing,
// watermarks, checkpointing, parallel execution, and sink connectors from the
// preceding lessons into a unified runtime with a declarative job API.
package streamengine

import (
	"context"
	"time"
)

// Record is a single event in the stream, identified by a key and carrying a
// binary payload with an event timestamp.
type Record struct {
	Key       string
	Value     []byte
	Timestamp time.Time
	Metadata  map[string]string
}

// MsgType discriminates the payload of a Message.
type MsgType uint8

const (
	// MsgRecord carries a data record.
	MsgRecord MsgType = iota
	// MsgBarrier carries a checkpoint barrier.
	MsgBarrier
	// MsgWatermark carries a watermark.
	MsgWatermark
)

// CheckpointBarrier flows downstream alongside records to trigger a
// distributed snapshot based on the Chandy-Lamport algorithm (lesson 05).
type CheckpointBarrier struct {
	ID        uint64
	CreatedAt time.Time
}

// Watermark signals that no Record with Timestamp earlier than T will arrive
// from upstream. Window operators close and emit results when the watermark
// advances past the window boundary.
type Watermark struct {
	T time.Time
}

// Message is the tagged union flowing through every inter-task channel.
// Combining all three kinds in one type lets barrier-alignment code handle a
// single channel per upstream link instead of maintaining separate channels
// per message type.
type Message struct {
	Type    MsgType
	Record  *Record
	Barrier *CheckpointBarrier
	Mark    *Watermark
}

// WindowKind identifies the windowing strategy.
type WindowKind uint8

const (
	// WindowTumbling is a fixed-size, non-overlapping window.
	WindowTumbling WindowKind = iota
	// WindowSliding is a fixed-size window that advances by a slide interval.
	WindowSliding
	// WindowSession closes when the gap between events exceeds a timeout.
	WindowSession
)

// WindowSpec describes a windowing operation.
type WindowSpec struct {
	Kind  WindowKind
	Size  time.Duration
	Slide time.Duration // meaningful only for WindowSliding
}

// TumblingWindow returns a tumbling-window specification of the given size.
func TumblingWindow(size time.Duration) WindowSpec {
	return WindowSpec{Kind: WindowTumbling, Size: size}
}

// SlidingWindow returns a sliding-window specification.
func SlidingWindow(size, slide time.Duration) WindowSpec {
	return WindowSpec{Kind: WindowSliding, Size: size, Slide: slide}
}

// MapFn transforms one Record into one Record.
type MapFn func(Record) (Record, error)

// FilterFn returns true if the Record should propagate downstream.
type FilterFn func(Record) bool

// KeyFn extracts the partition key from a Record for grouping or shuffling.
type KeyFn func(Record) string

// ReduceFn merges two Records within a window into one aggregated Record.
type ReduceFn func(a, b Record) Record

// Source emits a stream of Messages. Open starts the source's goroutines and
// returns a record channel and an error channel, both closed after the source
// exhausts its input or the context is cancelled. Close signals intent to
// stop and blocks until all goroutines have exited.
type Source interface {
	Open(ctx context.Context) (<-chan Message, <-chan error)
	Close() error
}

// Sink receives the final stream of Records. Open initialises the connection;
// Close flushes pending writes and releases resources.
type Sink interface {
	Open(ctx context.Context) error
	Write(r Record) error
	Close() error
}

// CheckpointableSink extends Sink with the two-phase commit protocol for
// exactly-once delivery (lesson 07). PrepareCommit persists buffered records
// to a staging area; Commit promotes the staged writes atomically.
type CheckpointableSink interface {
	Sink
	PrepareCommit(checkpointID uint64) error
	Commit(checkpointID uint64) error
}
```

Create `builder.go`:

```go
package streamengine

import "errors"

// Sentinel errors returned by JobBuilder methods and Build().
var (
	ErrEmptyJobName    = errors.New("streamengine: job name must not be empty")
	ErrNoSource        = errors.New("streamengine: job must have at least one source")
	ErrNoSink          = errors.New("streamengine: job must have at least one sink")
	ErrDuplicateSource = errors.New("streamengine: only one source is allowed per job")
	ErrDuplicateSink   = errors.New("streamengine: only one sink is allowed per job")
	ErrNilSource       = errors.New("streamengine: source must not be nil")
	ErrNilSink         = errors.New("streamengine: sink must not be nil")
	ErrNilMapFn        = errors.New("streamengine: map function must not be nil")
	ErrNilFilterFn     = errors.New("streamengine: filter function must not be nil")
	ErrNilKeyFn        = errors.New("streamengine: key function must not be nil")
	ErrNilReduceFn     = errors.New("streamengine: reduce function must not be nil")
	ErrWindowBeforeKey = errors.New("streamengine: Window must follow KeyBy")
	ErrReduceBeforeWin = errors.New("streamengine: Reduce must follow Window")
)

// StageKind identifies the declared kind of a pipeline step.
type StageKind uint8

const (
	StageSource StageKind = iota
	StageMap
	StageFilter
	StageKeyBy
	StageWindow
	StageReduce
	StageSink
)

// stage holds the configuration for one pipeline step.
type stage struct {
	kind   StageKind
	source Source
	sink   Sink
	mapFn  MapFn
	filter FilterFn
	keyFn  KeyFn
	window WindowSpec
	reduce ReduceFn
}

// JobDecl is the immutable, validated description of a pipeline. Obtain one
// via JobBuilder.Build(); pass it to Compile to produce a TaskGraph.
type JobDecl struct {
	name   string
	stages []stage
}

// Name returns the job's human-readable name.
func (d *JobDecl) Name() string { return d.name }

// StageCount returns the number of declared pipeline stages.
func (d *JobDecl) StageCount() int { return len(d.stages) }

// JobBuilder is a fluent API for declaring a streaming pipeline.
// Methods accumulate the first error; Build() reports it.
// Calling methods after an error is safe and returns the builder unchanged.
type JobBuilder struct {
	name   string
	stages []stage
	err    error
}

// NewJobBuilder starts building a job named name.
// Returns a builder whose Build() will fail if name is empty.
func NewJobBuilder(name string) *JobBuilder {
	if name == "" {
		return &JobBuilder{err: ErrEmptyJobName}
	}
	return &JobBuilder{name: name}
}

func (b *JobBuilder) setErr(err error) *JobBuilder {
	if b.err == nil {
		b.err = err
	}
	return b
}

// Source registers the single ingest source.
// Returns ErrNilSource if s is nil; ErrDuplicateSource if already registered.
func (b *JobBuilder) Source(s Source) *JobBuilder {
	if b.err != nil {
		return b
	}
	if s == nil {
		return b.setErr(ErrNilSource)
	}
	for _, st := range b.stages {
		if st.kind == StageSource {
			return b.setErr(ErrDuplicateSource)
		}
	}
	b.stages = append(b.stages, stage{kind: StageSource, source: s})
	return b
}

// Map appends a stateless one-to-one transformation.
func (b *JobBuilder) Map(fn MapFn) *JobBuilder {
	if b.err != nil {
		return b
	}
	if fn == nil {
		return b.setErr(ErrNilMapFn)
	}
	b.stages = append(b.stages, stage{kind: StageMap, mapFn: fn})
	return b
}

// Filter appends a predicate that discards non-matching Records.
func (b *JobBuilder) Filter(fn FilterFn) *JobBuilder {
	if b.err != nil {
		return b
	}
	if fn == nil {
		return b.setErr(ErrNilFilterFn)
	}
	b.stages = append(b.stages, stage{kind: StageFilter, filter: fn})
	return b
}

// KeyBy appends a partitioning stage. Compile automatically inserts a Shuffle
// node before this stage when the upstream is not already keyed.
func (b *JobBuilder) KeyBy(fn KeyFn) *JobBuilder {
	if b.err != nil {
		return b
	}
	if fn == nil {
		return b.setErr(ErrNilKeyFn)
	}
	b.stages = append(b.stages, stage{kind: StageKeyBy, keyFn: fn})
	return b
}

// Window appends a windowing stage. Returns ErrWindowBeforeKey if no KeyBy
// stage precedes it.
func (b *JobBuilder) Window(spec WindowSpec) *JobBuilder {
	if b.err != nil {
		return b
	}
	keyed := false
	for _, st := range b.stages {
		if st.kind == StageKeyBy {
			keyed = true
			break
		}
	}
	if !keyed {
		return b.setErr(ErrWindowBeforeKey)
	}
	b.stages = append(b.stages, stage{kind: StageWindow, window: spec})
	return b
}

// Reduce appends a per-window aggregation. Returns ErrReduceBeforeWin if no
// Window stage precedes it.
func (b *JobBuilder) Reduce(fn ReduceFn) *JobBuilder {
	if b.err != nil {
		return b
	}
	if fn == nil {
		return b.setErr(ErrNilReduceFn)
	}
	windowed := false
	for _, st := range b.stages {
		if st.kind == StageWindow {
			windowed = true
			break
		}
	}
	if !windowed {
		return b.setErr(ErrReduceBeforeWin)
	}
	b.stages = append(b.stages, stage{kind: StageReduce, reduce: fn})
	return b
}

// Sink registers the single output sink.
// Returns ErrNilSink if s is nil; ErrDuplicateSink if already registered.
func (b *JobBuilder) Sink(s Sink) *JobBuilder {
	if b.err != nil {
		return b
	}
	if s == nil {
		return b.setErr(ErrNilSink)
	}
	for _, st := range b.stages {
		if st.kind == StageSink {
			return b.setErr(ErrDuplicateSink)
		}
	}
	b.stages = append(b.stages, stage{kind: StageSink, sink: s})
	return b
}

// Build validates the pipeline declaration and returns an immutable *JobDecl.
// It returns the first error accumulated by builder methods, or ErrNoSource /
// ErrNoSink if the mandatory endpoints are absent.
func (b *JobBuilder) Build() (*JobDecl, error) {
	if b.err != nil {
		return nil, b.err
	}
	hasSource, hasSink := false, false
	for _, st := range b.stages {
		switch st.kind {
		case StageSource:
			hasSource = true
		case StageSink:
			hasSink = true
		}
	}
	if !hasSource {
		return nil, ErrNoSource
	}
	if !hasSink {
		return nil, ErrNoSink
	}
	return &JobDecl{
		name:   b.name,
		stages: append([]stage(nil), b.stages...),
	}, nil
}
```

`Build()` copies the stage slice with `append([]stage(nil), b.stages...)` so a builder reused after `Build()` cannot mutate the `JobDecl` it already returned — the standard defensive-copy pattern for immutable value objects.

Create `graph.go`:

```go
package streamengine

import (
	"errors"
	"fmt"
)

// NodeKind identifies the operator type of a compiled task node.
type NodeKind uint8

const (
	NodeSource  NodeKind = iota // data source
	NodeMap                     // one-to-one transformation
	NodeFilter                  // predicate filter
	NodeKeyBy                   // key-based logical partitioning
	NodeShuffle                 // auto-inserted physical repartition before KeyBy
	NodeWindow                  // windowing operator
	NodeReduce                  // per-window aggregation
	NodeSink                    // output sink
)

// String returns the human-readable name of the node kind.
func (k NodeKind) String() string {
	switch k {
	case NodeSource:
		return "Source"
	case NodeMap:
		return "Map"
	case NodeFilter:
		return "Filter"
	case NodeKeyBy:
		return "KeyBy"
	case NodeShuffle:
		return "Shuffle"
	case NodeWindow:
		return "Window"
	case NodeReduce:
		return "Reduce"
	case NodeSink:
		return "Sink"
	default:
		return "Unknown"
	}
}

// TaskNode is one vertex in the compiled task graph.
type TaskNode struct {
	// ID is the unique sequential identifier assigned by the compiler.
	ID int
	// Kind is the operator type.
	Kind NodeKind
	// Upstream holds the direct predecessors in the DAG.
	Upstream []*TaskNode
	// stage points into the JobDecl.stages slice. Nil for Shuffle nodes.
	stage *stage
}

// TaskGraph is the DAG produced by Compile. In a full execution layer each
// node maps to one or more goroutines (parallel instances); this module
// produces the graph structure, and the runnable engine in the next exercise
// executes a topology of the same shape.
type TaskGraph struct {
	nodes   []*TaskNode
	sources []*TaskNode
	sinks   []*TaskNode
}

// Nodes returns all task nodes in topological order (source first, sink last).
func (g *TaskGraph) Nodes() []*TaskNode { return g.nodes }

// Sources returns the source nodes.
func (g *TaskGraph) Sources() []*TaskNode { return g.sources }

// Sinks returns the sink nodes.
func (g *TaskGraph) Sinks() []*TaskNode { return g.sinks }

// NodeCount returns the total number of nodes, including auto-inserted Shuffle
// nodes.
func (g *TaskGraph) NodeCount() int { return len(g.nodes) }

// CompileOptions controls graph compilation.
type CompileOptions struct {
	// Parallelism is the default number of concurrent task instances per
	// operator. Must be at least 1.
	Parallelism int
}

// ErrBadParallelism is returned by Compile when Parallelism is less than 1.
var ErrBadParallelism = errors.New("streamengine: parallelism must be at least 1")

// Compile translates a *JobDecl into a *TaskGraph. It walks the declared
// stages in order and automatically inserts a NodeShuffle before each
// NodeKeyBy stage when the upstream is not already partitioned by key.
//
// The Shuffle models the network redistribution that groups records by key
// before they reach keyed operators. Its presence in the graph makes the
// data-redistribution cost explicit and observable in the topology view.
func Compile(decl *JobDecl, opts CompileOptions) (*TaskGraph, error) {
	if opts.Parallelism < 1 {
		return nil, fmt.Errorf("%w: got %d", ErrBadParallelism, opts.Parallelism)
	}
	g := &TaskGraph{}
	nextID := 0
	keyed := false
	var prev *TaskNode

	for i := range decl.stages {
		st := &decl.stages[i]

		// Auto-insert a Shuffle before KeyBy when upstream is unpartitioned.
		if st.kind == StageKeyBy && !keyed {
			shuffle := &TaskNode{ID: nextID, Kind: NodeShuffle}
			nextID++
			if prev != nil {
				shuffle.Upstream = []*TaskNode{prev}
			}
			g.nodes = append(g.nodes, shuffle)
			prev = shuffle
		}

		node := &TaskNode{
			ID:    nextID,
			Kind:  stageToNodeKind(st.kind),
			stage: st,
		}
		nextID++
		if prev != nil {
			node.Upstream = []*TaskNode{prev}
		}
		g.nodes = append(g.nodes, node)

		switch st.kind {
		case StageKeyBy:
			keyed = true
		case StageSource:
			g.sources = append(g.sources, node)
		case StageSink:
			g.sinks = append(g.sinks, node)
		}
		prev = node
	}
	return g, nil
}

func stageToNodeKind(k StageKind) NodeKind {
	switch k {
	case StageSource:
		return NodeSource
	case StageMap:
		return NodeMap
	case StageFilter:
		return NodeFilter
	case StageKeyBy:
		return NodeKeyBy
	case StageWindow:
		return NodeWindow
	case StageReduce:
		return NodeReduce
	case StageSink:
		return NodeSink
	default:
		panic(fmt.Sprintf("streamengine: unknown stage kind %d", k))
	}
}
```

Create `manager.go`:

```go
package streamengine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// JobStatus represents the lifecycle state of a submitted job.
type JobStatus uint8

const (
	// JobCreated is the state before the job starts executing.
	JobCreated JobStatus = iota
	// JobRunning means the job is actively processing records.
	JobRunning
	// JobCheckpointing means a distributed checkpoint is in progress.
	JobCheckpointing
	// JobFailing means one or more tasks have reported errors.
	JobFailing
	// JobCancelled is the terminal state after Cancel is called.
	JobCancelled
	// JobFinished is the terminal state after the source exhausts its input.
	JobFinished
)

// String returns the human-readable name of the status.
func (s JobStatus) String() string {
	switch s {
	case JobCreated:
		return "Created"
	case JobRunning:
		return "Running"
	case JobCheckpointing:
		return "Checkpointing"
	case JobFailing:
		return "Failing"
	case JobCancelled:
		return "Cancelled"
	case JobFinished:
		return "Finished"
	default:
		return "Unknown"
	}
}

// ErrJobNotFound is returned when the given job ID is unknown.
var ErrJobNotFound = errors.New("streamengine: job not found")

// ErrJobNotRunning is returned when Cancel is called on a job that is already
// in a terminal state (Cancelled or Finished).
var ErrJobNotRunning = errors.New("streamengine: job is not running")

type job struct {
	id     string
	graph  *TaskGraph
	status JobStatus
	ctx    context.Context //nolint:containedctx // intentional: job owns its lifetime
	cancel context.CancelFunc
}

// JobManager submits, monitors, and cancels jobs.
// All methods are safe for concurrent use.
type JobManager struct {
	mu      sync.Mutex
	jobs    map[string]*job
	counter atomic.Uint64
}

// NewJobManager returns an initialised *JobManager.
func NewJobManager() *JobManager {
	return &JobManager{jobs: make(map[string]*job)}
}

// Submit accepts a compiled *TaskGraph, transitions it to JobRunning, and
// returns a unique job ID. The derived context is stored in the job and
// cancelled when Cancel is called.
func (jm *JobManager) Submit(ctx context.Context, g *TaskGraph) (string, error) {
	jctx, cancel := context.WithCancel(ctx)
	id := fmt.Sprintf("job-%d", jm.counter.Add(1))
	jm.mu.Lock()
	jm.jobs[id] = &job{
		id:     id,
		graph:  g,
		status: JobRunning,
		ctx:    jctx,
		cancel: cancel,
	}
	jm.mu.Unlock()
	return id, nil
}

// Cancel cancels the job and transitions it to JobCancelled.
// Returns ErrJobNotFound if the ID is unknown; returns ErrJobNotRunning if
// the job is already in a terminal state.
func (jm *JobManager) Cancel(id string) error {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	j, ok := jm.jobs[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	switch j.status {
	case JobRunning, JobCheckpointing, JobFailing:
		// cancellable states
	default:
		return fmt.Errorf("%w: %s (current: %s)", ErrJobNotRunning, id, j.status)
	}
	j.cancel()
	j.status = JobCancelled
	return nil
}

// Status returns the current lifecycle state of the job.
// Returns ErrJobNotFound if the ID is unknown.
func (jm *JobManager) Status(id string) (JobStatus, error) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	j, ok := jm.jobs[id]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	return j.status, nil
}
```

Create `monitor.go`:

```go
package streamengine

import "sync"

// ChannelMetric describes the instantaneous fill level of one inter-task
// channel. Pct is Len/Cap*100; a value near 100 signals backpressure.
type ChannelMetric struct {
	NodeID      int
	Description string
	Len         int
	Cap         int
	Pct         float64
}

type monitorEntry struct {
	nodeID      int
	description string
	ch          chan Message
}

// BackpressureMonitor tracks the fill level of registered inter-task channels.
// Call Register once per channel and Sample periodically (e.g., every second)
// to obtain a backpressure snapshot without affecting the pipeline goroutines.
//
// Polling is appropriate: a channel that is persistently full is a bottleneck
// regardless of when you sample it. Per-record observation would itself
// become a bottleneck on hot paths and is unnecessary for operational telemetry.
type BackpressureMonitor struct {
	mu      sync.Mutex
	entries []monitorEntry
}

// NewBackpressureMonitor returns an empty monitor.
func NewBackpressureMonitor() *BackpressureMonitor {
	return &BackpressureMonitor{}
}

// Register adds ch to the monitored set. nodeID identifies the upstream task
// node; description is a human-readable "source->target" label for logging.
func (m *BackpressureMonitor) Register(nodeID int, description string, ch chan Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, monitorEntry{
		nodeID:      nodeID,
		description: description,
		ch:          ch,
	})
}

// Sample reads len/cap of every registered channel without blocking and
// returns a snapshot. The snapshot is independent of the pipeline goroutines:
// it observes channel state with no synchronisation beyond the monitor's own
// mutex protecting the entry list.
func (m *BackpressureMonitor) Sample() []ChannelMetric {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ChannelMetric, 0, len(m.entries))
	for _, e := range m.entries {
		l := len(e.ch)
		c := cap(e.ch)
		pct := 0.0
		if c > 0 {
			pct = float64(l) / float64(c) * 100
		}
		out = append(out, ChannelMetric{
			NodeID:      e.nodeID,
			Description: e.description,
			Len:         l,
			Cap:         c,
			Pct:         pct,
		})
	}
	return out
}
```

### The runnable demo

The demo declares a word-count pipeline, compiles it (watch the shuffle appear before `KeyBy`), submits it to the manager, takes a backpressure reading on a freshly made channel, then cancels. It prints no addresses or timings, so the output is identical on every run.

Create `cmd/demo/main.go`:

```go
// cmd/demo demonstrates the job-graph engine: build a pipeline declaration,
// compile it to a task graph, submit it to the job manager, sample the
// backpressure monitor, and cancel.
//
// Run with: go run ./cmd/demo
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"example.com/streamengine"
)

// counterSource emits n records with cycling word keys, then closes.
type counterSource struct{ n int }

func (s *counterSource) Open(ctx context.Context) (<-chan streamengine.Message, <-chan error) {
	out := make(chan streamengine.Message, s.n)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		words := []string{"go", "rust", "go", "python", "go", "rust"}
		for i := 0; i < s.n; i++ {
			r := streamengine.Record{
				Key:       words[i%len(words)],
				Timestamp: time.Now(),
			}
			select {
			case out <- streamengine.Message{Type: streamengine.MsgRecord, Record: &r}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, errs
}

func (s *counterSource) Close() error { return nil }

// logSink prints the key of each received record.
type logSink struct{}

func (logSink) Open(_ context.Context) error { return nil }
func (logSink) Write(r streamengine.Record) error {
	fmt.Printf("  record: key=%s\n", r.Key)
	return nil
}
func (logSink) Close() error { return nil }

func main() {
	decl, err := streamengine.NewJobBuilder("word-count").
		Source(&counterSource{n: 6}).
		Map(func(r streamengine.Record) (streamengine.Record, error) { return r, nil }).
		Filter(func(r streamengine.Record) bool { return r.Key != "" }).
		KeyBy(func(r streamengine.Record) string { return r.Key }).
		Window(streamengine.TumblingWindow(5 * time.Second)).
		Reduce(func(a, b streamengine.Record) streamengine.Record { return a }).
		Sink(logSink{}).
		Build()
	if err != nil {
		log.Fatalf("build: %v", err)
	}

	graph, err := streamengine.Compile(decl, streamengine.CompileOptions{Parallelism: 4})
	if err != nil {
		log.Fatalf("compile: %v", err)
	}

	fmt.Printf("job:   %s\n", decl.Name())
	fmt.Printf("nodes: %d (%d auto-inserted shuffle)\n",
		graph.NodeCount(), countKind(graph, streamengine.NodeShuffle))
	for _, n := range graph.Nodes() {
		fmt.Printf("  [%d] %-8s upstream=%d\n", n.ID, n.Kind, len(n.Upstream))
	}

	jm := streamengine.NewJobManager()
	id, _ := jm.Submit(context.Background(), graph)
	st, _ := jm.Status(id)
	fmt.Printf("submitted %s -> status: %s\n", id, st)

	// Simulate a backpressure reading on the source output channel.
	mon := streamengine.NewBackpressureMonitor()
	ch := make(chan streamengine.Message, 16)
	mon.Register(graph.Sources()[0].ID, "source output", ch)
	snap := mon.Sample()
	fmt.Printf("backpressure: node %d at %.0f%%\n", snap[0].NodeID, snap[0].Pct)

	if err := jm.Cancel(id); err != nil {
		log.Fatalf("cancel: %v", err)
	}
	st, _ = jm.Status(id)
	fmt.Printf("after cancel: %s\n", st)
}

func countKind(g *streamengine.TaskGraph, k streamengine.NodeKind) int {
	n := 0
	for _, node := range g.Nodes() {
		if node.Kind == k {
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

```text
job:   word-count
nodes: 8 (1 auto-inserted shuffle)
  [0] Source   upstream=0
  [1] Map      upstream=1
  [2] Filter   upstream=1
  [3] Shuffle  upstream=1
  [4] KeyBy    upstream=1
  [5] Window   upstream=1
  [6] Reduce   upstream=1
  [7] Sink     upstream=1
submitted job-1 -> status: Running
backpressure: node 0 at 0%
after cancel: Cancelled
```

### Tests

The tests pin builder validation and the accumulator-error latch, compilation and the auto-shuffle, the manager state machine including a concurrent-submit stress that exercises the `atomic.Uint64` counter and the mutex under load, and backpressure sampling. The example functions are verified against their `// Output:` comments by `go test`.

Create `engine_test.go`:

```go
package streamengine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// nullSource immediately closes both channels. Use it in tests that need a
// valid Source without emitting any data.
type nullSource struct{}

func (nullSource) Open(_ context.Context) (<-chan Message, <-chan error) {
	out := make(chan Message)
	errs := make(chan error)
	go func() { close(out); close(errs) }()
	return out, errs
}

func (nullSource) Close() error { return nil }

// nullSink discards every record. Use it in tests that need a valid Sink.
type nullSink struct{}

func (nullSink) Open(_ context.Context) error { return nil }
func (nullSink) Write(_ Record) error         { return nil }
func (nullSink) Close() error                 { return nil }

// --- JobBuilder validation ---

func TestJobBuilderEmptyName(t *testing.T) {
	t.Parallel()
	_, err := NewJobBuilder("").Build()
	if !errors.Is(err, ErrEmptyJobName) {
		t.Fatalf("err = %v, want ErrEmptyJobName", err)
	}
}

func TestJobBuilderMissingSource(t *testing.T) {
	t.Parallel()
	_, err := NewJobBuilder("test").Sink(nullSink{}).Build()
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("err = %v, want ErrNoSource", err)
	}
}

func TestJobBuilderMissingSink(t *testing.T) {
	t.Parallel()
	_, err := NewJobBuilder("test").Source(nullSource{}).Build()
	if !errors.Is(err, ErrNoSink) {
		t.Fatalf("err = %v, want ErrNoSink", err)
	}
}

func TestJobBuilderDuplicateSource(t *testing.T) {
	t.Parallel()
	_, err := NewJobBuilder("test").
		Source(nullSource{}).
		Source(nullSource{}).
		Sink(nullSink{}).
		Build()
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("err = %v, want ErrDuplicateSource", err)
	}
}

func TestJobBuilderNilSource(t *testing.T) {
	t.Parallel()
	_, err := NewJobBuilder("test").Source(nil).Build()
	if !errors.Is(err, ErrNilSource) {
		t.Fatalf("err = %v, want ErrNilSource", err)
	}
}

func TestJobBuilderWindowBeforeKeyBy(t *testing.T) {
	t.Parallel()
	_, err := NewJobBuilder("test").
		Source(nullSource{}).
		Window(TumblingWindow(time.Minute)).
		Sink(nullSink{}).
		Build()
	if !errors.Is(err, ErrWindowBeforeKey) {
		t.Fatalf("err = %v, want ErrWindowBeforeKey", err)
	}
}

func TestJobBuilderReduceBeforeWindow(t *testing.T) {
	t.Parallel()
	_, err := NewJobBuilder("test").
		Source(nullSource{}).
		KeyBy(func(r Record) string { return r.Key }).
		Reduce(func(a, b Record) Record { return a }).
		Sink(nullSink{}).
		Build()
	if !errors.Is(err, ErrReduceBeforeWin) {
		t.Fatalf("err = %v, want ErrReduceBeforeWin", err)
	}
}

// TestJobBuilderFirstErrorWins verifies the accumulator-error pattern: the
// first error is returned even if subsequent method calls would also fail.
func TestJobBuilderFirstErrorWins(t *testing.T) {
	t.Parallel()
	_, err := NewJobBuilder("").Source(nil).Sink(nil).Build()
	if !errors.Is(err, ErrEmptyJobName) {
		t.Fatalf("err = %v, want ErrEmptyJobName (first error wins)", err)
	}
}

func TestJobBuilderValidPipeline(t *testing.T) {
	t.Parallel()
	decl, err := NewJobBuilder("my-job").
		Source(nullSource{}).
		Map(func(r Record) (Record, error) { return r, nil }).
		Sink(nullSink{}).
		Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if decl.Name() != "my-job" {
		t.Fatalf("Name = %q, want %q", decl.Name(), "my-job")
	}
	// Source + Map + Sink = 3 stages.
	if decl.StageCount() != 3 {
		t.Fatalf("StageCount = %d, want 3", decl.StageCount())
	}
}

// --- Compile ---

func TestCompileBasicPipeline(t *testing.T) {
	t.Parallel()
	decl, err := NewJobBuilder("basic").
		Source(nullSource{}).
		Map(func(r Record) (Record, error) { return r, nil }).
		Sink(nullSink{}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	g, err := Compile(decl, CompileOptions{Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 3 stages, no KeyBy -> no Shuffle inserted -> 3 nodes.
	if g.NodeCount() != 3 {
		t.Fatalf("NodeCount = %d, want 3", g.NodeCount())
	}
	if len(g.Sources()) != 1 {
		t.Fatalf("Sources = %d, want 1", len(g.Sources()))
	}
	if len(g.Sinks()) != 1 {
		t.Fatalf("Sinks = %d, want 1", len(g.Sinks()))
	}
}

// TestCompileInsertsShuffle checks that a Shuffle is auto-inserted before the
// first KeyBy when the upstream is not already keyed.
// Pipeline: Source, Map, KeyBy, Window, Reduce, Sink (6 stages)
// Compiled:  Source, Map, Shuffle(auto), KeyBy, Window, Reduce, Sink (7 nodes)
func TestCompileInsertsShuffle(t *testing.T) {
	t.Parallel()
	decl, err := NewJobBuilder("keyed").
		Source(nullSource{}).
		Map(func(r Record) (Record, error) { return r, nil }).
		KeyBy(func(r Record) string { return r.Key }).
		Window(TumblingWindow(time.Minute)).
		Reduce(func(a, b Record) Record { return a }).
		Sink(nullSink{}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	g, err := Compile(decl, CompileOptions{Parallelism: 4})
	if err != nil {
		t.Fatal(err)
	}
	if g.NodeCount() != 7 {
		t.Fatalf("NodeCount = %d, want 7 (auto-inserted Shuffle)", g.NodeCount())
	}
	shuffles := 0
	for _, n := range g.Nodes() {
		if n.Kind == NodeShuffle {
			shuffles++
		}
	}
	if shuffles != 1 {
		t.Fatalf("Shuffle count = %d, want 1", shuffles)
	}
}

func TestCompileUpstreamLinks(t *testing.T) {
	t.Parallel()
	decl, err := NewJobBuilder("links").
		Source(nullSource{}).
		Map(func(r Record) (Record, error) { return r, nil }).
		Sink(nullSink{}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	g, _ := Compile(decl, CompileOptions{Parallelism: 1})
	nodes := g.Nodes()
	// nodes[0]=Source (no upstream), nodes[1]=Map (upstream=[Source]), nodes[2]=Sink (upstream=[Map]).
	if len(nodes[0].Upstream) != 0 {
		t.Fatalf("Source upstream = %d, want 0", len(nodes[0].Upstream))
	}
	if len(nodes[1].Upstream) != 1 || nodes[1].Upstream[0] != nodes[0] {
		t.Fatalf("Map upstream wrong: %v", nodes[1].Upstream)
	}
	if len(nodes[2].Upstream) != 1 || nodes[2].Upstream[0] != nodes[1] {
		t.Fatalf("Sink upstream wrong: %v", nodes[2].Upstream)
	}
}

func TestCompileBadParallelism(t *testing.T) {
	t.Parallel()
	decl, err := NewJobBuilder("test").
		Source(nullSource{}).
		Sink(nullSink{}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []int{0, -1, -100} {
		_, err := Compile(decl, CompileOptions{Parallelism: p})
		if !errors.Is(err, ErrBadParallelism) {
			t.Errorf("Parallelism=%d: err = %v, want ErrBadParallelism", p, err)
		}
	}
}

// --- JobManager ---

func TestJobManagerSubmitAndStatus(t *testing.T) {
	t.Parallel()
	decl, _ := NewJobBuilder("test").Source(nullSource{}).Sink(nullSink{}).Build()
	g, _ := Compile(decl, CompileOptions{Parallelism: 1})
	jm := NewJobManager()
	id, err := jm.Submit(context.Background(), g)
	if err != nil {
		t.Fatalf("Submit error = %v", err)
	}
	st, err := jm.Status(id)
	if err != nil {
		t.Fatalf("Status error = %v", err)
	}
	if st != JobRunning {
		t.Fatalf("status = %v, want Running", st)
	}
}

func TestJobManagerSubmitAssignsUniqueIDs(t *testing.T) {
	t.Parallel()
	decl, _ := NewJobBuilder("test").Source(nullSource{}).Sink(nullSink{}).Build()
	g, _ := Compile(decl, CompileOptions{Parallelism: 1})
	jm := NewJobManager()
	ids := make(map[string]bool)
	for i := 0; i < 20; i++ {
		id, _ := jm.Submit(context.Background(), g)
		if ids[id] {
			t.Fatalf("duplicate job ID: %s", id)
		}
		ids[id] = true
	}
}

func TestJobManagerCancel(t *testing.T) {
	t.Parallel()
	decl, _ := NewJobBuilder("test").Source(nullSource{}).Sink(nullSink{}).Build()
	g, _ := Compile(decl, CompileOptions{Parallelism: 1})
	jm := NewJobManager()
	id, _ := jm.Submit(context.Background(), g)
	if err := jm.Cancel(id); err != nil {
		t.Fatalf("Cancel error = %v", err)
	}
	st, _ := jm.Status(id)
	if st != JobCancelled {
		t.Fatalf("status = %v, want Cancelled", st)
	}
}

func TestJobManagerCancelAlreadyCancelled(t *testing.T) {
	t.Parallel()
	decl, _ := NewJobBuilder("test").Source(nullSource{}).Sink(nullSink{}).Build()
	g, _ := Compile(decl, CompileOptions{Parallelism: 1})
	jm := NewJobManager()
	id, _ := jm.Submit(context.Background(), g)
	_ = jm.Cancel(id)
	if err := jm.Cancel(id); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("second Cancel: err = %v, want ErrJobNotRunning", err)
	}
}

func TestJobManagerUnknownJob(t *testing.T) {
	t.Parallel()
	jm := NewJobManager()
	if _, err := jm.Status("nonexistent"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Status: err = %v, want ErrJobNotFound", err)
	}
	if err := jm.Cancel("nonexistent"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Cancel: err = %v, want ErrJobNotFound", err)
	}
}

// TestJobManagerConcurrentSubmit submits 100 jobs from 10 concurrent goroutines
// and asserts that every returned ID is unique and every Status call returns
// JobRunning. This pins the atomic.Uint64 counter and the mutex under load.
func TestJobManagerConcurrentSubmit(t *testing.T) {
	t.Parallel()
	decl, _ := NewJobBuilder("test").Source(nullSource{}).Sink(nullSink{}).Build()
	g, _ := Compile(decl, CompileOptions{Parallelism: 1})
	jm := NewJobManager()

	const (
		workers = 10
		each    = 10
	)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := make(map[string]bool, workers*each)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				id, err := jm.Submit(context.Background(), g)
				if err != nil {
					t.Errorf("Submit error = %v", err)
					return
				}
				st, err := jm.Status(id)
				if err != nil || st != JobRunning {
					t.Errorf("Status(%s) = %v, %v; want Running, nil", id, st, err)
				}
				mu.Lock()
				if ids[id] {
					t.Errorf("duplicate job ID: %s", id)
				}
				ids[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(ids) != workers*each {
		t.Fatalf("unique IDs = %d, want %d", len(ids), workers*each)
	}
}

// --- BackpressureMonitor ---

func TestBackpressureMonitorSample(t *testing.T) {
	t.Parallel()
	m := NewBackpressureMonitor()
	ch := make(chan Message, 10)
	m.Register(1, "source->map", ch)
	// Fill exactly half the buffer to produce a 50% utilisation reading.
	for i := 0; i < 5; i++ {
		r := Record{Key: "k"}
		ch <- Message{Type: MsgRecord, Record: &r}
	}
	metrics := m.Sample()
	if len(metrics) != 1 {
		t.Fatalf("metrics len = %d, want 1", len(metrics))
	}
	got := metrics[0]
	if got.NodeID != 1 {
		t.Fatalf("NodeID = %d, want 1", got.NodeID)
	}
	if got.Len != 5 || got.Cap != 10 {
		t.Fatalf("Len=%d Cap=%d, want Len=5 Cap=10", got.Len, got.Cap)
	}
	if got.Pct != 50.0 {
		t.Fatalf("Pct = %g, want 50.0", got.Pct)
	}
}

func TestBackpressureMonitorEmptyChannel(t *testing.T) {
	t.Parallel()
	m := NewBackpressureMonitor()
	ch := make(chan Message, 8)
	m.Register(2, "map->sink", ch)
	metrics := m.Sample()
	if metrics[0].Pct != 0.0 {
		t.Fatalf("Pct = %g, want 0.0", metrics[0].Pct)
	}
}

func TestBackpressureMonitorMultipleChannels(t *testing.T) {
	t.Parallel()
	m := NewBackpressureMonitor()
	ch1 := make(chan Message, 4)
	ch2 := make(chan Message, 8)
	m.Register(1, "a->b", ch1)
	m.Register(2, "b->c", ch2)
	// Fill ch1 completely; leave ch2 empty.
	for i := 0; i < 4; i++ {
		r := Record{Key: "k"}
		ch1 <- Message{Type: MsgRecord, Record: &r}
	}
	metrics := m.Sample()
	if len(metrics) != 2 {
		t.Fatalf("metrics len = %d, want 2", len(metrics))
	}
	if metrics[0].Pct != 100.0 {
		t.Fatalf("ch1 Pct = %g, want 100.0", metrics[0].Pct)
	}
	if metrics[1].Pct != 0.0 {
		t.Fatalf("ch2 Pct = %g, want 0.0", metrics[1].Pct)
	}
}

// --- Example functions (auto-verified by go test) ---

func ExampleTumblingWindow() {
	spec := TumblingWindow(5 * time.Minute)
	fmt.Printf("kind=%d size=%s\n", spec.Kind, spec.Size)
	// Output: kind=0 size=5m0s
}

func ExampleNewJobBuilder() {
	_, err := NewJobBuilder("").Build()
	fmt.Println(err)
	// Output: streamengine: job name must not be empty
}

func ExampleJobStatus_String() {
	fmt.Println(JobRunning.String())
	fmt.Println(JobCancelled.String())
	// Output:
	// Running
	// Cancelled
}
```

## Review

The control plane is correct when the builder latches the first error, the compiler inserts exactly one shuffle before an unkeyed `KeyBy`, the manager serialises every status write, and the monitor never blocks a pipeline goroutine. The two mistakes most likely to bite are checking for the wrong builder error (the latch keeps the *first* error, so `NewJobBuilder("").Source(nil)` reports the empty-name error, which `TestJobBuilderFirstErrorWins` pins) and assuming `KeyBy` after `KeyBy` re-shuffles unconditionally — it does not, because the `keyed` flag is already set. Confirm correctness by reading the compiled topology in the demo output: the `Shuffle` node sits at index 3, directly before `KeyBy`, and `Map`/`Filter` before it carry no shuffle. Run the suite with `-race`; `TestJobManagerConcurrentSubmit` is the one that would expose a missing lock around the jobs map or a non-atomic counter.

## Resources

- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical fan-in/fan-out pattern with WaitGroup and context cancellation.
- [Apache Flink architecture](https://nightlies.apache.org/flink/flink-docs-stable/docs/concepts/flink-architecture/) — reference for job-graph compilation, task scheduling, and checkpoint coordination in a production engine.
- [pkg.go.dev/sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Uint64` and the memory-ordering guarantees used by `JobManager.counter`.
- [pkg.go.dev/context](https://pkg.go.dev/context) — `WithCancel`, the derived-context pattern, and the `CancelFunc` contract.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-runnable-windowed-engine.md](02-runnable-windowed-engine.md)
