# 4. Cgroups v2: CPU and Memory Limits

Cgroups v2 separate isolation (what a container sees) from limitation (how much of the host it can consume). Without cgroup limits, a container with namespace isolation can still exhaust every CPU cycle and byte of RAM on the host. This lesson builds the cgroup layer of a container runtime: a Go package that creates, configures, and tears down a cgroups v2 leaf cgroup, reads resource accounting, and detects OOM kills.

The hard parts are ordering and hierarchy. Controllers must be enabled in the parent cgroup before the child can use them. Limits must be set before the process starts real work. The cgroup directory must be removed with `os.Remove` (a plain `rmdir`), not `os.RemoveAll`, which tries to unlink virtual kernel files the kernel will not allow you to delete.

```
~/go-exercises/cgroupdemo/
  go.mod
  cgroup/
    cgroup.go
    parse.go
    cgroup_test.go
  cmd/demo/
    main.go
```

## Concepts

### The Unified Hierarchy and Its Filesystem API

Cgroups v2 mounts a single pseudo-filesystem at `/sys/fs/cgroup` — the unified hierarchy. Every object in the hierarchy is a directory. Every knob is a file. You create a cgroup by calling `os.MkdirAll`, set a CPU limit by calling `os.WriteFile` on `cpu.max`, and move a process into the cgroup by writing its PID to `cgroup.procs`.

When you remove the directory with `os.Remove`, the kernel removes the cgroup and frees all its virtual files atomically. The virtual files (`cpu.max`, `memory.max`, `cgroup.procs`, etc.) are not regular files and cannot be removed individually with `unlink`; only the directory removal is valid.

Verify that cgroups v2 is the active hierarchy:

```bash
mount | grep cgroup2
# cgroup2 on /sys/fs/cgroup type cgroup2 (rw,nosuid,nodev,noexec,...)
```

If the output is empty, your kernel uses the legacy v1 hierarchy. Any modern systemd-based distro defaults to v2 since 2021.

### The CPU Bandwidth Controller

CPU limits use the bandwidth controller, written to `cpu.max`:

```
$QUOTA $PERIOD
```

Both values are in microseconds. Within each scheduling period of `$PERIOD` microseconds, processes in the cgroup may use at most `$QUOTA` microseconds of CPU time. Writing `max $PERIOD` removes the quota limit.

Common examples:

- `50000 100000` — 50% of one core (50 ms of CPU per 100 ms window)
- `200000 100000` — two full cores (throttled across all CPUs, not pinned to two)
- `max 100000` — unlimited (period set but no cap)

The period defaults to 100 ms (100000 µs) on all current kernels. Container runtimes rarely change it.

### Memory Limits and the OOM Killer

`memory.max` sets the hard memory limit in bytes. When a process in the cgroup allocates memory that would push usage past this value, the kernel OOM killer fires, kills one or more processes in the cgroup, and increments `oom_kill` in `memory.events`.

Writing "max" to `memory.max` removes the limit.

`memory.swap.max` limits the combined memory+swap usage. Writing "0" to `memory.swap.max` disables swap for the cgroup entirely. This is almost always the right choice in containers: swap thrashing is unpredictable and makes OOM behavior difficult to reason about. Set `memory.swap.max=0` alongside any non-zero `memory.max` for predictable limits.

Resource accounting is available at any time by reading `memory.current` (current memory in bytes) and `cpu.stat` (a flat-keyed file containing `usage_usec`, the total CPU microseconds consumed).

### Enabling Controllers: subtree_control Delegation

Not every cgroup starts with every controller enabled. The available controllers for a child cgroup are those listed in the parent's `cgroup.subtree_control` file. Before creating a leaf cgroup that uses `cpu` and `memory`, write `+cpu +memory` to the parent's `cgroup.subtree_control`.

For a leaf cgroup directly under the root hierarchy:

```bash
echo "+cpu +memory" > /sys/fs/cgroup/cgroup.subtree_control
```

The kernel enforces a no-internal-process constraint: a cgroup that has children in the domain model cannot have processes assigned directly to it. The root hierarchy on a systemd host typically has no processes of its own, making it valid to enable controllers there.

Writing `+cpu` when `cpu` is already enabled is harmless — the kernel accepts repeated enable writes as no-ops.

### Lifecycle Ordering

The correct creation sequence is:

1. Enable controllers in the parent (`cgroup.subtree_control`)
2. Create the leaf cgroup directory (`os.MkdirAll`)
3. Write resource limits (`cpu.max`, `memory.max`, `memory.swap.max`)
4. Start the process (`exec.Command(...).Start()`)
5. Write the child PID to `cgroup.procs`

Steps 4 and 5 must happen in quick succession to minimize the window during which the process runs unconstrained. Production runtimes use `clone(2)` with a synchronization pipe: the child blocks until the parent finishes step 5, guaranteeing zero unconstrained execution. The demo in this lesson calls `cmd.Start()` then immediately `c.AddPID`, which is sufficient for demonstration but should not be used in a security-sensitive runtime.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/38-capstone-container-runtime/04-cgroups-v2-cpu-memory/04-cgroups-v2-cpu-memory/cgroup
mkdir -p go-solutions/38-capstone-container-runtime/04-cgroups-v2-cpu-memory/04-cgroups-v2-cpu-memory/cmd/demo
cd go-solutions/38-capstone-container-runtime/04-cgroups-v2-cpu-memory/04-cgroups-v2-cpu-memory
```

This is a library package verified with `go test`. The CLI in `cmd/demo` requires root on Linux; the tests run on any platform using temp directories to simulate the cgroup filesystem.

### Exercise 1: The cgroup Package

Create `cgroup/cgroup.go`:

```go
package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Sentinel errors for validation failures.
var (
	ErrCgroupV2NotMounted = errors.New("cgroups v2 not mounted")
	ErrInvalidCPUPercent  = errors.New("cpu percent must be between 1 and 10000")
	ErrInvalidMemoryLimit = errors.New("memory limit must be positive")
)

// CPUConfig describes a CPU bandwidth limit written to cpu.max.
// Quota and Period are both in microseconds.
// Quota <= 0 means unlimited (writes "max $Period").
// Period <= 0 defaults to 100000 (100 ms).
type CPUConfig struct {
	Quota  int64
	Period int64
}

// MemoryConfig describes memory limits.
// MaxBytes == 0 writes "max" to memory.max (unlimited).
// DisableSwap, when true, writes "0" to memory.swap.max.
type MemoryConfig struct {
	MaxBytes    int64
	DisableSwap bool
}

// Stats holds resource accounting data from a cgroup.
type Stats struct {
	MemoryCurrentBytes int64 // from memory.current
	CPUUsageMicros     int64 // usage_usec from cpu.stat
	OOMKillCount       int64 // oom_kill from memory.events
}

// Controller manages one cgroup v2 directory.
type Controller struct {
	root string // e.g. /sys/fs/cgroup
	name string // e.g. mycontainer-abc123
	path string // filepath.Join(root, name)
}

// New returns a Controller for the cgroup named name under root.
// It does not create the directory; call Create for that.
func New(root, name string) *Controller {
	return &Controller{
		root: root,
		name: name,
		path: filepath.Join(root, name),
	}
}

// Create enables the cpu and memory controllers in the parent cgroup and
// creates the leaf cgroup directory. It is idempotent.
func (c *Controller) Create() error {
	// Enable controllers in the parent first. The child directory's controller
	// files only appear after the parent lists them in subtree_control.
	subtreeCtl := filepath.Join(c.root, "cgroup.subtree_control")
	if err := os.WriteFile(subtreeCtl, []byte("+cpu +memory"), 0644); err != nil {
		return fmt.Errorf("cgroup %s: enable controllers: %w", c.name, err)
	}
	if err := os.MkdirAll(c.path, 0755); err != nil {
		return fmt.Errorf("cgroup %s: mkdir: %w", c.name, err)
	}
	return nil
}

// SetCPU writes the bandwidth limit to cpu.max.
// A zero or negative Quota writes "max $Period" (unlimited).
// A zero or negative Period defaults to 100000 (100 ms).
func (c *Controller) SetCPU(cfg CPUConfig) error {
	period := cfg.Period
	if period <= 0 {
		period = 100000
	}
	var value string
	if cfg.Quota <= 0 {
		value = fmt.Sprintf("max %d", period)
	} else {
		value = fmt.Sprintf("%d %d", cfg.Quota, period)
	}
	if err := os.WriteFile(filepath.Join(c.path, "cpu.max"), []byte(value), 0644); err != nil {
		return fmt.Errorf("cgroup %s: set cpu.max: %w", c.name, err)
	}
	return nil
}

// SetMemory writes memory.max and, when cfg.DisableSwap is true,
// writes "0" to memory.swap.max to prevent any swap usage.
func (c *Controller) SetMemory(cfg MemoryConfig) error {
	var memValue string
	if cfg.MaxBytes == 0 {
		memValue = "max"
	} else {
		memValue = strconv.FormatInt(cfg.MaxBytes, 10)
	}
	if err := os.WriteFile(filepath.Join(c.path, "memory.max"), []byte(memValue), 0644); err != nil {
		return fmt.Errorf("cgroup %s: set memory.max: %w", c.name, err)
	}
	if cfg.DisableSwap {
		if err := os.WriteFile(filepath.Join(c.path, "memory.swap.max"), []byte("0"), 0644); err != nil {
			return fmt.Errorf("cgroup %s: set memory.swap.max: %w", c.name, err)
		}
	}
	return nil
}

// AddPID moves the process with the given PID into this cgroup
// by writing to cgroup.procs.
func (c *Controller) AddPID(pid int) error {
	path := filepath.Join(c.path, "cgroup.procs")
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return fmt.Errorf("cgroup %s: add pid %d: %w", c.name, pid, err)
	}
	return nil
}

// Stats reads resource accounting data from the cgroup.
func (c *Controller) Stats() (Stats, error) {
	var s Stats

	mem, err := c.readSingleInt(filepath.Join(c.path, "memory.current"))
	if err != nil {
		return s, fmt.Errorf("cgroup %s: memory.current: %w", c.name, err)
	}
	s.MemoryCurrentBytes = mem

	usage, err := c.parseKeyedFile(filepath.Join(c.path, "cpu.stat"), "usage_usec", true)
	if err != nil {
		return s, fmt.Errorf("cgroup %s: cpu.stat: %w", c.name, err)
	}
	s.CPUUsageMicros = usage

	// oom_kill may be absent in a freshly created cgroup; treat absence as zero.
	oomKills, err := c.parseKeyedFile(filepath.Join(c.path, "memory.events"), "oom_kill", false)
	if err != nil {
		return s, fmt.Errorf("cgroup %s: memory.events: %w", c.name, err)
	}
	s.OOMKillCount = oomKills

	return s, nil
}

// Remove deletes the cgroup directory. All processes in the cgroup must have
// exited before this is called; the kernel rejects rmdir on a populated cgroup.
// Use os.Remove (a single rmdir), not os.RemoveAll: the kernel owns the virtual
// files inside and does not permit unlinking them individually.
func (c *Controller) Remove() error {
	if err := os.Remove(c.path); err != nil {
		return fmt.Errorf("cgroup %s: remove: %w", c.name, err)
	}
	return nil
}

// Path returns the absolute filesystem path of this cgroup directory.
func (c *Controller) Path() string { return c.path }

// Name returns the name of this cgroup.
func (c *Controller) Name() string { return c.name }

// CheckV2Mounted returns nil when the cgroups v2 unified hierarchy is mounted
// at root (typically /sys/fs/cgroup). It reads /proc/mounts.
func CheckV2Mounted(root string) error {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return fmt.Errorf("cgroup: read /proc/mounts: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		// /proc/mounts columns: device mountpoint fstype options dump pass
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == root && fields[2] == "cgroup2" {
			return nil
		}
	}
	return fmt.Errorf("%w: no cgroup2 mounted at %s", ErrCgroupV2NotMounted, root)
}

// readSingleInt reads a file whose content is a single integer.
func (c *Controller) readSingleInt(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

// parseKeyedFile scans a flat-keyed file (like cpu.stat or memory.events)
// for a line matching "key <integer>". When required is true, absence of the
// key is an error; when required is false, absence returns 0.
func (c *Controller) parseKeyedFile(path, key string, required bool) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	prefix := key + " "
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, prefix) {
			return strconv.ParseInt(strings.TrimSpace(line[len(prefix):]), 10, 64)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if required {
		return 0, fmt.Errorf("key %q not found in %s", key, path)
	}
	return 0, nil
}
```

Create `cgroup/parse.go`. Both functions are pure (no I/O) and fully testable without root. `ParseMemoryBytes` uses binary prefixes (1 KiB = 1024 B), matching the kernel's byte interpretation of `memory.max`.

```go
package cgroup

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseMemoryBytes converts a human-readable size string to bytes.
// Accepted suffixes: k/K (kibibytes), m/M (mebibytes), g/G (gibibytes).
// A plain integer is treated as bytes. The value must be positive.
//
// Examples: "256m" -> 268435456, "1g" -> 1073741824, "512k" -> 524288.
func ParseMemoryBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("cgroup: empty memory string")
	}
	multipliers := map[byte]int64{
		'k': 1 << 10, 'K': 1 << 10,
		'm': 1 << 20, 'M': 1 << 20,
		'g': 1 << 30, 'G': 1 << 30,
	}
	last := s[len(s)-1]
	if mult, ok := multipliers[last]; ok {
		n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cgroup: parse memory %q: %w", s, err)
		}
		if n <= 0 {
			return 0, fmt.Errorf("%w: got %q", ErrInvalidMemoryLimit, s)
		}
		return n * mult, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cgroup: parse memory %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%w: got %q", ErrInvalidMemoryLimit, s)
	}
	return n, nil
}

// CPUPercentToConfig converts a CPU percentage to a CPUConfig.
// percent must be between 1 and 10000 inclusive.
// 100 means one full CPU core, 200 means two cores.
// The scheduling period is fixed at 100 ms (100000 microseconds).
func CPUPercentToConfig(percent int) (CPUConfig, error) {
	if percent < 1 || percent > 10000 {
		return CPUConfig{}, fmt.Errorf("%w: got %d", ErrInvalidCPUPercent, percent)
	}
	const period int64 = 100000
	quota := int64(percent) * period / 100
	return CPUConfig{Quota: quota, Period: period}, nil
}
```

### Exercise 2: Test Suite

Create `cgroup/cgroup_test.go`. The tests use `t.TempDir()` to simulate the cgroup filesystem: `os.WriteFile` on a temp directory behaves identically to the cgroup filesystem from the Go standard library's perspective. Only the kernel's enforcement behavior differs — here we are testing data formats and parsing logic.

```go
package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// --- Pure function tests ---

func TestParseMemoryBytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input   string
		want    int64
		wantErr error // non-nil: check errors.Is; combined with isError
		isError bool  // true if any error is expected
	}{
		{"256m", 256 << 20, nil, false},
		{"256M", 256 << 20, nil, false},
		{"1g", 1 << 30, nil, false},
		{"1G", 1 << 30, nil, false},
		{"512k", 512 << 10, nil, false},
		{"512K", 512 << 10, nil, false},
		{"1024", 1024, nil, false},
		{"0", 0, ErrInvalidMemoryLimit, true},
		{"-1", 0, ErrInvalidMemoryLimit, true},
		{"-1m", 0, ErrInvalidMemoryLimit, true},
		{"abc", 0, nil, true},
		{"", 0, nil, true},
		{"1.5g", 0, nil, true},
	}

	for _, tc := range cases {
		got, err := ParseMemoryBytes(tc.input)
		if tc.isError {
			if err == nil {
				t.Errorf("ParseMemoryBytes(%q): want error, got nil", tc.input)
				continue
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("ParseMemoryBytes(%q): err = %v, want errors.Is(%v)", tc.input, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMemoryBytes(%q): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMemoryBytes(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestCPUPercentToConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		percent    int
		wantQuota  int64
		wantPeriod int64
		wantErr    error
	}{
		{50, 50000, 100000, nil},
		{100, 100000, 100000, nil},
		{200, 200000, 100000, nil},
		{1, 1000, 100000, nil},
		{10000, 10000000, 100000, nil},
		{0, 0, 0, ErrInvalidCPUPercent},
		{-1, 0, 0, ErrInvalidCPUPercent},
		{10001, 0, 0, ErrInvalidCPUPercent},
	}

	for _, tc := range cases {
		cfg, err := CPUPercentToConfig(tc.percent)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("CPUPercentToConfig(%d): err = %v, want %v", tc.percent, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("CPUPercentToConfig(%d): unexpected error: %v", tc.percent, err)
			continue
		}
		if cfg.Quota != tc.wantQuota || cfg.Period != tc.wantPeriod {
			t.Errorf("CPUPercentToConfig(%d) = {%d %d}, want {%d %d}",
				tc.percent, cfg.Quota, cfg.Period, tc.wantQuota, tc.wantPeriod)
		}
	}
}

// --- Controller filesystem tests ---

func TestControllerCreate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := New(root, "testcg")

	if err := c.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(c.Path()); err != nil {
		t.Fatalf("cgroup directory not created: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("cgroup.subtree_control not written: %v", err)
	}
	if string(data) != "+cpu +memory" {
		t.Errorf("subtree_control = %q, want %q", data, "+cpu +memory")
	}
}

func TestControllerSetCPU(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := New(root, "testcg")
	if err := os.MkdirAll(c.Path(), 0755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		cfg     CPUConfig
		wantVal string
	}{
		{"50 percent", CPUConfig{Quota: 50000, Period: 100000}, "50000 100000"},
		{"25 percent", CPUConfig{Quota: 25000, Period: 100000}, "25000 100000"},
		{"unlimited", CPUConfig{Quota: 0, Period: 100000}, "max 100000"},
		{"negative quota", CPUConfig{Quota: -1, Period: 100000}, "max 100000"},
		{"default period", CPUConfig{Quota: 50000, Period: 0}, "50000 100000"},
	}

	for _, tc := range cases {
		if err := c.SetCPU(tc.cfg); err != nil {
			t.Errorf("%s: SetCPU: %v", tc.name, err)
			continue
		}
		got, err := os.ReadFile(filepath.Join(c.Path(), "cpu.max"))
		if err != nil {
			t.Fatalf("%s: read cpu.max: %v", tc.name, err)
		}
		if string(got) != tc.wantVal {
			t.Errorf("%s: cpu.max = %q, want %q", tc.name, got, tc.wantVal)
		}
	}
}

func TestControllerSetMemory(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		cfg        MemoryConfig
		wantMax    string
		wantSwap   string
		noSwapFile bool
	}{
		{
			name:     "bytes with swap disabled",
			cfg:      MemoryConfig{MaxBytes: 256 << 20, DisableSwap: true},
			wantMax:  "268435456",
			wantSwap: "0",
		},
		{
			name:       "unlimited no swap config",
			cfg:        MemoryConfig{MaxBytes: 0, DisableSwap: false},
			wantMax:    "max",
			noSwapFile: true,
		},
		{
			name:       "1 gib no swap config",
			cfg:        MemoryConfig{MaxBytes: 1 << 30, DisableSwap: false},
			wantMax:    "1073741824",
			noSwapFile: true,
		},
		{
			name:     "128 mib swap disabled",
			cfg:      MemoryConfig{MaxBytes: 128 << 20, DisableSwap: true},
			wantMax:  "134217728",
			wantSwap: "0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			c := New(root, "testcg")
			if err := os.MkdirAll(c.Path(), 0755); err != nil {
				t.Fatal(err)
			}
			if err := c.SetMemory(tc.cfg); err != nil {
				t.Fatalf("SetMemory: %v", err)
			}
			gotMax, err := os.ReadFile(filepath.Join(c.Path(), "memory.max"))
			if err != nil {
				t.Fatalf("read memory.max: %v", err)
			}
			if string(gotMax) != tc.wantMax {
				t.Errorf("memory.max = %q, want %q", gotMax, tc.wantMax)
			}
			swapPath := filepath.Join(c.Path(), "memory.swap.max")
			if tc.noSwapFile {
				if _, err := os.Stat(swapPath); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("memory.swap.max should not exist, err=%v", err)
				}
				return
			}
			gotSwap, err := os.ReadFile(swapPath)
			if err != nil {
				t.Fatalf("read memory.swap.max: %v", err)
			}
			if string(gotSwap) != tc.wantSwap {
				t.Errorf("memory.swap.max = %q, want %q", gotSwap, tc.wantSwap)
			}
		})
	}
}

func TestControllerAddPID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := New(root, "testcg")
	if err := os.MkdirAll(c.Path(), 0755); err != nil {
		t.Fatal(err)
	}

	if err := c.AddPID(42); err != nil {
		t.Fatalf("AddPID: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(c.Path(), "cgroup.procs"))
	if err != nil {
		t.Fatalf("read cgroup.procs: %v", err)
	}
	if string(got) != "42" {
		t.Errorf("cgroup.procs = %q, want %q", got, "42")
	}
}

func TestControllerStats(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := New(root, "testcg")
	if err := os.MkdirAll(c.Path(), 0755); err != nil {
		t.Fatal(err)
	}

	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.Path(), name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	writeFile("memory.current", "12345\n")
	writeFile("cpu.stat", "usage_usec 67890\nuser_usec 12345\nsystem_usec 55545\nnr_periods 100\nnr_throttled 5\n")
	writeFile("memory.events", "low 0\nhigh 0\nmax 5\noom 2\noom_kill 1\noom_group_kill 0\n")

	stats, err := c.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.MemoryCurrentBytes != 12345 {
		t.Errorf("MemoryCurrentBytes = %d, want 12345", stats.MemoryCurrentBytes)
	}
	if stats.CPUUsageMicros != 67890 {
		t.Errorf("CPUUsageMicros = %d, want 67890", stats.CPUUsageMicros)
	}
	if stats.OOMKillCount != 1 {
		t.Errorf("OOMKillCount = %d, want 1", stats.OOMKillCount)
	}
}

func TestControllerStatsOOMKillAbsent(t *testing.T) {
	t.Parallel()
	// memory.events without oom_kill should return OOMKillCount=0, not an error.
	root := t.TempDir()
	c := New(root, "testcg")
	if err := os.MkdirAll(c.Path(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Path(), "memory.current"), []byte("0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Path(), "cpu.stat"), []byte("usage_usec 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Path(), "memory.events"), []byte("low 0\nhigh 0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	stats, err := c.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.OOMKillCount != 0 {
		t.Errorf("OOMKillCount = %d, want 0 for absent key", stats.OOMKillCount)
	}
}

func TestControllerRemove(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	c := New(root, "testcg")
	if err := os.MkdirAll(c.Path(), 0755); err != nil {
		t.Fatal(err)
	}

	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(c.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cgroup dir should not exist after Remove, err=%v", err)
	}
}

// --- Example functions (auto-verified by go test) ---

func ExampleParseMemoryBytes() {
	n, _ := ParseMemoryBytes("256m")
	fmt.Println(n)
	// Output: 268435456
}

func ExampleCPUPercentToConfig() {
	cfg, _ := CPUPercentToConfig(50)
	fmt.Printf("quota=%d period=%d\n", cfg.Quota, cfg.Period)
	// Output: quota=50000 period=100000
}

// Your turn: add TestCPUPercentToConfigOneCoreAt100Percent to verify that
// CPUPercentToConfig(100) returns Quota=100000 and Period=100000,
// confirming that 100% equals exactly one full period.
```

### Exercise 3: CLI Demo

Create `cmd/demo/main.go`. This program requires root on Linux. Run it as:

```bash
sudo go run ./cmd/demo --cpu 50 --memory 128m -- stress --cpu 1 --timeout 5
```

```go
// Package main demonstrates cgroup v2 resource limiting in a container runtime.
// It requires root on a Linux host with cgroups v2 (unified hierarchy).
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"

	"example.com/cgroupdemo/cgroup"
)

func main() {
	cpuPct := flag.Int("cpu", 0, "CPU limit as percent of one core (1-10000)")
	memStr := flag.String("memory", "", "memory limit (e.g. 128m, 1g)")
	name := flag.String("name", "demo-container", "cgroup name")
	flag.Parse()

	if err := run(*name, *cpuPct, *memStr, flag.Args()); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func run(name string, cpuPct int, memStr string, args []string) error {
	const cgroupRoot = "/sys/fs/cgroup"

	if err := cgroup.CheckV2Mounted(cgroupRoot); err != nil {
		return fmt.Errorf("cgroups v2: %w", err)
	}

	c := cgroup.New(cgroupRoot, name)
	if err := c.Create(); err != nil {
		return fmt.Errorf("create cgroup: %w", err)
	}
	defer func() {
		if err := c.Remove(); err != nil {
			fmt.Fprintf(os.Stderr, "remove cgroup %s: %v\n", c.Name(), err)
		}
	}()

	if cpuPct > 0 {
		cfg, err := cgroup.CPUPercentToConfig(cpuPct)
		if err != nil {
			return err
		}
		if err := c.SetCPU(cfg); err != nil {
			return fmt.Errorf("set cpu: %w", err)
		}
		fmt.Printf("cpu limit: %d%% (quota=%d period=%d)\n", cpuPct, cfg.Quota, cfg.Period)
	}

	if memStr != "" {
		bytes, err := cgroup.ParseMemoryBytes(memStr)
		if err != nil {
			return err
		}
		memCfg := cgroup.MemoryConfig{MaxBytes: bytes, DisableSwap: true}
		if err := c.SetMemory(memCfg); err != nil {
			return fmt.Errorf("set memory: %w", err)
		}
		fmt.Printf("memory limit: %s (%d bytes), swap disabled\n", memStr, bytes)
	}

	if len(args) == 0 {
		args = []string{"sh", "-c", "echo running inside cgroup; cat /proc/self/cgroup"}
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// Assign the child to the cgroup immediately after Start. The process has
	// not yet run user code, so the unconstrained window is negligibly short.
	// Production runtimes eliminate this window entirely via clone(2) + a pipe.
	if err := c.AddPID(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("add pid %d to cgroup: %w", cmd.Process.Pid, err)
	}

	runErr := cmd.Wait()

	var stats cgroup.Stats
	if s, err := c.Stats(); err != nil {
		fmt.Fprintf(os.Stderr, "stats: %v\n", err)
	} else {
		stats = s
		fmt.Printf("--- cgroup stats ---\n")
		fmt.Printf("memory.current: %d bytes\n", s.MemoryCurrentBytes)
		fmt.Printf("cpu.stat usage_usec: %d\n", s.CPUUsageMicros)
		if s.OOMKillCount > 0 {
			fmt.Printf("OOM kills: %d\n", s.OOMKillCount)
		}
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			if stats.OOMKillCount > 0 {
				return fmt.Errorf("process OOM killed (exit %d)", exitErr.ExitCode())
			}
			return fmt.Errorf("process exited with code %d", exitErr.ExitCode())
		}
		return runErr
	}
	return nil
}
```

## Common Mistakes

### Not Disabling Swap When Setting memory.max

Wrong: the container sets `memory.max` to 256 MiB but leaves swap unrestricted.

```go
// Wrong: memory.max limits RAM but swap is still available.
c.SetMemory(cgroup.MemoryConfig{MaxBytes: 256 << 20})
```

What happens: the process can allocate well beyond 256 MiB by using swap. The OOM killer does not fire at the RAM limit; it fires only when both RAM and swap are exhausted. The effective limit becomes `memory.max + swap space`, which can be several gigabytes on a typical host.

Fix: always set `DisableSwap: true` alongside a non-zero `MaxBytes`. The kernel writes "0" to `memory.swap.max`, capping memory+swap at the RAM limit.

```go
// Fix: both memory.max and memory.swap.max are configured.
c.SetMemory(cgroup.MemoryConfig{MaxBytes: 256 << 20, DisableSwap: true})
```

### Writing subtree_control to the Child Instead of the Parent

Wrong: trying to enable controllers inside the new cgroup directory.

```go
// Wrong: child's subtree_control has no effect for the leaf itself;
// it controls what the leaf's own children (which don't exist) may use.
os.WriteFile(filepath.Join(c.Path(), "cgroup.subtree_control"), []byte("+cpu +memory"), 0644)
```

What happens: the kernel may accept the write or reject it; either way, `cpu.max` and `memory.max` do not appear in the leaf because the parent never delegated the controllers. The limits appear to be set but are never enforced.

Fix: write to the parent's `cgroup.subtree_control`. In `Create()`, the parent is `c.root`, not `c.path`.

```go
// Fix: parent delegates cpu and memory to its subtree.
subtreeCtl := filepath.Join(c.root, "cgroup.subtree_control")
os.WriteFile(subtreeCtl, []byte("+cpu +memory"), 0644)
```

### Using os.RemoveAll to Clean Up a Cgroup Directory

Wrong: calling `os.RemoveAll(c.Path())` to tear down the cgroup.

```go
// Wrong: RemoveAll tries to unlink cpu.max, memory.max, and other virtual files.
os.RemoveAll(c.Path())
```

What happens: the kernel does not permit `unlink` on virtual cgroup files. `os.RemoveAll` calls `unlink` on each file before calling `rmdir`, so it returns an error and leaves the cgroup directory in place. The cgroup leaks in `/sys/fs/cgroup` until the next reboot.

Fix: call `os.Remove(c.Path())`, which issues a single `rmdir` syscall. The kernel removes the cgroup and all its virtual files atomically. The cgroup must have no live processes before `os.Remove` succeeds.

```go
// Fix: single rmdir via c.Remove().
c.Remove()
```

### Ignoring the Race Window Between Start and AddPID

Wrong: assuming the gap between `cmd.Start()` and `c.AddPID(cmd.Process.Pid)` is safe.

What happens: if the workload allocates memory quickly (a C program that `malloc`s immediately), it may consume beyond the intended limit during the window before `AddPID` moves it into the cgroup. The OOM killer may not fire, or fires only after excess allocation.

Fix for production: use `clone(2)` via `syscall.SysProcAttr.Cloneflags` and a synchronization pipe. The child blocks on a pipe read until the parent has completed `AddPID`; only then does the child `exec` the workload. This guarantees zero unconstrained execution. The demo above accepts the race because `cmd.Start()` runs only the Go runtime's fork+exec path, which is shorter than any real user-space workload.

## Verification

From `~/go-exercises/cgroupdemo`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must produce no output and exit 0. The test suite runs on any platform (macOS, Linux) because it uses temp directories to simulate the cgroup filesystem.

To verify actual cgroup enforcement on Linux as root:

```bash
# Confirm cgroups v2 is active.
mount | grep cgroup2

# Run a memory-limited workload that will be OOM-killed.
sudo go run ./cmd/demo --memory 64m -- sh -c 'yes | head -c 200000000 | sort'
# Expect: OOM kills: 1 in stats output.

# Run a CPU-limited workload and observe throttling.
sudo go run ./cmd/demo --cpu 10 -- sh -c 'for i in $(seq 1 1000000); do :; done'
# Expect: cpu.stat usage_usec represents approximately 10% of elapsed wall time.
```

## Summary

- Cgroups v2 uses a single filesystem at `/sys/fs/cgroup`; every knob is a file write and every accounting metric is a file read.
- `cpu.max` takes `$QUOTA $PERIOD` (both in microseconds); "max $PERIOD" removes the quota.
- `memory.max` sets the RAM hard limit; `memory.swap.max=0` disables swap for predictable OOM behavior.
- Controllers must be enabled in the parent cgroup's `cgroup.subtree_control` before the child cgroup can use them.
- The correct lifecycle: enable controllers, create directory, set limits, start process, assign PID — in that order.
- Clean up with `os.Remove` (a single `rmdir`), never `os.RemoveAll`, which attempts to unlink kernel-owned virtual files.
- Resource accounting (`memory.current`, `cpu.stat usage_usec`, `memory.events oom_kill`) is readable at any time while the cgroup exists.

## What's Next

Next: [Overlay Filesystem](../05-overlay-filesystem/05-overlay-filesystem.md).

## Resources

- [Kernel cgroups v2 documentation](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html) — authoritative reference for the unified hierarchy, controller interfaces, and delegation model
- [CPU bandwidth control documentation](https://www.kernel.org/doc/html/latest/scheduler/sched-bwc.html) — quota/period semantics and enforcement details
- [cgroups(7) Linux man page](https://man7.org/linux/man-pages/man7/cgroups.7.html) — v1 vs v2 comparison, the no-internal-process constraint, and the delegation model
- [OCI Runtime Spec: Linux Resources](https://github.com/opencontainers/runtime-spec/blob/main/config-linux.md#control-groups) — how production container runtimes specify resource limits
- [runc cgroupsv2 implementation](https://github.com/opencontainers/runc/tree/main/libcontainer/cgroups/fs2) — real production code using the same filesystem API
