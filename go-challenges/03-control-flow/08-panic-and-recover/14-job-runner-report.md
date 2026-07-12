# Exercise 14: Job Runner: Recording Panicking Jobs and Continuing the Run

**Nivel: Intermedio** — validacion rapida (un test corto).

A startup health-check pass or a nightly maintenance run is a short list of
named, independent jobs — warm a cache, flush a queue, compact an index,
rotate logs. One job panicking (a bad assumption about disk state, an
unhandled edge case) must not abort the jobs after it; the operator needs a
report of exactly which jobs succeeded, which returned an error, and which
panicked. This module builds `RunAll`, which runs every job in order,
isolates each one with its own recover, and returns a per-job `Result` slice
plus a `Summary` tally.

## What you'll build

```text
jobrunner/                  independent module: example.com/jobrunner
  go.mod                    go 1.24
  jobrunner.go               Status, Job, Result, RunAll, Summary
  jobrunner_test.go           mixed outcomes, order preserved, tallies, empty run
```

Files: `jobrunner.go`, `jobrunner_test.go`.
Implement: `RunAll(jobs []Job) []Result` that runs each `Job` in order and isolates its panic with a per-job `defer`/`recover`; `Summary(results []Result) (ok, failed, panicked int)`.
Test: five jobs — one clean, one returning an error, one panicking with an error, one panicking with a string, one clean again — all run in order despite the two panics, with statuses and a wrapped sentinel error asserted via `errors.Is`; `Summary` tallies a mixed run correctly; an empty job list returns an empty result slice.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/14-job-runner-report
cd go-solutions/03-control-flow/08-panic-and-recover/14-job-runner-report
go mod edit -go=1.24
```

Create `jobrunner.go`:

```go
package jobrunner

import "fmt"

// Status classifies how a Job finished.
type Status string

const (
	StatusOK       Status = "ok"
	StatusFailed   Status = "failed"
	StatusPanicked Status = "panicked"
)

// Job is one named unit of work — a maintenance task, a startup check, a
// migration step — run as part of a larger sequence where one bad job must
// not stop the rest from being attempted and reported on.
type Job struct {
	Name string
	Run  func() error
}

// Result records the outcome of a single Job.
type Result struct {
	Name   string
	Status Status
	Err    error
}

// RunAll runs every job in order, in the current goroutine, isolating each
// one so a panic in job 3 still lets jobs 4..N run. The returned slice
// preserves job order regardless of which jobs failed or panicked.
func RunAll(jobs []Job) []Result {
	results := make([]Result, 0, len(jobs))
	for _, j := range jobs {
		results = append(results, runOne(j))
	}
	return results
}

// runOne is the recover boundary: exactly one job's worth of untrusted logic.
func runOne(j Job) (result Result) {
	result = Result{Name: j.Name, Status: StatusOK}
	defer func() {
		if r := recover(); r != nil {
			result.Status = StatusPanicked
			if err, ok := r.(error); ok {
				result.Err = fmt.Errorf("job %q panicked: %w", j.Name, err)
			} else {
				result.Err = fmt.Errorf("job %q panicked: %v", j.Name, r)
			}
		}
	}()

	if err := j.Run(); err != nil {
		result.Status = StatusFailed
		result.Err = err
	}
	return result
}

// Summary tallies results by status, for a one-line end-of-run report.
func Summary(results []Result) (ok, failed, panicked int) {
	for _, r := range results {
		switch r.Status {
		case StatusOK:
			ok++
		case StatusFailed:
			failed++
		case StatusPanicked:
			panicked++
		}
	}
	return ok, failed, panicked
}
```

Create `jobrunner_test.go`:

```go
package jobrunner

import (
	"errors"
	"testing"
)

func TestRunAllIsolatesEachJob(t *testing.T) {
	sentinel := errors.New("disk full")
	var ran []string

	jobs := []Job{
		{Name: "warm-cache", Run: func() error {
			ran = append(ran, "warm-cache")
			return nil
		}},
		{Name: "flush-queue", Run: func() error {
			ran = append(ran, "flush-queue")
			return sentinel
		}},
		{Name: "compact-index", Run: func() error {
			ran = append(ran, "compact-index")
			panic(sentinel)
		}},
		{Name: "rotate-logs", Run: func() error {
			ran = append(ran, "rotate-logs")
			panic("out of memory")
		}},
		{Name: "final-check", Run: func() error {
			ran = append(ran, "final-check")
			return nil
		}},
	}

	results := RunAll(jobs)

	if len(results) != len(jobs) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(jobs))
	}
	if len(ran) != len(jobs) {
		t.Fatalf("only %d/%d jobs ran; a panic must not stop the remaining jobs", len(ran), len(jobs))
	}
	wantOrder := []string{"warm-cache", "flush-queue", "compact-index", "rotate-logs", "final-check"}
	for i, name := range wantOrder {
		if ran[i] != name {
			t.Fatalf("ran[%d] = %q, want %q: job order must be preserved", i, ran[i], name)
		}
	}

	wantStatus := map[string]Status{
		"warm-cache":    StatusOK,
		"flush-queue":   StatusFailed,
		"compact-index": StatusPanicked,
		"rotate-logs":   StatusPanicked,
		"final-check":   StatusOK,
	}
	for _, r := range results {
		if r.Status != wantStatus[r.Name] {
			t.Errorf("job %q status = %v, want %v", r.Name, r.Status, wantStatus[r.Name])
		}
	}

	var compact Result
	for _, r := range results {
		if r.Name == "compact-index" {
			compact = r
		}
	}
	if !errors.Is(compact.Err, sentinel) {
		t.Fatalf("compact-index error %v does not wrap the sentinel", compact.Err)
	}
}

func TestSummaryTallies(t *testing.T) {
	jobs := []Job{
		{Name: "a", Run: func() error { return nil }},
		{Name: "b", Run: func() error { return errors.New("nope") }},
		{Name: "c", Run: func() error { panic("boom") }},
		{Name: "d", Run: func() error { return nil }},
	}
	results := RunAll(jobs)
	ok, failed, panicked := Summary(results)

	if ok != 2 || failed != 1 || panicked != 1 {
		t.Fatalf("Summary = (ok=%d failed=%d panicked=%d), want (2, 1, 1)", ok, failed, panicked)
	}
}

func TestRunAllEmpty(t *testing.T) {
	results := RunAll(nil)
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(results))
	}
}
```

## Review

`RunAll` is correct when a panic in any one job never reduces the number of
jobs attempted: the test asserts all five jobs ran, in order, by recording
names as a side effect independent of the `Result` slice. The recover lives
in `runOne`, one job wide — the same discipline as isolating one batch item
or one plugin call — so it is re-armed by the surrounding loop on every
iteration rather than being consumed by the first panic. `Result.Err` wraps a
panicking job's error with `%w`, not just `%v`, which is why `errors.Is`
still reaches the sentinel through the wrapping `fmt.Errorf`; a bare `%v`
would have flattened it to an unrecoverable string. `Summary` is deliberately
separate from `RunAll`: the runner's job is to isolate and record, not to
decide what a "successful run" means — that policy (is one panic tolerable?
are failures fatal?) belongs to whatever calls `RunAll` and reads the tally.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-iteration recover pattern this reuses from a single item to a whole job list.
- [fmt: %w verb via Errorf](https://pkg.go.dev/fmt#Errorf) — wrapping a recovered panic's error so `errors.Is`/`errors.As` still reach it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-json-path-extractor.md](13-json-path-extractor.md) | Next: [15-connection-pool-worker-isolation.md](15-connection-pool-worker-isolation.md)
