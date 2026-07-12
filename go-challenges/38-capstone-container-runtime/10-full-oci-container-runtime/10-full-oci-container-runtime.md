# 10. Full OCI Container Runtime

The nine previous exercises built the individual components: namespaces, pivot_root, cgroups, overlay mounts, image pulling, container lifecycle, exec, and bridge networking. This exercise assembles them into a single, standards-compliant tool. The hard parts are not new syscalls -- they are the OCI contract (a JSON schema that every piece of tooling depends on), the state machine whose invariants must survive crashes, the security model (five independent capability sets plus a seccomp-BPF filter), and the hook protocol (arbitrary executables called at precise lifecycle points with a strictly enforced timeout). Get any of these wrong and higher-level tools (containerd, Podman, crictl) cannot use the runtime.

```text
oci-runtime/
  go.mod
  runtime/
    spec.go              -- OCI config.json types and parser
    state.go             -- container state machine and persistence
    hooks.go             -- lifecycle hook runner
    security_linux.go    -- capability dropping and seccomp (//go:build linux)
    runtime_test.go      -- unit tests (pure stdlib, runs on any Linux host)
  cmd/oci-runtime/
    main.go              -- CLI: create | start | kill | delete | state
```

The `runtime` package implements the portable subset: config parsing, state transitions, and hook execution. The `//go:build linux` security file and the actual container spawning code require `golang.org/x/sys/unix` (an external module) and Linux-specific syscalls; they are not compiled offline.

## Concepts

### The OCI Bundle: config.json and rootfs

The OCI Runtime Specification defines a bundle as a directory containing two items:

1. `config.json` -- the container configuration file.
2. A root filesystem directory (its path given by `root.path` in `config.json`).

`config.json` is a JSON document versioned with `ociVersion`. It specifies the process to run, the filesystem configuration, Linux namespace types, cgroup resource limits, security profiles, additional mounts, and lifecycle hooks. Every OCI-compliant runtime must read this file and create a container matching the specification.

A minimal `config.json` for an Alpine-based container looks like this:

```json
{
  "ociVersion": "1.0.2",
  "process": {
    "args": ["/bin/sh"],
    "cwd": "/"
  },
  "root": {
    "path": "rootfs",
    "readonly": true
  },
  "linux": {
    "namespaces": [
      {"type": "pid"},
      {"type": "mount"},
      {"type": "uts"},
      {"type": "ipc"},
      {"type": "network"}
    ]
  }
}
```

### The Container State Machine

The OCI spec defines four container states and the valid transitions between them:

```
  (initial)
      |
      v create
  creating --> created --> running --> stopped
                    |                     ^
                    +---------------------+
                      (process exits immediately)
```

The states are:

| State | Meaning |
|---|---|
| `creating` | `create` has been called; namespaces are being set up. |
| `created` | `create` has completed; the user process has not started. |
| `running` | `start` has been called; the user process is executing. |
| `stopped` | The user process has exited. |

The `state` command must write JSON to stdout in this exact format:

```json
{
  "ociVersion": "1.0.2",
  "id": "mycontainer",
  "status": "running",
  "pid": 12345,
  "bundle": "/var/lib/oci/bundles/mycontainer"
}
```

State is persisted between calls. The canonical location is `/run/<runtime>/<id>/state.json`, following the runc convention.

### Security Hardening: Capabilities and Seccomp

Linux capabilities divide traditional root privilege into 64 distinct rights. A container process runs with a drastically reduced set -- typically just what a web server or application needs, not the ability to load kernel modules or reboot the machine.

There are five capability sets per process:

| Set | Meaning |
|---|---|
| `bounding` | Upper bound; no capability can ever exceed this set. |
| `permitted` | Capabilities the process is currently allowed to use. |
| `effective` | Capabilities currently in effect (subset of permitted). |
| `inheritable` | Capabilities that survive `execve`. |
| `ambient` | Inherited across `execve` without needing `inheritable`. |

The correct hardening sequence is:

1. Clear the ambient set via `PR_CAP_AMBIENT_CLEAR_ALL`.
2. Drop from the bounding set every capability not in the spec's `bounding` list, via `PR_CAPBSET_DROP` for each capability number.
3. Set permitted and effective to the intersection of the bounding set and the spec's requested sets.

Seccomp-BPF adds a syscall filter expressed as a Berkeley Packet Filter program. Every syscall is checked against the filter at entry; blocked calls receive `ENOSYS` or cause the process to be killed. A minimal default filter blocks the most dangerous syscalls (`kexec_load`, `reboot`, `init_module`, `delete_module`) while allowing everything else.

### Lifecycle Hooks

OCI hooks are executables called at specific points in the container lifecycle. The runtime writes the current container state as JSON to the hook's stdin. Each hook has an optional timeout in seconds; the runtime must kill the hook process and return an error if it exceeds this limit.

The six hook points, in execution order:

1. `createRuntime` -- after namespaces are created, before `pivot_root`.
2. `createContainer` -- after `pivot_root`, before the user process.
3. `startContainer` -- immediately before `exec` of the user process.
4. `poststart` -- after the user process starts.
5. `poststop` -- after the container process exits.
6. `prestart` -- deprecated in OCI 1.0.2; accepted for backward compatibility.

### Integrating All Components

The `create` operation sequence:

1. Read and validate `config.json`.
2. Write `status: creating` to `state.json` before spawning any child.
3. Fork a child with `Cloneflags` set for the requested namespaces.
4. In the child: set up mounts, `pivot_root` into rootfs, drop capabilities, apply seccomp.
5. Run `createRuntime` and `createContainer` hooks.
6. Write `status: created` and the child's PID to `state.json`.
7. The child blocks on a pipe, waiting for `start`.

The `start` operation:

1. Load state; assert status is `created`.
2. Run `startContainer` hooks.
3. Write to the pipe to unblock the child, which then calls `syscall.Exec` for the user process.
4. Write `status: running` to `state.json`.
5. Run `poststart` hooks.

## Exercises

The `runtime` package is verified with `go test`. The CLI is verified with `go build ./cmd/oci-runtime`.

### Exercise 1: OCI Config Types and Parser

Create `runtime/spec.go`:

```go
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrMissingOCIVersion is returned when config.json has no ociVersion field.
var ErrMissingOCIVersion = errors.New("oci: config.json missing ociVersion")

// ErrInvalidTransition is returned when a state transition is not allowed.
var ErrInvalidTransition = errors.New("oci: invalid state transition")

// ErrHookTimeout is returned when a lifecycle hook exceeds its timeout.
var ErrHookTimeout = errors.New("oci: hook timed out")

// Spec is the top-level OCI runtime configuration read from config.json.
// The schema is defined in the OCI Runtime Specification §4.
type Spec struct {
	// OCIVersion is the OCI Runtime Spec version (e.g., "1.0.2").
	OCIVersion string `json:"ociVersion"`
	// Process describes the container process.
	Process *Process `json:"process,omitempty"`
	// Root describes the container's root filesystem.
	Root *Root `json:"root,omitempty"`
	// Mounts is the list of additional mounts inside the container.
	Mounts []Mount `json:"mounts,omitempty"`
	// Linux contains Linux-specific configuration.
	Linux *Linux `json:"linux,omitempty"`
	// Hooks defines lifecycle hook executables.
	Hooks *Hooks `json:"hooks,omitempty"`
}

// Process describes the container process configuration.
type Process struct {
	// Terminal specifies whether to attach a pseudo-terminal.
	Terminal bool `json:"terminal,omitempty"`
	// Args holds the process path and its arguments.
	Args []string `json:"args"`
	// Env are environment variables for the process.
	Env []string `json:"env,omitempty"`
	// Cwd is the working directory for the process inside the container.
	Cwd string `json:"cwd"`
	// Capabilities sets the Linux capability sets for the process.
	Capabilities *LinuxCapabilities `json:"capabilities,omitempty"`
}

// Root describes the container's root filesystem.
type Root struct {
	// Path is the path to the rootfs directory, relative to the bundle.
	Path string `json:"path"`
	// Readonly makes the root filesystem read-only when true.
	Readonly bool `json:"readonly,omitempty"`
}

// Mount describes a mount point inside the container.
type Mount struct {
	// Destination is the absolute path of the mount inside the container.
	Destination string `json:"destination"`
	// Type is the filesystem type (e.g., "proc", "tmpfs", "bind").
	Type string `json:"type,omitempty"`
	// Source is the source path on the host or a device name.
	Source string `json:"source,omitempty"`
	// Options are mount options (e.g., ["rbind", "ro"]).
	Options []string `json:"options,omitempty"`
}

// Linux contains Linux-specific container configuration.
type Linux struct {
	// Namespaces lists the namespaces to create for the container.
	Namespaces []LinuxNamespace `json:"namespaces,omitempty"`
	// Resources sets cgroup resource limits.
	Resources *LinuxResources `json:"resources,omitempty"`
	// Seccomp configures the seccomp-BPF syscall filter.
	Seccomp *LinuxSeccomp `json:"seccomp,omitempty"`
	// UIDMappings maps host UIDs to container UIDs for user namespaces.
	UIDMappings []LinuxIDMapping `json:"uidMappings,omitempty"`
	// GIDMappings maps host GIDs to container GIDs for user namespaces.
	GIDMappings []LinuxIDMapping `json:"gidMappings,omitempty"`
	// MaskedPaths are paths inside the container hidden from reads (bind-mounted
	// over with /dev/null).
	MaskedPaths []string `json:"maskedPaths,omitempty"`
	// ReadonlyPaths are paths mounted read-only inside the container.
	ReadonlyPaths []string `json:"readonlyPaths,omitempty"`
}

// LinuxNamespace specifies a namespace type and an optional path to join.
type LinuxNamespace struct {
	// Type is the namespace type: "pid", "network", "mount", "uts", "ipc",
	// "user", or "cgroup".
	Type string `json:"type"`
	// Path is the path to an existing namespace file to join.
	// When empty, a new namespace of this type is created.
	Path string `json:"path,omitempty"`
}

// LinuxCapabilities sets the five POSIX capability sets for the process.
// Each field is a list of capability name strings (e.g., "CAP_NET_BIND_SERVICE").
type LinuxCapabilities struct {
	Bounding    []string `json:"bounding,omitempty"`
	Effective   []string `json:"effective,omitempty"`
	Inheritable []string `json:"inheritable,omitempty"`
	Permitted   []string `json:"permitted,omitempty"`
	Ambient     []string `json:"ambient,omitempty"`
}

// LinuxResources configures cgroup resource limits.
type LinuxResources struct {
	CPU    *LinuxCPU    `json:"cpu,omitempty"`
	Memory *LinuxMemory `json:"memory,omitempty"`
}

// LinuxCPU sets CPU cgroup v2 parameters.
type LinuxCPU struct {
	// Shares is cpu.weight in cgroup v2 (range 1-10000).
	Shares *uint64 `json:"shares,omitempty"`
	// Quota is the CPU bandwidth quota in microseconds per Period.
	Quota *int64 `json:"quota,omitempty"`
	// Period is the CPU bandwidth period in microseconds.
	Period *uint64 `json:"period,omitempty"`
}

// LinuxMemory sets memory cgroup parameters.
type LinuxMemory struct {
	// Limit is the maximum memory in bytes (memory.max in cgroup v2).
	Limit *int64 `json:"limit,omitempty"`
}

// LinuxSeccomp configures the seccomp-BPF syscall filter.
type LinuxSeccomp struct {
	// DefaultAction is applied to syscalls not matched by any Syscalls rule.
	// Common values: "SCMP_ACT_ALLOW", "SCMP_ACT_ERRNO", "SCMP_ACT_KILL".
	DefaultAction string `json:"defaultAction"`
	// Syscalls is the list of per-syscall rules, evaluated in order.
	Syscalls []LinuxSyscall `json:"syscalls,omitempty"`
}

// LinuxSyscall is one rule in the seccomp filter.
type LinuxSyscall struct {
	// Names is the list of syscall names this rule applies to.
	Names []string `json:"names"`
	// Action is the action for matched syscalls (e.g., "SCMP_ACT_ALLOW").
	Action string `json:"action"`
}

// LinuxIDMapping maps a contiguous range of host IDs to container IDs.
type LinuxIDMapping struct {
	ContainerID uint32 `json:"containerID"`
	HostID      uint32 `json:"hostID"`
	Size        uint32 `json:"size"`
}

// Hooks defines executables called at specific container lifecycle points.
// The OCI spec specifies execution order within create and start.
type Hooks struct {
	// Prestart hooks run after create but before the user process.
	// Deprecated in OCI 1.0.2; accepted for backward compatibility.
	Prestart []Hook `json:"prestart,omitempty"`
	// CreateRuntime hooks run after namespaces are set up, before pivot_root.
	CreateRuntime []Hook `json:"createRuntime,omitempty"`
	// CreateContainer hooks run after pivot_root, before the user process.
	CreateContainer []Hook `json:"createContainer,omitempty"`
	// StartContainer hooks run immediately before exec of the user process.
	StartContainer []Hook `json:"startContainer,omitempty"`
	// Poststart hooks run after the user process starts.
	Poststart []Hook `json:"poststart,omitempty"`
	// Poststop hooks run after the container process exits.
	Poststop []Hook `json:"poststop,omitempty"`
}

// Hook is a single lifecycle hook: an executable called with a timeout.
type Hook struct {
	// Path is the absolute path to the hook executable.
	Path string `json:"path"`
	// Args are passed to the hook; Args[0] is the executable name (argv[0]).
	Args []string `json:"args,omitempty"`
	// Env are additional environment variables for the hook process.
	Env []string `json:"env,omitempty"`
	// Timeout is the maximum number of seconds the hook may run.
	// When nil or zero, the runtime's default timeout applies.
	Timeout *int `json:"timeout,omitempty"`
}

// ParseSpec reads and validates an OCI config.json from raw JSON bytes.
// It returns ErrMissingOCIVersion when the ociVersion field is absent or empty.
func ParseSpec(data []byte) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("oci: parse config.json: %w", err)
	}
	if s.OCIVersion == "" {
		return nil, ErrMissingOCIVersion
	}
	return &s, nil
}
```

`ParseSpec` is the entry point for every OCI operation: `create`, `start`, `kill`, `delete`, and `state` all read the bundle's `config.json` via this function. Three sentinel errors are defined here so they can be shared by all files in the package: `ErrMissingOCIVersion`, `ErrInvalidTransition`, and `ErrHookTimeout`.

### Exercise 2: Container State Machine

Create `runtime/state.go`:

```go
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Status is the OCI-defined container status string.
type Status string

const (
	// StatusCreating means create has been called but has not yet completed.
	StatusCreating Status = "creating"
	// StatusCreated means the container is ready; the user process has not started.
	StatusCreated Status = "created"
	// StatusRunning means the user process is executing.
	StatusRunning Status = "running"
	// StatusStopped means the user process has exited.
	StatusStopped Status = "stopped"
)

// ContainerState is the JSON document written by the state command.
// The format is defined in OCI Runtime Spec §5.2.
type ContainerState struct {
	// OCIVersion is the OCI Runtime Spec version used for this state document.
	OCIVersion string `json:"ociVersion"`
	// ID is the container identifier supplied by the caller.
	ID string `json:"id"`
	// Status is the current container status.
	Status Status `json:"status"`
	// PID is the host PID of the container's init process.
	// Zero when status is creating, created, or stopped.
	PID int `json:"pid"`
	// Bundle is the absolute path to the OCI bundle directory.
	Bundle string `json:"bundle"`
}

// validTransitions maps each status to the statuses it may transition to.
// The OCI spec prohibits arbitrary transitions; this table encodes the rules.
var validTransitions = map[Status][]Status{
	StatusCreating: {StatusCreated},
	StatusCreated:  {StatusRunning, StatusStopped},
	StatusRunning:  {StatusStopped},
	StatusStopped:  {},
}

// Transition advances the container state to the given status.
// It returns ErrInvalidTransition when the transition is not in the allowed set.
func (cs *ContainerState) Transition(to Status) error {
	allowed, ok := validTransitions[cs.Status]
	if !ok {
		return fmt.Errorf("%w: unknown source status %q", ErrInvalidTransition, cs.Status)
	}
	for _, a := range allowed {
		if a == to {
			cs.Status = to
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, cs.Status, to)
}

// stateDir returns the directory used to persist state for the given container ID.
// Following the runc convention, state lives under /run/<runtime>/<id>/.
func stateDir(runtimeName, id string) string {
	return filepath.Join("/run", runtimeName, id)
}

// SaveState writes cs to /run/<runtimeName>/<id>/state.json.
// It creates the directory if it does not exist.
func SaveState(runtimeName string, cs ContainerState) error {
	dir := stateDir(runtimeName, cs.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("oci: create state dir: %w", err)
	}
	data, err := json.MarshalIndent(cs, "", "\t")
	if err != nil {
		return fmt.Errorf("oci: marshal state: %w", err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("oci: write state: %w", err)
	}
	return nil
}

// LoadState reads the persisted state for the given container ID.
// It returns an error wrapping os.ErrNotExist when the container is unknown.
func LoadState(runtimeName, id string) (ContainerState, error) {
	path := filepath.Join(stateDir(runtimeName, id), "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ContainerState{}, fmt.Errorf("oci: container %q not found: %w", id, os.ErrNotExist)
		}
		return ContainerState{}, fmt.Errorf("oci: read state: %w", err)
	}
	var cs ContainerState
	if err := json.Unmarshal(data, &cs); err != nil {
		return ContainerState{}, fmt.Errorf("oci: parse state: %w", err)
	}
	return cs, nil
}

// DeleteState removes the state directory for the given container ID.
// It is called by the delete operation after confirming the container is stopped.
func DeleteState(runtimeName, id string) error {
	dir := stateDir(runtimeName, id)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("oci: delete state for %q: %w", id, err)
	}
	return nil
}
```

`SaveState` uses `MarshalIndent` for human-readable state files. `LoadState` wraps `os.ErrNotExist` explicitly so callers can distinguish "container not found" from I/O errors with `errors.Is(err, os.ErrNotExist)`.

### Exercise 3: Lifecycle Hook Runner

Create `runtime/hooks.go`:

```go
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// DefaultHookTimeout is applied to hooks that specify no timeout.
// Five seconds is adequate for typical hook operations: writing a file,
// sending a notification, or configuring a network interface.
const DefaultHookTimeout = 5 * time.Second

// RunHooks executes a slice of OCI lifecycle hooks sequentially.
//
// Each hook receives the current container state as JSON on its stdin, matching
// the OCI spec requirement. RunHooks stops at the first hook failure and returns
// an error identifying the hook index and path.
//
// The timeout for each hook is taken from Hook.Timeout (seconds). When nil or
// zero, defaultTimeout is used. When defaultTimeout is also zero, the hook runs
// without a time limit.
func RunHooks(hooks []Hook, state ContainerState, defaultTimeout time.Duration) error {
	if len(hooks) == 0 {
		return nil
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("oci: marshal state for hooks: %w", err)
	}
	for i, h := range hooks {
		if err := runOneHook(h, stateJSON, defaultTimeout); err != nil {
			return fmt.Errorf("oci: hook[%d] %s: %w", i, h.Path, err)
		}
	}
	return nil
}

// runOneHook runs a single hook and enforces its timeout.
func runOneHook(h Hook, stateJSON []byte, defaultTimeout time.Duration) error {
	timeout := defaultTimeout
	if h.Timeout != nil && *h.Timeout > 0 {
		timeout = time.Duration(*h.Timeout) * time.Second
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	// The OCI spec says Args[0] is the argv[0] of the hook (its executable name),
	// not an extra argument. Pass Args[1:] as the argument list.
	args := h.Args
	if len(args) == 0 {
		args = []string{h.Path}
	}
	cmd := exec.CommandContext(ctx, h.Path, args[1:]...)
	cmd.Stdin = bytes.NewReader(stateJSON)
	cmd.Env = append(os.Environ(), h.Env...)
	// Route hook diagnostic output to the runtime's stderr, not the container
	// stdout, to avoid contaminating the user process output stream.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%w after %s", ErrHookTimeout, timeout)
		}
		return err
	}
	return nil
}
```

### Exercise 4: Security Hardening (Linux, external module required)

Create `runtime/security_linux.go`. This file requires `golang.org/x/sys/unix`; add it with `go get golang.org/x/sys`. It is only compiled on Linux:

```go
//go:build linux

package runtime

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// DropCapabilities trims the capability sets of the current process to match
// the spec's bounding list.
//
// The algorithm:
//  1. Clear the ambient capability set so no capabilities are inherited across
//     exec without being in the permitted set.
//  2. For each capability number 0..unix.CAP_LAST_CAP, if it is absent from
//     caps.Bounding, drop it from the bounding set via PR_CAPBSET_DROP.
//
// After this function returns, the process cannot regain dropped capabilities
// even through setuid executables, because they are absent from the bounding set.
// This function must be called inside the child process after namespaces are set
// up and before the final execve into the user command.
func DropCapabilities(caps *LinuxCapabilities) error {
	permitted := capabilitySet(nil)
	if caps != nil {
		permitted = capabilitySet(caps.Bounding)
	}

	// Step 1: clear the ambient capability set.
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("oci: clear ambient caps: %w", err)
	}

	// Step 2: drop every capability not in the permitted set from the bounding set.
	for cap := 0; cap <= unix.CAP_LAST_CAP; cap++ {
		if permitted[cap] {
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(cap), 0, 0, 0); err != nil { //nolint:gosec
			// EINVAL: the kernel does not know this capability number.
			// This happens when x/sys/unix defines CAP_LAST_CAP for a newer
			// kernel than the running one; skip safely.
			if errors.Is(err, unix.EINVAL) {
				continue
			}
			return fmt.Errorf("oci: drop bounding cap %d: %w", cap, err)
		}
	}
	return nil
}

// capabilitySet converts a slice of capability name strings (e.g.,
// "CAP_NET_BIND_SERVICE") to a map from capability number to bool.
// Unknown names are skipped; the OCI spec requires callers to validate names
// against the kernel's supported set.
func capabilitySet(names []string) map[int]bool {
	m := make(map[int]bool, len(names))
	for _, name := range names {
		if v, ok := capByName[name]; ok {
			m[v] = true
		}
	}
	return m
}

// capByName maps OCI capability name strings to their numeric values.
// Values are derived from linux/capability.h via golang.org/x/sys/unix constants.
var capByName = map[string]int{
	"CAP_CHOWN":            unix.CAP_CHOWN,
	"CAP_DAC_OVERRIDE":     unix.CAP_DAC_OVERRIDE,
	"CAP_DAC_READ_SEARCH":  unix.CAP_DAC_READ_SEARCH,
	"CAP_FOWNER":           unix.CAP_FOWNER,
	"CAP_FSETID":           unix.CAP_FSETID,
	"CAP_KILL":             unix.CAP_KILL,
	"CAP_SETGID":           unix.CAP_SETGID,
	"CAP_SETUID":           unix.CAP_SETUID,
	"CAP_SETPCAP":          unix.CAP_SETPCAP,
	"CAP_NET_BIND_SERVICE": unix.CAP_NET_BIND_SERVICE,
	"CAP_NET_RAW":          unix.CAP_NET_RAW,
	"CAP_SYS_CHROOT":       unix.CAP_SYS_CHROOT,
	"CAP_MKNOD":            unix.CAP_MKNOD,
	"CAP_AUDIT_WRITE":      unix.CAP_AUDIT_WRITE,
	"CAP_SETFCAP":          unix.CAP_SETFCAP,
}
```

Seccomp filtering requires either `libseccomp-go` (a cgo wrapper) or direct BPF construction via `unix.SetsockoptSockFprog`. The `libseccomp-go` approach is shown below; it requires both `cgo` and `libseccomp-dev` installed on the host:

```go
//go:build linux && cgo

package runtime

import (
	"fmt"

	libseccomp "github.com/seccomp/libseccomp-golang"
)

// ApplyDefaultSeccomp installs a seccomp-BPF filter that blocks the most
// dangerous syscalls while allowing all others. This covers the essential
// subset of the Docker default profile.
//
// Blocked syscalls:
//   - kexec_load, kexec_file_load: load a new kernel image
//   - reboot: reboot or halt the system
//   - create_module, init_module, finit_module, delete_module: load/unload kernel modules
func ApplyDefaultSeccomp() error {
	filter, err := libseccomp.NewFilter(libseccomp.ActAllow)
	if err != nil {
		return fmt.Errorf("oci: seccomp new filter: %w", err)
	}
	denied := []string{
		"kexec_load",
		"kexec_file_load",
		"reboot",
		"create_module",
		"init_module",
		"finit_module",
		"delete_module",
	}
	for _, sc := range denied {
		id, err := libseccomp.GetSyscallFromName(sc)
		if err != nil {
			// Syscall not known to this libseccomp version; skip safely.
			continue
		}
		if err := filter.AddRule(id, libseccomp.ActErrno.SetReturnCode(int16(1))); err != nil {
			return fmt.Errorf("oci: seccomp deny %s: %w", sc, err)
		}
	}
	if err := filter.Load(); err != nil {
		return fmt.Errorf("oci: seccomp load: %w", err)
	}
	return nil
}
```

### Exercise 5: Tests

Create `runtime/runtime_test.go`:

```go
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestParseSpecValid(t *testing.T) {
	t.Parallel()

	const raw = `{
		"ociVersion": "1.0.2",
		"process": {"args": ["/bin/sh"], "cwd": "/"},
		"root": {"path": "rootfs", "readonly": true}
	}`
	spec, err := ParseSpec([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSpec() error = %v", err)
	}
	if spec.OCIVersion != "1.0.2" {
		t.Fatalf("OCIVersion = %q, want 1.0.2", spec.OCIVersion)
	}
	if spec.Root == nil || !spec.Root.Readonly {
		t.Fatal("Root.Readonly should be true")
	}
	if spec.Process == nil || len(spec.Process.Args) == 0 || spec.Process.Args[0] != "/bin/sh" {
		t.Fatalf("Process.Args = %v, want [/bin/sh]", spec.Process.Args)
	}
}

func TestParseSpecMissingVersion(t *testing.T) {
	t.Parallel()

	_, err := ParseSpec([]byte(`{"process":{"args":["/bin/sh"]}}`))
	if !errors.Is(err, ErrMissingOCIVersion) {
		t.Fatalf("err = %v, want ErrMissingOCIVersion", err)
	}
}

func TestParseSpecInvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := ParseSpec([]byte(`{not valid json`))
	if err == nil {
		t.Fatal("ParseSpec with invalid JSON should return error")
	}
}

func TestParseSpecLinuxNamespaces(t *testing.T) {
	t.Parallel()

	const raw = `{
		"ociVersion": "1.0.2",
		"linux": {
			"namespaces": [
				{"type": "pid"},
				{"type": "network"},
				{"type": "mount"},
				{"type": "uts"},
				{"type": "ipc"}
			]
		}
	}`
	spec, err := ParseSpec([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSpec() error = %v", err)
	}
	if spec.Linux == nil {
		t.Fatal("Linux section should not be nil")
	}
	if len(spec.Linux.Namespaces) != 5 {
		t.Fatalf("Namespaces len = %d, want 5", len(spec.Linux.Namespaces))
	}
}

func TestParseSpecCapabilities(t *testing.T) {
	t.Parallel()

	const raw = `{
		"ociVersion": "1.0.2",
		"process": {
			"args": ["/bin/sh"],
			"cwd": "/",
			"capabilities": {
				"bounding": ["CAP_NET_BIND_SERVICE", "CAP_KILL"],
				"effective": ["CAP_NET_BIND_SERVICE"],
				"permitted": ["CAP_NET_BIND_SERVICE"]
			}
		}
	}`
	spec, err := ParseSpec([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSpec() error = %v", err)
	}
	caps := spec.Process.Capabilities
	if caps == nil {
		t.Fatal("Capabilities should not be nil")
	}
	if len(caps.Bounding) != 2 {
		t.Fatalf("Bounding len = %d, want 2", len(caps.Bounding))
	}
}

func TestStateTransitionValid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		from Status
		to   Status
	}{
		{StatusCreating, StatusCreated},
		{StatusCreated, StatusRunning},
		{StatusCreated, StatusStopped},
		{StatusRunning, StatusStopped},
	}
	for _, tc := range cases {
		cs := ContainerState{Status: tc.from}
		if err := cs.Transition(tc.to); err != nil {
			t.Errorf("%s -> %s: unexpected error: %v", tc.from, tc.to, err)
			continue
		}
		if cs.Status != tc.to {
			t.Errorf("after transition, status = %q, want %q", cs.Status, tc.to)
		}
	}
}

func TestStateTransitionInvalid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		from Status
		to   Status
	}{
		{StatusCreating, StatusRunning},
		{StatusCreating, StatusStopped},
		{StatusRunning, StatusCreated},
		{StatusStopped, StatusRunning},
		{StatusStopped, StatusCreated},
		{StatusStopped, StatusCreating},
	}
	for _, tc := range cases {
		cs := ContainerState{Status: tc.from}
		if err := cs.Transition(tc.to); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s -> %s: err = %v, want ErrInvalidTransition", tc.from, tc.to, err)
		}
	}
}

func TestContainerStateJSONFields(t *testing.T) {
	t.Parallel()

	cs := ContainerState{
		OCIVersion: "1.0.2",
		ID:         "mycontainer",
		Status:     StatusRunning,
		PID:        12345,
		Bundle:     "/var/lib/oci/bundles/mycontainer",
	}
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	for _, field := range []string{"ociVersion", "id", "status", "pid", "bundle"} {
		if _, ok := got[field]; !ok {
			t.Errorf("JSON output missing required OCI field %q", field)
		}
	}
	if got["status"] != "running" {
		t.Errorf("status = %v, want running", got["status"])
	}
}

func TestRunHooksEmpty(t *testing.T) {
	t.Parallel()

	cs := ContainerState{OCIVersion: "1.0.2", ID: "x", Status: StatusCreated}
	if err := RunHooks(nil, cs, DefaultHookTimeout); err != nil {
		t.Fatalf("RunHooks(nil) = %v, want nil", err)
	}
}

func TestRunHooksSuccess(t *testing.T) {
	t.Parallel()

	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true not found; skipping hook success test")
	}
	h := Hook{Path: truePath}
	cs := ContainerState{OCIVersion: "1.0.2", ID: "x", Status: StatusCreated}
	if err := RunHooks([]Hook{h}, cs, DefaultHookTimeout); err != nil {
		t.Fatalf("RunHooks with true: %v", err)
	}
}

func TestRunHooksFailure(t *testing.T) {
	t.Parallel()

	falsePath, err := exec.LookPath("false")
	if err != nil {
		t.Skip("false not found; skipping hook failure test")
	}
	h := Hook{Path: falsePath}
	cs := ContainerState{OCIVersion: "1.0.2", ID: "x", Status: StatusCreated}
	if err := RunHooks([]Hook{h}, cs, DefaultHookTimeout); err == nil {
		t.Fatal("RunHooks with false should return error")
	}
}

func TestRunHooksTimeout(t *testing.T) {
	t.Parallel()

	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found; skipping timeout test")
	}
	timeout := 1
	h := Hook{
		Path:    sleepPath,
		Args:    []string{"sleep", "60"},
		Timeout: &timeout,
	}
	cs := ContainerState{OCIVersion: "1.0.2", ID: "x", Status: StatusCreated}
	err = RunHooks([]Hook{h}, cs, DefaultHookTimeout)
	if !errors.Is(err, ErrHookTimeout) {
		t.Fatalf("err = %v, want ErrHookTimeout", err)
	}
}

// ExampleContainerState demonstrates the OCI state JSON format.
func ExampleContainerState() {
	cs := ContainerState{
		OCIVersion: "1.0.2",
		ID:         "mycontainer",
		Status:     StatusStopped,
		PID:        0,
		Bundle:     "/var/lib/oci/bundles/mycontainer",
	}
	b, _ := json.Marshal(cs)
	fmt.Println(string(b))
	// Output:
	// {"ociVersion":"1.0.2","id":"mycontainer","status":"stopped","pid":0,"bundle":"/var/lib/oci/bundles/mycontainer"}
}
```

Your turn: add `TestParseSpecSeccomp` that parses a `config.json` with a `linux.seccomp` section having `defaultAction: "SCMP_ACT_ERRNO"` and two syscall rules, then asserts `spec.Linux.Seccomp.DefaultAction == "SCMP_ACT_ERRNO"` and `len(spec.Linux.Seccomp.Syscalls) == 2`.

Create `cmd/oci-runtime/main.go`:

```go
// oci-runtime is an OCI Runtime Specification-compliant container runtime.
//
// Usage:
//
//	oci-runtime create  <id> --bundle <path>
//	oci-runtime start   <id>
//	oci-runtime kill    <id> [signal]
//	oci-runtime delete  <id>
//	oci-runtime state   <id>
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"syscall"

	"example.com/oci-runtime/runtime"
)

const runtimeName = "oci-runtime"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: oci-runtime <create|start|kill|delete|state> ...")
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "create":
		err = cmdCreate(os.Args[2:])
	case "start":
		err = cmdStart(os.Args[2:])
	case "kill":
		err = cmdKill(os.Args[2:])
	case "delete":
		err = cmdDelete(os.Args[2:])
	case "state":
		err = cmdState(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "oci-runtime: unknown command %q\n", os.Args[1])
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "oci-runtime:", err)
		os.Exit(1)
	}
}

// cmdCreate implements: oci-runtime create <id> --bundle <path>
func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	bundle := fs.String("bundle", "", "path to the OCI bundle directory (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("create: usage: oci-runtime create <id> --bundle <path>")
	}
	if *bundle == "" {
		return fmt.Errorf("create: --bundle is required")
	}
	id := fs.Arg(0)

	data, err := os.ReadFile(*bundle + "/config.json")
	if err != nil {
		return fmt.Errorf("create: read config.json: %w", err)
	}
	if _, err := runtime.ParseSpec(data); err != nil {
		return fmt.Errorf("create: %w", err)
	}

	// Write creating state before spawning any child process. If the spawn
	// fails, this file allows a recovery path (the runtime can clean up).
	cs := runtime.ContainerState{
		OCIVersion: "1.0.2",
		ID:         id,
		Status:     runtime.StatusCreating,
		Bundle:     *bundle,
	}
	if err := runtime.SaveState(runtimeName, cs); err != nil {
		return fmt.Errorf("create: %w", err)
	}

	// TODO: spawn the container child process using the namespace, mount, and
	// cgroup setup from exercises 1-9. On success the child blocks on a pipe
	// waiting for start to signal it.

	if err := cs.Transition(runtime.StatusCreated); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if err := runtime.SaveState(runtimeName, cs); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	return nil
}

// cmdStart implements: oci-runtime start <id>
func cmdStart(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("start: usage: oci-runtime start <id>")
	}
	cs, err := runtime.LoadState(runtimeName, args[0])
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if cs.Status != runtime.StatusCreated {
		return fmt.Errorf("start: container %q is %s, not created", args[0], cs.Status)
	}
	// TODO: run startContainer hooks, then write to the child's pipe to unblock it.
	if err := cs.Transition(runtime.StatusRunning); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := runtime.SaveState(runtimeName, cs); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return nil
}

// cmdKill implements: oci-runtime kill <id> [signal]
func cmdKill(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("kill: usage: oci-runtime kill <id> [signal]")
	}
	cs, err := runtime.LoadState(runtimeName, args[0])
	if err != nil {
		return fmt.Errorf("kill: %w", err)
	}
	if cs.Status != runtime.StatusRunning && cs.Status != runtime.StatusCreated {
		return fmt.Errorf("kill: container %q is %s; cannot send signal", args[0], cs.Status)
	}

	sig := syscall.Signal(syscall.SIGTERM)
	if len(args) == 2 {
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("kill: invalid signal %q: %w", args[1], err)
		}
		sig = syscall.Signal(n)
	}
	if cs.PID > 0 {
		if err := syscall.Kill(cs.PID, sig); err != nil {
			return fmt.Errorf("kill: send signal to PID %d: %w", cs.PID, err)
		}
	}
	return nil
}

// cmdDelete implements: oci-runtime delete <id>
func cmdDelete(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("delete: usage: oci-runtime delete <id>")
	}
	cs, err := runtime.LoadState(runtimeName, args[0])
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // already deleted; idempotent
		}
		return fmt.Errorf("delete: %w", err)
	}
	if cs.Status == runtime.StatusRunning {
		return fmt.Errorf("delete: container %q is running; stop it first", args[0])
	}
	if err := runtime.DeleteState(runtimeName, args[0]); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// cmdState implements: oci-runtime state <id>
// It writes the OCI state JSON to stdout; higher-level tools parse this output.
func cmdState(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("state: usage: oci-runtime state <id>")
	}
	cs, err := runtime.LoadState(runtimeName, args[0])
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "\t")
	return enc.Encode(cs)
}
```

## Common Mistakes

### Writing State After the Fork, Not Before

Wrong: spawning the child process first, then writing `status: creating` to `state.json` on success.

What happens: if the fork succeeds but the state write fails (disk full, permissions error), the child is running with no record. The runtime cannot kill it, delete it, or report its status. If the fork itself fails, no cleanup is possible.

Fix: write `status: creating` before any fork attempt. The presence of the file on failure allows recovery: the runtime can read the state, detect the orphan, and delete the state directory. This is the pattern runc uses.

### Passing All of Hook.Args as Arguments

Wrong: `exec.Command(h.Path, h.Args...)` when `h.Args = []string{"myname", "--verbose"}`.

What happens: the hook receives `"myname"` as its argv[0] from the kernel, then `"myname"` again as its first argument from the argument list -- double argv[0]. Hooks that inspect `os.Args[0]` see the right name; hooks that iterate all args process `"myname"` as a flag.

Fix: the OCI spec says `Args[0]` is the argv[0] of the hook process (its name), not an extra argument to pass. Pass `h.Args[1:]` as the argument list. The lesson's `runOneHook` does this: `exec.CommandContext(ctx, h.Path, args[1:]...)`.

### Dropping Capabilities in the Parent Before exec

Wrong: calling `DropCapabilities` in the parent process before `exec.Command` spawns the child.

What happens: `execve` recomputes the effective and ambient capability sets based on the new binary's file capabilities. The bounding set survives `execve`, but the permitted set is recalculated by the kernel. Capability changes made in the parent have no effect inside the child's final user command.

Fix: call `DropCapabilities` inside the child process, after namespace setup and `pivot_root`, but before the final `syscall.Exec` into the user command. In the re-exec pattern from exercise 1, the child side calls `DropCapabilities` between mounting `/proc` and calling `syscall.Exec`.

### Allowing State to Transition Backward

Wrong: in the `start` command, setting `cs.Status = runtime.StatusCreated` to retry a failed start, then calling `SaveState`.

What happens: a `delete` that races with an active container resets the state to `created`. A subsequent `start` signals the wrong PID, sending `SIGCONT` to a process that has already been replaced by the user command.

Fix: always call `cs.Transition(to)` instead of setting `Status` directly. `Transition` rejects backward transitions with `ErrInvalidTransition`.

## Verification

From `~/go-exercises/oci-runtime`:

```bash
# Format check: no output means clean
test -z "$(gofmt -l ./runtime/ ./cmd/oci-runtime/)"

# Vet the pure-stdlib subset
# security_linux.go is excluded: it requires golang.org/x/sys and //go:build linux
go vet ./runtime/ ./cmd/oci-runtime/

# Build the CLI
go build ./cmd/oci-runtime/

# Unit tests; runs on any Linux host without root
# TestRunHooksTimeout requires sleep; TestRunHooksSuccess requires true
go test -count=1 -race ./runtime/
```

The unit tests in `runtime_test.go` are hermetic: they do not write to `/run`, do not call Linux syscalls, and do not require root. The three hook tests skip automatically when the required binary (`true`, `false`, `sleep`) is not on PATH.

To exercise the full runtime including security hardening:

```bash
# Add the external dependency for security_linux.go
go get golang.org/x/sys

# Build with Linux constraints active
GOOS=linux go build ./...

# Run against a real OCI bundle (requires root)
sudo ./oci-runtime create mybox --bundle /path/to/bundle
sudo ./oci-runtime start mybox
sudo ./oci-runtime state mybox
sudo ./oci-runtime kill mybox 15
sudo ./oci-runtime delete mybox
```

To validate OCI compliance, install `oci-runtime-tool` from `github.com/opencontainers/runtime-tools` and run:

```bash
sudo oci-runtime-tool validate --path /path/to/bundle
```

## Summary

- An OCI bundle is a directory with `config.json` and a `rootfs`. `ParseSpec` validates the JSON and returns a typed `Spec`; every OCI operation starts here.
- The four-state machine (creating -> created -> running -> stopped) must be enforced by a transition table and persisted to disk before each side-effect, not after.
- Capability hardening: clear ambient, trim bounding, set permitted and effective. All three steps happen inside the child process after namespaces are set up and before the final `execve`.
- Lifecycle hooks receive the current container state as JSON on stdin and must complete within their configured timeout. `exec.CommandContext` with `context.WithTimeout` enforces the limit; `ErrHookTimeout` is returned on deadline exceeded.
- The `state` command writes the OCI state JSON to stdout. Every higher-level tool (containerd, crictl, Podman) depends on this output being well-formed.
- The security files (`security_linux.go`) require `golang.org/x/sys/unix` (an external module) and the `cgo`/`libseccomp-go` file requires `libseccomp-dev`. These are the reason this lesson cannot be built offline.

## What's Next

Next: [Write-Ahead Log](../../39-capstone-database-engine/01-write-ahead-log/01-write-ahead-log.md).

## Resources

- [OCI Runtime Specification](https://github.com/opencontainers/runtime-spec/blob/main/spec.md) -- the authoritative standard defining all operations, the config.json schema, and the state machine
- [OCI Runtime Spec: config.json](https://github.com/opencontainers/runtime-spec/blob/main/config.md) -- complete configuration schema with field descriptions, required/optional annotations, and hook execution order
- [capabilities(7) -- Linux man page](https://man7.org/linux/man-pages/man7/capabilities.7.html) -- bounding/inheritable/ambient set semantics, prctl interactions, and how execve transforms capability sets
- [prctl(2) -- Linux man page](https://man7.org/linux/man-pages/man2/prctl.2.html) -- PR_CAPBSET_DROP, PR_CAP_AMBIENT, and PR_SET_SECCOMP operation reference with error codes
- [runc source code](https://github.com/opencontainers/runc) -- the reference OCI runtime; libcontainer/capabilities.go and libcontainer/seccomp implement the patterns in this lesson
