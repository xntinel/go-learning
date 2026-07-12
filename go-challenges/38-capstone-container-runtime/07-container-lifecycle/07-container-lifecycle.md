# 7. Container Lifecycle Management

Container processes do not run in isolation — they move through a defined sequence of states, and every state transition must be persisted so that a restarted runtime can recover without losing track of what is running. This lesson builds a container lifecycle manager: a state machine backed by atomic JSON writes, with process monitoring, signal forwarding, and graceful shutdown.

The hard parts are not the state machine itself but the failure modes: a crash between state writes, a signal that arrives before the process has started, a process that ignores SIGTERM, and a PID that the kernel has recycled by the time the runtime restarts.

```text
lifecycle/
  go.mod
  lifecycle.go           portable state machine and persistence
  process_linux.go       Start, Stop, Kill, RecoverState (Linux only)
  lifecycle_test.go      table-driven tests for the portable layer
  cmd/demo/main.go       CLI exercising the full API (requires Linux + root)
```

## Concepts

### The OCI Container State Machine

The OCI Runtime Specification defines five container states. Each transition is a discrete, observable event:

```
(none) -> creating -> created -> running -> stopped -> removed
```

- **creating**: The runtime is setting up namespaces, cgroup hierarchies, and the overlay mount. The container process has not started.
- **created**: Setup is complete. The container exists on disk and may be inspected or deleted without ever starting.
- **running**: The container process (pid 1 inside the PID namespace) is alive.
- **stopped**: The process has exited — by its own decision, a received signal, or OOM kill. The state directory still exists; exit code and timestamps are readable.
- **removed**: The state directory has been deleted; the container no longer exists in the runtime's view.

Backward transitions are illegal. A created container cannot return to creating. A stopped container cannot be restarted; create a new one instead.

### Atomic State Persistence

The runtime must survive crashes. If a state write is interrupted mid-way, the next restart must not read a partial JSON file.

The standard technique is write-then-rename:

1. Write the new state to `state.json.tmp` in the same directory (same filesystem).
2. Call `os.Rename(tmp, stateFile)`. On POSIX systems this is atomic: readers see either the old file or the new one, never a partial intermediate.

A crash between `os.Create` and `os.Rename` leaves a `.tmp` artifact. On restart the runtime can detect and discard these orphans without corrupting state.

### PID Tracking and Crash Recovery

A container's init PID is written to `state.json` as soon as `exec.Cmd.Start()` returns. On a subsequent restart, the runtime checks each container in state `running` by calling `syscall.Kill(pid, 0)`. Signal zero is not delivered; it only checks whether the process exists. If the syscall returns `syscall.ESRCH` (no such process), the PID has exited or been recycled, and the container is marked stopped.

There is a known race: on a heavily loaded system, a new process could be assigned the same PID before the runtime restarts. This is the same race that affects any Unix daemon manager. The OCI spec accepts it; production runtimes mitigate it by writing a pidfile inside the PID namespace (`/proc/self`) and cross-checking.

### Signal Forwarding and Graceful Shutdown

When the user sends `SIGTERM` or `SIGINT` to the runtime's `stop` command, the runtime must forward the signal to the container's init process, not consume it internally. Direct forwarding uses `syscall.Kill(pid, syscall.SIGTERM)`. The runtime then waits for a configurable grace period (OCI default: 10 seconds) before escalating to `SIGKILL`.

Two critical details:

1. PID 1 inside a PID namespace ignores `SIGTERM` by default (many shells do this). Containers that must respond to SIGTERM need an explicit signal handler in their entrypoint, or a shim such as `tini` as their init.
2. Sending a signal to `pid` targets the host PID, which is the correct way to signal the container process from the parent. Do not confuse this with the process's view of its own PID (always 1 inside a new PID namespace).

### Log Capture and Retrieval

Container stdout and stderr are redirected to a log file by setting `exec.Cmd.Stdout` and `exec.Cmd.Stderr` to an `*os.File` pointing to `container.log` in the state directory. After `cmd.Start()` returns, the parent closes its copy of the file descriptor; the child retains the inherited fd through the namespace fork. Log files persist after the container exits and are readable via a `logs` subcommand.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/38-capstone-container-runtime/07-container-lifecycle/07-container-lifecycle/cmd/demo
cd go-solutions/38-capstone-container-runtime/07-container-lifecycle/07-container-lifecycle
```

### Exercise 1: State Machine and Persistence (lifecycle.go)

Create `lifecycle.go`. This file is fully portable — it uses only `encoding/json`, `crypto/rand`, and `os` from the standard library.

```go
package lifecycle

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is the container's position in the OCI lifecycle.
// Transitions are strictly forward: creating -> created -> running ->
// stopped. The removed state is reached by deleting the state directory.
type State string

const (
	StateCreating State = "creating"
	StateCreated  State = "created"
	StateRunning  State = "running"
	StateStopped  State = "stopped"
)

// Sentinel errors for state machine and lookup failures.
var (
	ErrNotFound       = errors.New("container not found")
	ErrNotCreated     = errors.New("container must be in created state to start")
	ErrAlreadyRunning = errors.New("container is already running")
	ErrNotRunning     = errors.New("container is not running")
	ErrStillRunning   = errors.New("container is running; stop or use --force")
)

// Config is the immutable container specification written at create time.
type Config struct {
	Image      string   `json:"image"`       // e.g. "alpine:3.19"
	Args       []string `json:"args"`        // process and arguments
	Env        []string `json:"env"`         // "KEY=VALUE" pairs
	WorkingDir string   `json:"working_dir"` // process working directory
}

// Record is the mutable container state, persisted as state.json.
// Fields are written atomically (write-to-tmp then rename) so a crash
// between writes cannot produce a torn file.
type Record struct {
	ID         string    `json:"id"`
	Status     State     `json:"status"`
	PID        int       `json:"pid,omitempty"`
	ExitCode   int       `json:"exit_code"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Config     Config    `json:"config"`
}

// Manager is a thread-safe container lifecycle manager. State is persisted
// under root (typically /var/run/mycontainer) as one subdirectory per
// container, each containing state.json and container.log.
type Manager struct {
	root string
	mu   sync.RWMutex
}

// NewManager opens (or creates) a Manager rooted at dir.
func NewManager(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("lifecycle: create root %q: %w", dir, err)
	}
	return &Manager{root: dir}, nil
}

// Root returns the state root directory.
func (m *Manager) Root() string { return m.root }

// LogFile returns the path to the container's stdout+stderr log.
func (m *Manager) LogFile(id string) string {
	return filepath.Join(m.root, id, "container.log")
}

func (m *Manager) containerDir(id string) string {
	return filepath.Join(m.root, id)
}

func (m *Manager) stateFile(id string) string {
	return filepath.Join(m.containerDir(id), "state.json")
}

// writeRecord serializes r to disk atomically using write-then-rename.
func (m *Manager) writeRecord(r Record) error {
	dir := m.containerDir(r.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("lifecycle: mkdir %q: %w", dir, err)
	}
	tmp := m.stateFile(r.ID) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("lifecycle: create tmp: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	encErr := enc.Encode(r)
	closeErr := f.Close()
	if encErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("lifecycle: encode state: %w", encErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("lifecycle: close tmp: %w", closeErr)
	}
	if err := os.Rename(tmp, m.stateFile(r.ID)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("lifecycle: commit state: %w", err)
	}
	return nil
}

// readRecord parses the state file for the given container.
func (m *Manager) readRecord(id string) (Record, error) {
	f, err := os.Open(m.stateFile(id))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Record{}, fmt.Errorf("lifecycle: open state %q: %w", id, err)
	}
	defer f.Close()
	var r Record
	if err := json.NewDecoder(f).Decode(&r); err != nil {
		return Record{}, fmt.Errorf("lifecycle: decode state %q: %w", id, err)
	}
	return r, nil
}

// newID generates a 12-hex-character container ID using crypto/rand.
func newID() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("lifecycle: generate id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Create prepares a new container from cfg without starting a process.
// The container transitions (none) -> creating -> created; both states
// are written to disk so that a crash leaves a recoverable record.
func (m *Manager) Create(cfg Config) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, err := newID()
	if err != nil {
		return "", err
	}
	r := Record{
		ID:        id,
		Status:    StateCreating,
		CreatedAt: time.Now().UTC(),
		Config:    cfg,
	}
	if err := m.writeRecord(r); err != nil {
		return "", err
	}
	r.Status = StateCreated
	if err := m.writeRecord(r); err != nil {
		return "", err
	}
	return id, nil
}

// Inspect returns a snapshot of the current state of container id.
func (m *Manager) Inspect(id string) (Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.readRecord(id)
}

// List returns all container records in the state root.
// Containers in any state (including stopped) are returned.
// Partially-written or corrupted state files are silently skipped.
func (m *Manager) List() ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries, err := os.ReadDir(m.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lifecycle: readdir %q: %w", m.root, err)
	}
	records := make([]Record, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := m.readRecord(e.Name())
		if err != nil {
			continue
		}
		records = append(records, r)
	}
	return records, nil
}

// Remove deletes the container's state directory. If the container is in
// StateRunning and force is false, Remove returns ErrStillRunning. With
// force=true the state directory is removed unconditionally; callers
// should call Kill before Remove to avoid leaving the process orphaned.
func (m *Manager) Remove(id string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, err := m.readRecord(id)
	if err != nil {
		return err
	}
	if r.Status == StateRunning && !force {
		return fmt.Errorf("%w: %s", ErrStillRunning, id)
	}
	if err := os.RemoveAll(m.containerDir(id)); err != nil {
		return fmt.Errorf("lifecycle: remove %q: %w", id, err)
	}
	return nil
}

// updateStatus applies update to the stored record for id.
// Callers must hold m.mu (write lock).
func (m *Manager) updateStatus(id string, update func(*Record)) error {
	r, err := m.readRecord(id)
	if err != nil {
		return err
	}
	update(&r)
	return m.writeRecord(r)
}
```

Defaults after `Create`: `Status = StateCreated`, `PID = 0`, `ExitCode = 0`. The two-step write (creating then created) means a crash during directory setup leaves a `state.json` in `StateCreating`. These orphans are not removed automatically; at startup a production runtime would scan for containers stuck in `StateCreating` and delete their state directories. The recovery pass in Exercise 2 handles only `StateRunning` containers whose PID has disappeared.

### Exercise 2: Process Management (process_linux.go)

Create `process_linux.go`. The `//go:build linux` constraint excludes this file on other platforms.

```go
//go:build linux

package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Start launches the container process inside new Linux namespaces.
// The container must be in StateCreated; Start transitions it to StateRunning
// and records the init PID. Container stdout and stderr are redirected to
// LogFile, which persists after the process exits.
//
// Namespace flags:
//   - CLONE_NEWUTS: isolate hostname and NIS domain name
//   - CLONE_NEWPID: new PID namespace (process becomes pid 1 inside)
//   - CLONE_NEWIPC: isolate System V IPC and POSIX message queues
//   - CLONE_NEWNS:  new mount namespace
//
// Network namespace setup is in exercise 03; user namespace and cgroup v2
// resource limits are in exercise 04.
func (m *Manager) Start(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, err := m.readRecord(id)
	if err != nil {
		return err
	}
	if r.Status != StateCreated {
		return fmt.Errorf("%w: %s is %s", ErrNotCreated, id, r.Status)
	}
	if len(r.Config.Args) == 0 {
		return fmt.Errorf("lifecycle: container %s has no command", id)
	}

	logPath := m.LogFile(id)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("lifecycle: open log %q: %w", logPath, err)
	}

	cmd := exec.Command(r.Config.Args[0], r.Config.Args[1:]...)
	cmd.Env = r.Config.Env
	cmd.Dir = r.Config.WorkingDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWIPC |
			syscall.CLONE_NEWNS,
	}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("lifecycle: start process: %w", err)
	}
	// The child inherited the fd on fork; the parent no longer needs it.
	_ = logFile.Close()

	r.Status = StateRunning
	r.PID = cmd.Process.Pid
	r.StartedAt = time.Now().UTC()
	if err := m.writeRecord(r); err != nil {
		// State write failed; kill the process to avoid an untracked orphan.
		_ = cmd.Process.Kill()
		return err
	}

	// Monitor the container process and update state when it exits.
	go func() {
		_ = cmd.Wait()
		exitCode := 0
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			if ws.Exited() {
				exitCode = ws.ExitStatus()
			} else if ws.Signaled() {
				// Convention: signal death encodes as 128 + signal number.
				exitCode = 128 + int(ws.Signal())
			}
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		_ = m.updateStatus(id, func(rec *Record) {
			rec.Status = StateStopped
			rec.PID = 0
			rec.ExitCode = exitCode
			rec.FinishedAt = time.Now().UTC()
		})
	}()

	return nil
}

// Stop gracefully stops the container: sends SIGTERM, waits up to timeout,
// then sends SIGKILL. The OCI spec recommends a default timeout of 10 seconds.
func (m *Manager) Stop(id string, timeout time.Duration) error {
	m.mu.RLock()
	r, err := m.readRecord(id)
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	if r.Status != StateRunning {
		return fmt.Errorf("%w: %s", ErrNotRunning, id)
	}

	if err := syscall.Kill(r.PID, syscall.SIGTERM); err != nil && !isESRCH(err) {
		return fmt.Errorf("lifecycle: SIGTERM pid %d: %w", r.PID, err)
	}

	// Poll until the container transitions out of StateRunning or the
	// grace period expires.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		m.mu.RLock()
		rec, readErr := m.readRecord(id)
		m.mu.RUnlock()
		if readErr != nil || rec.Status != StateRunning {
			return nil
		}
	}

	// Grace period expired; escalate to SIGKILL.
	return m.Kill(id)
}

// Kill sends SIGKILL to the container's init process immediately.
func (m *Manager) Kill(id string) error {
	m.mu.RLock()
	r, err := m.readRecord(id)
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	if r.Status != StateRunning {
		return fmt.Errorf("%w: %s", ErrNotRunning, id)
	}
	if err := syscall.Kill(r.PID, syscall.SIGKILL); err != nil && !isESRCH(err) {
		return fmt.Errorf("lifecycle: SIGKILL pid %d: %w", r.PID, err)
	}
	return nil
}

// RecoverState reconciles persisted state with kernel reality.
// For each container in StateRunning, it checks whether the stored PID
// is still alive via kill(pid, 0). If not, the container is marked stopped.
// Call RecoverState once at runtime startup before accepting any requests.
func (m *Manager) RecoverState() error {
	records, err := m.List()
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Status != StateRunning {
			continue
		}
		if pidAlive(r.PID) {
			continue
		}
		m.mu.Lock()
		_ = m.updateStatus(r.ID, func(rec *Record) {
			rec.Status = StateStopped
			rec.PID = 0
			rec.FinishedAt = time.Now().UTC()
		})
		m.mu.Unlock()
	}
	return nil
}

// pidAlive returns true if the process with the given PID is running.
// kill(pid, 0) does not deliver a signal; it only checks for process existence.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// isESRCH reports whether the error from kill(2) means the target
// process is gone (ESRCH: no such process or process group).
func isESRCH(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}
```

### Exercise 3: Test the Contract (lifecycle_test.go)

Create `lifecycle_test.go`. These tests cover the portable state machine and persistence layer and run on any platform. The `package lifecycle` declaration allows access to unexported helpers (`newID`, `readRecord`, `writeRecord`, `containerDir`).

```go
package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// newTestManager creates a Manager backed by a temporary directory that is
// automatically cleaned up when the test ends.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestNewID(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for range 50 {
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 12 {
			t.Errorf("id length = %d, want 12", len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Errorf("id %q contains non-hex character %q", id, c)
			}
		}
		if seen[id] {
			t.Errorf("collision: duplicate id %q in 50 samples", id)
		}
		seen[id] = true
	}
}

func TestCreateAndInspect(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	cfg := Config{
		Image: "alpine:3.19",
		Args:  []string{"/bin/sh", "-c", "echo hello"},
		Env:   []string{"PATH=/usr/local/bin:/usr/bin:/bin"},
	}

	id, err := m.Create(cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(id) != 12 {
		t.Errorf("id length = %d, want 12", len(id))
	}

	r, err := m.Inspect(id)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if r.Status != StateCreated {
		t.Errorf("Status = %s, want %s", r.Status, StateCreated)
	}
	if r.ID != id {
		t.Errorf("ID = %s, want %s", r.ID, id)
	}
	if r.Config.Image != "alpine:3.19" {
		t.Errorf("Config.Image = %q, want %q", r.Config.Image, "alpine:3.19")
	}
	if r.CreatedAt.IsZero() {
		t.Error("CreatedAt must be set")
	}
	if r.PID != 0 {
		t.Errorf("PID = %d, want 0 (not started)", r.PID)
	}
}

func TestInspectNotFound(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	_, err := m.Inspect("nonexistent12")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Inspect missing container: err = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	const n = 3
	ids := make(map[string]bool, n)
	for range n {
		id, err := m.Create(Config{Image: "alpine:3.19", Args: []string{"/bin/sh"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids[id] = true
	}

	records, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != n {
		t.Errorf("List returned %d records, want %d", len(records), n)
	}
	for _, r := range records {
		if !ids[r.ID] {
			t.Errorf("List returned unexpected id %s", r.ID)
		}
	}
}

func TestListEmptyDir(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	records, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("List on empty manager returned %d records, want 0", len(records))
	}
}

func TestRemoveStopped(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	id, err := m.Create(Config{Image: "alpine:3.19", Args: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Remove(id, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, err = m.Inspect(id)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Inspect after Remove: err = %v, want ErrNotFound", err)
	}
}

func TestRemoveRunningWithoutForce(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	id, err := m.Create(Config{Image: "alpine:3.19", Args: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}

	// Inject a running record directly (the process itself is not started).
	r, _ := m.readRecord(id)
	r.Status = StateRunning
	r.PID = 99999
	if err := m.writeRecord(r); err != nil {
		t.Fatal(err)
	}

	err = m.Remove(id, false)
	if !errors.Is(err, ErrStillRunning) {
		t.Errorf("Remove(force=false) on running container: err = %v, want ErrStillRunning", err)
	}
}

func TestRemoveRunningWithForce(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	id, err := m.Create(Config{Image: "alpine:3.19", Args: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}

	// Inject a running record.
	r, _ := m.readRecord(id)
	r.Status = StateRunning
	r.PID = 99999
	if err := m.writeRecord(r); err != nil {
		t.Fatal(err)
	}

	if err := m.Remove(id, true); err != nil {
		t.Fatalf("Remove(force=true): %v", err)
	}
	_, err = m.Inspect(id)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Inspect after force Remove: err = %v, want ErrNotFound", err)
	}
}

func TestAtomicWriteNoTmpLeftover(t *testing.T) {
	t.Parallel()

	m := newTestManager(t)
	id, _ := m.Create(Config{Image: "alpine:3.19", Args: []string{"/bin/sh"}})

	entries, err := os.ReadDir(m.containerDir(id))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("unexpected leftover tmp file: %s", e.Name())
		}
	}
}

func TestStateIsPersistedBetweenManagerInstances(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	m1, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := m1.Create(Config{Image: "busybox:1.36", Args: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a runtime restart by opening a new Manager over the same dir.
	m2, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := m2.Inspect(id)
	if err != nil {
		t.Fatalf("Inspect after restart: %v", err)
	}
	if r.Status != StateCreated {
		t.Errorf("Status after restart = %s, want %s", r.Status, StateCreated)
	}
	if r.Config.Image != "busybox:1.36" {
		t.Errorf("Config.Image after restart = %q, want %q", r.Config.Image, "busybox:1.36")
	}
}

// ExampleManager_Create demonstrates creating a container record and reading
// back the initial state. The actual process is not started; use Start for
// that (Linux only).
func ExampleManager_Create() {
	dir, err := os.MkdirTemp("", "lifecycle-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(dir)

	m, err := NewManager(dir)
	if err != nil {
		return
	}
	id, err := m.Create(Config{
		Image: "alpine:3.19",
		Args:  []string{"/bin/sh", "-c", "echo hello"},
	})
	if err != nil {
		return
	}
	r, err := m.Inspect(id)
	if err != nil {
		return
	}
	fmt.Println(r.Status)
	fmt.Println(r.Config.Image)
	// Output:
	// created
	// alpine:3.19
}
```

Your turn: add `TestCreatePreservesEnv` that calls `Create` with `Env: []string{"HOME=/root", "TERM=xterm"}` and asserts that `Inspect` returns the same env values after the JSON round-trip.

### Exercise 4: Command-Line Demo (cmd/demo/main.go)

Create `cmd/demo/main.go`. The `//go:build linux` constraint is required because the demo calls `Start`, `Stop`, and `Kill`, which use Linux namespace syscalls. Run as root.

```go
//go:build linux

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"text/tabwriter"
	"time"

	"example.com/lifecycle"
)

func main() {
	root := flag.String("root", "/var/run/mycontainer", "state root directory")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: demo [-root DIR] <command> [args]")
		fmt.Fprintln(os.Stderr, "commands: create start stop kill rm ps inspect logs recover")
		os.Exit(1)
	}

	m, err := lifecycle.NewManager(*root)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}

	switch flag.Arg(0) {
	case "create":
		// demo create <image> <cmd> [args...]
		if flag.NArg() < 3 {
			log.Fatal("usage: demo create <image> <cmd> [args...]")
		}
		id, err := m.Create(lifecycle.Config{
			Image: flag.Arg(1),
			Args:  flag.Args()[2:],
			Env:   []string{"PATH=/usr/local/bin:/usr/bin:/bin"},
		})
		if err != nil {
			log.Fatalf("create: %v", err)
		}
		fmt.Println(id)

	case "start":
		if flag.NArg() < 2 {
			log.Fatal("usage: demo start <id>")
		}
		if err := m.Start(flag.Arg(1)); err != nil {
			log.Fatalf("start: %v", err)
		}

	case "stop":
		if flag.NArg() < 2 {
			log.Fatal("usage: demo stop <id>")
		}
		if err := m.Stop(flag.Arg(1), 10*time.Second); err != nil {
			log.Fatalf("stop: %v", err)
		}

	case "kill":
		if flag.NArg() < 2 {
			log.Fatal("usage: demo kill <id>")
		}
		if err := m.Kill(flag.Arg(1)); err != nil {
			log.Fatalf("kill: %v", err)
		}

	case "rm":
		if flag.NArg() < 2 {
			log.Fatal("usage: demo rm [-f] <id>")
		}
		force := false
		for _, arg := range flag.Args()[1 : flag.NArg()-1] {
			if arg == "-f" || arg == "--force" {
				force = true
			}
		}
		id := flag.Arg(flag.NArg() - 1)
		if err := m.Remove(id, force); err != nil {
			log.Fatalf("rm: %v", err)
		}

	case "ps":
		records, err := m.List()
		if err != nil {
			log.Fatalf("ps: %v", err)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tIMAGE\tSTATUS\tCREATED")
		for _, r := range records {
			age := time.Since(r.CreatedAt).Round(time.Second)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s ago\n",
				r.ID, r.Config.Image, r.Status, age)
		}
		_ = w.Flush()

	case "inspect":
		if flag.NArg() < 2 {
			log.Fatal("usage: demo inspect <id>")
		}
		r, err := m.Inspect(flag.Arg(1))
		if err != nil {
			log.Fatalf("inspect: %v", err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)

	case "logs":
		if flag.NArg() < 2 {
			log.Fatal("usage: demo logs <id>")
		}
		logPath := m.LogFile(flag.Arg(1))
		data, err := os.ReadFile(logPath)
		if err != nil {
			log.Fatalf("logs: %v", err)
		}
		_, _ = os.Stdout.Write(data)

	case "recover":
		if err := m.RecoverState(); err != nil {
			log.Fatalf("recover: %v", err)
		}
		fmt.Println("state reconciled")

	default:
		log.Fatalf("unknown command: %s", flag.Arg(0))
	}
}
```

Example session on Linux (requires root and a prepared rootfs from exercises 01-06):

```bash
# Build the CLI
GOARCH=amd64 GOOS=linux go build -o demo ./cmd/demo

# Create a container record (does not start the process)
sudo ./demo -root /var/run/mycontainer create alpine:3.19 \
    /bin/sh -c 'while true; do echo alive; sleep 5; done'
# a12f3c8b4e91

# List containers
sudo ./demo -root /var/run/mycontainer ps
# ID             IMAGE        STATUS   CREATED
# a12f3c8b4e91  alpine:3.19  created  1s ago

# Start the container (namespace isolation + log capture)
sudo ./demo -root /var/run/mycontainer start a12f3c8b4e91

# Inspect the running record
sudo ./demo -root /var/run/mycontainer inspect a12f3c8b4e91
# {
#   "id": "a12f3c8b4e91",
#   "status": "running",
#   "pid": 14302,
#   ...
# }

# Stop gracefully (SIGTERM, 10s grace, then SIGKILL)
sudo ./demo -root /var/run/mycontainer stop a12f3c8b4e91

# Read the captured log
sudo ./demo -root /var/run/mycontainer logs a12f3c8b4e91

# Remove the container record
sudo ./demo -root /var/run/mycontainer rm a12f3c8b4e91

# After a crash, reconcile persisted state with running processes
sudo ./demo -root /var/run/mycontainer recover
```

## Common Mistakes

### Overwriting State Without an Atomic Rename

Wrong: writing new JSON directly to `state.json`.

```go
// Wrong: a crash here leaves state.json half-written
os.WriteFile(stateFile, data, 0o600)
```

What happens: the kernel can interrupt a write mid-page. The next read produces a JSON parse error and the container is stuck in an unrecoverable state.

Fix: write to a `.tmp` file in the same directory, then rename:

```go
// Fix: rename is atomic on POSIX; readers see old or new, never partial
os.Rename(tmpFile, stateFile)
```

### Using /proc to Check Liveness Instead of Signal Zero

Wrong: checking whether a container is alive with `os.Stat("/proc/" + strconv.Itoa(pid))`.

What happens: the stat succeeds for zombie processes — processes that have exited but whose wait status has not been collected. A container may appear alive in the stat check while it is actually dead.

Fix: use `syscall.Kill(pid, 0)`. Signal 0 is not delivered; the kernel returns nil if the process exists and is not a zombie that belongs to a different user. ESRCH means the process is gone.

### Removing a Running Container Without Killing the Process First

Wrong: calling `Remove(id, true)` without a prior `Kill` or `Stop`.

What happens: the state directory is deleted, but the container process continues to run. It is now untracked — it will keep running until it exits or the host reboots. Its PID may be recycled by the kernel and assigned to an unrelated process, causing a false liveness check on the next restart.

Fix: stop or kill the container before removing it:

```go
// Fix: stop first, then remove
if err := m.Stop(id, 10*time.Second); err != nil {
	log.Fatalf("stop: %v", err)
}
if err := m.Remove(id, false); err != nil {
	log.Fatalf("rm: %v", err)
}
```

### Ignoring the Signal Convention for Exit Codes

Wrong: using `cmd.ProcessState.ExitCode()` to record the container exit code when the process was killed by a signal.

What happens: `ExitCode()` returns -1 for signal-killed processes. Tools that inspect the exit code (like a CI system checking container results) see -1 instead of the expected convention.

Fix: use `syscall.WaitStatus` to distinguish normal exit from signal death:

```go
// Fix: encode signal death as 128 + signal number (shell convention)
exitCode := 0
if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
	if ws.Exited() {
		exitCode = ws.ExitStatus()
	} else if ws.Signaled() {
		exitCode = 128 + int(ws.Signal())
	}
}
```

### PID 1 Ignoring SIGTERM

Wrong: relying on the default SIGTERM behavior when the container runs a shell as pid 1.

What happens: most shells (bash, sh) ignore SIGTERM when they are pid 1 inside a PID namespace. `Stop` sends SIGTERM, waits the full grace period, and then sends SIGKILL — the container is force-killed rather than cleanly shut down.

Fix: install an explicit trap in the container entrypoint, or use `tini` (a minimal init process that forwards signals and reaps zombies) as the container's init:

```bash
# Use tini as the container init in the create command
demo create alpine:3.19 /sbin/tini -- /bin/sh -c 'while true; do sleep 1; done'
```

## Verification

The portable layer (`lifecycle.go` + `lifecycle_test.go`) gates on any platform. The process management layer (`process_linux.go`) and the CLI (`cmd/demo`) require Linux and root access.

From `~/go-exercises/lifecycle`:

```bash
# 1. Format check — must print nothing
test -z "$(gofmt -l .)"

# 2. Static analysis
go vet ./...

# 3. Portable tests (any platform)
go test -count=1 -race ./...

# 4. Build the CLI (cross-compile from macOS, run on Linux)
GOARCH=amd64 GOOS=linux go build ./cmd/demo
```

For the full integration test on Linux with a rootfs prepared by exercises 01-06:

```bash
CID=$(sudo ./demo create alpine:3.19 /bin/sh -c 'while true; do echo alive; sleep 1; done')
sudo ./demo start "$CID"

# Confirm running
sudo ./demo inspect "$CID" | grep '"status"'

# Kill the runtime process (simulates a crash) and verify recovery
sudo kill -9 <runtime-pid>
sudo ./demo recover
sudo ./demo inspect "$CID" | grep '"status"'
# "status": "stopped"

sudo ./demo rm "$CID"
```

## Summary

- The OCI container lifecycle is a forward-only state machine: creating -> created -> running -> stopped; state files are deleted on remove.
- Atomic persistence (write-to-tmp, rename) ensures a crashed runtime leaves a recoverable record, never a torn JSON file.
- Crash recovery reads the saved PID and uses `kill(pid, 0)` (signal zero) to determine whether the process is still alive; containers with dead PIDs are marked stopped.
- `Stop` sends SIGTERM, waits a configurable grace period, then escalates to SIGKILL; the container's init process must handle SIGTERM (or use a shim like `tini`).
- Log capture uses `exec.Cmd.Stdout` and `exec.Cmd.Stderr` pointing to a log file; the parent closes its fd after `cmd.Start()` and the child retains it through the namespace fork.
- `Remove(id, true)` deletes the state directory unconditionally; always call `Kill` or `Stop` first to avoid leaving orphaned processes.

## What's Next

Next: [Exec into Running Container](../08-exec-into-running-container/08-exec-into-running-container.md).

## Resources

- [OCI Runtime Specification: Lifecycle](https://github.com/opencontainers/runtime-spec/blob/main/runtime.md#lifecycle) - canonical container state transitions and required operations
- [OCI Runtime Specification: State](https://github.com/opencontainers/runtime-spec/blob/main/runtime.md#state) - the exact fields a runtime must track per container
- [pkg.go.dev: os/exec](https://pkg.go.dev/os/exec) - `Cmd`, `SysProcAttr`, `Cmd.Start`, `Cmd.Wait`, `ProcessState`
- [pkg.go.dev: syscall](https://pkg.go.dev/syscall) - `Kill`, `CLONE_NEW*` flags, `WaitStatus`, `ESRCH`
- [runc libcontainer: state.go](https://github.com/opencontainers/runc/blob/main/libcontainer/state_linux.go) - production OCI runtime state machine implementation in Go
