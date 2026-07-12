# 5. Overlay Filesystem

OverlayFS is the Linux kernel filesystem behind almost every container runtime in production: Docker, containerd, Podman, and Kubernetes nodes all default to it. It merges one or more read-only image layers with a private writable layer into a single coherent directory tree, giving each container the illusion of a mutable root filesystem without copying gigabytes of base image data.

The hard part is not the mount syscall itself — it is one `syscall.Mount` call. The hard parts are: ordering the lower layers correctly (leftmost is topmost, not bottommost), keeping the work directory on the same filesystem as the upper directory, understanding how the kernel represents deletions as whiteout files without ever mutating lower layers, and integrating the overlay mount with the `pivot_root` mechanism from exercise 2 so the container's root filesystem becomes the merged view.

This exercise builds a complete overlay manager: it creates the directory scaffold, activates and deactivates mounts, detects and classifies whiteout entries, measures the upper layer footprint, and supports both ephemeral (discard on exit) and snapshot (preserve upper layer) container modes.

## Concepts

### The Four Required Directories

Every OverlayFS mount needs exactly four directory paths passed to the kernel:

- `lowerdir` — one or more read-only source directories, colon-separated. The kernel never writes here.
- `upperdir` — the private writable layer. All writes go here regardless of which lower layer originally contained the file.
- `workdir` — an empty housekeeping directory the kernel uses for atomic renames during copy-on-write. It must be on the same filesystem as upperdir.
- `merged` — the unified view presented to the container. This is where the container's rootfs appears.

The mount call takes these four paths as a single comma-separated options string:

```
lowerdir=/layer2:/layer1,upperdir=/upper,workdir=/work
```

After `syscall.Mount("overlay", merged, "overlay", 0, opts)` succeeds, `merged` shows the union of all layers. Reads check upper first, then lower layers in order. Writes always go to upper.

### Layer Order: Left Is Top

The kernel documentation states: "the order [in lowerdir] is top to bottom." The leftmost path has the highest priority. When the same filename exists in two layers, the leftmost layer wins.

For OCI image layers, the most recently applied diff (the outermost layer) must appear leftmost. If your image has three layers applied in order `base → deps → app`, the correct options string is:

```
lowerdir=/layers/app:/layers/deps:/layers/base,upperdir=...,workdir=...
```

Reversing the order silently gives wrong behavior: the base layer shadows the app layer, so the container sees the old binaries instead of the new ones. There is no error — the kernel accepts any order. This is the most common ordering mistake.

### The Work Directory Is Not Optional

The kernel uses workdir to implement atomic copy-on-write: when a container writes to a file from a lower layer, the kernel copies the file into workdir, modifies it there, then atomically renames it into upperdir. This rename is atomic only if workdir and upperdir are on the same filesystem (rename(2) is only atomic within a filesystem).

If workdir is on a different filesystem than upperdir, `syscall.Mount` returns `EINVAL`. The error message from `errno` is not always clear about the cause, which is why this constraint must be enforced structurally: `New` creates both `upper/` and `work/` as siblings under the same root, guaranteeing they share a filesystem.

### Copy-on-Write: What Happens on First Write

When a container reads `/etc/passwd`, the kernel finds it in a lower layer and returns it directly — no copy. When the container writes to `/etc/passwd`, the kernel triggers copy-on-write:

1. It copies the full file from the lower layer to `workdir`.
2. It atomically renames the copy into `upperdir/etc/passwd`.
3. Future reads and writes use the upper copy.

The lower layer is never modified. This is why ten containers can share the same base image layer without interfering: they each have their own upper layer, and copy-on-write creates independent copies only for files that are actually modified.

The trade-off is storage amplification: a container that modifies one byte of a 100 MB library file causes a full 100 MB copy into upperdir. Container runtimes mitigate this for large binaries by using sparse files and by encouraging application layers to be small.

### Whiteout Files: Deletions Without Mutation

Lower layers are read-only, so the kernel cannot delete a file from them when the container removes it. Instead, the kernel creates a whiteout file in upperdir with the name `.wh.<original>`. When the overlay resolves a path, a whiteout entry in upperdir hides the corresponding file in all lower layers.

Two kinds of whiteout entries exist in the OCI specification:

- **Plain whiteout** — `.wh.<filename>` in a directory hides that filename from lower layers. Example: deleting `/etc/passwd` inside a container creates `upper/etc/.wh.passwd`.
- **Opaque whiteout** — `.wh..wh..opq` inside a directory hides all of that directory's lower-layer contents. Example: replacing `/etc/` entirely creates `upper/etc/.wh..wh..opq` plus the new `/etc/` contents in upper.

The kernel creates whiteout files automatically when files are deleted through the merged view. When building a new image layer by committing a container's upper layer, the runtime must walk upperdir, detect whiteout entries, and translate them into OCI layer changesets.

### Ephemeral Containers vs. Snapshots

When a container exits, the runtime chooses between two policies:

- **Ephemeral** (default): unmount the overlay and delete upperdir, workdir, and merged. Lower layers are untouched and ready for the next container.
- **Snapshot**: unmount the overlay but preserve upperdir. The runtime can later use this upper layer as an additional lower layer for a new image, implementing `docker commit` semantics.

Both policies leave lower layers intact. The only difference is whether upperdir is deleted after unmount.

## Exercises

This package uses `syscall.Mount`, which requires Linux with CAP_SYS_ADMIN (root). The whiteout and size logic is platform-independent and can be tested anywhere. Test the full mount path inside a Linux VM or a rootful container.

### Exercise 1: Directory Layout and the Mount Type

Create `overlayfs.go`. The `//go:build linux` constraint keeps the file out of non-Linux builds entirely:

```go
//go:build linux

package overlayfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrNoLayers is returned when New is called with an empty lowers slice.
var ErrNoLayers = errors.New("overlayfs: at least one lower layer is required")

// Layer is a read-only image layer directory.
// In a []Layer slice, index 0 is the topmost (highest-priority) layer in
// the overlay stack. For OCI images, index 0 should be the most recently
// applied diff layer.
type Layer struct {
	Path string
}

// Mount holds the directory layout for one overlay mount.
// All methods that call into the kernel require CAP_SYS_ADMIN on Linux.
type Mount struct {
	Merged string  // unified view exposed to the container
	Upper  string  // container's private writable layer
	Work   string  // overlay housekeeping dir; must be on same filesystem as Upper
	Lowers []Layer // read-only image layers, index 0 is topmost
}

// New creates Merged, Upper, and Work directories under root and returns an
// unmounted Mount. root is a per-container scratch directory, for example
// /run/containers/<id>. Lower layer directories must already exist; New does
// not create them.
func New(root string, lowers []Layer) (*Mount, error) {
	if len(lowers) == 0 {
		return nil, ErrNoLayers
	}
	m := &Mount{
		Merged: filepath.Join(root, "merged"),
		Upper:  filepath.Join(root, "upper"),
		Work:   filepath.Join(root, "work"),
		Lowers: lowers,
	}
	for _, dir := range []string{m.Merged, m.Upper, m.Work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("overlayfs: mkdir %s: %w", dir, err)
		}
	}
	return m, nil
}

// MergedPath returns the unified view directory. This path should become the
// container's root filesystem after Activate is called (via pivot_root or
// chroot).
func (m *Mount) MergedPath() string { return m.Merged }

// UpperPath returns the private writable layer directory. Use it to inspect
// container changes, measure storage usage, or preserve a snapshot.
func (m *Mount) UpperPath() string { return m.Upper }

// UpperLayerSize returns the total byte count of regular files in the upper
// (writable) layer. Call after Deactivate to measure the container's storage
// footprint without the overlay being active.
func (m *Mount) UpperLayerSize() (int64, error) {
	return dirSize(m.Upper)
}
```

### Exercise 2: Mount Options Builder and Lifecycle Methods

Add the remaining methods to `overlayfs.go`:

```go
// Activate mounts the overlay filesystem. The call requires CAP_SYS_ADMIN
// and a kernel with OverlayFS support (CONFIG_OVERLAY_FS=y, default on all
// mainline kernels since Linux 3.18).
func (m *Mount) Activate() error {
	opts := m.buildOptions()
	if err := syscall.Mount("overlay", m.Merged, "overlay", 0, opts); err != nil {
		return fmt.Errorf("overlayfs: mount %s: %w", m.Merged, err)
	}
	return nil
}

// Deactivate unmounts the overlay without removing any directories.
// The upper layer is preserved. Call Discard to remove it, or keep it for
// snapshot operations.
func (m *Mount) Deactivate() error {
	if err := syscall.Unmount(m.Merged, 0); err != nil {
		return fmt.Errorf("overlayfs: unmount %s: %w", m.Merged, err)
	}
	return nil
}

// Discard deactivates the overlay and removes the Upper, Work, and Merged
// directories. Lower layers are never touched. Use Discard for ephemeral
// containers where the writable layer should be discarded on exit.
func (m *Mount) Discard() error {
	if err := m.Deactivate(); err != nil {
		return err
	}
	for _, dir := range []string{m.Upper, m.Work, m.Merged} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("overlayfs: remove %s: %w", dir, err)
		}
	}
	return nil
}

// buildOptions constructs the mount(2) data string for the overlay driver.
//
//	lowerdir=<top>:<middle>:<bottom>,upperdir=<upper>,workdir=<work>
//
// The leftmost path in lowerdir has the highest priority. Lowers[0] is
// always leftmost, matching the Layer slice convention (index 0 = topmost).
func (m *Mount) buildOptions() string {
	paths := make([]string, len(m.Lowers))
	for i, l := range m.Lowers {
		paths[i] = l.Path
	}
	return fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(paths, ":"), m.Upper, m.Work)
}
```

### Exercise 3: Whiteout Detection and Storage Accounting

Create `whiteout.go`. This file has no build constraint because whiteout detection is pure string logic; it is useful to upper-layer walkers and layer exporters on any platform:

```go
package overlayfs

import (
	"path/filepath"
	"strings"
)

const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
)

// IsWhiteout reports whether name is any OCI whiteout marker. Both plain
// whiteouts (.wh.<name>) and opaque whiteouts (.wh..wh..opq) return true.
// name may be a bare filename or a full path; only the base component is
// examined.
func IsWhiteout(name string) bool {
	return strings.HasPrefix(filepath.Base(name), whiteoutPrefix)
}

// IsOpaqueWhiteout reports whether name is an opaque whiteout (.wh..wh..opq),
// which hides all lower-layer content in the directory that contains it.
func IsOpaqueWhiteout(name string) bool {
	return filepath.Base(name) == opaqueWhiteout
}

// WhiteoutTarget returns the filename concealed by a plain whiteout entry and
// true. It returns ("", false) for opaque whiteouts and non-whiteout names.
// The returned string is a bare filename, not a path.
func WhiteoutTarget(name string) (string, bool) {
	base := filepath.Base(name)
	if base == opaqueWhiteout || !strings.HasPrefix(base, whiteoutPrefix) {
		return "", false
	}
	return strings.TrimPrefix(base, whiteoutPrefix), true
}
```

Create `size.go`:

```go
package overlayfs

import (
	"fmt"
	"io/fs"
	"path/filepath"
)

// dirSize returns the total byte count of all regular files under root.
// Symbolic links and directories do not contribute to the total.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("overlayfs: stat %s: %w", path, err)
		}
		total += info.Size()
		return nil
	})
	return total, err
}
```

### Exercise 4: The Test Suite

Create `overlayfs_test.go` for the platform-independent tests. These run on any OS because they test only whiteout detection and directory size — no mount syscalls:

```go
package overlayfs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIsWhiteout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{".wh.passwd", true},
		{".wh..wh..opq", true},
		{"passwd", false},
		{"etc", false},
		{".wh.hosts", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsWhiteout(tc.name); got != tc.want {
				t.Errorf("IsWhiteout(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestIsOpaqueWhiteout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{".wh..wh..opq", true},
		{".wh.passwd", false},
		{"passwd", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsOpaqueWhiteout(tc.name); got != tc.want {
				t.Errorf("IsOpaqueWhiteout(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestWhiteoutTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		wantTarget string
		wantOK     bool
	}{
		{".wh.passwd", "passwd", true},
		{".wh.hosts", "hosts", true},
		{".wh..wh..opq", "", false},
		{"passwd", "", false},
		{"etc", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := WhiteoutTarget(tc.name)
			if ok != tc.wantOK || got != tc.wantTarget {
				t.Errorf("WhiteoutTarget(%q) = (%q, %v), want (%q, %v)",
					tc.name, got, ok, tc.wantTarget, tc.wantOK)
			}
		})
	}
}

func TestDirSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := dirSize(dir)
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}
	if got != 10 {
		t.Errorf("dirSize = %d, want 10", got)
	}
}

func TestDirSizeEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got, err := dirSize(dir)
	if err != nil {
		t.Fatalf("dirSize empty: %v", err)
	}
	if got != 0 {
		t.Errorf("dirSize empty = %d, want 0", got)
	}
}

func ExampleIsWhiteout() {
	fmt.Println(IsWhiteout(".wh.passwd"))
	fmt.Println(IsWhiteout(".wh..wh..opq"))
	fmt.Println(IsWhiteout("passwd"))
	// Output:
	// true
	// true
	// false
}

func ExampleWhiteoutTarget() {
	target, ok := WhiteoutTarget(".wh.passwd")
	fmt.Printf("%s %v\n", target, ok)
	_, ok = WhiteoutTarget(".wh..wh..opq")
	fmt.Println(ok)
	// Output:
	// passwd true
	// false
}
```

Create `overlayfs_linux_test.go` for the Linux-specific tests. The `_linux` filename suffix restricts compilation to Linux automatically:

```go
package overlayfs

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestNewRejectsNoLayers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	_, err := New(root, nil)
	if !errors.Is(err, ErrNoLayers) {
		t.Errorf("New() err = %v, want ErrNoLayers", err)
	}
}

func TestNewCreatesDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lower := t.TempDir() // lower must exist; New does not create it
	m, err := New(root, []Layer{{Path: lower}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, dir := range []string{m.MergedPath(), m.UpperPath(), m.Work} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("missing directory %s: %v", dir, err)
		}
	}
}

func TestBuildOptionsSingleLayer(t *testing.T) {
	t.Parallel()
	m := &Mount{
		Merged: "/merged",
		Upper:  "/upper",
		Work:   "/work",
		Lowers: []Layer{{Path: "/base"}},
	}
	opts := m.buildOptions()
	want := "lowerdir=/base,upperdir=/upper,workdir=/work"
	if opts != want {
		t.Errorf("buildOptions() = %q, want %q", opts, want)
	}
	if strings.Contains(opts, "lowerdir=:") || strings.HasSuffix(opts[:strings.Index(opts, ",")], ":") {
		t.Errorf("single-layer lowerdir must not contain a colon separator: %q", opts)
	}
}

func TestBuildOptionsMultipleLayers(t *testing.T) {
	t.Parallel()
	m := &Mount{
		Merged: "/merged",
		Upper:  "/upper",
		Work:   "/work",
		Lowers: []Layer{{"/app"}, {"/deps"}, {"/base"}},
	}
	opts := m.buildOptions()
	want := "lowerdir=/app:/deps:/base,upperdir=/upper,workdir=/work"
	if opts != want {
		t.Errorf("buildOptions() = %q, want %q", opts, want)
	}
}

func TestBuildOptionsLayerOrderIndex0IsLeftmost(t *testing.T) {
	t.Parallel()
	m := &Mount{
		Merged: "/merged",
		Upper:  "/upper",
		Work:   "/work",
		Lowers: []Layer{{"/newest"}, {"/older"}, {"/oldest"}},
	}
	opts := m.buildOptions()
	// lowerdir= value must start with the index-0 layer (topmost/newest).
	prefix := "lowerdir=/newest:"
	if !strings.Contains(opts, prefix) {
		t.Errorf("buildOptions(): Lowers[0] must be leftmost in lowerdir; got %q", opts)
	}
}
```

Your turn: add a test `TestNewPathsAreUnderRoot` that calls `New` with a temp root and two lower layers, then asserts that `m.MergedPath()`, `m.UpperPath()`, and `m.Work` all have the root as a prefix. Use `strings.HasPrefix`.

### Demo Program

Create `cmd/demo/main.go`. The build constraint restricts this binary to Linux. It uses only the exported API, so it compiles without any package-internal access:

```go
//go:build linux

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"example.com/overlayfs"
)

// layerFlag accumulates multiple -layer flag values into a slice.
type layerFlag []string

func (f *layerFlag) String() string {
	if len(*f) == 0 {
		return ""
	}
	return (*f)[0]
}

func (f *layerFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func main() {
	var (
		root     = flag.String("root", "/tmp/overlay-demo", "container scratch root")
		snapshot = flag.Bool("snapshot", false, "preserve upper layer on exit")
		layers   layerFlag
	)
	flag.Var(&layers, "layer", "read-only layer path (repeat for multiple; index 0 is topmost)")
	flag.Parse()

	if len(layers) == 0 {
		log.Fatal("provide at least one -layer path")
	}

	lowers := make([]overlayfs.Layer, len(layers))
	for i, p := range layers {
		lowers[i] = overlayfs.Layer{Path: p}
	}

	m, err := overlayfs.New(*root, lowers)
	if err != nil {
		log.Fatalf("new: %v", err)
	}

	if err := m.Activate(); err != nil {
		log.Fatalf("activate (requires root): %v", err)
	}
	fmt.Printf("overlay mounted at %s\n", m.MergedPath())

	// Write through the merged view; the kernel places the file in upper.
	sentinel := filepath.Join(m.MergedPath(), "container-was-here")
	if err := os.WriteFile(sentinel, []byte("container payload\n"), 0o644); err != nil {
		log.Printf("write sentinel: %v", err)
	}

	// Upper layer is readable while the overlay is active.
	size, err := m.UpperLayerSize()
	if err != nil {
		log.Printf("upper layer size: %v", err)
	} else {
		fmt.Printf("upper layer size: %d bytes\n", size)
	}

	if *snapshot {
		if err := m.Deactivate(); err != nil {
			log.Fatalf("deactivate: %v", err)
		}
		fmt.Printf("snapshot preserved at %s\n", m.UpperPath())
	} else {
		if err := m.Discard(); err != nil {
			log.Fatalf("discard: %v", err)
		}
		fmt.Println("ephemeral container cleaned up")
	}
}
```

Run on Linux as root, pointing at a populated lower layer:

```bash
mkdir -p /tmp/base-layer
echo "base file" > /tmp/base-layer/base.txt

sudo go run ./cmd/demo -layer /tmp/base-layer -root /tmp/overlay-demo
# overlay mounted at /tmp/overlay-demo/merged
# upper layer size: 18 bytes
# ephemeral container cleaned up

# With snapshot:
sudo go run ./cmd/demo -layer /tmp/base-layer -root /tmp/overlay-demo -snapshot
# overlay mounted at /tmp/overlay-demo/merged
# upper layer size: 18 bytes
# snapshot preserved at /tmp/overlay-demo/upper
```

## Common Mistakes

**Wrong: reversing the layer order in buildOptions**

```go
// Wrong: iterating lowers in reverse puts the oldest layer leftmost.
for i := len(m.Lowers) - 1; i >= 0; i-- {
	paths = append(paths, m.Lowers[i].Path)
}
```

What happens: the mount succeeds with no error. The oldest base layer has higher priority than the application layer, so files that the application layer is supposed to shadow are still visible. Debugging is hard because `ls` inside the container looks plausible until you notice version numbers are wrong.

Fix: iterate lowers forward. `Lowers[0]` must be leftmost in the options string. The invariant to pin in a test is: `strings.HasPrefix(opts after "lowerdir=", Lowers[0].Path)`.

---

**Wrong: placing workdir on a different filesystem than upperdir**

```go
// Wrong: /tmp may be a tmpfs while /var/lib/containers is ext4.
m := &Mount{
	Upper: "/var/lib/containers/upper",
	Work:  "/tmp/work",
	...
}
```

What happens: `Activate()` returns an error wrapping `EINVAL`. The kernel checks at mount time that workdir and upperdir are on the same filesystem. The error message is not always obvious; `EINVAL` from `mount(2)` has dozens of causes.

Fix: create workdir as a sibling of upperdir under the same root directory. `New` enforces this: both `upper/` and `work/` are created under the caller-supplied `root`, which is a single directory on one filesystem.

---

**Wrong: calling WhiteoutTarget to test whether to remove a file**

```go
// Wrong: IsWhiteout returns true for opaque whiteouts too.
if IsWhiteout(entry) {
	target, _ := WhiteoutTarget(entry)
	os.Remove(filepath.Join(lowerDir, target)) // panics for opaque whiteouts: target is ""
}
```

What happens: for an opaque whiteout (`.wh..wh..opq`), `WhiteoutTarget` returns `("", false)`, so the caller passes an empty string to `os.Remove`, attempting to remove the directory itself. It also misses the semantics: an opaque whiteout hides all lower-layer contents, not just one file.

Fix: check `IsOpaqueWhiteout` first, handle it separately (hide the whole directory), then use `WhiteoutTarget` only for the `ok == true` case.

```go
if IsOpaqueWhiteout(entry) {
	// hide all lower-layer content in this directory
} else if target, ok := WhiteoutTarget(entry); ok {
	// hide this specific file in lower layers
}
```

---

**Wrong: calling UpperLayerSize before Deactivate and comparing it to "disk usage"**

```go
size, _ := m.UpperLayerSize() // called while overlay is active
fmt.Printf("container used %d bytes\n", size)
```

What happens: the result is correct in terms of bytes, but it counts only the files the kernel has already written to upper through copy-on-write. Files the container has only read, but not written, are not in upper and not counted. This is the correct semantics for "written bytes," but it is easy to misread as "total bytes accessed."

Fix: document the method clearly. `UpperLayerSize` reports writable-layer footprint, not read I/O. Call it after `Deactivate` to get a stable snapshot for logging or quota enforcement.

## Verification

From `~/go-exercises/overlayfs`, run the platform-independent tests (no root required, works on macOS and Linux):

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -count=1 -race ./...
```

On a Linux system with root access, build and run the full test suite and the demo:

```bash
# Build the Linux-only binary to check it compiles:
GOOS=linux go build ./cmd/demo

# Run all tests including the Linux-specific ones (requires Linux, no root needed for mount-free tests):
go test -count=1 -race ./...

# Run the demo with a real lower layer (requires root):
mkdir -p /tmp/base && echo "hello" > /tmp/base/hello.txt
sudo go run ./cmd/demo -layer /tmp/base -root /tmp/ov-scratch
```

Add at least one test of your own: verify that `WhiteoutTarget` on any name without the `.wh.` prefix returns `ok == false`.

## Summary

- OverlayFS requires four paths: lowerdir (read-only stack), upperdir (writable layer), workdir (same filesystem as upper), and merged (the unified view).
- The lowerdir order is top-to-bottom: the leftmost path has the highest priority. `Lowers[0]` must be leftmost.
- workdir and upperdir must be on the same filesystem; `New` enforces this by creating both as siblings under one root.
- All writes go to upperdir regardless of which lower layer originally held the file; lower layers are never modified.
- The kernel creates plain whiteout files (`.wh.<name>`) to represent file deletions and opaque whiteout files (`.wh..wh..opq`) to hide entire directories.
- Ephemeral containers call `Discard` on exit; snapshot containers call `Deactivate` and preserve upperdir for later use as an image layer.

## What's Next

Next: [OCI Image Pulling](../06-oci-image-pulling/06-oci-image-pulling.md).

## Resources

- [kernel.org: OverlayFS documentation](https://www.kernel.org/doc/Documentation/filesystems/overlayfs.txt) — authoritative specification for layer order, whiteout semantics, workdir constraints, and mount option format.
- [OCI Image Spec: Layer Filesystem Changeset](https://github.com/opencontainers/image-spec/blob/main/layer.md) — defines the whiteout file conventions (.wh. prefix, .wh..wh..opq) used by all OCI-compliant runtimes.
- [pkg.go.dev: syscall.Mount (Linux)](https://pkg.go.dev/syscall#Mount) — signature and flag constants for the Go syscall wrapper around mount(2).
- [pkg.go.dev: path/filepath.WalkDir](https://pkg.go.dev/path/filepath#WalkDir) — the directory walker used for upper-layer size accounting.
- [man7.org: mount(2)](https://man7.org/linux/man-pages/man2/mount.2.html) — system call reference; see EINVAL conditions and the MS_* flag constants.
