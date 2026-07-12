# 6. OCI Image Pulling

Pulling a container image is a multi-step protocol: authenticate against the registry's token service, retrieve an image manifest (which may be a manifest list requiring platform selection), download each layer as a gzip-compressed tar archive, verify every blob by SHA-256 digest, cache blobs in a content-addressable store to skip re-downloading, and extract layers in order to build the overlay root filesystem. Each step has non-obvious constraints specified in the OCI Distribution Specification, and most production bugs occur at the seams between steps.

```text
ociimage/
  go.mod
  manifest.go
  reference.go
  store.go
  auth.go
  client.go
  extract.go
  puller.go
  reference_test.go
  store_test.go
  extract_test.go
  puller_test.go
  cmd/demo/main.go
```

## Concepts

### The OCI Distribution Specification

The OCI Distribution Specification (originally Docker Registry HTTP API V2) defines the REST API that all compliant registries implement: Docker Hub, GitHub Container Registry, Amazon ECR, Google Artifact Registry. Two endpoints carry almost all the work:

- `GET /v2/<name>/manifests/<reference>` — fetch an image manifest by tag or digest.
- `GET /v2/<name>/blobs/<digest>` — fetch a content blob (layer or config) by its SHA-256 digest.

Both endpoints may redirect (HTTP 307) to a CDN for the actual blob data. Follow redirects; the Go default `http.Client` does so automatically. An `Accept` request header on the manifest endpoint tells the registry which manifest format you can consume; the response `Content-Type` header tells you which one was returned.

### Image References

A complete image reference has the form `[registry/]repository[:tag][@digest]`. The defaulting rules for Docker Hub are:

- No slash in the name — official image: `registry-1.docker.io` / `library/<name>`.
- One slash, first component has no dot or colon — user image: `registry-1.docker.io` / `<name>`.
- First component contains a dot or colon, or equals `localhost` — explicit registry.
- Neither tag nor digest present: default tag is `latest`.

The colon in `localhost:5000/img:v1` must not be treated as a tag separator; the guard is to check whether any slash follows the colon.

### Manifest Types and Content Negotiation

A registry can return several manifest formats:

- `application/vnd.oci.image.manifest.v1+json` — single-platform manifest, lists config and layers.
- `application/vnd.oci.image.index.v1+json` — OCI index, a list of per-platform manifests.
- `application/vnd.docker.distribution.manifest.v2+json` — Docker v2 manifest (same structure as OCI).
- `application/vnd.docker.distribution.manifest.list.v2+json` — Docker manifest list (same role as OCI index).

Send all four types in the `Accept` header. Inspect the response `Content-Type` to distinguish an index from a manifest. When an index is returned, find the descriptor whose `platform.os == "linux"` and `platform.architecture == "amd64"` and fetch that manifest by digest.

### Bearer Token Authentication

Most registries use RFC 6750 Bearer tokens. The protocol:

1. Make an unauthenticated request.
2. Receive `401 Unauthorized` with `Www-Authenticate: Bearer realm="<url>",service="...",scope="..."`.
3. Fetch a token: `GET <realm>?service=<service>&scope=<scope>`. Docker Hub returns `{"token":"..."}`, some registries return `{"access_token":"..."}`.
4. Retry the original request with `Authorization: Bearer <token>`.

Docker Hub delegates auth to `auth.docker.io/token`, separate from `registry-1.docker.io`. Store tokens by registry host so each subsequent request skips the 401 round-trip.

### Content-Addressable Storage

Every blob in the OCI protocol is identified by `sha256:<64-hex-chars>`. A content-addressable store saves each blob at `<dir>/sha256/<first-2-hex>/<full-hex>`, matching the layout used by containerd and Docker. Before downloading a layer, check whether the digest is already present; if yes, skip the download entirely. This is why twenty containers sharing the same Alpine base image only store it once.

Write downloaded blobs to a temp file, stream through `crypto/sha256.New()` as bytes arrive, compare the final digest before renaming to the final path. A rename is atomic on Linux; it ensures no partially-written blob is ever visible to readers.

### Layer Extraction and Whiteout Files

OCI layers are gzip-compressed tar archives (`archive/tar` inside `compress/gzip`). Extract them bottom-to-top. For each entry:

- Resolve the target path as `filepath.Join(dst, filepath.Clean("/"+hdr.Name))`. The leading-slash-then-clean idiom is the standard path-traversal guard: `filepath.Clean("/../../../etc/passwd")` returns `"/etc/passwd"`, and `filepath.Join(dst, "/etc/passwd")` joins by concatenation (not replacement), landing inside `dst`.
- Handle `tar.TypeDir`, `tar.TypeReg` (and `tar.TypeRegA`), `tar.TypeSymlink`, and `tar.TypeLink`. Skip unknown types.
- Whiteout files mark deletions without modifying read-only lower layers. A file `etc/.wh.hosts` means remove `etc/hosts`. A file `.wh..wh..opq` in a directory means hide the entire directory's contents from lower layers (opaque whiteout). Process whiteouts before creating files.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/38-capstone-container-runtime/06-oci-image-pulling/06-oci-image-pulling/cmd/demo
cd go-solutions/38-capstone-container-runtime/06-oci-image-pulling/06-oci-image-pulling
```

### Exercise 1: Image Reference Parsing

Create `reference.go`:

```go
package ociimage

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidReference is returned when an image reference string cannot be parsed.
var ErrInvalidReference = errors.New("invalid image reference")

// Reference holds the parsed components of an OCI image reference.
type Reference struct {
	// Registry is the host (and optional port) of the registry,
	// e.g. "registry-1.docker.io", "ghcr.io", "localhost:5000".
	Registry string
	// Repository is the path within the registry, e.g. "library/alpine".
	Repository string
	// Tag is the mutable pointer to an image, e.g. "3.19".
	// Empty when Digest is set.
	Tag string
	// Digest is the immutable content identifier, e.g. "sha256:abc...".
	// Empty when only Tag is set.
	Digest string
}

// ParseReference parses an OCI image reference into its components.
// It applies the Docker Hub defaulting rules:
//
//	"alpine"              -> registry-1.docker.io / library/alpine  : latest
//	"alpine:3.19"         -> registry-1.docker.io / library/alpine  : 3.19
//	"user/app:v1"         -> registry-1.docker.io / user/app        : v1
//	"ghcr.io/user/img"   -> ghcr.io              / user/img         : latest
//	"localhost:5000/myimg"-> localhost:5000        / myimg            : latest
func ParseReference(ref string) (Reference, error) {
	if ref == "" {
		return Reference{}, fmt.Errorf("%w: empty string", ErrInvalidReference)
	}

	r := Reference{}

	// Strip digest.
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		r.Digest = ref[i+1:]
		ref = ref[:i]
	}

	// Strip tag. Only treat the rightmost colon as a tag separator when no
	// slash appears after it — that guard prevents consuming the port in
	// "localhost:5000/img".
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		if !strings.Contains(ref[i:], "/") {
			r.Tag = ref[i+1:]
			ref = ref[:i]
		}
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}

	// Determine the registry host vs. the repository path.
	// A registry host contains a dot, a colon, or is exactly "localhost".
	if slash := strings.Index(ref, "/"); slash >= 0 {
		host := ref[:slash]
		if strings.ContainsAny(host, ".:") || host == "localhost" {
			r.Registry = host
			r.Repository = ref[slash+1:]
		} else {
			// Docker Hub user image: the full path is the repository.
			r.Registry = "registry-1.docker.io"
			r.Repository = ref
		}
	} else {
		// No slash: Docker Hub official image.
		r.Registry = "registry-1.docker.io"
		r.Repository = "library/" + ref
	}

	if r.Repository == "" {
		return Reference{}, fmt.Errorf("%w: empty repository in %q", ErrInvalidReference, ref)
	}
	return r, nil
}
```

The `!strings.Contains(ref[i:], "/")` guard at the tag-stripping step is the only line that prevents `localhost:5000/img` from being misread as repository `localhost` with tag `5000/img`.

### Exercise 2: Manifest Types and Content-Addressable Storage

Create `manifest.go`:

```go
package ociimage

// OCI and Docker manifest media types. Include all four in the Accept header
// so the registry can return whichever format it prefers.
const (
	MediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex           = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// Descriptor identifies a blob by media type, digest, and byte size.
// Used for both image layers and the image configuration blob.
type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *Platform `json:"platform,omitempty"`
}

// Platform describes the OS and CPU architecture a manifest targets.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// Manifest is a single-platform OCI or Docker v2 image manifest.
// It lists the image configuration blob and the ordered set of layer blobs.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Index is an OCI image index or Docker manifest list.
// It holds one Descriptor per supported platform; each Descriptor points
// to the platform-specific Manifest.
type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []Descriptor `json:"manifests"`
}

// ImageConfig holds the fields from the OCI image configuration that are
// relevant to starting a container process.
type ImageConfig struct {
	Config struct {
		Env        []string `json:"Env"`
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"config"`
}
```

Create `store.go`:

```go
package ociimage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrDigestMismatch is returned when a downloaded blob's actual SHA-256 digest
// does not match the expected digest declared in the manifest.
var ErrDigestMismatch = errors.New("digest mismatch")

// Store is a content-addressable store for OCI blobs.
// Blobs are stored at <dir>/<algo>/<first-2-hex>/<full-hex>.
// This two-level directory sharding matches the layout used by containerd.
type Store struct {
	dir string
}

// NewStore creates or opens a content-addressable store rooted at dir.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ociimage: create store %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Has reports whether a blob with the given digest is already present.
func (s *Store) Has(digest string) bool {
	p, err := s.blobPath(digest)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Path returns the on-disk path of a stored blob.
// Check Has first; Path does not verify existence.
func (s *Store) Path(digest string) (string, error) {
	return s.blobPath(digest)
}

// Write streams r into the store, verifies the final SHA-256 matches digest,
// and renames the temp file into place atomically.
// A digest mismatch removes the temp file and returns ErrDigestMismatch.
func (s *Store) Write(digest string, r io.Reader) error {
	dst, err := s.blobPath(digest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("ociimage: store mkdir: %w", err)
	}

	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("ociimage: store create temp: %w", err)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), r); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("ociimage: store write: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != digest {
		os.Remove(tmp)
		return fmt.Errorf("%w: got %s, want %s", ErrDigestMismatch, got, digest)
	}

	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ociimage: store rename: %w", err)
	}
	return nil
}

// blobPath converts "sha256:<hex>" into a sharded filesystem path.
func (s *Store) blobPath(digest string) (string, error) {
	algo, sum, ok := strings.Cut(digest, ":")
	if !ok || len(sum) < 2 {
		return "", fmt.Errorf("%w: malformed digest %q", ErrDigestMismatch, digest)
	}
	return filepath.Join(s.dir, algo, sum[:2], sum), nil
}
```

### Exercise 3: Authentication and Registry Client

Create `auth.go`:

```go
package ociimage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// parseBearer extracts key=value pairs from a Www-Authenticate Bearer header.
//
// Example input:
//
//	Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"
func parseBearer(www string) map[string]string {
	rest, ok := strings.CutPrefix(www, "Bearer ")
	if !ok {
		return nil
	}
	m := make(map[string]string)
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		m[k] = strings.Trim(v, `"`)
	}
	return m
}

// fetchToken obtains a Bearer token from the token service described in a
// Www-Authenticate header value. It handles both "token" and "access_token"
// response fields used by different registries.
func fetchToken(ctx context.Context, hc *http.Client, www string) (string, error) {
	params := parseBearer(www)
	if params == nil {
		return "", fmt.Errorf("ociimage: cannot parse Www-Authenticate: %q", www)
	}
	realm, ok := params["realm"]
	if !ok {
		return "", fmt.Errorf("ociimage: no realm in Www-Authenticate")
	}

	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("ociimage: bad token realm URL: %w", err)
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	if scope := params["scope"]; scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("ociimage: token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ociimage: token service returned %d", resp.StatusCode)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("ociimage: decode token response: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("ociimage: token response contained no token field")
}
```

Create `client.go`:

```go
package ociimage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrManifestNotFound is returned when the registry returns 404 for a manifest.
var ErrManifestNotFound = errors.New("manifest not found")

// Client speaks the OCI Distribution Specification to a registry.
// It caches Bearer tokens per registry host and deduplicates auth round-trips.
type Client struct {
	httpClient *http.Client
	store      *Store
	// scheme is "https" in production. Tests set it to "http" to use httptest servers.
	scheme string
	mu     sync.RWMutex
	tokens map[string]string // registry host -> bearer token
}

// NewClient creates a Client backed by the given content-addressable store.
func NewClient(store *Store) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		store:      store,
		scheme:     "https",
		tokens:     make(map[string]string),
	}
}

// doGET performs an authenticated GET, handling the 401 → token → retry flow.
// Each call creates a fresh *http.Request so the method is safe to call
// concurrently and after the first attempt's response body has been closed.
func (c *Client) doGET(ctx context.Context, rawURL string, headers map[string]string) (*http.Response, error) {
	newReq := func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, nil
	}

	host := hostOf(rawURL)

	c.mu.RLock()
	token := c.tokens[host]
	c.mu.RUnlock()

	req, err := newReq(token)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	resp.Body.Close()

	www := resp.Header.Get("Www-Authenticate")
	newToken, err := fetchToken(ctx, c.httpClient, www)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.tokens[host] = newToken
	c.mu.Unlock()

	req2, err := newReq(newToken)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req2)
}

// FetchManifest fetches an image manifest, resolving OCI indexes and Docker
// manifest lists to the linux/amd64 single-platform manifest automatically.
func (c *Client) FetchManifest(ctx context.Context, ref Reference) (*Manifest, error) {
	accept := strings.Join([]string{
		MediaTypeOCIManifest,
		MediaTypeOCIIndex,
		MediaTypeDockerManifest,
		MediaTypeDockerManifestList,
	}, ", ")

	ref2 := ref.Tag
	if ref.Digest != "" {
		ref2 = ref.Digest
	}
	manifestURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", c.scheme, ref.Registry, ref.Repository, ref2)

	resp, err := c.doGET(ctx, manifestURL, map[string]string{"Accept": accept})
	if err != nil {
		return nil, fmt.Errorf("ociimage: fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrManifestNotFound, manifestURL)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ociimage: manifest %s: status %d", manifestURL, resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ociimage: read manifest body: %w", err)
	}

	if isIndexMediaType(ct) {
		return c.resolveIndex(ctx, ref, body)
	}
	return parseManifest(body)
}

// resolveIndex selects the linux/amd64 manifest from an OCI index or manifest list.
func (c *Client) resolveIndex(ctx context.Context, ref Reference, body []byte) (*Manifest, error) {
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("ociimage: parse index: %w", err)
	}
	for _, d := range idx.Manifests {
		if d.Platform != nil && d.Platform.OS == "linux" && d.Platform.Architecture == "amd64" {
			return c.FetchManifest(ctx, Reference{
				Registry:   ref.Registry,
				Repository: ref.Repository,
				Digest:     d.Digest,
			})
		}
	}
	return nil, fmt.Errorf("ociimage: no linux/amd64 manifest in index for %s/%s", ref.Registry, ref.Repository)
}

// FetchBlob downloads a blob into the content-addressable store.
// If the blob is already present in the store, it is a no-op.
// progress is called with the number of bytes received per read; pass nil to
// suppress progress reporting.
func (c *Client) FetchBlob(ctx context.Context, ref Reference, d Descriptor, progress func(int64)) error {
	if c.store.Has(d.Digest) {
		return nil
	}

	blobURL := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", c.scheme, ref.Registry, ref.Repository, d.Digest)
	resp, err := c.doGET(ctx, blobURL, nil)
	if err != nil {
		return fmt.Errorf("ociimage: fetch blob %s: %w", d.Digest, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ociimage: blob %s: status %d", d.Digest, resp.StatusCode)
	}

	var r io.Reader = resp.Body
	if progress != nil {
		r = &progressReader{r: resp.Body, fn: progress}
	}
	return c.store.Write(d.Digest, r)
}

func parseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("ociimage: parse manifest: %w", err)
	}
	return &m, nil
}

func isIndexMediaType(ct string) bool {
	return ct == MediaTypeOCIIndex || ct == MediaTypeDockerManifestList ||
		strings.Contains(ct, "index") || strings.Contains(ct, "manifest.list")
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Host
}

// progressReader wraps a reader and calls fn with each chunk's byte count.
type progressReader struct {
	r  io.Reader
	fn func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.fn(int64(n))
	}
	return n, err
}
```

### Exercise 4: Layer Extraction

Create `extract.go`:

```go
package ociimage

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// isWhiteout reports whether name is an OCI whiteout entry.
// For regular whiteouts, it returns the name of the file to delete and opaque=false.
// For an opaque whiteout (.wh..wh..opq), it returns del="" and opaque=true.
// For non-whiteout names, both return values are zero.
func isWhiteout(name string) (del string, opaque bool) {
	base := filepath.Base(name)
	if base == ".wh..wh..opq" {
		return "", true
	}
	if rest, ok := strings.CutPrefix(base, ".wh."); ok {
		return filepath.Join(filepath.Dir(name), rest), false
	}
	return "", false
}

// ExtractLayer extracts a gzip-compressed tar archive from r into dst.
// Layers must be extracted in order from bottom (base image) to top (latest changes).
//
// Path traversal is neutralized: each entry is resolved with
// filepath.Join(dst, filepath.Clean("/"+hdr.Name)), which keeps every path under dst.
//
// Whiteout semantics are applied:
//   - A file named .wh.<name> deletes <name> from dst.
//   - A file named .wh..wh..opq removes all existing entries from its parent directory.
func ExtractLayer(dst string, r io.Reader) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("ociimage: gzip reader: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("ociimage: tar read: %w", err)
		}

		// Resolve and sanitize the target path under dst.
		target := filepath.Join(dst, filepath.Clean("/"+hdr.Name))

		// Apply whiteouts before any file creation.
		if del, opaque := isWhiteout(hdr.Name); opaque {
			dir := filepath.Join(dst, filepath.Clean("/"+filepath.Dir(hdr.Name)))
			entries, readErr := os.ReadDir(dir)
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return fmt.Errorf("ociimage: opaque whiteout readdir: %w", readErr)
			}
			for _, e := range entries {
				os.RemoveAll(filepath.Join(dir, e.Name()))
			}
			continue
		} else if del != "" {
			os.RemoveAll(filepath.Join(dst, filepath.Clean("/"+del)))
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("ociimage: mkdir %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("ociimage: mkdir parent of %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("ociimage: create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("ociimage: write %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("ociimage: close %s: %w", target, err)
			}
		case tar.TypeSymlink:
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("ociimage: symlink %s: %w", target, err)
			}
		case tar.TypeLink:
			linkSrc := filepath.Join(dst, filepath.Clean("/"+hdr.Linkname))
			os.Remove(target)
			if err := os.Link(linkSrc, target); err != nil {
				return fmt.Errorf("ociimage: hardlink %s: %w", target, err)
			}
		}
		// Unknown entry types (devices, fifos, sockets) are skipped silently.
		// Container images rarely contain them, and they require root on Linux.
	}
	return nil
}
```

### Exercise 5: Top-Level Pull Function

Create `puller.go`:

```go
package ociimage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// PullResult holds the outcome of a successful image pull.
type PullResult struct {
	// LayerPaths contains the on-disk path of each layer in order, bottom to top.
	// Pass these to ExtractLayer in order to build the overlay root filesystem.
	LayerPaths []string
	// Config holds the image configuration: environment, entrypoint, cmd, workdir.
	Config ImageConfig
}

// Pull downloads an image from its registry into store and returns the ordered
// layer paths and image configuration. Layers already in the store are skipped.
// progress is called with the number of bytes received per read; pass nil to
// suppress progress reporting.
func Pull(ctx context.Context, store *Store, rawRef string, progress func(int64)) (*PullResult, error) {
	ref, err := ParseReference(rawRef)
	if err != nil {
		return nil, fmt.Errorf("pull: %w", err)
	}

	client := NewClient(store)

	manifest, err := client.FetchManifest(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("pull: %w", err)
	}

	// Download the image configuration blob for env/entrypoint/cmd/workdir.
	if err := client.FetchBlob(ctx, ref, manifest.Config, nil); err != nil {
		return nil, fmt.Errorf("pull: config blob: %w", err)
	}
	cfgPath, err := store.Path(manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("pull: config path: %w", err)
	}
	cfgData, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("pull: read config: %w", err)
	}
	var cfg ImageConfig
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		return nil, fmt.Errorf("pull: parse config: %w", err)
	}

	// Download each layer in manifest order (bottom to top).
	layerPaths := make([]string, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		if err := client.FetchBlob(ctx, ref, layer, progress); err != nil {
			return nil, fmt.Errorf("pull: layer %s: %w", layer.Digest, err)
		}
		p, err := store.Path(layer.Digest)
		if err != nil {
			return nil, fmt.Errorf("pull: layer path: %w", err)
		}
		layerPaths = append(layerPaths, p)
	}

	return &PullResult{LayerPaths: layerPaths, Config: cfg}, nil
}
```

### Exercise 6: Test Suite

Create `reference_test.go`:

```go
package ociimage

import (
	"errors"
	"fmt"
	"testing"
)

func TestParseReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		wantReg  string
		wantRepo string
		wantTag  string
		wantErr  error
	}{
		{
			input:    "alpine",
			wantReg:  "registry-1.docker.io",
			wantRepo: "library/alpine",
			wantTag:  "latest",
		},
		{
			input:    "alpine:3.19",
			wantReg:  "registry-1.docker.io",
			wantRepo: "library/alpine",
			wantTag:  "3.19",
		},
		{
			input:    "user/app:v1",
			wantReg:  "registry-1.docker.io",
			wantRepo: "user/app",
			wantTag:  "v1",
		},
		{
			input:    "ghcr.io/user/img:tag",
			wantReg:  "ghcr.io",
			wantRepo: "user/img",
			wantTag:  "tag",
		},
		{
			input:    "localhost:5000/myimg:latest",
			wantReg:  "localhost:5000",
			wantRepo: "myimg",
			wantTag:  "latest",
		},
		{
			input:    "ghcr.io/org/app",
			wantReg:  "ghcr.io",
			wantRepo: "org/app",
			wantTag:  "latest",
		},
		{
			input:   "",
			wantErr: ErrInvalidReference,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()

			ref, err := ParseReference(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseReference(%q) err = %v, want %v", tc.input, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseReference(%q) unexpected error: %v", tc.input, err)
			}
			if ref.Registry != tc.wantReg {
				t.Errorf("Registry = %q, want %q", ref.Registry, tc.wantReg)
			}
			if ref.Repository != tc.wantRepo {
				t.Errorf("Repository = %q, want %q", ref.Repository, tc.wantRepo)
			}
			if ref.Tag != tc.wantTag {
				t.Errorf("Tag = %q, want %q", ref.Tag, tc.wantTag)
			}
		})
	}
}

// TestParseReferencePortNotTag verifies that the colon in "localhost:5000/img"
// is not consumed as a tag separator.
func TestParseReferencePortNotTag(t *testing.T) {
	t.Parallel()

	ref, err := ParseReference("localhost:5000/myimg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Registry != "localhost:5000" {
		t.Errorf("Registry = %q, want %q", ref.Registry, "localhost:5000")
	}
	if ref.Tag != "latest" {
		t.Errorf("Tag = %q, want %q (port must not be consumed as tag)", ref.Tag, "latest")
	}
}

func ExampleParseReference() {
	ref, err := ParseReference("alpine:3.19")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s/%s:%s\n", ref.Registry, ref.Repository, ref.Tag)
	// Output:
	// registry-1.docker.io/library/alpine:3.19
}
```

Note: the `tc := tc` copy on line 62 is retained for compatibility with Go versions before 1.22. Remove it if your toolchain is 1.22 or later, where loop variables are no longer shared across iterations.

Create `store_test.go`:

```go
package ociimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestStoreWriteAndHas(t *testing.T) {
	t.Parallel()

	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	content := []byte("hello, layer content")
	h := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(h[:])

	if s.Has(digest) {
		t.Fatal("Has() should return false before Write")
	}

	if err := s.Write(digest, bytes.NewReader(content)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !s.Has(digest) {
		t.Error("Has() should return true after Write")
	}
}

func TestStoreWriteIsIdempotent(t *testing.T) {
	t.Parallel()

	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	content := []byte("idempotent content")
	h := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(h[:])

	for i := 0; i < 2; i++ {
		if err := s.Write(digest, bytes.NewReader(content)); err != nil {
			t.Fatalf("Write call %d: %v", i+1, err)
		}
	}
}

func TestStoreWriteRejectsDigestMismatch(t *testing.T) {
	t.Parallel()

	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	wrongDigest := "sha256:" + strings.Repeat("0", 64)
	err = s.Write(wrongDigest, bytes.NewReader([]byte("actual content")))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("Write with wrong digest: err = %v, want ErrDigestMismatch", err)
	}

	// Ensure the temp file was removed on mismatch.
	if s.Has(wrongDigest) {
		t.Error("Has() should return false after a failed Write")
	}
}

func TestStoreBlobPath(t *testing.T) {
	t.Parallel()

	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	digest := "sha256:" + strings.Repeat("ab", 32)
	p, err := s.Path(digest)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	// The path must include the two-hex-char shard directory.
	if !strings.Contains(p, "/ab/") {
		t.Errorf("Path = %q, expected shard directory /ab/", p)
	}
}

// Your turn: add TestStoreMalformedDigest that calls s.Path("notadigest") and
// asserts errors.Is(err, ErrDigestMismatch).
```

Create `extract_test.go`:

```go
package ociimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// makeTarGz creates a gzip-compressed tar archive from the given name→content map.
func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Size:     int64(len(content)),
			Mode:     0o644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractLayerWritesFiles(t *testing.T) {
	t.Parallel()

	dst := t.TempDir()
	data := makeTarGz(t, map[string]string{
		"etc/os-release": "ID=alpine\n",
		"usr/bin/sh":     "#!/bin/sh\n",
	})

	if err := ExtractLayer(dst, bytes.NewReader(data)); err != nil {
		t.Fatalf("ExtractLayer: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "etc/os-release"))
	if err != nil {
		t.Fatalf("read etc/os-release: %v", err)
	}
	if string(got) != "ID=alpine\n" {
		t.Errorf("etc/os-release = %q, want %q", string(got), "ID=alpine\n")
	}
}

func TestExtractLayerAppliesWhiteout(t *testing.T) {
	t.Parallel()

	dst := t.TempDir()

	// Layer 1: create a file.
	layer1 := makeTarGz(t, map[string]string{"etc/hosts": "127.0.0.1 localhost\n"})
	if err := ExtractLayer(dst, bytes.NewReader(layer1)); err != nil {
		t.Fatalf("extract layer1: %v", err)
	}

	// Layer 2: whiteout the file.
	layer2 := makeTarGz(t, map[string]string{"etc/.wh.hosts": ""})
	if err := ExtractLayer(dst, bytes.NewReader(layer2)); err != nil {
		t.Fatalf("extract layer2: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "etc/hosts")); !os.IsNotExist(err) {
		t.Errorf("etc/hosts should not exist after whiteout; got stat err = %v", err)
	}
}

func TestExtractLayerOpaque(t *testing.T) {
	t.Parallel()

	dst := t.TempDir()

	// Layer 1: populate a directory.
	layer1 := makeTarGz(t, map[string]string{
		"usr/share/doc/readme":  "docs",
		"usr/share/doc/license": "MIT",
	})
	if err := ExtractLayer(dst, bytes.NewReader(layer1)); err != nil {
		t.Fatalf("extract layer1: %v", err)
	}

	// Layer 2: opaque whiteout hides all of usr/share/doc from lower layers.
	layer2 := makeTarGz(t, map[string]string{
		"usr/share/doc/.wh..wh..opq": "",
		"usr/share/doc/newfile":      "new",
	})
	if err := ExtractLayer(dst, bytes.NewReader(layer2)); err != nil {
		t.Fatalf("extract layer2: %v", err)
	}

	// The old files should be gone.
	if _, err := os.Stat(filepath.Join(dst, "usr/share/doc/readme")); !os.IsNotExist(err) {
		t.Errorf("readme should have been removed by opaque whiteout")
	}
	// The new file from layer2 must exist.
	if _, err := os.ReadFile(filepath.Join(dst, "usr/share/doc/newfile")); err != nil {
		t.Errorf("newfile from layer2 should exist after opaque whiteout: %v", err)
	}
}

func TestExtractLayerNeutralizesPathTraversal(t *testing.T) {
	t.Parallel()

	dst := t.TempDir()

	// Build a tar manually with a traversal path.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name:     "../../../traversal-probe",
		Typeflag: tar.TypeReg,
		Size:     int64(len("oops")),
		Mode:     0o644,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("oops")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	if err := ExtractLayer(dst, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("ExtractLayer should neutralize traversal paths silently: %v", err)
	}

	// The file must reside inside dst, not escape to a parent directory.
	// filepath.Join(dst, filepath.Clean("/"+"../../../traversal-probe"))
	// = filepath.Join(dst, "/traversal-probe") = dst+"/traversal-probe".
	if _, err := os.Stat(filepath.Join(dst, "traversal-probe")); err != nil {
		// Neutral path: file is inside dst (OK), or was skipped (also OK).
		t.Logf("traversal-probe inside dst: %v", err)
	}

	// Verify the file does NOT exist outside dst.
	outside := filepath.Join(filepath.Dir(dst), "traversal-probe")
	if _, err := os.Stat(outside); err == nil {
		os.Remove(outside)
		t.Errorf("path traversal succeeded: file created at %s", outside)
	}
}
```

Create `puller_test.go`:

```go
package ociimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRegistry builds an httptest.Server that behaves like a minimal OCI registry.
// It requires a single fixed Bearer token and serves one manifest and one blob.
func fakeRegistry(t *testing.T, manifest *Manifest, blobContent []byte) (srv *httptest.Server, blobDigest string) {
	t.Helper()

	h := sha256.Sum256(blobContent)
	blobDigest = "sha256:" + hex.EncodeToString(h[:])

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	const token = "test-bearer-token"
	var srvURL string

	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	})

	mux.HandleFunc("/v2/library/testimg/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("Www-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:library/testimg:pull"`, srvURL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", MediaTypeDockerManifest)
		w.Write(manifestJSON)
	})

	mux.HandleFunc("/v2/library/testimg/blobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("Www-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:library/testimg:pull"`, srvURL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write(blobContent)
	})

	srv = httptest.NewServer(mux)
	srvURL = srv.URL
	t.Cleanup(srv.Close)
	return srv, blobDigest
}

func TestClientFetchManifest(t *testing.T) {
	t.Parallel()

	// Create a blob whose digest we know so we can reference it in the manifest.
	blobContent := []byte("fake config blob")
	h := sha256.Sum256(blobContent)
	blobDigest := "sha256:" + hex.EncodeToString(h[:])

	fakeManifest := &Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeDockerManifest,
		Config: Descriptor{
			MediaType: "application/vnd.docker.container.image.v1+json",
			Digest:    blobDigest,
			Size:      int64(len(blobContent)),
		},
	}

	srv, _ := fakeRegistry(t, fakeManifest, blobContent)

	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(store)
	client.scheme = "http"

	ref := Reference{
		Registry:   srv.Listener.Addr().String(),
		Repository: "library/testimg",
		Tag:        "latest",
	}

	m, err := client.FetchManifest(context.Background(), ref)
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if m.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, want 2", m.SchemaVersion)
	}
	if m.Config.Digest != blobDigest {
		t.Errorf("Config.Digest = %q, want %q", m.Config.Digest, blobDigest)
	}
}

func TestClientFetchBlobVerifiesDigest(t *testing.T) {
	t.Parallel()

	content := []byte("hello, layer")
	h := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(h[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/test/repo/blobs/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(store)
	client.scheme = "http"

	ref := Reference{
		Registry:   srv.Listener.Addr().String(),
		Repository: "test/repo",
	}
	desc := Descriptor{Digest: digest, Size: int64(len(content))}

	if err := client.FetchBlob(context.Background(), ref, desc, nil); err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	if !store.Has(digest) {
		t.Error("store should have blob after FetchBlob")
	}
}

func TestClientFetchBlobDigestMismatch(t *testing.T) {
	t.Parallel()

	content := []byte("hello, layer")
	wrongDigest := "sha256:" + strings.Repeat("0", 64)

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/test/repo/blobs/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(store)
	client.scheme = "http"

	ref := Reference{Registry: srv.Listener.Addr().String(), Repository: "test/repo"}
	desc := Descriptor{Digest: wrongDigest}

	err = client.FetchBlob(context.Background(), ref, desc, nil)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("FetchBlob with wrong digest: err = %v, want ErrDigestMismatch", err)
	}
}

func TestClientFetchBlobSkipsExisting(t *testing.T) {
	t.Parallel()

	content := []byte("existing blob")
	h := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(h[:])

	requestCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/test/repo/blobs/", func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Write(content)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Pre-populate the store.
	if err := store.Write(digest, strings.NewReader(string(content))); err != nil {
		t.Fatal(err)
	}

	client := NewClient(store)
	client.scheme = "http"
	ref := Reference{Registry: srv.Listener.Addr().String(), Repository: "test/repo"}
	desc := Descriptor{Digest: digest}

	if err := client.FetchBlob(context.Background(), ref, desc, nil); err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	if requestCount != 0 {
		t.Errorf("FetchBlob made %d HTTP requests for an already-cached blob, want 0", requestCount)
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"example.com/ociimage"
)

func main() {
	rawRef := "alpine:3.19"
	if len(os.Args) > 1 {
		rawRef = os.Args[1]
	}

	// Show the parsed reference components before attempting a pull.
	ref, err := ociimage.ParseReference(rawRef)
	if err != nil {
		log.Fatalf("bad reference %q: %v", rawRef, err)
	}
	fmt.Printf("Registry:   %s\n", ref.Registry)
	fmt.Printf("Repository: %s\n", ref.Repository)
	fmt.Printf("Tag:        %s\n", ref.Tag)
	if ref.Digest != "" {
		fmt.Printf("Digest:     %s\n", ref.Digest)
	}

	storeDir := os.Getenv("OCI_STORE")
	if storeDir == "" {
		storeDir = "/tmp/oci-store"
	}

	store, err := ociimage.NewStore(storeDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	fmt.Printf("\nPulling %s (store: %s)...\n", rawRef, storeDir)

	var totalBytes int64
	result, err := ociimage.Pull(context.Background(), store, rawRef, func(n int64) {
		totalBytes += n
		fmt.Printf("\r  downloaded %d bytes", totalBytes)
	})
	if err != nil {
		log.Fatalf("pull: %v", err)
	}
	fmt.Println()

	fmt.Printf("\nPulled %d layer(s):\n", len(result.LayerPaths))
	for i, p := range result.LayerPaths {
		fi, _ := os.Stat(p)
		if fi != nil {
			fmt.Printf("  [%d] %s (%d bytes)\n", i, p, fi.Size())
		} else {
			fmt.Printf("  [%d] %s\n", i, p)
		}
	}

	cfg := result.Config.Config
	if len(cfg.Env) > 0 {
		fmt.Println("\nEnvironment:")
		for _, e := range cfg.Env {
			fmt.Printf("  %s\n", e)
		}
	}
	if len(cfg.Entrypoint) > 0 {
		fmt.Printf("Entrypoint: %v\n", cfg.Entrypoint)
	}
	if len(cfg.Cmd) > 0 {
		fmt.Printf("Cmd:        %v\n", cfg.Cmd)
	}
	if cfg.WorkingDir != "" {
		fmt.Printf("WorkingDir: %s\n", cfg.WorkingDir)
	}
}
```

Run the demo against Docker Hub (requires network):

```bash
go run ./cmd/demo alpine:3.19
go run ./cmd/demo ghcr.io/user/myapp:latest
OCI_STORE=/var/cache/oci go run ./cmd/demo ubuntu:22.04
```

## Common Mistakes

### Treating the Registry Port as a Tag

Wrong: `ParseReference("localhost:5000/img")` returns tag `"5000/img"` because the code splits on the last colon unconditionally.

What happens: the manifest URL becomes `.../manifests/5000/img`, a 404.

Fix: only treat the colon as a tag separator when no slash appears after it. The guard `!strings.Contains(ref[i:], "/")` in `reference.go` implements this rule.

### Not Following Blob Redirects

Wrong: treat an HTTP 307 response from the blob endpoint as an error.

What happens: blob downloads from Docker Hub always redirect to a CDN. Without following the redirect, every layer download fails.

Fix: the default `http.Client` follows redirects automatically up to ten hops. Do not override `CheckRedirect` unless you specifically need to cap redirects.

### Verifying Digest Only at the End

Wrong: download the entire layer to a file, then compute the SHA-256 of the file.

What happens: if the disk runs full mid-download, the partial file has no meaningful digest to compare against. The corrupt file is renamed into the store.

Fix: stream through `io.MultiWriter(f, h)` as bytes arrive. The digest is ready the moment all bytes are written, and the file is either complete or the write failed. Rename only after `got == digest`.

### Extracting Layers in Reverse Order

Wrong: process `manifest.Layers` from last to first.

What happens: files added in later layers are overwritten by earlier layers. The container sees the base image without any of the applied changes.

Fix: extract layers in the order they appear in the manifest array — index 0 is the bottommost layer. Each subsequent layer's changes (including whiteouts) are applied on top of the previous result.

### Skipping the Opaque Whiteout Check

Wrong: handle only `.wh.<name>` whiteouts, ignore `.wh..wh..opq`.

What happens: when a new image layer replaces an entire directory tree, the old files remain visible through the overlay. The container sees a mixture of old and new directory contents.

Fix: when `hdr.Name` contains `.wh..wh..opq`, remove all existing entries from that directory before processing the rest of the layer.

### Using `strings.Contains(ct, "json")` to Detect Manifest Type

Wrong: `if strings.Contains(ct, "json") { ... }` — this matches both manifests and indexes.

What happens: an OCI index is parsed as a `Manifest` struct. The `Config` and `Layers` fields are empty; the pull silently produces zero layers.

Fix: check for the index/manifest-list media types explicitly before falling back to single-platform manifest parsing.

## Verification

The tests are hermetic — no network access is required — because all HTTP interactions go through `httptest.Server`. The `cmd/demo` program requires network access and is excluded from `go test`.

```bash
cd ~/go-exercises/ociimage
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector is important here: `Client` uses `sync.RWMutex` to guard the token cache, and the race detector catches any locking mistakes before they become intermittent production failures.

To run the full pull against a real registry (requires network and a running Docker Hub):

```bash
go run ./cmd/demo alpine:3.19
go run ./cmd/demo ubuntu:22.04
```

Add at least one test of your own: `TestParseReferenceWithDigest` — call `ParseReference("alpine@sha256:" + strings.Repeat("a", 64))` and assert `ref.Digest` is set and `ref.Tag` is empty.

## Summary

- The OCI Distribution Specification defines `/v2/<name>/manifests/<ref>` and `/v2/<name>/blobs/<digest>` as the two primary endpoints for all compliant registries.
- Bearer token authentication follows a 401 → fetch-token → retry flow; Docker Hub uses a separate auth service at `auth.docker.io`.
- Multi-platform images use an OCI index or Docker manifest list; the client must select the correct platform descriptor and fetch that manifest by digest.
- Content-addressable storage keyed by SHA-256 digest eliminates redundant downloads across images that share layers.
- Digest verification must stream through `crypto/sha256` as bytes arrive; the temp-file-then-rename pattern ensures the store never contains a partial blob.
- Layer extraction order is bottom-to-top; whiteout files (`.wh.<name>` and `.wh..wh..opq`) must be processed to correctly represent deletions from lower layers.

## What's Next

Next: [Container Lifecycle Management](../07-container-lifecycle/07-container-lifecycle.md).

## Resources

- [OCI Distribution Specification](https://github.com/opencontainers/distribution-spec/blob/main/spec.md) — the authoritative registry API reference.
- [OCI Image Specification: Layer Filesystem Changeset](https://github.com/opencontainers/image-spec/blob/main/layer.md) — defines manifest structure, layer media types, and whiteout semantics.
- [Docker Registry Token Authentication](https://distribution.github.io/distribution/spec/auth/token/) — the Bearer token flow used by Docker Hub and compatible registries.
- [pkg.go.dev: archive/tar](https://pkg.go.dev/archive/tar) — `tar.Reader`, `tar.Header`, and type flag constants used in layer extraction.
- [pkg.go.dev: crypto/sha256](https://pkg.go.dev/crypto/sha256) — `sha256.New()` returns a `hash.Hash` suitable for streaming digest verification via `io.MultiWriter`.
