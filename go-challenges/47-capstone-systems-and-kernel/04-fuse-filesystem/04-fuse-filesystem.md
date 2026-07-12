# 4. FUSE Filesystem

FUSE (Filesystem in Userspace) lets a Go program serve a complete POSIX filesystem from an ordinary process. When the kernel receives `open`, `read`, `mkdir`, or `rename` on the mount point, it forwards each call as a binary message through `/dev/fuse` to your daemon, which replies with the result. The application sees a normal filesystem; the kernel never knows the data comes from Go.

This lesson builds an overlay filesystem modeled on Docker's overlay2 storage driver and the Linux kernel's OverlayFS. An overlay merges two host directories into one virtual namespace: a writable upper layer and a read-only base layer. Writes land in the upper layer; base files survive untouched; deleted names are hidden with whiteout marker files. Three rules govern the merge: upper wins, whiteouts hide, opaque directories replace. The overlay logic — path resolution, copy-on-write, whiteout creation — is the testable core of the lesson. The FUSE callbacks are the integration shell around it.

```text
overlay-fs/
  go.mod
  overlay/
    overlay.go        (merge logic; no FUSE imports; unit-testable on any platform)
    overlay_test.go   (table-driven tests + Example functions)
  cmd/overlay-fs/
    main.go           (FUSE mount and request dispatch; Linux only)
```

## Concepts

### How FUSE Routes VFS Calls

A FUSE mount registers the kernel's VFS layer to forward filesystem calls to `/dev/fuse`. The sequence for a single `open("/mnt/overlay/foo", O_RDONLY)`:

1. The kernel's VFS layer identifies the mount point as FUSE-backed.
2. It encodes the operation (opcode `FUSE_OPEN`, path `"foo"`, flags) and writes it to `/dev/fuse`.
3. The calling thread blocks inside the kernel.
4. The FUSE daemon reads the message from `/dev/fuse`, executes the operation (here: stat the host path, open the file descriptor), and writes a reply back.
5. The kernel unblocks the calling thread and returns the file descriptor to the application.

The round-trip crosses the kernel boundary twice per VFS call, which makes FUSE roughly 2-5x slower than native I/O for metadata-heavy workloads. For data-heavy reads and writes the overhead is smaller because the kernel can transfer data directly between page cache and the host file descriptor with `splice`.

The `github.com/hanwen/go-fuse/v2/fs` package handles the `/dev/fuse` protocol and the request dispatch loop. You never touch the binary format; you implement Go interfaces.

### The Inode-Embedding Pattern

Every node in `go-fuse/v2/fs` embeds `fs.Inode`:

```go
type OverlayDir struct {
	fs.Inode
	upper string
	base  string
}
```

The embedded `Inode` carries the kernel inode number, the parent pointer, and the children map managed by go-fuse. Your struct implements zero or more `NodeXxxer` interfaces; go-fuse dispatches each VFS call to the matching method. Unimplemented operations return `ENOSYS`, which tells the kernel to use a VFS-level default or report the error to the caller.

The key interfaces for a directory node:

| Interface | Method | VFS operation |
|---|---|---|
| `NodeLookuper` | `Lookup` | `stat("/dir/name")` |
| `NodeReaddirer` | `Readdir` | `getdents` / `ls` |
| `NodeCreater` | `Create` | `open(O_CREAT)` |
| `NodeMkdirer` | `Mkdir` | `mkdir` |
| `NodeUnlinker` | `Unlink` | `unlink` / `rm` |
| `NodeRmdirer` | `Rmdir` | `rmdir` |
| `NodeSymlinker` | `Symlink` | `ln -s` |
| `NodeStatfser` | `Statfs` | `df` |
| `NodeGetattrer` | `Getattr` | `stat` |

For a file node, the important ones are `NodeGetattrer`, `NodeOpener`, `NodeReadlinker`, and, if you use handle-less I/O, `NodeReader` and `NodeWriter`. This lesson delegates file I/O to `fs.NewLoopbackFile`, which wraps a host file descriptor and implements all file-handle interfaces automatically.

### Overlay Semantics: Upper Wins, Whiteouts Hide, Opaque Directories Replace

Three invariants govern the merged view:

**Upper wins.** If a name appears in both layers, the upper version is visible. The base version is inaccessible.

**Whiteouts hide.** Deleting a name from the base creates a whiteout marker file in the upper layer: a regular file named `.wh.<original>`. `Resolve` checks for the whiteout before checking either layer, so the name appears absent in the merged view even though the base file exists untouched.

**Opaque directories replace.** When a directory is replaced (`rmdir` + `mkdir`), the upper directory is marked opaque with a sentinel file `.wh..wh..opq`. `MergedNames` skips the base layer entirely for an opaque directory, so its contents completely replace (not merge with) the base directory's contents. This is how `docker commit` captures a layer that deletes an entire directory tree.

### Copy-on-Write

Base-layer files are read-only from the overlay's perspective. When a read-only base file is opened for writing, the overlay copies it to the upper layer first and then hands the upper copy to the caller. This is copy-on-write (CoW). The copy must happen atomically with respect to concurrent writers: `CopyUp` checks whether the destination already exists before copying, making it safe to call from multiple goroutines.

After CoW, all subsequent reads and writes go to the upper copy. The base file is never modified.

### Attribute Caching and Concurrency

The `AttrOut` and `EntryOut` structs each carry a TTL that tells the kernel how long to cache attribute and directory-entry data before asking the FUSE daemon again. A TTL of zero makes every access hit the daemon (maximally correct, maximally slow); a TTL of one second is typical for a local overlay where only the daemon modifies the upper layer.

FUSE dispatches concurrent requests from multiple kernel threads. go-fuse handles the multiplexing; your node types are responsible for protecting mutable fields. In this implementation the `upper` and `base` fields on every node are set at construction and never changed, so no mutex is needed for those. `OverlayFile` holds a `sync.RWMutex` for the CoW promotion, which reads the resolved path and conditionally copies the file.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/47-capstone-systems-and-kernel/04-fuse-filesystem/04-fuse-filesystem/overlay
mkdir -p go-solutions/47-capstone-systems-and-kernel/04-fuse-filesystem/04-fuse-filesystem/cmd/overlay-fs
cd go-solutions/47-capstone-systems-and-kernel/04-fuse-filesystem/04-fuse-filesystem
go get github.com/hanwen/go-fuse/v2@latest
```

Because FUSE requires a Linux kernel with the `fuse` module loaded, the exercises split the work in two:

- `overlay/` contains the merge logic with no FUSE imports. It compiles and tests on any platform.
- `cmd/overlay-fs/main.go` carries `//go:build linux` and mounts the filesystem. It is tested manually on Linux.

### Exercise 1: Overlay Path Resolution

Create `overlay/overlay.go`. This is the testable core: pure host-filesystem operations that the FUSE callbacks will delegate to.

```go
package overlay

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// WhiteoutPrefix is the prefix used for whiteout marker files.
const WhiteoutPrefix = ".wh."

// OpaqueMarkerFile is the sentinel file name that marks a directory as opaque.
// When present inside an upper-layer directory, the directory's contents
// completely replace (rather than merge with) the base layer.
const OpaqueMarkerFile = ".wh..wh..opq"

// ErrWhiteout is returned by Resolve when a name is covered by a whiteout
// marker in the upper layer. The caller should treat the entry as deleted.
var ErrWhiteout = errors.New("overlay: entry is whited out")

// WhiteoutName returns the name of the whiteout marker for entry name.
func WhiteoutName(name string) string {
	return WhiteoutPrefix + name
}

// IsWhiteoutMarker reports whether name is itself a whiteout marker file.
func IsWhiteoutMarker(name string) bool {
	return strings.HasPrefix(name, WhiteoutPrefix)
}

// OriginalName returns the entry name covered by a whiteout marker.
// The caller must verify IsWhiteoutMarker(name) before calling.
func OriginalName(name string) string {
	return strings.TrimPrefix(name, WhiteoutPrefix)
}

// Resolve returns the absolute host path for the overlay entry (upperDir, baseDir, name).
// Priority:
//
//  1. If a whiteout marker exists in upperDir, ErrWhiteout is returned.
//  2. If name exists in upperDir, that path is returned with isUpper=true.
//  3. If name exists in baseDir, that path is returned with isUpper=false.
//
// If the name is absent from both layers, syscall.ENOENT is returned.
func Resolve(upperDir, baseDir, name string) (path string, isUpper bool, err error) {
	wo := filepath.Join(upperDir, WhiteoutName(name))
	if _, statErr := os.Lstat(wo); statErr == nil {
		return "", false, ErrWhiteout
	}

	up := filepath.Join(upperDir, name)
	if _, statErr := os.Lstat(up); statErr == nil {
		return up, true, nil
	}

	base := filepath.Join(baseDir, name)
	if _, statErr := os.Lstat(base); statErr == nil {
		return base, false, nil
	}

	return "", false, syscall.ENOENT
}

// IsOpaque reports whether the directory at dirPath is marked opaque.
func IsOpaque(dirPath string) bool {
	_, err := os.Lstat(filepath.Join(dirPath, OpaqueMarkerFile))
	return err == nil
}

// MarkOpaque creates the opaque sentinel inside dirPath.
func MarkOpaque(dirPath string) error {
	f, err := os.Create(filepath.Join(dirPath, OpaqueMarkerFile))
	if err != nil {
		return fmt.Errorf("overlay: mark opaque %s: %w", dirPath, err)
	}
	return f.Close()
}

// CopyUp copies the file at srcPath into destDir, preserving its mode bits.
// Called before the first write to a base-layer file (copy-on-write).
// If the destination already exists, CopyUp is a no-op and returns destPath.
func CopyUp(srcPath, destDir string) (destPath string, err error) {
	name := filepath.Base(srcPath)
	destPath = filepath.Join(destDir, name)

	if _, statErr := os.Lstat(destPath); statErr == nil {
		return destPath, nil
	}

	srcInfo, err := os.Lstat(srcPath)
	if err != nil {
		return "", fmt.Errorf("overlay: copy-up stat %s: %w", srcPath, err)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("overlay: copy-up mkdir %s: %w", destDir, err)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("overlay: copy-up open src: %w", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return "", fmt.Errorf("overlay: copy-up create dst: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return "", fmt.Errorf("overlay: copy-up data: %w", err)
	}
	return destPath, nil
}

// CreateWhiteout creates a whiteout marker in upperDir for entry name.
// Subsequent calls to Resolve will return ErrWhiteout for that name,
// hiding the base-layer entry without modifying it.
func CreateWhiteout(upperDir, name string) error {
	wo := filepath.Join(upperDir, WhiteoutName(name))
	f, err := os.Create(wo)
	if err != nil {
		return fmt.Errorf("overlay: create whiteout for %q: %w", name, err)
	}
	return f.Close()
}

// MergedNames returns the deduplicated, whiteout-filtered list of entry names
// visible in the merged view of (upperDir, baseDir). Whiteout markers and the
// opaque marker itself are excluded from the result.
//
// If upperDir is marked opaque, base-layer entries are not included.
func MergedNames(upperDir, baseDir string) ([]string, error) {
	seen := make(map[string]bool)
	whited := make(map[string]bool)

	upperEntries, err := os.ReadDir(upperDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("overlay: read upper %s: %w", upperDir, err)
	}
	for _, e := range upperEntries {
		n := e.Name()
		switch {
		case n == OpaqueMarkerFile:
			// never expose the opaque marker
		case IsWhiteoutMarker(n):
			whited[OriginalName(n)] = true
		default:
			seen[n] = true
		}
	}

	if IsOpaque(upperDir) {
		return mapKeys(seen), nil
	}

	baseEntries, err := os.ReadDir(baseDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("overlay: read base %s: %w", baseDir, err)
	}
	for _, e := range baseEntries {
		n := e.Name()
		if seen[n] || whited[n] || IsWhiteoutMarker(n) || n == OpaqueMarkerFile {
			continue
		}
		seen[n] = true
	}

	return mapKeys(seen), nil
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
```

The three merge rules are implemented in three functions: `Resolve` (upper wins + whiteout), `MergedNames` (whiteout + opaque), and `CopyUp` (copy-on-write). Each is independent and testable without FUSE.

### Exercise 2: Unit Tests for the Overlay Logic

Create `overlay/overlay_test.go`:

```go
package overlay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
)

func tmpOverlay(t *testing.T) (upper, base string) {
	t.Helper()
	root := t.TempDir()
	upper = filepath.Join(root, "upper")
	base = filepath.Join(root, "base")
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	return upper, base
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveUpperWins(t *testing.T) {
	t.Parallel()
	upper, base := tmpOverlay(t)

	writeFile(t, filepath.Join(base, "config.txt"), "base")
	writeFile(t, filepath.Join(upper, "config.txt"), "upper")

	path, isUpper, err := Resolve(upper, base, "config.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !isUpper {
		t.Fatal("expected isUpper=true when name exists in both layers")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "upper" {
		t.Fatalf("got %q, want %q", data, "upper")
	}
}

func TestResolveBaseFallthrough(t *testing.T) {
	t.Parallel()
	upper, base := tmpOverlay(t)

	writeFile(t, filepath.Join(base, "readme.txt"), "from base")

	path, isUpper, err := Resolve(upper, base, "readme.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if isUpper {
		t.Fatal("expected isUpper=false for a base-only file")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "from base" {
		t.Fatalf("got %q, want %q", data, "from base")
	}
}

func TestResolveWhiteout(t *testing.T) {
	t.Parallel()
	upper, base := tmpOverlay(t)

	writeFile(t, filepath.Join(base, "secret.txt"), "hidden")
	writeFile(t, filepath.Join(upper, WhiteoutName("secret.txt")), "")

	_, _, err := Resolve(upper, base, "secret.txt")
	if !errors.Is(err, ErrWhiteout) {
		t.Fatalf("expected ErrWhiteout, got %v", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	t.Parallel()
	upper, base := tmpOverlay(t)

	_, _, err := Resolve(upper, base, "ghost.txt")
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("expected ENOENT, got %v", err)
	}
}

func TestCopyUpContent(t *testing.T) {
	t.Parallel()
	upper, base := tmpOverlay(t)

	src := filepath.Join(base, "data.bin")
	writeFile(t, src, "original content")

	dest, err := CopyUp(src, upper)
	if err != nil {
		t.Fatalf("CopyUp: %v", err)
	}
	data, _ := os.ReadFile(dest)
	if string(data) != "original content" {
		t.Fatalf("copy-up content mismatch: got %q", data)
	}
}

func TestCopyUpPreservesBaseFile(t *testing.T) {
	t.Parallel()
	upper, base := tmpOverlay(t)

	src := filepath.Join(base, "data.bin")
	writeFile(t, src, "original content")
	if _, err := CopyUp(src, upper); err != nil {
		t.Fatalf("CopyUp: %v", err)
	}

	baseData, _ := os.ReadFile(src)
	if string(baseData) != "original content" {
		t.Fatal("CopyUp must not modify the source file")
	}
}

func TestCopyUpIsIdempotent(t *testing.T) {
	t.Parallel()
	upper, base := tmpOverlay(t)

	src := filepath.Join(base, "data.bin")
	writeFile(t, src, "original content")

	dest1, err := CopyUp(src, upper)
	if err != nil {
		t.Fatalf("first CopyUp: %v", err)
	}
	dest2, err := CopyUp(src, upper)
	if err != nil {
		t.Fatalf("second CopyUp: %v", err)
	}
	if dest1 != dest2 {
		t.Fatalf("idempotent CopyUp returned different paths: %q vs %q", dest1, dest2)
	}
}

func TestCreateWhiteout(t *testing.T) {
	t.Parallel()
	upper, _ := tmpOverlay(t)

	if err := CreateWhiteout(upper, "gone.txt"); err != nil {
		t.Fatalf("CreateWhiteout: %v", err)
	}
	wo := filepath.Join(upper, WhiteoutName("gone.txt"))
	if _, err := os.Lstat(wo); err != nil {
		t.Fatalf("whiteout file not created: %v", err)
	}
}

func TestMergedNamesBasic(t *testing.T) {
	t.Parallel()
	upper, base := tmpOverlay(t)

	writeFile(t, filepath.Join(base, "a.txt"), "a")
	writeFile(t, filepath.Join(base, "b.txt"), "b")
	writeFile(t, filepath.Join(base, "c.txt"), "c")
	writeFile(t, filepath.Join(upper, "b.txt"), "b-override")     // upper wins
	writeFile(t, filepath.Join(upper, WhiteoutName("c.txt")), "") // c deleted
	writeFile(t, filepath.Join(upper, "d.txt"), "d")              // new upper file

	names, err := MergedNames(upper, base)
	if err != nil {
		t.Fatalf("MergedNames: %v", err)
	}
	sort.Strings(names)

	want := []string{"a.txt", "b.txt", "d.txt"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestMergedNamesOpaqueDir(t *testing.T) {
	t.Parallel()
	upper, base := tmpOverlay(t)

	writeFile(t, filepath.Join(base, "a.txt"), "a")
	writeFile(t, filepath.Join(base, "b.txt"), "b")
	writeFile(t, filepath.Join(upper, "x.txt"), "x")
	if err := MarkOpaque(upper); err != nil {
		t.Fatalf("MarkOpaque: %v", err)
	}

	names, err := MergedNames(upper, base)
	if err != nil {
		t.Fatalf("MergedNames opaque: %v", err)
	}
	sort.Strings(names)

	// Only x.txt should be visible; a.txt and b.txt are hidden by the opaque marker.
	if len(names) != 1 || names[0] != "x.txt" {
		t.Fatalf("opaque: names = %v, want [x.txt]", names)
	}
}

// Your turn: add TestWhiteoutMarkerHelpers that asserts:
//   - WhiteoutName("foo") == ".wh.foo"
//   - IsWhiteoutMarker(".wh.foo") == true
//   - IsWhiteoutMarker("foo") == false
//   - OriginalName(".wh.foo") == "foo"

func ExampleWhiteoutName() {
	fmt.Println(WhiteoutName("config.txt"))
	// Output:
	// .wh.config.txt
}

func ExampleIsWhiteoutMarker() {
	fmt.Println(IsWhiteoutMarker(".wh.config.txt"))
	fmt.Println(IsWhiteoutMarker("config.txt"))
	// Output:
	// true
	// false
}

func ExampleOriginalName() {
	fmt.Println(OriginalName(".wh.config.txt"))
	// Output:
	// config.txt
}
```

### Exercise 3: FUSE Overlay Node

Create `cmd/overlay-fs/main.go`. This file carries `//go:build linux` because FUSE requires the Linux kernel. It mounts the overlay and serves requests until unmounted.

```go
//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"example.com/overlay-fs/overlay"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const attrTTL = time.Second

// OverlayDir is a directory node that merges upper and base layers.
// upper and base are absolute host paths and are immutable after construction.
type OverlayDir struct {
	fs.Inode
	upper string
	base  string
}

var (
	_ fs.NodeGetattrer = (*OverlayDir)(nil)
	_ fs.NodeLookuper  = (*OverlayDir)(nil)
	_ fs.NodeReaddirer = (*OverlayDir)(nil)
	_ fs.NodeCreater   = (*OverlayDir)(nil)
	_ fs.NodeMkdirer   = (*OverlayDir)(nil)
	_ fs.NodeUnlinker  = (*OverlayDir)(nil)
	_ fs.NodeRmdirer   = (*OverlayDir)(nil)
	_ fs.NodeSymlinker = (*OverlayDir)(nil)
	_ fs.NodeStatfser  = (*OverlayDir)(nil)
)

func (d *OverlayDir) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	dir := d.upper
	if _, err := os.Lstat(d.upper); err != nil {
		dir = d.base
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return syscall.ENOENT
	}
	fillAttr(&out.Attr, info)
	out.SetTimeout(attrTTL)
	return fs.OK
}

func (d *OverlayDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path, _, err := overlay.Resolve(d.upper, d.base, name)
	if errors.Is(err, overlay.ErrWhiteout) || errors.Is(err, syscall.ENOENT) {
		return nil, syscall.ENOENT
	}
	if err != nil {
		return nil, syscall.EIO
	}

	info, statErr := os.Lstat(path)
	if statErr != nil {
		return nil, syscall.EIO
	}
	fillAttr(&out.Attr, info)
	out.SetEntryTimeout(attrTTL)
	out.SetAttrTimeout(attrTTL)

	childUpper := filepath.Join(d.upper, name)
	childBase := filepath.Join(d.base, name)

	var node fs.InodeEmbedder
	var stableMode uint32
	if info.IsDir() {
		stableMode = syscall.S_IFDIR
		node = &OverlayDir{upper: childUpper, base: childBase}
	} else if info.Mode()&os.ModeSymlink != 0 {
		stableMode = syscall.S_IFLNK
		node = &OverlayFile{upper: d.upper, base: d.base, name: name}
	} else {
		stableMode = syscall.S_IFREG
		node = &OverlayFile{upper: d.upper, base: d.base, name: name}
	}
	ch := d.NewInode(ctx, node, fs.StableAttr{Mode: stableMode})
	return ch, fs.OK
}

func (d *OverlayDir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	names, err := overlay.MergedNames(d.upper, d.base)
	if err != nil {
		return nil, syscall.EIO
	}
	entries := make([]fuse.DirEntry, 0, len(names)+2)
	entries = append(entries,
		fuse.DirEntry{Name: ".", Mode: syscall.S_IFDIR},
		fuse.DirEntry{Name: "..", Mode: syscall.S_IFDIR},
	)
	for _, n := range names {
		path, _, resolveErr := overlay.Resolve(d.upper, d.base, n)
		if resolveErr != nil {
			continue
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			continue
		}
		mode := uint32(syscall.S_IFREG)
		if info.IsDir() {
			mode = syscall.S_IFDIR
		} else if info.Mode()&os.ModeSymlink != 0 {
			mode = syscall.S_IFLNK
		}
		entries = append(entries, fuse.DirEntry{Name: n, Mode: mode})
	}
	return fs.NewListDirStream(entries), fs.OK
}

func (d *OverlayDir) Create(
	ctx context.Context,
	name string,
	flags uint32,
	mode uint32,
	out *fuse.EntryOut,
) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if err := os.MkdirAll(d.upper, 0o755); err != nil {
		return nil, nil, 0, syscall.EIO
	}
	path := filepath.Join(d.upper, name)
	fd, err := syscall.Open(path, int(flags)|syscall.O_CREAT, mode)
	if err != nil {
		return nil, nil, 0, err.(syscall.Errno)
	}
	info, _ := os.Lstat(path)
	fillAttr(&out.Attr, info)
	out.SetEntryTimeout(attrTTL)
	out.SetAttrTimeout(attrTTL)
	node := &OverlayFile{upper: d.upper, base: d.base, name: name}
	ch := d.NewInode(ctx, node, fs.StableAttr{Mode: syscall.S_IFREG})
	return ch, fs.NewLoopbackFile(fd), fuse.FOPEN_KEEP_CACHE, fs.OK
}

func (d *OverlayDir) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childUpper := filepath.Join(d.upper, name)
	if err := os.Mkdir(childUpper, os.FileMode(mode)); err != nil {
		return nil, syscall.EEXIST
	}
	info, _ := os.Lstat(childUpper)
	fillAttr(&out.Attr, info)
	out.SetEntryTimeout(attrTTL)
	out.SetAttrTimeout(attrTTL)
	node := &OverlayDir{upper: childUpper, base: filepath.Join(d.base, name)}
	ch := d.NewInode(ctx, node, fs.StableAttr{Mode: syscall.S_IFDIR})
	return ch, fs.OK
}

func (d *OverlayDir) Unlink(ctx context.Context, name string) syscall.Errno {
	upperPath := filepath.Join(d.upper, name)
	if _, err := os.Lstat(upperPath); err == nil {
		if err := os.Remove(upperPath); err != nil {
			return syscall.EIO
		}
	}
	// If the name exists in the base, create a whiteout so it appears deleted.
	if _, err := os.Lstat(filepath.Join(d.base, name)); err == nil {
		if woErr := overlay.CreateWhiteout(d.upper, name); woErr != nil {
			return syscall.EIO
		}
	}
	return fs.OK
}

func (d *OverlayDir) Rmdir(ctx context.Context, name string) syscall.Errno {
	upperPath := filepath.Join(d.upper, name)
	if err := os.Remove(upperPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return syscall.EIO
	}
	if _, err := os.Lstat(filepath.Join(d.base, name)); err == nil {
		if woErr := overlay.CreateWhiteout(d.upper, name); woErr != nil {
			return syscall.EIO
		}
	}
	return fs.OK
}

func (d *OverlayDir) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if err := os.MkdirAll(d.upper, 0o755); err != nil {
		return nil, syscall.EIO
	}
	linkPath := filepath.Join(d.upper, name)
	if err := os.Symlink(target, linkPath); err != nil {
		return nil, syscall.EEXIST
	}
	info, _ := os.Lstat(linkPath)
	fillAttr(&out.Attr, info)
	out.SetEntryTimeout(attrTTL)
	out.SetAttrTimeout(attrTTL)
	node := &OverlayFile{upper: d.upper, base: d.base, name: name}
	ch := d.NewInode(ctx, node, fs.StableAttr{Mode: syscall.S_IFLNK})
	return ch, fs.OK
}

func (d *OverlayDir) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.upper, &st); err != nil {
		return syscall.EIO
	}
	out.FromStatfsT(&st)
	return fs.OK
}

// OverlayFile is a regular file or symlink node. Its resolved host path may
// change after a CoW promotion (base → upper), so Open holds mu while
// performing the copy.
type OverlayFile struct {
	fs.Inode
	mu    sync.RWMutex
	upper string // parent's upper dir
	base  string // parent's base dir
	name  string // entry name within the parent
}

var (
	_ fs.NodeGetattrer  = (*OverlayFile)(nil)
	_ fs.NodeOpener     = (*OverlayFile)(nil)
	_ fs.NodeReadlinker = (*OverlayFile)(nil)
)

func (f *OverlayFile) hostPath() (string, bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return overlay.Resolve(f.upper, f.base, f.name)
}

func (f *OverlayFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	path, _, err := f.hostPath()
	if err != nil {
		return syscall.ENOENT
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		return syscall.EIO
	}
	fillAttr(&out.Attr, info)
	out.SetTimeout(attrTTL)
	return fs.OK
}

func (f *OverlayFile) Open(ctx context.Context, openFlags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	willWrite := openFlags&(syscall.O_WRONLY|syscall.O_RDWR) != 0
	if willWrite {
		f.mu.Lock()
		path, isUpper, resolveErr := overlay.Resolve(f.upper, f.base, f.name)
		if resolveErr != nil {
			f.mu.Unlock()
			return nil, 0, syscall.ENOENT
		}
		if !isUpper {
			if _, copyErr := overlay.CopyUp(path, f.upper); copyErr != nil {
				f.mu.Unlock()
				log.Printf("copy-up %s: %v", f.name, copyErr)
				return nil, 0, syscall.EIO
			}
		}
		f.mu.Unlock()
	}

	path, _, err := f.hostPath()
	if err != nil {
		return nil, 0, syscall.ENOENT
	}
	fd, openErr := syscall.Open(path, int(openFlags)|syscall.O_NOFOLLOW, 0)
	if openErr != nil {
		return nil, 0, openErr.(syscall.Errno)
	}
	return fs.NewLoopbackFile(fd), fuse.FOPEN_KEEP_CACHE, fs.OK
}

func (f *OverlayFile) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	path, _, err := f.hostPath()
	if err != nil {
		return nil, syscall.ENOENT
	}
	target, readlinkErr := os.Readlink(path)
	if readlinkErr != nil {
		return nil, syscall.EIO
	}
	return []byte(target), fs.OK
}

// fillAttr populates a fuse.Attr from an os.FileInfo.
func fillAttr(a *fuse.Attr, info os.FileInfo) {
	a.Size = uint64(info.Size())
	a.Mode = uint32(info.Mode())
	a.Mtime = uint64(info.ModTime().Unix())
	a.Mtimensec = uint32(info.ModTime().Nanosecond())
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		a.Ino = st.Ino
		a.Uid = st.Uid
		a.Gid = st.Gid
	}
}

func durationPtr(d time.Duration) *time.Duration { return &d }

func main() {
	upperFlag := flag.String("upper", "", "writable upper layer directory")
	baseFlag := flag.String("base", "", "read-only base layer directory")
	mountFlag := flag.String("mount", "", "mount point")
	debugFlag := flag.Bool("debug", false, "enable FUSE protocol debug logging")
	flag.Parse()

	if *upperFlag == "" || *baseFlag == "" || *mountFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: overlay-fs -upper DIR -base DIR -mount DIR [-debug]")
		os.Exit(1)
	}
	for _, dir := range []string{*upperFlag, *baseFlag, *mountFlag} {
		if _, err := os.Stat(dir); err != nil {
			fmt.Fprintf(os.Stderr, "directory %q: %v\n", dir, err)
			os.Exit(1)
		}
	}

	root := &OverlayDir{
		upper: *upperFlag,
		base:  *baseFlag,
	}
	opts := &fs.Options{
		AttrTimeout:  durationPtr(attrTTL),
		EntryTimeout: durationPtr(attrTTL),
		MountOptions: fuse.MountOptions{
			FsName: "overlay-fs",
			Name:   "overlay-fs",
			Debug:  *debugFlag,
		},
	}
	server, err := fs.Mount(*mountFlag, root, opts)
	if err != nil {
		log.Fatalf("Mount: %v", err)
	}
	log.Printf("overlay-fs mounted on %s (upper=%s base=%s); ctrl-C or fusermount3 -u to unmount",
		*mountFlag, *upperFlag, *baseFlag)
	server.Wait()
}
```

The compile-time interface assertions (`var _ fs.NodeLookuper = (*OverlayDir)(nil)`) catch missing method implementations at build time rather than at the first kernel call to that operation.

## Common Mistakes

**Wrong: checking upper before whiteout in Resolve.**

```go
// Wrong order — upper file shadows whiteout marker
up := filepath.Join(upperDir, name)
if _, err := os.Lstat(up); err == nil {
    return up, true, nil
}
wo := filepath.Join(upperDir, WhiteoutName(name))
if _, err := os.Lstat(wo); err == nil {
    return "", false, ErrWhiteout
}
```

What happens: a name deleted from the base (whiteout created) then recreated in the upper (new file) looks correct, but a name that is only whited out (no upper file) would return the base file instead of ErrWhiteout if the code ever lands in the wrong branch due to a race. More importantly, the whiteout IS in the upper directory, so the "upper first" logic would never reach the whiteout check — the whiteout file itself would be returned as the upper path, which is wrong.

Fix: always check for the whiteout marker before checking for the actual name. The correct order is: whiteout → upper name → base name.

**Wrong: using the old `fuse` package instead of `fs`.**

```go
// Wrong — the fuse package is the low-level protocol layer
import "github.com/hanwen/go-fuse/v2/fuse"

type myFS struct{}
func (m *myFS) Lookup(...) { ... }
```

What happens: `github.com/hanwen/go-fuse/v2/fuse` is the raw protocol layer. It works, but requires implementing the entire `RawFileSystem` interface (~30 methods) and manually managing inode numbers. The `fs` package wraps it with an inode tree and a much smaller set of interfaces.

Fix: embed `fs.Inode` and import `github.com/hanwen/go-fuse/v2/fs`. Only implement the `NodeXxxer` interfaces your filesystem needs; the rest default to `ENOSYS`.

**Wrong: setting TTL to zero for "correctness".**

Setting `AttrTimeout` and `EntryTimeout` to zero forces the kernel to ask the FUSE daemon for attributes on every `stat` and every directory walk. A `find /mnt/overlay -type f` on a large tree becomes thousands of FUSE round-trips where it could have been zero (all cached). The overlay in this lesson is the sole writer to the upper layer, so a one-second TTL loses no correctness and eliminates most metadata round-trips.

Fix: set `AttrTimeout` and `EntryTimeout` to `time.Second` in `fs.Options` and call `out.SetTimeout` / `out.SetEntryTimeout` in every `Getattr` and `Lookup`.

**Wrong: panicking on `syscall.Open` error without checking the type.**

```go
fd, err := syscall.Open(path, flags, 0)
return fs.NewLoopbackFile(fd), 0, err.(syscall.Errno) // panics if err is not Errno
```

`syscall.Open` always returns `syscall.Errno` when it errors, but the type assertion `err.(syscall.Errno)` panics if `err` is `nil` (the success case) or any other type. Always check `err != nil` before asserting.

## Verification

The `overlay` package compiles and tests on any platform:

```bash
cd ~/go-exercises/overlay-fs
test -z "$(gofmt -l ./overlay/)"
go vet ./overlay/
go test -count=1 -race ./overlay/
```

The full build and manual mount test requires Linux with the `fuse` kernel module:

```bash
# build the daemon
go build ./cmd/overlay-fs/

# prepare directories
mkdir -p /tmp/base /tmp/upper /tmp/mnt

# populate the base layer (read-only content)
echo "base version" > /tmp/base/hello.txt
echo "will be deleted" > /tmp/base/gone.txt

# mount
./overlay-fs -upper /tmp/upper -base /tmp/base -mount /tmp/mnt &

# verify merge
ls /tmp/mnt                         # hello.txt, gone.txt
cat /tmp/mnt/hello.txt              # base version

# write triggers copy-on-write
echo "new version" > /tmp/mnt/hello.txt
cat /tmp/mnt/hello.txt              # new version
cat /tmp/base/hello.txt             # base version (unmodified)
ls /tmp/upper/                      # hello.txt (the upper copy)

# delete triggers whiteout
rm /tmp/mnt/gone.txt
ls /tmp/mnt                         # gone.txt is absent
ls /tmp/upper/                      # .wh.gone.txt whiteout present
ls /tmp/base/                       # gone.txt still exists in base

# unmount
fusermount3 -u /tmp/mnt
```

## Summary

- FUSE forwards VFS calls from the kernel to a userspace daemon through `/dev/fuse`; go-fuse/v2/fs handles the protocol and dispatches calls to `NodeXxxer` interfaces on node types that embed `fs.Inode`.
- Three rules govern the overlay merge: upper wins over base, whiteout markers hide base entries, opaque directory markers suppress the base layer entirely.
- Copy-on-write promotes a base-layer file to the upper layer on the first write; the base file is never modified. `CopyUp` is idempotent: concurrent callers hitting the same file are safe.
- The overlay logic (`Resolve`, `CopyUp`, `MergedNames`) has no FUSE imports and tests on any platform; the FUSE callbacks are thin wrappers that call these functions.
- Attribute TTL controls the kernel's metadata cache; zero TTL is correct but expensive; one second is appropriate for a local overlay where the daemon is the sole writer.

## What's Next

Next: [io_uring Integration](../05-io-uring-integration/05-io-uring-integration.md).

## Resources

- go-fuse v2/fs package documentation: https://pkg.go.dev/github.com/hanwen/go-fuse/v2/fs
- go-fuse v2/fuse package (protocol types): https://pkg.go.dev/github.com/hanwen/go-fuse/v2/fuse
- Linux kernel OverlayFS documentation: https://www.kernel.org/doc/html/latest/filesystems/overlayfs.html
- FUSE protocol specification and /dev/fuse message format: https://libfuse.github.io/doxygen/fuse__kernel_8h.html
- Docker overlay2 storage driver internals: https://docs.docker.com/storage/storagedriver/overlayfs-driver/
