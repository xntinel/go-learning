# Exercise 1: Compute Effective GOMAXPROCS from cgroup CPU Quota

This is the real on-the-job exercise: a pure, dependency-injected library that
reproduces the Go 1.25 (and `go.uber.org/automaxprocs`) algorithm for turning a
cgroup CPU quota into an effective `GOMAXPROCS`. It is exactly what teams wrote by
hand before 1.25, and exactly what you still reach for when auditing a throttled
pod.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
cgroupmax/                 independent module: example.com/cgroupmax
  go.mod                   go 1.25
  cgroupmax.go             ReadCgroupLimit, Effective, ErrMalformed (pure, fs.FS-injected)
  cmd/
    demo/
      main.go              naive NumCPU sizing vs cgroup-aware, over an in-memory FS
  cgroupmax_test.go        table-driven over fstest.MapFS + os.DirFS(t.TempDir), errors.Is
```

- Files: `cgroupmax.go`, `cmd/demo/main.go`, `cgroupmax_test.go`.
- Implement: `ReadCgroupLimit(fsys fs.FS) (cores float64, ok bool, err error)` reading cgroup v2 `cpu.max` then v1 `cpu.cfs_quota_us`/`cpu.cfs_period_us`, and `Effective(fsys fs.FS, logical int) (int, error)` applying `min(logical, max(2, ceil(cores)))`.
- Test: table-driven cases over an injected `fs.FS` covering v2, v1, the `max`/`-1`/missing no-limit paths, the ceil-up rule, the floor-of-2 rule, the `min` selection, and a malformed file asserted with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why an injected fs.FS

The value of this library is that it is *pure*: it never touches the host's real
`/sys/fs/cgroup`. Instead it takes an `io/fs.FS` rooted at a cgroup mount, so the
same code runs against `os.DirFS("/sys/fs/cgroup")` in production and against an
`fstest.MapFS` in a test. That is what makes the tests hermetic and deterministic
— they do not depend on whether the machine running `go test` is itself in a
container, uses cgroup v1 or v2, or has any limit at all. `fs.ReadFile(fsys, name)`
is the single I/O primitive; in production you wire `fsys` with `os.DirFS`, and the
reads underneath become ordinary `os.ReadFile` calls.

### The algorithm, precisely

`ReadCgroupLimit` returns `(cores, ok, err)` where `cores` is the quota expressed
as a fractional number of cores (`quota / period`) and `ok` reports whether a
finite limit is configured at all. The order matters: try cgroup v2 first, and
only fall through to v1 when `cpu.max` does not exist.

- **cgroup v2** — read `cpu.max`. It holds two space-separated fields,
  `"<quota> <period>"` in microseconds. If the first field is the literal `max`,
  there is no limit: return `ok == false`. Otherwise parse both integers and
  return `quota/period`.
- **cgroup v1** — read `cpu.cfs_quota_us`. A value of `-1` means no limit
  (`ok == false`). Otherwise read `cpu.cfs_period_us` (default `100000` when that
  file is absent) and return `quota/period`.
- **missing files** — `fs.ReadFile` returns an error wrapping `fs.ErrNotExist`
  when a file is not present. A missing `cpu.max` triggers the v1 fall-through; a
  missing `cpu.cfs_quota_us` means "no limit here", so we return `ok == false`
  with a `nil` error. A missing file is never a failure.

`Effective(fsys, logical)` then combines the limit with the supplied logical CPU
count (inject `runtime.NumCPU()` in production):

```text
if no limit:  return logical
otherwise:    adjusted = max(2, ceil(cores)); return min(logical, adjusted)
```

The `max(2, ...)` floor applies to the cgroup component *before* the `min`, so a
single-CPU machine (`logical == 1`) still returns 1 even under a sub-core quota,
while a 0.5-core limit on a multi-core box returns 2. Malformed file contents (a
non-numeric quota, the wrong field count) are a real error: they are wrapped with
the package sentinel `ErrMalformed` so a caller can distinguish "the file is
garbage" from "there is no limit" with `errors.Is`.

Create `cgroupmax.go`:

```go
package cgroupmax

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strconv"
	"strings"
)

// ErrMalformed is returned when a cgroup CPU file exists but cannot be parsed.
// It is distinct from the no-limit case (ok == false, err == nil) and from a
// missing file (which is treated as no limit), so callers can tell a corrupt
// file apart from an absent one with errors.Is.
var ErrMalformed = errors.New("cgroupmax: malformed cpu limit file")

// defaultPeriodUS is the kernel default CFS period when cpu.cfs_period_us is
// absent (100 ms in microseconds).
const defaultPeriodUS = 100_000

// ReadCgroupLimit reports the cgroup CPU bandwidth limit as a fractional number
// of cores. It reads cgroup v2 (cpu.max) first, then falls back to cgroup v1
// (cpu.cfs_quota_us / cpu.cfs_period_us). ok is false when no finite limit is
// configured (the "max"/-1 sentinels or a missing file); in that case cores is
// zero and err is nil. A file that exists but cannot be parsed returns a non-nil
// err wrapping ErrMalformed.
func ReadCgroupLimit(fsys fs.FS) (cores float64, ok bool, err error) {
	// cgroup v2: a single cpu.max file.
	data, err := fs.ReadFile(fsys, "cpu.max")
	switch {
	case err == nil:
		return parseV2(string(data))
	case !errors.Is(err, fs.ErrNotExist):
		return 0, false, err
	}

	// cgroup v1: separate quota and period files.
	quotaData, err := fs.ReadFile(fsys, "cpu.cfs_quota_us")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil // no limit configured
	}
	if err != nil {
		return 0, false, err
	}
	return parseV1(string(quotaData), fsys)
}

func parseV2(s string) (float64, bool, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return 0, false, fmt.Errorf("%w: cpu.max=%q", ErrMalformed, strings.TrimSpace(s))
	}
	if fields[0] == "max" {
		return 0, false, nil // no limit
	}
	quota, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%w: quota %q: %v", ErrMalformed, fields[0], err)
	}
	period, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || period <= 0 {
		return 0, false, fmt.Errorf("%w: period %q", ErrMalformed, fields[1])
	}
	return float64(quota) / float64(period), true, nil
}

func parseV1(quotaStr string, fsys fs.FS) (float64, bool, error) {
	quota, err := strconv.ParseInt(strings.TrimSpace(quotaStr), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%w: cpu.cfs_quota_us %q: %v", ErrMalformed, strings.TrimSpace(quotaStr), err)
	}
	if quota < 0 {
		return 0, false, nil // -1 means no limit
	}

	period := int64(defaultPeriodUS)
	periodData, err := fs.ReadFile(fsys, "cpu.cfs_period_us")
	if err == nil {
		period, err = strconv.ParseInt(strings.TrimSpace(string(periodData)), 10, 64)
		if err != nil || period <= 0 {
			return 0, false, fmt.Errorf("%w: cpu.cfs_period_us %q", ErrMalformed, strings.TrimSpace(string(periodData)))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, false, err
	}
	return float64(quota) / float64(period), true, nil
}

// Effective returns the container-aware GOMAXPROCS the Go 1.25 runtime would
// choose: min(logical, max(2, ceil(cores))) when a cgroup limit is set, and
// logical when there is none. The floor of 2 applies to the cgroup component
// before the min, so a machine with logical < 2 still returns logical.
func Effective(fsys fs.FS, logical int) (int, error) {
	cores, ok, err := ReadCgroupLimit(fsys)
	if err != nil {
		return 0, err
	}
	if !ok {
		return logical, nil
	}
	adjusted := int(math.Ceil(cores))
	if adjusted < 2 {
		adjusted = 2
	}
	return min(logical, adjusted), nil
}
```

### The runnable demo

The demo builds an in-memory `fstest.MapFS` that models a 2-CPU-limited pod
(`cpu.max = "200000 100000"`) and contrasts the naive `NumCPU`-style sizing
(pretending the pod is on a 64-core node) with the cgroup-aware result. This is the
64x over-provisioning that pre-1.25 binaries suffered, reduced to a single
comparison.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"testing/fstest"

	"example.com/cgroupmax"
)

func main() {
	// A pod with limits.cpu: "2" landing on a 64-core node.
	const nodeLogicalCPUs = 64
	cgroup := fstest.MapFS{
		"cpu.max": &fstest.MapFile{Data: []byte("200000 100000\n")},
	}

	gomaxprocs, err := cgroupmax.Effective(cgroup, nodeLogicalCPUs)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("naive NumCPU-based sizing: %d\n", nodeLogicalCPUs)
	fmt.Printf("cgroup-aware GOMAXPROCS:   %d\n", gomaxprocs)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
naive NumCPU-based sizing: 64
cgroup-aware GOMAXPROCS:   2
```

### Tests

The tests are table-driven over an injected `fstest.MapFS`, so every case is a
self-contained cgroup snapshot with no dependence on the host. They cover both
cgroup versions, all three no-limit paths (`max`, `-1`, missing files), the
round-up rule, the floor-of-2 rule, the `min` selection when the logical count is
the tighter bound, and the single-CPU case where the floor does not apply. A
separate test uses `os.DirFS(t.TempDir())` to prove the same code works against
real files on disk, and a malformed-input test asserts the `ErrMalformed` sentinel
with `errors.Is`.

Create `cgroupmax_test.go`:

```go
package cgroupmax

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestEffective(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   fstest.MapFS
		logical int
		want    int
	}{
		{
			name:    "v2 two cores exact",
			files:   fstest.MapFS{"cpu.max": &fstest.MapFile{Data: []byte("200000 100000")}},
			logical: 8,
			want:    2,
		},
		{
			name:    "v2 fractional rounds up",
			files:   fstest.MapFS{"cpu.max": &fstest.MapFile{Data: []byte("250000 100000")}},
			logical: 8,
			want:    3,
		},
		{
			name:    "v2 half core floored to two",
			files:   fstest.MapFS{"cpu.max": &fstest.MapFile{Data: []byte("50000 100000")}},
			logical: 8,
			want:    2,
		},
		{
			name:    "v2 max is no limit",
			files:   fstest.MapFS{"cpu.max": &fstest.MapFile{Data: []byte("max 100000")}},
			logical: 8,
			want:    8,
		},
		{
			name:    "min selects logical count",
			files:   fstest.MapFS{"cpu.max": &fstest.MapFile{Data: []byte("800000 100000")}},
			logical: 4,
			want:    4,
		},
		{
			name:    "single cpu machine bypasses floor",
			files:   fstest.MapFS{"cpu.max": &fstest.MapFile{Data: []byte("200000 100000")}},
			logical: 1,
			want:    1,
		},
		{
			name: "v1 quota and period",
			files: fstest.MapFS{
				"cpu.cfs_quota_us":  &fstest.MapFile{Data: []byte("200000")},
				"cpu.cfs_period_us": &fstest.MapFile{Data: []byte("100000")},
			},
			logical: 8,
			want:    2,
		},
		{
			name: "v1 default period when file absent",
			files: fstest.MapFS{
				"cpu.cfs_quota_us": &fstest.MapFile{Data: []byte("300000")},
			},
			logical: 8,
			want:    3,
		},
		{
			name: "v1 unlimited quota",
			files: fstest.MapFS{
				"cpu.cfs_quota_us":  &fstest.MapFile{Data: []byte("-1")},
				"cpu.cfs_period_us": &fstest.MapFile{Data: []byte("100000")},
			},
			logical: 8,
			want:    8,
		},
		{
			name:    "no files is no limit",
			files:   fstest.MapFS{},
			logical: 8,
			want:    8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Effective(tt.files, tt.logical)
			if err != nil {
				t.Fatalf("Effective: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Effective(logical=%d) = %d, want %d", tt.logical, got, tt.want)
			}
		})
	}
}

func TestEffectiveMalformed(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{"cpu.max": &fstest.MapFile{Data: []byte("not-a-number 100000")}}
	_, err := Effective(files, 8)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Effective on garbage: error = %v, want wrapping ErrMalformed", err)
	}
}

func TestEffectiveRealFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte("150000 100000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// os.DirFS is the production wiring; the same code path serves it and MapFS.
	got, err := Effective(os.DirFS(dir), 8)
	if err != nil {
		t.Fatalf("Effective over os.DirFS: %v", err)
	}
	if want := 2; got != want { // ceil(1.5) = 2
		t.Fatalf("Effective over os.DirFS = %d, want %d", got, want)
	}
}

func Example() {
	cgroup := fstest.MapFS{"cpu.max": &fstest.MapFile{Data: []byte("250000 100000")}}
	n, _ := Effective(cgroup, 16)
	fmt.Println(n)
	// Output: 3
}
```

## Review

Correctness here is entirely about matching the runtime's arithmetic, and the
tests pin each rule to a distinct case. The three that people get wrong: fractional
quotas round *up* (`ceil(2.5) == 3`, the "fractional rounds up" case), a sub-core
limit is floored to 2 (the "half core floored to two" case), and the floor is
applied to the cgroup component before the `min`, so a genuine single-CPU host
returns 1 (the "single cpu machine bypasses floor" case). If you truncate instead
of `ceil`, the fractional case fails; if you floor after the `min`, the single-CPU
case fails.

The other trap is conflating "no limit" with "error". A missing `cpu.max` must
fall through to v1, and a missing `cpu.cfs_quota_us` must return `ok == false` with
a `nil` error, not an `fs.ErrNotExist` propagated to the caller — otherwise every
unconstrained host looks broken. Only a file that exists and cannot be parsed is an
error, and it is wrapped with `ErrMalformed` so `errors.Is` can classify it. Run
`go test -race` to confirm; the library holds no shared state, so the value here is
catching an accidental data race in a future extension.

## Resources

- [Go 1.25 release notes — container-aware GOMAXPROCS](https://go.dev/doc/go1.25) — the default formula, the `containermaxprocs`/`updatemaxprocs` GODEBUGs, and the limit-not-request rule.
- [golang/go issue #73193](https://github.com/golang/go/issues/73193) — the design: `max(2, ceil(limit))`, the min across sources, and cgroup v1 vs v2 parsing.
- [`io/fs` package](https://pkg.go.dev/io/fs) — `fs.FS`, `fs.ReadFile`, and `fs.ErrNotExist`, the injection seam this library is built on.
- [`go.uber.org/automaxprocs`](https://github.com/uber-go/automaxprocs) — the pre-1.25 library whose algorithm this exercise reproduces.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-runtime-gomaxprocs-control.md](02-runtime-gomaxprocs-control.md)
