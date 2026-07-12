# 2. Mount Namespace and Root Filesystem

Mount namespace isolation (`CLONE_NEWNS`) and the `pivot_root` system call are what give a container its private filesystem view. Without them a container process shares the host mount table and can read or modify any host mount point. This lesson builds a Go package that performs the complete five-step sequence: set mount propagation to private, bind-mount the rootfs onto itself, mount pseudo-filesystems, pivot the root, and detach the old root. The hard parts are the non-obvious bind-onto-self requirement of `pivot_root(2)`, the security implications of mount propagation defaults, and why `pivot_root` is not replaceable by `chroot`.

```text
containerruntime/
  go.mod
  rootfs/
    config.go          (no build constraint — Config, errors, validate)
    rootfs_linux.go    (//go:build linux — mount and pivot operations)
    rootfs_test.go     (no build constraint — tests for validate and Config)
    rootfs_linux_test.go  (//go:build linux — integration tests)
  cmd/
    demo/
      main.go          (//go:build linux — re-exec demo)
```

## Concepts

### The Mount Namespace

A mount namespace is a per-process view of the kernel's mount table — the data structure that maps paths to device or filesystem combinations. When a process enters a new mount namespace via `clone(2)` with `CLONE_NEWNS` (or `unshare(2)`), it receives a copy of the parent's mount table. Mounts created inside the new namespace are visible only within it.

In Go, a child process enters a new mount namespace by setting `CLONE_NEWNS` in `SysProcAttr.Cloneflags` before the fork:

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
	Cloneflags: syscall.CLONE_NEWNS |
		syscall.CLONE_NEWUTS |
		syscall.CLONE_NEWPID,
}
```

The kernel creates the namespace before the child's first instruction runs. The child starts with a copy of the parent's mount table; from that point forward, any mount or unmount inside the child is invisible to the parent, and vice versa — once propagation is set correctly (see next section).

### Mount Propagation: The Hidden Default

Linux tracks a propagation type for every mount point. The four types are:

- `MS_SHARED` (the default on most distributions): mount and unmount events propagate to all peers in the group. A container's mounts leak to the host unless this default is changed.
- `MS_SLAVE`: events propagate from a master into this namespace but changes inside do not propagate outward. Useful when the host needs to push new mounts into containers.
- `MS_PRIVATE`: no propagation in or out. This is what every container runtime sets as the first step.
- `MS_UNBINDABLE`: like `MS_PRIVATE` but the mount cannot be bind-mounted, preventing recursive bind-mount attacks.

The first call inside every new mount namespace must be:

```go
syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, "")
```

The empty source and target strings mean "change propagation on the current root mount". `MS_REC` applies the change recursively to all submounts. Skipping this step is a real vulnerability: the OCI Runtime Specification mandates it, and Docker's security advisory CVE-2015-3627 traces directly to missing propagation isolation.

### `pivot_root` vs `chroot`: Security Model

`chroot(2)` changes the root directory pointer for the process but does not change the mount namespace. A process with `CAP_SYS_CHROOT` can escape a chroot jail with a well-known sequence: open a directory file descriptor before chroot, call chroot into a subdirectory, then use the saved fd and `fchdir` to traverse back to the real root.

`pivot_root(2)` replaces the root mount entry in the namespace, not just the path pointer. After `pivot_root` and after unmounting `.pivot_old`, the old root mount no longer exists in the namespace's mount table. There is no path, file descriptor trick, or `/proc` link that can reach it.

The kernel enforces three requirements at `pivot_root` entry:

1. `new_root` must be a mount point (not just a directory on an existing mount).
2. `put_old` must be at or underneath `new_root` in the directory tree.
3. Neither `new_root` nor `put_old` may be on the same mount as the current root.

Requirement 1 is what trips up most first implementations. A plain directory is not a mount point. The fix is to bind-mount the rootfs directory onto itself before calling `pivot_root`; a bind mount promotes any directory to a mount point.

### The Five-Step Sequence

After entering `CLONE_NEWNS`, the five steps must run in this exact order inside the child:

1. `MS_PRIVATE|MS_REC` on `/` — sever propagation to the host namespace.
2. `MS_BIND|MS_REC` on rootfs onto itself — promote rootfs to a mount point.
3. Mount pseudo-filesystems inside rootfs (`/proc`, `/sys`, optionally `/dev`).
4. `pivot_root(rootfs, rootfs/.pivot_old)` — atomically swap the root mount.
5. `chdir("/")`, then `unmount("/.pivot_old", MNT_DETACH)`, then `rmdir("/.pivot_old")` — detach and erase all paths to the host.

Reordering these steps produces either permission errors (pivot_root before MS_PRIVATE) or security holes (mount proc after pivot, missing unmount of old root).

`MNT_DETACH` (lazy unmount) removes the mount entry from the namespace immediately but does not force-close file descriptors held on files inside the old root. It is the correct choice here: the executable image itself is a file descriptor into the old root, and using `MNT_FORCE` would kill the process.

### Pseudo-Filesystem Mounts

Three pseudo-filesystems are expected inside a container:

- `/proc` (type `proc`): required by almost every userspace utility. Mount with `MS_NOSUID|MS_NODEV|MS_NOEXEC` to prevent proc from being used to execute setuid binaries or access raw devices.
- `/sys` (type `sysfs`): kernel and device information. Mount read-only (`MS_RDONLY`) in containers that do not need to modify kernel parameters.
- `/dev`: device nodes. Production runtimes create a fresh `devtmpfs` and populate only the standard device nodes (`null`, `zero`, `full`, `random`, `urandom`, `tty`, `console`) using `mknod(2)` with `CAP_MKNOD`. Bind-mounting the host `/dev` is a development shortcut that accepts the tradeoff of exposing all host device nodes inside the container.

## Exercises

This is a library verified with `go test`. The demo binary in `cmd/demo` shows how to wire the library into a real namespace entry.

### Exercise 1: Configuration and Portable Validation

Create `rootfs/config.go` with no build constraint. The `Config` type and input validation contain no Linux-only symbols and compile on any platform:

```go
package rootfs

import (
	"errors"
	"fmt"
	"os"
)

// ErrRootFSNotFound is returned when the rootfs path is empty or does not exist.
var ErrRootFSNotFound = errors.New("rootfs path does not exist")

// ErrNotDirectory is returned when the rootfs path names a file, not a directory.
var ErrNotDirectory = errors.New("rootfs path is not a directory")

// ErrOldPutDir is returned when .pivot_old cannot be created inside the rootfs.
// This usually means the rootfs directory is read-only or lacks write permission.
var ErrOldPutDir = errors.New("cannot create .pivot_old inside rootfs")

// Config holds the parameters for a root filesystem switch.
type Config struct {
	// RootFS is the absolute path to the directory that becomes the container
	// root. pivot_root(2) requires it to be a mount point; Setup satisfies
	// this by bind-mounting the directory onto itself before pivoting.
	RootFS string

	// ProcMount controls whether a fresh procfs is mounted at /proc inside
	// the new root. Required by any container that reads /proc/self or runs
	// ps(1), top(1), or strace(1).
	ProcMount bool

	// SysMount controls whether sysfs is mounted read-only at /sys inside the
	// new root. Disable for containers that must not see kernel interfaces.
	SysMount bool

	// DevMount controls whether the host /dev is bind-mounted into the new root.
	// Suitable for development only; production containers use a filtered devtmpfs.
	DevMount bool
}

// validate checks that cfg.RootFS is a non-empty path to an existing directory.
// It is called by Setup before any syscall that requires CAP_SYS_ADMIN so that
// configuration errors are reported cheaply and without side effects.
func validate(cfg Config) error {
	if cfg.RootFS == "" {
		return fmt.Errorf("rootfs: %w: path is empty", ErrRootFSNotFound)
	}
	info, err := os.Stat(cfg.RootFS)
	if err != nil {
		return fmt.Errorf("rootfs: %w: %s: %v", ErrRootFSNotFound, cfg.RootFS, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("rootfs: %w: %s", ErrNotDirectory, cfg.RootFS)
	}
	return nil
}
```

Sentinel errors are wrapped with `%w` so callers use `errors.Is` to distinguish them. Validation lives in a constraint-free file so the unit tests for it compile and run on any OS.

### Exercise 2: Mount Namespace Operations

Create `rootfs/rootfs_linux.go`. Every function in this file calls a Linux-only syscall, so the build constraint is mandatory:

```go
//go:build linux

package rootfs

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// MakeRootPrivate sets mount propagation on the current root to MS_PRIVATE
// with MS_REC so the flag applies recursively to all submounts. This must be
// the first operation after entering CLONE_NEWNS. Skipping it allows mounts
// created inside the namespace to propagate back to the host peer group.
func MakeRootPrivate() error {
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("rootfs: set / to MS_PRIVATE: %w", err)
	}
	return nil
}

// BindSelf bind-mounts rootfsPath onto itself, promoting it from a plain
// directory to a mount point. pivot_root(2) requires new_root to be a mount
// point; this is the standard technique for satisfying that requirement without
// creating a new filesystem.
func BindSelf(rootfsPath string) error {
	if err := syscall.Mount(rootfsPath, rootfsPath, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("rootfs: bind-mount %s onto itself: %w", rootfsPath, err)
	}
	return nil
}

// MountProc mounts a fresh procfs at <rootfsPath>/proc.
// MS_NOSUID, MS_NODEV, and MS_NOEXEC prevent proc from being abused to
// execute setuid binaries, access raw devices, or run programs via /proc/pid/exe.
func MountProc(rootfsPath string) error {
	target := filepath.Join(rootfsPath, "proc")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("rootfs: mkdir %s: %w", target, err)
	}
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
	if err := syscall.Mount("proc", target, "proc", flags, ""); err != nil {
		return fmt.Errorf("rootfs: mount proc at %s: %w", target, err)
	}
	return nil
}

// MountSys mounts sysfs read-only at <rootfsPath>/sys.
// MS_RDONLY prevents the container from modifying kernel parameters via sysfs.
func MountSys(rootfsPath string) error {
	target := filepath.Join(rootfsPath, "sys")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("rootfs: mkdir %s: %w", target, err)
	}
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC | syscall.MS_RDONLY)
	if err := syscall.Mount("sysfs", target, "sysfs", flags, ""); err != nil {
		return fmt.Errorf("rootfs: mount sysfs at %s: %w", target, err)
	}
	return nil
}

// BindDev bind-mounts the host /dev into <rootfsPath>/dev.
// This exposes every host device node inside the container and is a development
// shortcut. Production containers mount a fresh devtmpfs and use mknod(2) to
// create only the seven standard device nodes.
func BindDev(rootfsPath string) error {
	target := filepath.Join(rootfsPath, "dev")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("rootfs: mkdir %s: %w", target, err)
	}
	if err := syscall.Mount("/dev", target, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("rootfs: bind /dev to %s: %w", target, err)
	}
	return nil
}

// PivotInto atomically replaces the process's root with rootfsPath.
//
// The sequence is:
//  1. Create rootfsPath/.pivot_old to hold the old root during the pivot.
//  2. Call pivot_root(2) to swap the root mount.
//  3. Chdir to "/" inside the new root so relative paths resolve correctly.
//  4. Lazily unmount /.pivot_old so the old root vanishes from the namespace.
//  5. Remove the now-empty /.pivot_old directory.
//
// After PivotInto returns successfully, all host mount points are unreachable:
// no path in the namespace leads to any host filesystem.
//
// MNT_DETACH performs a lazy unmount: the mount entry is removed from the
// namespace immediately, but the kernel keeps the underlying superblock alive
// until the last open file descriptor on it is closed. This avoids blocking
// on the current process's open executable image.
func PivotInto(rootfsPath string) error {
	putOld := filepath.Join(rootfsPath, ".pivot_old")
	if err := os.MkdirAll(putOld, 0o700); err != nil {
		return fmt.Errorf("%w: mkdir %s: %v", ErrOldPutDir, putOld, err)
	}

	if err := syscall.PivotRoot(rootfsPath, putOld); err != nil {
		return fmt.Errorf("rootfs: pivot_root(%s, %s): %w", rootfsPath, putOld, err)
	}

	// After pivot_root the working directory is still a handle into the old root.
	// Switch to "/" inside the new root before any further path operations.
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("rootfs: chdir / after pivot_root: %w", err)
	}

	// /.pivot_old is now relative to the new root; the old root is behind it.
	if err := syscall.Unmount("/.pivot_old", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("rootfs: unmount /.pivot_old: %w", err)
	}

	if err := os.Remove("/.pivot_old"); err != nil {
		return fmt.Errorf("rootfs: remove /.pivot_old: %w", err)
	}

	return nil
}

// Setup performs the complete root filesystem switch for a mount namespace
// entered with CLONE_NEWNS. The steps run in mandatory order:
//
//  1. Set / to MS_PRIVATE|MS_REC — no mount events leak to the host.
//  2. Bind-mount RootFS onto itself — promote it to a mount point.
//  3. Mount /proc inside RootFS (if ProcMount is true).
//  4. Mount /sys  inside RootFS (if SysMount  is true).
//  5. Bind /dev   inside RootFS (if DevMount   is true).
//  6. pivot_root into RootFS, then unmount and remove the old root.
//
// The caller must enter CLONE_NEWNS before calling Setup. All steps require
// CAP_SYS_ADMIN. On failure, the error chain identifies which syscall failed.
func Setup(cfg Config) error {
	if err := validate(cfg); err != nil {
		return err
	}
	if err := MakeRootPrivate(); err != nil {
		return err
	}
	if err := BindSelf(cfg.RootFS); err != nil {
		return err
	}
	if cfg.ProcMount {
		if err := MountProc(cfg.RootFS); err != nil {
			return err
		}
	}
	if cfg.SysMount {
		if err := MountSys(cfg.RootFS); err != nil {
			return err
		}
	}
	if cfg.DevMount {
		if err := BindDev(cfg.RootFS); err != nil {
			return err
		}
	}
	return PivotInto(cfg.RootFS)
}
```

`Setup` is a direct translation of the five-step sequence: every prose step in Concepts maps to exactly one function call. The function calls preserve the mandatory ordering; rearranging them without understanding the kernel semantics is the single most common source of container escape vulnerabilities.

### Exercise 3: Tests and Demo

Create `rootfs/rootfs_test.go` with no build constraint. These tests exercise `validate` and `Config`, which have no Linux-only symbols and run on any platform:

```go
package rootfs

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestValidateRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	if err := validate(Config{}); !errors.Is(err, ErrRootFSNotFound) {
		t.Fatalf("validate({}): err = %v, want ErrRootFSNotFound", err)
	}
}

func TestValidateRejectsNonExistentPath(t *testing.T) {
	t.Parallel()

	err := validate(Config{RootFS: "/this/does/not/exist/4a7b2c9d"})
	if !errors.Is(err, ErrRootFSNotFound) {
		t.Fatalf("validate(nonexistent): err = %v, want ErrRootFSNotFound", err)
	}
}

func TestValidateRejectsFilePath(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp(t.TempDir(), "rootfs-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if got := validate(Config{RootFS: f.Name()}); !errors.Is(got, ErrNotDirectory) {
		t.Fatalf("validate(file): err = %v, want ErrNotDirectory", got)
	}
}

func TestValidateAcceptsDirectory(t *testing.T) {
	t.Parallel()

	if err := validate(Config{RootFS: t.TempDir()}); err != nil {
		t.Fatalf("validate(dir): unexpected error: %v", err)
	}
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target error
		other  error
	}{
		{"ErrRootFSNotFound is not ErrNotDirectory", ErrRootFSNotFound, ErrNotDirectory},
		{"ErrNotDirectory is not ErrRootFSNotFound", ErrNotDirectory, ErrRootFSNotFound},
		{"ErrOldPutDir is not ErrRootFSNotFound", ErrOldPutDir, ErrRootFSNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if errors.Is(tc.target, tc.other) {
				t.Fatalf("errors.Is(%v, %v) = true; sentinel errors must be distinct", tc.target, tc.other)
			}
		})
	}
}

func ExampleConfig() {
	cfg := Config{
		RootFS:    "/var/lib/containers/alpine",
		ProcMount: true,
		SysMount:  true,
		DevMount:  false,
	}
	fmt.Printf("rootfs=%s proc=%v sys=%v dev=%v\n",
		cfg.RootFS, cfg.ProcMount, cfg.SysMount, cfg.DevMount)
	// Output:
	// rootfs=/var/lib/containers/alpine proc=true sys=true dev=false
}
```

Create `rootfs/rootfs_linux_test.go` for integration tests that require a Linux kernel:

```go
//go:build linux

package rootfs

import (
	"errors"
	"os"
	"testing"
)

// requireRoot skips the test if the process does not have CAP_SYS_ADMIN.
// mount(2) and pivot_root(2) both require this capability.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("mount namespace tests require root (CAP_SYS_ADMIN)")
	}
}

// inMountNamespace returns true when the current process is in a mount
// namespace distinct from PID 1's namespace. Running tests that call
// MakeRootPrivate on the host namespace modifies real host propagation,
// so integration tests that call Setup must only run after unshare(1).
func inMountNamespace() bool {
	self, err1 := os.Readlink("/proc/self/ns/mnt")
	init, err2 := os.Readlink("/proc/1/ns/mnt")
	if err1 != nil || err2 != nil {
		return false
	}
	return self != init
}

func TestSetupRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	if err := Setup(Config{}); !errors.Is(err, ErrRootFSNotFound) {
		t.Fatalf("Setup({}): err = %v, want ErrRootFSNotFound", err)
	}
}

func TestSetupRejectsFilePath(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp(t.TempDir(), "rootfs-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if got := Setup(Config{RootFS: f.Name()}); !errors.Is(got, ErrNotDirectory) {
		t.Fatalf("Setup(file): err = %v, want ErrNotDirectory", got)
	}
}

// TestMakeRootPrivateRequiresPrivilege verifies that MakeRootPrivate returns a
// non-nil error when CAP_SYS_ADMIN is absent instead of panicking or silently
// succeeding.
func TestMakeRootPrivateRequiresPrivilege(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("this test checks non-root behaviour; run without sudo")
	}
	if err := MakeRootPrivate(); err == nil {
		t.Fatal("MakeRootPrivate without CAP_SYS_ADMIN returned nil; expected EPERM")
	}
}

// TestFullSetupInsideNamespace exercises the complete five-step sequence.
// It requires root AND a separate mount namespace so that MakeRootPrivate does
// not modify host propagation. Run with:
//
//	sudo unshare -m go test -v -run TestFullSetupInsideNamespace ./rootfs/
func TestFullSetupInsideNamespace(t *testing.T) {
	requireRoot(t)
	if !inMountNamespace() {
		t.Skip("run inside 'sudo unshare -m go test ./rootfs/' to test pivot_root")
	}

	dir := t.TempDir()

	// Call Setup with all pseudo-fs mounts disabled. proc and sys require a
	// kernel that has these filesystems compiled in; inside a bare tmpfs rootfs
	// their mount points exist but the kernel module may not be present.
	// The goal here is to verify the pivot_root + unmount sequence succeeds and
	// returns no error; the calling process's root is now dir.
	err := Setup(Config{
		RootFS:    dir,
		ProcMount: false,
		SysMount:  false,
		DevMount:  false,
	})
	// After a successful pivot, the process is inside dir. The test framework
	// cannot continue validating the host environment. Log and return.
	if err != nil {
		t.Fatalf("Setup: unexpected error: %v", err)
	}
	t.Log("pivot_root succeeded; process is now inside the new root")
}
```

Your turn: add `TestValidateWrapsError` to `rootfs_test.go`. It should call `validate(Config{RootFS: "/nonexistent/path"})`, unwrap the error chain with `errors.As`, and assert that the underlying error satisfies `errors.Is(err, ErrRootFSNotFound)` while also verifying that the error message contains the path string. This pins two contracts at once: the sentinel wrapping and the diagnostic message.

Create `cmd/demo/main.go`. Because it is `package main`, it can only call exported API:

```go
//go:build linux

package main

import (
	"flag"
	"log"
	"os"
	"os/exec"
	"syscall"

	"example.com/containerruntime/rootfs"
)

// childSentinel separates parent and child execution paths in the same binary.
// The parent forks /proc/self/exe with the sentinel as the first argument; the
// child detects it and calls rootfs.Setup before exec-ing the container shell.
const childSentinel = "__child"

func main() {
	if len(os.Args) > 1 && os.Args[1] == childSentinel {
		runChild(os.Args[2:])
		return
	}
	runParent()
}

// runParent parses flags and forks a copy of itself with CLONE_NEWNS,
// CLONE_NEWUTS, and CLONE_NEWPID set. The kernel creates all three namespaces
// atomically before the child process runs its first instruction.
func runParent() {
	rootfsPath := flag.String("rootfs", "", "absolute path to the container root filesystem (required)")
	flag.Parse()
	if *rootfsPath == "" {
		log.Fatal("--rootfs is required; example: --rootfs /var/lib/containers/alpine")
	}

	cmd := exec.Command("/proc/self/exe", childSentinel, *rootfsPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS |
			syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWPID,
	}

	if err := cmd.Run(); err != nil {
		log.Fatalf("container exited: %v", err)
	}
}

// runChild runs after the kernel has placed the process into the new namespaces.
// It calls rootfs.Setup to perform the five-step root switch, then replaces
// itself with /bin/sh from inside the new root. The shell becomes PID 1 inside
// the PID namespace.
func runChild(args []string) {
	if len(args) < 1 {
		log.Fatal("child: missing rootfs path")
	}
	cfg := rootfs.Config{
		RootFS:    args[0],
		ProcMount: true,
		SysMount:  true,
		DevMount:  true,
	}
	if err := rootfs.Setup(cfg); err != nil {
		log.Fatalf("rootfs.Setup: %v", err)
	}
	// exec(2) replaces this process image with /bin/sh from the new root.
	// The environment is inherited; the shell sees the container's filesystem.
	if err := syscall.Exec("/bin/sh", []string{"sh"}, os.Environ()); err != nil {
		log.Fatalf("exec /bin/sh: %v", err)
	}
}
```

Run the demo with an Alpine miniroot:

```bash
# Download Alpine miniroot (~2.8 MB):
curl -L https://dl-cdn.alpinelinux.org/alpine/v3.19/releases/x86_64/alpine-minirootfs-3.19.1-x86_64.tar.gz \
	| sudo tar -xzf - -C /var/lib/containers/alpine

go build -o demo ./cmd/demo
sudo ./demo --rootfs /var/lib/containers/alpine
# Inside the shell: ls / shows Alpine contents, not the host.
# ls /proc shows kernel process entries.
# The host filesystem is unreachable.
```

## Common Mistakes

### Skipping `MS_PRIVATE` Before Any Mount Operation

Wrong: bind-mounting the rootfs before setting propagation.

```go
// Wrong: bind mount event propagates back to host peer group.
syscall.Mount(cfg.RootFS, cfg.RootFS, "", syscall.MS_BIND|syscall.MS_REC, "")
```

What happens: the bind mount appears in the host's mount table because the inherited mount table has `MS_SHARED` propagation (the default on most systemd distributions). The host's `mount` command shows the container bind mount, which means the container's filesystem activity is visible to the host.

Fix: call `MakeRootPrivate` as the very first operation inside the new namespace.

### Calling `pivot_root` on a Plain Directory

Wrong: omitting the bind-onto-self step.

```go
// Wrong: cfg.RootFS is a directory on an existing mount, not a mount point.
syscall.PivotRoot(cfg.RootFS, filepath.Join(cfg.RootFS, ".pivot_old"))
// Returns: EINVAL
```

What happens: `pivot_root(2)` returns `EINVAL` because `new_root` is not a mount point. The process is now in an indeterminate state: `MS_PRIVATE` was already set on the root, but the pivot failed.

Fix: call `BindSelf(cfg.RootFS)` before `PivotInto`. The bind mount makes `cfg.RootFS` a mount point, satisfying `pivot_root`'s requirement.

### Forgetting `chdir("/")` After `pivot_root`

Wrong: not changing the working directory immediately after `pivot_root`.

```go
syscall.PivotRoot(rootfsPath, putOld)
// Missing: syscall.Chdir("/")
syscall.Unmount("/.pivot_old", syscall.MNT_DETACH)
```

What happens: the process's current working directory is still a `dentry` inside the old root. Relative paths resolve against the old root rather than the new one. More critically, `/proc/<pid>/cwd` in the old namespace points into the old root, giving any process that can read `/proc` a traversal path back to the host filesystem.

Fix: call `syscall.Chdir("/")` immediately after `pivot_root` returns and before any other file system operation.

### Using `chroot` to Avoid the Bind-Mount Complexity

Wrong: switching roots with `chroot` because the bind-mount requirement seems unnecessary.

```go
// Wrong: old root is still reachable via fd tricks and /proc links.
syscall.Chroot(cfg.RootFS)
syscall.Chdir("/")
```

What happens: the mount table is unchanged. A process with an open file descriptor to a directory outside the chroot can call `fchdir` on that fd and then walk up with `..` to escape. `/proc/<pid>/root` still points to the real root for any process that entered the jail without `PR_SET_DUMPABLE` cleared.

Fix: use `pivot_root`. After unmounting `.pivot_old`, the host root mount entry does not exist in the namespace. There is no path, open fd in the same namespace, or `/proc` symlink that reaches it.

## Verification

From `~/go-exercises/containerruntime`:

```bash
# Format check — runs on any platform:
test -z "$(gofmt -l .)"

# Static analysis — vets config.go and rootfs_test.go on any OS;
# linux-constrained files are excluded automatically on non-Linux:
go vet ./rootfs/

# Unit tests that run on any OS (validation logic, sentinel errors, ExampleConfig):
go test -count=1 -race ./rootfs/

# Linux integration tests — validation failures reported before any syscall:
# go test -count=1 -race ./rootfs/ -run TestSetup

# Full integration test — requires root + mount namespace:
# sudo unshare -m go test -v -count=1 -run TestFullSetupInsideNamespace ./rootfs/

# Build the demo (Linux only):
# GOOS=linux go build ./cmd/demo
```

`go test -count=1 -race ./rootfs/` must pass on any platform with no failures. The linux-tagged tests are compiled only on Linux; on other platforms the unit tests for `validate` and `Config` run and cover the portable logic.

## Summary

- Mount namespace (`CLONE_NEWNS`) gives the child process a private copy of the mount table; changes inside the namespace are invisible to the host.
- Mount propagation defaults to `MS_SHARED` on most distributions; the first operation in a new namespace must set `MS_PRIVATE|MS_REC` on `/` to prevent mount events from leaking.
- `pivot_root` replaces the root mount entry atomically; after unmounting `.pivot_old` with `MNT_DETACH`, no path in the namespace reaches any host filesystem. `chroot` only changes the root directory pointer and is escapable via file descriptor tricks.
- `pivot_root` requires `new_root` to be a mount point. Bind-mounting the rootfs onto itself is the standard technique to satisfy this requirement without creating a separate filesystem.
- `MNT_DETACH` (lazy unmount) removes the mount entry from the namespace immediately without blocking on open file descriptors, including the current process's executable image.
- The five-step sequence is mandatory in order; reordering any pair of steps produces either permission errors or security holes.

## What's Next

Next: [Network Namespace and Veth Pairs](../03-network-namespace-veth/03-network-namespace-veth.md).

## Resources

- [pivot_root(2) man page](https://man7.org/linux/man-pages/man2/pivot_root.2.html) — system call requirements, all EINVAL conditions, the bind-onto-self pattern, and why rootfs is excluded.
- [mount_namespaces(7) man page](https://man7.org/linux/man-pages/man7/mount_namespaces.7.html) — propagation types (shared, slave, private, unbindable), peer groups, and the default propagation inherited from the host.
- [pkg.go.dev/syscall — PivotRoot, Mount, Unmount](https://pkg.go.dev/syscall#PivotRoot) — exact Go function signatures and the MS_*/MNT_* constants.
- [OCI Runtime Specification: Linux Namespaces](https://github.com/opencontainers/runtime-spec/blob/main/config-linux.md#namespaces) — how production runtimes specify namespace entry; mandates MS_PRIVATE before any container mount.
- [Linux kernel: shared subtrees](https://www.kernel.org/doc/Documentation/filesystems/sharedsubtree.txt) — authoritative reference on propagation semantics, peer groups, and the four propagation types.
