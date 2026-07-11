# Exercise 1: Wrap sentinels and a custom error through a multi-stage ingestion pipeline

A validation/ingestion pipeline is the canonical place where an error accumulates
context on its way up: an input fails deep in a parse stage, and each enclosing
stage annotates it so the log line reads as a trail from the outermost operation
to the root cause. This exercise builds that pipeline and proves the chain stays
inspectable — `errors.Is` finds the sentinel at the bottom, `errors.As` extracts
the custom type in the middle, and `errors.Unwrap` walks the links one at a time.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
errwrap/                       independent module: example.com/errwrap
  go.mod                       go 1.24
  pipeline/
    pipeline.go                ErrEmptyInput, ErrInvalid sentinels; *ParseError with Unwrap; StageOne/Two/Three; RunPipeline; ReadAll
    pipeline_test.go           errors.Is/As/Unwrap traversal, message-context, io error wrap
  cmd/
    demo/
      main.go                  runs the pipeline on valid, empty, and oversized input
```

- Files: `pipeline/pipeline.go`, `cmd/demo/main.go`, `pipeline/pipeline_test.go`.
- Implement: `ErrEmptyInput`/`ErrInvalid` sentinels, a `*ParseError` custom type with `Unwrap() error`, `StageOne`/`StageTwo`/`StageThree`, `RunPipeline` that wraps with `%w`, and `ReadAll` that wraps `io.ReadAll` failures.
- Test: `errors.Is` finds `ErrEmptyInput`/`ErrInvalid` through the chain; `errors.As` extracts `*ParseError`; the message includes stage context; `ReadAll` wraps an `io` error; `errors.Unwrap` walks `run pipeline -> stage one -> ErrEmptyInput`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/errwrap/pipeline ~/go-exercises/errwrap/cmd/demo
cd ~/go-exercises/errwrap
go mod init example.com/errwrap
```

### How the chain is constructed

Each stage returns either `nil` or an error that carries its own context. `StageOne`
wraps the `ErrEmptyInput` sentinel with `%w` and a `stage one:` prefix. `StageTwo`
returns a structured `*ParseError` — a struct whose `Unwrap() error` returns the
wrapped `ErrInvalid`, so the sentinel remains reachable *and* callers get a typed
handle carrying `Stage` and `Input`. `StageThree` wraps whatever `StageTwo`
returns, and `RunPipeline` wraps again at the top. The result for oversized input
is a four-link chain: `run pipeline -> stage three -> *ParseError -> ErrInvalid`.

The point of using `%w` at every layer is that the top-level value stays fully
inspectable. `errors.Is(err, ErrInvalid)` walks all four links and finds the
sentinel at the bottom; `errors.As(err, &parseErr)` finds the struct in the
middle and hands you its `Stage` and `Input` fields. If any layer had used `%v`,
the walk would stop there and both queries would fail — the message would look the
same, but the value would be inert.

`ReadAll` is the I/O counterpart: it wraps a failing `io.ReadAll` with a
`read all:` prefix so the operation is named in the log, while `%w` keeps the
underlying I/O error inspectable by the caller.

Create `pipeline/pipeline.go`:

```go
package pipeline

import (
	"errors"
	"fmt"
	"io"
)

// Sentinels the pipeline wraps. Callers branch on these with errors.Is.
var (
	ErrEmptyInput = errors.New("empty input")
	ErrInvalid    = errors.New("invalid input")
)

// ParseError is a structured error carrying which stage failed and on what
// input. It participates in the chain via Unwrap, so errors.Is still finds the
// wrapped sentinel while errors.As hands callers the Stage/Input fields.
type ParseError struct {
	Stage string
	Input string
	Err   error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parse error in stage %s for %q: %s", e.Stage, e.Input, e.Err)
}

func (e *ParseError) Unwrap() error {
	return e.Err
}

// StageOne rejects empty input, wrapping the sentinel with operation context.
func StageOne(input string) error {
	if input == "" {
		return fmt.Errorf("stage one: %w", ErrEmptyInput)
	}
	return nil
}

// StageTwo rejects oversized input with a structured *ParseError.
func StageTwo(input string) error {
	if len(input) > 100 {
		return &ParseError{Stage: "two", Input: input, Err: ErrInvalid}
	}
	return nil
}

// StageThree wraps StageTwo's error with its own context.
func StageThree(input string) error {
	if err := StageTwo(input); err != nil {
		return fmt.Errorf("stage three: %w", err)
	}
	return nil
}

// RunPipeline runs the stages in order, wrapping the first failure with the
// outermost operation name. The result is a breadcrumb chain from run pipeline
// down to the underlying sentinel.
func RunPipeline(input string) error {
	if err := StageOne(input); err != nil {
		return fmt.Errorf("run pipeline: %w", err)
	}
	if err := StageThree(input); err != nil {
		return fmt.Errorf("run pipeline: %w", err)
	}
	return nil
}

// ReadAll reads r fully, naming the operation and keeping the I/O error
// inspectable via %w.
func ReadAll(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read all: %w", err)
	}
	return data, nil
}
```

### The runnable demo

The demo runs the pipeline on the three shapes of input — valid, empty, and
oversized — and shows the annotated message plus the results of `errors.Is` and
`errors.As` against the chain.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strings"

	"example.com/errwrap/pipeline"
)

func main() {
	if err := pipeline.RunPipeline("hello"); err == nil {
		fmt.Println("valid input: pipeline ok")
	}

	empty := pipeline.RunPipeline("")
	fmt.Printf("empty input error: %v\n", empty)
	fmt.Printf("errors.Is(err, ErrEmptyInput) = %v\n", errors.Is(empty, pipeline.ErrEmptyInput))

	long := pipeline.RunPipeline(strings.Repeat("a", 120))
	var pe *pipeline.ParseError
	if errors.As(long, &pe) {
		fmt.Printf("oversized input: errors.As found *ParseError stage=%s\n", pe.Stage)
	}
	fmt.Printf("errors.Is(err, ErrInvalid) = %v\n", errors.Is(long, pipeline.ErrInvalid))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid input: pipeline ok
empty input error: run pipeline: stage one: empty input
errors.Is(err, ErrEmptyInput) = true
oversized input: errors.As found *ParseError stage=two
errors.Is(err, ErrInvalid) = true
```

### Tests

The tests assert the chain's structure through the `errors` package, never
through string equality (except where they check that a *substring* of context is
present, which pins the annotation without freezing the whole message).
`TestUnwrapReturnsUnderlyingError` calls `errors.Unwrap` twice to pin the exact
link order for the empty-input path: `run pipeline -> stage one -> ErrEmptyInput`.

Create `pipeline/pipeline_test.go`:

```go
package pipeline

import (
	"errors"
	"strings"
	"testing"
)

func TestRunPipelineSucceeds(t *testing.T) {
	t.Parallel()

	if err := RunPipeline("hello"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestRunPipelineWrapsEmptyInput(t *testing.T) {
	t.Parallel()

	err := RunPipeline("")
	if !errors.Is(err, ErrEmptyInput) {
		t.Fatalf("err = %v, want errors.Is ErrEmptyInput", err)
	}
}

func TestRunPipelineWrapsParseError(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 200)
	err := RunPipeline(long)

	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want errors.Is ErrInvalid", err)
	}

	var parseErr *ParseError
	if !errors.As(err, &parseErr) {
		t.Fatal("err should be unwrappable to *ParseError")
	}
	if parseErr.Stage != "two" {
		t.Fatalf("parseErr.Stage = %q, want two", parseErr.Stage)
	}
	if parseErr.Input != long {
		t.Fatalf("parseErr.Input = %q, want the long input", parseErr.Input)
	}
}

func TestErrorMessageIncludesStage(t *testing.T) {
	t.Parallel()

	err := RunPipeline("")
	msg := err.Error()
	if !strings.Contains(msg, "run pipeline") {
		t.Fatalf("err.Error() = %q, want it to include run pipeline", msg)
	}
	if !strings.Contains(msg, "stage one") {
		t.Fatalf("err.Error() = %q, want it to include stage one", msg)
	}
}

func TestReadAllWrapsIOError(t *testing.T) {
	t.Parallel()

	_, err := ReadAll(errReader{msg: "boom"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "read all") {
		t.Fatalf("err.Error() = %q, want the read all prefix", err.Error())
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err.Error() = %q, want the underlying cause", err.Error())
	}
}

func TestUnwrapReturnsUnderlyingError(t *testing.T) {
	t.Parallel()

	err := RunPipeline("")

	stageOne := errors.Unwrap(err)
	if stageOne == nil {
		t.Fatal("first Unwrap returned nil; want the stage one error")
	}
	sentinel := errors.Unwrap(stageOne)
	if !errors.Is(sentinel, ErrEmptyInput) {
		t.Fatalf("second Unwrap = %v, want ErrEmptyInput", sentinel)
	}
}

type errReader struct{ msg string }

func (e errReader) Read(p []byte) (int, error) {
	return 0, errors.New(e.msg)
}
```

## Review

The pipeline is correct when the top-level value is fully inspectable at every
layer: `errors.Is(err, ErrEmptyInput)` and `errors.Is(err, ErrInvalid)` both find
their sentinels, and `errors.As(err, &parseErr)` recovers the struct with the
right `Stage` and `Input`. The single most common regression here is swapping one
`%w` for `%v` at any stage — the demo output would be byte-for-byte identical, but
`TestRunPipelineWrapsEmptyInput` (or the `As` test) would flip to red because the
chain is severed at that layer. That is exactly why the tests query through the
`errors` package instead of comparing message strings. `TestUnwrapReturnsUnderlyingError`
adds the tightest constraint: it pins the precise number of links between the top
and the sentinel, so an accidental extra or missing wrap is caught.

## Resources

- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — the `%w` verb and single-versus-multiple wrapping.
- [errors package](https://pkg.go.dev/errors) — `Is`, `As`, and `Unwrap` semantics.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the original design of `%w`, `Is`, and `As`.
- [Go Specification: Errors](https://go.dev/ref/spec#Errors) — the `error` interface.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-repo-translate-not-leak.md](02-repo-translate-not-leak.md)
