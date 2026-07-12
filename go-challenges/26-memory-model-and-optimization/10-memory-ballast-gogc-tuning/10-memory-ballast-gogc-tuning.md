# 10. Memory Ballast and GOGC Tuning

Go's garbage collector exposes two practical knobs: `GOGC`, which chooses the CPU-versus-memory trade-off, and `GOMEMLIMIT`, which gives the runtime a soft memory target. This lesson builds a small library that computes safe runtime settings for a service and contrasts that with the older ballast technique without requiring tests to mutate global GC state.

```text
gctune/
  go.mod
  tune.go
  tune_test.go
  cmd/demo/main.go
```

## Concepts

### GOGC Chooses A Trade-Off

`GOGC` controls when the next collection cycle should begin. A larger value lets the heap grow more between collections, which usually reduces GC CPU work but increases peak memory. A smaller value collects more often, which can reduce memory but spend more CPU in GC work. The default is `100`; programmatic control uses `runtime/debug.SetGCPercent`.

### GOMEMLIMIT Is A Soft Runtime Memory Target

`GOMEMLIMIT` and `debug.SetMemoryLimit` tell the Go runtime to try to keep runtime-managed memory under a target. The limit is soft, not a hard OS-enforced ceiling. The runtime may run GC more often and return memory to the operating system more aggressively, but the application can still exceed the target briefly or slow down if the limit is set too low.

### Ballasts Are Historical, Not A Default Design

A ballast is a large allocation kept alive to make the heap appear larger so the GC runs less frequently. That approach was common before Go had a soft memory limit. It can distort memory accounting and wastes address space. Prefer `GOMEMLIMIT` for modern services because it expresses the actual operational constraint directly.

### Tests Should Not Fight Global Runtime State

Changing GC settings is process-global. Parallel tests that call `debug.SetGCPercent` or `debug.SetMemoryLimit` can interfere with each other and with the test runner. The library below separates planning from applying: tests verify deterministic plans and environment strings, while `Apply` exists for application startup code that intentionally owns the process configuration.

## Exercises

This is a library package. The demo is a separate program that imports the package through its public API.

### Exercise 1: Build A GC Tuning Planner

Create `tune.go`:

```go
package gctune

import (
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"strconv"
)

var (
	ErrInvalidContainerLimit = errors.New("container limit must be positive")
	ErrInvalidHeadroom       = errors.New("headroom must be between 0 and 0.5")
	ErrInvalidGCPercent      = errors.New("gc percent must be -1 or non-negative")
	ErrInvalidMemoryLimit    = errors.New("memory limit must be positive")
	ErrInvalidBallast        = errors.New("ballast bytes must not be negative")
)

type Config struct {
	GCPercent       int
	MemoryLimitByte int64
	BallastByte     int64
}

type Plan struct {
	GCPercent       int
	MemoryLimitByte int64
	BallastByte     int64
	Env             map[string]string
}

func NewPlan(containerLimitByte int64, headroom float64, gcPercent int) (Plan, error) {
	if containerLimitByte <= 0 {
		return Plan{}, fmt.Errorf("gctune: %w: got %d", ErrInvalidContainerLimit, containerLimitByte)
	}
	if headroom < 0 || headroom > 0.5 {
		return Plan{}, fmt.Errorf("gctune: %w: got %.2f", ErrInvalidHeadroom, headroom)
	}
	if gcPercent < -1 {
		return Plan{}, fmt.Errorf("gctune: %w: got %d", ErrInvalidGCPercent, gcPercent)
	}

	limit := int64(float64(containerLimitByte) * (1 - headroom))
	if limit <= 0 {
		return Plan{}, fmt.Errorf("gctune: %w: got %d", ErrInvalidMemoryLimit, limit)
	}

	plan := Plan{
		GCPercent:       gcPercent,
		MemoryLimitByte: limit,
		Env: map[string]string{
			"GOGC":       formatGCPercent(gcPercent),
			"GOMEMLIMIT": strconv.FormatInt(limit, 10),
		},
	}
	return plan, nil
}

func WithBallast(plan Plan, ballastByte int64) (Plan, error) {
	if ballastByte < 0 {
		return Plan{}, fmt.Errorf("gctune: %w: got %d", ErrInvalidBallast, ballastByte)
	}
	plan.BallastByte = ballastByte
	return plan, nil
}

func Apply(cfg Config) (restore func(), err error) {
	if cfg.GCPercent < -1 {
		return nil, fmt.Errorf("gctune: %w: got %d", ErrInvalidGCPercent, cfg.GCPercent)
	}
	if cfg.MemoryLimitByte <= 0 {
		return nil, fmt.Errorf("gctune: %w: got %d", ErrInvalidMemoryLimit, cfg.MemoryLimitByte)
	}
	if cfg.BallastByte < 0 {
		return nil, fmt.Errorf("gctune: %w: got %d", ErrInvalidBallast, cfg.BallastByte)
	}

	previousGC := debug.SetGCPercent(cfg.GCPercent)
	previousLimit := debug.SetMemoryLimit(cfg.MemoryLimitByte)
	return func() {
		debug.SetGCPercent(previousGC)
		debug.SetMemoryLimit(previousLimit)
	}, nil
}

func DisabledLimit() int64 {
	return math.MaxInt64
}

func formatGCPercent(percent int) string {
	if percent < 0 {
		return "off"
	}
	return strconv.Itoa(percent)
}
```

`NewPlan` computes the environment settings you would pass to a service. `Apply` exists for startup code, but the tests below avoid calling it because it mutates process-wide GC settings.

### Exercise 2: Test Plans And Sentinel Validation Errors

Create `tune_test.go`:

```go
package gctune

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewPlanComputesEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		container int64
		headroom  float64
		gcPercent int
		wantLimit int64
		wantGOGC  string
	}{
		{name: "default gc", container: 1 << 30, headroom: 0.10, gcPercent: 100, wantLimit: 966367641, wantGOGC: "100"},
		{name: "gc off", container: 512 << 20, headroom: 0.25, gcPercent: -1, wantLimit: 402653184, wantGOGC: "off"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plan, err := NewPlan(tt.container, tt.headroom, tt.gcPercent)
			if err != nil {
				t.Fatal(err)
			}
			if plan.MemoryLimitByte != tt.wantLimit {
				t.Fatalf("MemoryLimitByte = %d, want %d", plan.MemoryLimitByte, tt.wantLimit)
			}
			if plan.Env["GOGC"] != tt.wantGOGC {
				t.Fatalf("GOGC = %q, want %q", plan.Env["GOGC"], tt.wantGOGC)
			}
			if plan.Env["GOMEMLIMIT"] == "" {
				t.Fatal("GOMEMLIMIT should be set")
			}
		})
	}
}

func TestNewPlanRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		container int64
		headroom  float64
		gcPercent int
		want      error
	}{
		{name: "container", container: 0, headroom: 0.1, gcPercent: 100, want: ErrInvalidContainerLimit},
		{name: "negative headroom", container: 1, headroom: -0.1, gcPercent: 100, want: ErrInvalidHeadroom},
		{name: "large headroom", container: 1, headroom: 0.75, gcPercent: 100, want: ErrInvalidHeadroom},
		{name: "gc percent", container: 1, headroom: 0.1, gcPercent: -2, want: ErrInvalidGCPercent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewPlan(tt.container, tt.headroom, tt.gcPercent)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestWithBallast(t *testing.T) {
	t.Parallel()

	plan, err := NewPlan(1<<30, 0.1, 100)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = WithBallast(plan, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if plan.BallastByte != 64<<20 {
		t.Fatalf("BallastByte = %d, want %d", plan.BallastByte, 64<<20)
	}
	if _, err := WithBallast(plan, -1); !errors.Is(err, ErrInvalidBallast) {
		t.Fatalf("err = %v, want ErrInvalidBallast", err)
	}
}

func TestApplyRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want error
	}{
		{name: "gc", cfg: Config{GCPercent: -2, MemoryLimitByte: 1}, want: ErrInvalidGCPercent},
		{name: "limit", cfg: Config{GCPercent: 100, MemoryLimitByte: 0}, want: ErrInvalidMemoryLimit},
		{name: "ballast", cfg: Config{GCPercent: 100, MemoryLimitByte: 1, BallastByte: -1}, want: ErrInvalidBallast},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Apply(tt.cfg)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func ExampleNewPlan() {
	plan, _ := NewPlan(512<<20, 0.10, 100)
	fmt.Printf("GOGC=%s GOMEMLIMIT=%s\n", plan.Env["GOGC"], plan.Env["GOMEMLIMIT"])
	// Output: GOGC=100 GOMEMLIMIT=483183820
}
```

The table-driven tests assert wrapped validation errors with `errors.Is`. They do not depend on the machine's current GC counters.

### Exercise 3: Add A Demo That Prints A Startup Plan

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"gctune"
)

func main() {
	plan, err := gctune.NewPlan(512<<20, 0.10, 100)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("GOGC=%s\n", plan.Env["GOGC"])
	fmt.Printf("GOMEMLIMIT=%s\n", plan.Env["GOMEMLIMIT"])
	fmt.Printf("disabled-limit=%d\n", gctune.DisabledLimit())
}
```

In a real service, compute the plan from the container limit at startup and pass the values through environment configuration or call `Apply` once before serving requests.

## Common Mistakes

### Treating GOMEMLIMIT As A Hard Limit

Wrong: assuming `GOMEMLIMIT=512MiB` means the process cannot exceed 512 MiB.

Fix: treat it as a runtime-managed soft target. Leave headroom for stacks, the binary, cgo, memory mappings, and non-Go memory that the runtime cannot fully account for.

### Mutating GC Settings In Parallel Tests

Wrong: calling `debug.SetGCPercent` from many `t.Parallel` tests and expecting isolation.

Fix: test planning logic in parallel and reserve `Apply` for application startup or carefully serialized integration tests.

### Keeping A Ballast Because It Was Once Useful

Wrong: retaining a large global ballast in a modern service without re-evaluating it.

Fix: prefer `GOMEMLIMIT` and remove the ballast unless measurement proves a specific need. The plan type keeps `BallastByte` visible so the decision is explicit.

## Verification

Run this from `~/go-exercises/gctune`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test case to `TestNewPlanComputesEnvironment` with `container: 2 << 30`, `headroom: 0.05`, and `gcPercent: 200`. Confirm the computed `GOMEMLIMIT` and `GOGC` are stable.

## Summary

- `GOGC` controls the memory-versus-CPU trade-off by changing GC frequency.
- `GOMEMLIMIT` is a soft target for runtime-managed memory, not an operating-system hard limit.
- Ballasts are a historical workaround; modern services should usually express memory policy with `GOMEMLIMIT`.
- Planning GC settings is deterministic and testable; applying settings is process-global and should be done deliberately.

## What's Next

Next: [False Sharing and Cache Contention](../11-false-sharing-cache-contention/11-false-sharing-cache-contention.md).

## Resources

- [A Guide to the Go Garbage Collector](https://go.dev/doc/gc-guide)
- [runtime/debug.SetGCPercent](https://pkg.go.dev/runtime/debug#SetGCPercent)
- [runtime/debug.SetMemoryLimit](https://pkg.go.dev/runtime/debug#SetMemoryLimit)
- [Soft memory limit proposal](https://github.com/golang/go/issues/48409)
