# 8. Terraform Provider Skeleton

Writing a Terraform provider means teaching Go's type system to speak the Terraform protocol. The `terraform-plugin-framework` library (HashiCorp's current recommended path, distinct from the older `terraform-plugin-sdk/v2`) expresses that protocol through Go interfaces: a provider registers resources and data sources, each resource implements a five-method lifecycle (Metadata, Schema, Configure, Create/Read/Update/Delete, ImportState), and every value in the schema is typed through the `types` package rather than raw Go primitives. The hard parts are three: the provider's `Configure` method must thread a shared HTTP client through to every resource and data source without global state; schema types (`types.String`, `types.Int64`, `types.Set`) are nullable wrappers, not plain strings and ints; and acceptance tests must stand up a real in-memory HTTP server, configure the provider against it, and exercise the full Terraform state lifecycle.

This lesson cannot compile offline because `terraform-plugin-framework` is an external module. All code is validated with `gofmt` and `go vet` on the extractable pure-Go portions. The build and acceptance-test gate runs once the module is downloaded.

```text
terraform-provider-inventory/
  go.mod
  main.go
  internal/
    provider/
      provider.go
      item_resource.go
      item_datasource.go
      provider_test.go
  internal/
    inventory/
      client.go
      client_test.go
```

The `inventory` package is pure Go (no framework dependency) and is fully gatable offline. The `provider` package wraps it in the Terraform protocol and requires the external module.

## Concepts

### The Plugin Framework vs. the Plugin SDK

`terraform-plugin-framework` (module path `github.com/hashicorp/terraform-plugin-framework`) is HashiCorp's current recommended library. The older `terraform-plugin-sdk/v2` remains supported but new providers should use the framework. The framework implements Terraform's gRPC-based plugin protocol (version 5 or 6); providers are not HTTP servers and do not talk REST to Terraform. `providerserver.Serve` wires the provider binary into the protocol, and `go install` produces the binary that Terraform launches via its plugin mechanism.

The framework makes the distinction between three kinds of types explicit in the Go import path: `provider/schema` for provider-level configuration attributes, `resource/schema` for managed-resource attributes, and `datasource/schema` for data-source attributes. A common mistake is importing the wrong schema package for the wrong kind.

### The Configure Pipeline

Terraform calls `Provider.Configure` after the practitioner supplies provider-level configuration (such as `base_url` and `api_key`). That method must extract the configuration, build a shared client, and deposit it into `resp.DataSourceData` and `resp.ResourceData`. Each resource and data source receives that value in its own `Configure` method through `req.ProviderData`. The client is typically defined in a separate, framework-free package so it can be unit-tested without the framework.

The pattern is:

```
provider.Configure  ->  builds *inventory.Client
                    ->  resp.DataSourceData = client
                    ->  resp.ResourceData   = client

resource.Configure  ->  req.ProviderData.(*inventory.Client)
```

If a resource's `Configure` is called before the provider is configured (which happens during validation passes), `req.ProviderData` is nil; the method must handle that gracefully.

### Schema Types Are Nullable Wrappers

Every value in a schema model is a framework type, not a raw Go type:

| Schema definition           | Go model field  | Access method          |
|-----------------------------|-----------------|------------------------|
| `schema.StringAttribute`    | `types.String`  | `.ValueString()`       |
| `schema.Int64Attribute`     | `types.Int64`   | `.ValueInt64()`        |
| `schema.SetAttribute`       | `types.Set`     | `.ElementsAs(ctx, &v)` |

A `types.String` that is not set in the configuration has `.IsNull() == true`. Calling `.ValueString()` on a null value returns `""`, not a panic, so the null check is a semantic concern, not a safety one. The null/unknown distinction matters: unknown means "known after apply" (set during plan, not yet created), null means "not configured". Write methods that populate state must set every attribute, including computed ones, to a concrete value so Terraform does not see `(known after apply)` in subsequent plans.

### Computed Attributes and Plan Modifiers

Attributes computed by the API (like `id`) must be declared `Computed: true`. If the value will not change once set, add the `stringplanmodifier.UseStateForUnknown()` plan modifier so Terraform does not show `=> (known after apply)` on every plan after creation. Without `UseStateForUnknown`, every subsequent plan will show the attribute as unknown even though the value in state is already known.

### Acceptance Tests vs. Unit Tests

Resources can be unit-tested by invoking their lifecycle methods directly, but the standard acceptance pattern is to configure the provider against a real (but in-process) HTTP server using `httptest.NewServer`. Acceptance tests are guarded by `os.Getenv("TF_ACC") != ""` (the framework's standard convention) so they do not run in normal `go test` invocations. Unit tests for the HTTP client in the `inventory` package run unconditionally.

## Exercises

Set up the module. The framework requires Go 1.21+; this lesson pins 1.26:

```bash
mkdir -p ~/go-exercises/terraform-provider-inventory/internal/inventory
mkdir -p ~/go-exercises/terraform-provider-inventory/internal/provider
cd ~/go-exercises/terraform-provider-inventory
go mod init github.com/example/terraform-provider-inventory
go get github.com/hashicorp/terraform-plugin-framework@latest
```

### Exercise 1: The Inventory HTTP Client (Pure Go, Offline-Testable)

The client is deliberately framework-free so it can be unit-tested without Terraform. Create `internal/inventory/client.go`:

```go
// internal/inventory/client.go
package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Item is the domain object managed by the Inventory API.
type Item struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Quantity    int64    `json:"quantity"`
	Tags        []string `json:"tags"`
}

// Sentinel errors for callers to match with errors.Is.
var (
	ErrNotFound         = errors.New("inventory: item not found")
	ErrNegativeQty      = errors.New("inventory: quantity must not be negative")
	ErrEmptyName        = errors.New("inventory: name must not be empty")
	ErrUnexpectedStatus = errors.New("inventory: unexpected HTTP status")
)

// Client calls the Inventory REST API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// New returns a Client pointed at baseURL. apiKey may be empty.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: &http.Client{},
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.httpClient.Do(req)
}

// CreateItem posts a new item to the API and returns the created item with its
// server-assigned ID.
func (c *Client) CreateItem(ctx context.Context, item Item) (Item, error) {
	if item.Name == "" {
		return Item{}, ErrEmptyName
	}
	if item.Quantity < 0 {
		return Item{}, ErrNegativeQty
	}

	body, err := json.Marshal(item)
	if err != nil {
		return Item{}, fmt.Errorf("inventory: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/items", bytes.NewReader(body))
	if err != nil {
		return Item{}, fmt.Errorf("inventory: new request: %w", err)
	}

	resp, err := c.do(req)
	if err != nil {
		return Item{}, fmt.Errorf("inventory: create: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return Item{}, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}
	var created Item
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return Item{}, fmt.Errorf("inventory: decode: %w", err)
	}
	return created, nil
}

// GetItem retrieves an item by ID.
func (c *Client) GetItem(ctx context.Context, id string) (Item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/items/"+id, nil)
	if err != nil {
		return Item{}, fmt.Errorf("inventory: new request: %w", err)
	}

	resp, err := c.do(req)
	if err != nil {
		return Item{}, fmt.Errorf("inventory: get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return Item{}, fmt.Errorf("%w: id=%s", ErrNotFound, id)
	}
	if resp.StatusCode != http.StatusOK {
		return Item{}, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}
	var item Item
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return Item{}, fmt.Errorf("inventory: decode: %w", err)
	}
	return item, nil
}

// UpdateItem replaces an existing item. Returns ErrNotFound if the ID does not exist.
func (c *Client) UpdateItem(ctx context.Context, item Item) (Item, error) {
	if item.Name == "" {
		return Item{}, ErrEmptyName
	}
	if item.Quantity < 0 {
		return Item{}, ErrNegativeQty
	}

	body, err := json.Marshal(item)
	if err != nil {
		return Item{}, fmt.Errorf("inventory: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.baseURL+"/items/"+item.ID, bytes.NewReader(body))
	if err != nil {
		return Item{}, fmt.Errorf("inventory: new request: %w", err)
	}

	resp, err := c.do(req)
	if err != nil {
		return Item{}, fmt.Errorf("inventory: update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return Item{}, fmt.Errorf("%w: id=%s", ErrNotFound, item.ID)
	}
	if resp.StatusCode != http.StatusOK {
		return Item{}, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}
	var updated Item
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		return Item{}, fmt.Errorf("inventory: decode: %w", err)
	}
	return updated, nil
}

// DeleteItem removes an item by ID. Returns ErrNotFound if the ID does not exist.
func (c *Client) DeleteItem(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.baseURL+"/items/"+id, nil)
	if err != nil {
		return fmt.Errorf("inventory: new request: %w", err)
	}

	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("inventory: delete: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: id=%s", ErrNotFound, id)
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}
	// Drain body so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// GetItemByName finds the first item whose name matches. Returns ErrNotFound if
// no item matches. Used by the data source.
func (c *Client) GetItemByName(ctx context.Context, name string) (Item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/items?name="+name, nil)
	if err != nil {
		return Item{}, fmt.Errorf("inventory: new request: %w", err)
	}

	resp, err := c.do(req)
	if err != nil {
		return Item{}, fmt.Errorf("inventory: get by name: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return Item{}, fmt.Errorf("%w: name=%s", ErrNotFound, name)
	}
	if resp.StatusCode != http.StatusOK {
		return Item{}, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}
	var item Item
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return Item{}, fmt.Errorf("inventory: decode: %w", err)
	}
	return item, nil
}
```

Create `internal/inventory/client_test.go`. This package has no framework dependency and gates cleanly offline:

```go
// internal/inventory/client_test.go
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// mockServer is a minimal in-memory Inventory API used by client tests.
type mockServer struct {
	mu    sync.Mutex
	items map[string]Item
	next  int
}

func newMockServer(t *testing.T) (*Client, *httptest.Server) {
	t.Helper()
	ms := &mockServer{items: make(map[string]Item)}
	srv := httptest.NewServer(ms.handler())
	t.Cleanup(srv.Close)
	return New(srv.URL, ""), srv
}

func (ms *mockServer) newID() string {
	ms.next++
	return fmt.Sprintf("%d", ms.next)
}

func (ms *mockServer) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /items", func(w http.ResponseWriter, r *http.Request) {
		var item Item
		if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ms.mu.Lock()
		item.ID = ms.newID()
		ms.items[item.ID] = item
		ms.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(item)
	})

	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ms.mu.Lock()
		item, ok := ms.items[id]
		ms.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(item)
	})

	mux.HandleFunc("PUT /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var updated Item
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ms.mu.Lock()
		if _, ok := ms.items[id]; !ok {
			ms.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		updated.ID = id
		ms.items[id] = updated
		ms.mu.Unlock()
		json.NewEncoder(w).Encode(updated)
	})

	mux.HandleFunc("DELETE /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ms.mu.Lock()
		_, ok := ms.items[id]
		if ok {
			delete(ms.items, id)
		}
		ms.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /items", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		ms.mu.Lock()
		defer ms.mu.Unlock()
		for _, item := range ms.items {
			if item.Name == name {
				json.NewEncoder(w).Encode(item)
				return
			}
		}
		http.NotFound(w, r)
	})

	return mux
}

func TestClientCreateAndGet(t *testing.T) {
	t.Parallel()
	client, _ := newMockServer(t)
	ctx := context.Background()

	created, err := client.CreateItem(ctx, Item{
		Name:        "widget",
		Description: "a small widget",
		Quantity:    10,
		Tags:        []string{"prod", "v1"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateItem: expected non-empty ID")
	}

	got, err := client.GetItem(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Name != "widget" || got.Quantity != 10 {
		t.Fatalf("GetItem = %+v, want Name=widget Quantity=10", got)
	}
}

func TestClientUpdate(t *testing.T) {
	t.Parallel()
	client, _ := newMockServer(t)
	ctx := context.Background()

	created, err := client.CreateItem(ctx, Item{Name: "gadget", Quantity: 5})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	created.Quantity = 99
	updated, err := client.UpdateItem(ctx, created)
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if updated.Quantity != 99 {
		t.Fatalf("Quantity = %d, want 99", updated.Quantity)
	}
}

func TestClientDelete(t *testing.T) {
	t.Parallel()
	client, _ := newMockServer(t)
	ctx := context.Background()

	created, err := client.CreateItem(ctx, Item{Name: "thing", Quantity: 1})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if err := client.DeleteItem(ctx, created.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	_, err = client.GetItem(ctx, created.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItem after delete: err = %v, want ErrNotFound", err)
	}
}

func TestClientGetNotFound(t *testing.T) {
	t.Parallel()
	client, _ := newMockServer(t)

	_, err := client.GetItem(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestClientValidation(t *testing.T) {
	t.Parallel()
	client, _ := newMockServer(t)
	ctx := context.Background()

	cases := []struct {
		name string
		item Item
		want error
	}{
		{"empty name", Item{Quantity: 1}, ErrEmptyName},
		{"negative qty", Item{Name: "x", Quantity: -1}, ErrNegativeQty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := client.CreateItem(ctx, tc.item)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestClientGetByName(t *testing.T) {
	t.Parallel()
	client, _ := newMockServer(t)
	ctx := context.Background()

	_, err := client.CreateItem(ctx, Item{Name: "sprocket", Quantity: 3})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	got, err := client.GetItemByName(ctx, "sprocket")
	if err != nil {
		t.Fatalf("GetItemByName: %v", err)
	}
	if got.Name != "sprocket" {
		t.Fatalf("Name = %q, want sprocket", got.Name)
	}

	_, err = client.GetItemByName(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
```

### Exercise 2: The Provider

Create `internal/provider/provider.go`. This file requires the external module:

```go
// internal/provider/provider.go
package provider

import (
	"context"
	"os"

	"github.com/example/terraform-provider-inventory/internal/inventory"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure InventoryProvider implements the provider.Provider interface.
var _ provider.Provider = &InventoryProvider{}

// InventoryProvider is the root provider struct. Version is injected at build
// time via ldflags in main.go.
type InventoryProvider struct {
	version string
}

// New returns a function that constructs the provider. The function signature
// is what providerserver.Serve expects.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &InventoryProvider{version: version}
	}
}

// providerModel maps to the HCL provider block the practitioner writes.
type providerModel struct {
	BaseURL types.String `tfsdk:"base_url"`
	APIKey  types.String `tfsdk:"api_key"`
}

// Metadata sets the provider type name shown in error messages and resource
// type prefixes.
func (p *InventoryProvider) Metadata(
	_ context.Context,
	_ provider.MetadataRequest,
	resp *provider.MetadataResponse,
) {
	resp.TypeName = "inventory"
	resp.Version = p.version
}

// Schema defines the provider-level configuration block.
func (p *InventoryProvider) Schema(
	_ context.Context,
	_ provider.SchemaRequest,
	resp *provider.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		Description: "Manages resources in the Inventory API.",
		Attributes: map[string]schema.Attribute{
			"base_url": schema.StringAttribute{
				Required:    true,
				Description: "Base URL of the Inventory API, e.g. https://api.example.com",
			},
			"api_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "API key. If omitted, the INVENTORY_API_KEY environment variable is used.",
			},
		},
	}
}

// Configure extracts provider configuration, builds the shared client, and
// stores it in resp.ResourceData and resp.DataSourceData for resources and
// data sources to retrieve in their own Configure methods.
func (p *InventoryProvider) Configure(
	ctx context.Context,
	req provider.ConfigureRequest,
	resp *provider.ConfigureResponse,
) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	baseURL := cfg.BaseURL.ValueString()
	apiKey := cfg.APIKey.ValueString()
	if apiKey == "" {
		apiKey = os.Getenv("INVENTORY_API_KEY")
	}

	client := inventory.New(baseURL, apiKey)
	resp.ResourceData = client
	resp.DataSourceData = client
}

// Resources returns the set of managed-resource factories this provider offers.
func (p *InventoryProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewItemResource,
	}
}

// DataSources returns the set of data-source factories this provider offers.
func (p *InventoryProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewItemDataSource,
	}
}
```

### Exercise 3: The Item Resource

Create `internal/provider/item_resource.go`:

```go
// internal/provider/item_resource.go
package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/example/terraform-provider-inventory/internal/inventory"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Compile-time interface checks.
var (
	_ resource.Resource                = &ItemResource{}
	_ resource.ResourceWithConfigure   = &ItemResource{}
	_ resource.ResourceWithImportState = &ItemResource{}
)

// ItemResource manages a single inventory_item in Terraform state.
type ItemResource struct {
	client *inventory.Client
}

// NewItemResource is the factory used in provider.Resources.
func NewItemResource() resource.Resource {
	return &ItemResource{}
}

// itemResourceModel is the Go struct that maps 1:1 to the Terraform state schema.
// Every field must be a framework type (types.String, types.Int64, types.Set, …),
// not a plain Go type, so the framework can handle null and unknown values.
type itemResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Quantity    types.Int64  `tfsdk:"quantity"`
	Tags        types.Set    `tfsdk:"tags"`
}

// Metadata returns the resource type name as it appears in Terraform
// configuration files (inventory_item).
func (r *ItemResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_item"
}

// Schema defines the Terraform schema for inventory_item. Every attribute
// must be declared here; the framework rejects any tfsdk tag in the model
// that has no matching schema attribute.
func (r *ItemResource) Schema(
	_ context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		Description: "Manages an item in the Inventory API.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Server-assigned unique identifier.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Human-readable name. Must not be empty.",
			},
			"description": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Free-text description of the item.",
			},
			"quantity": schema.Int64Attribute{
				Required:    true,
				Description: "Stock count. Must be >= 0.",
			},
			"tags": schema.SetAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Description: "Unordered set of string labels.",
			},
		},
	}
}

// Configure receives the shared *inventory.Client deposited by the provider's
// Configure method and stores it for use in CRUD methods.
func (r *ItemResource) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		// Called before the provider is configured (validation pass); safe to ignore.
		return
	}
	client, ok := req.ProviderData.(*inventory.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *inventory.Client, got %T", req.ProviderData),
		)
		return
	}
	r.client = client
}

// Create is called when the resource does not yet exist in state. It calls the
// API, receives the server-assigned ID, and saves the full item into state.
func (r *ItemResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var plan itemResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var tags []string
	resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	created, err := r.client.CreateItem(ctx, inventory.Item{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		Quantity:    plan.Quantity.ValueInt64(),
		Tags:        tags,
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create item", err.Error())
		return
	}

	r.itemToState(ctx, created, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// Read refreshes the Terraform state by fetching the current item from the
// API. If the item no longer exists, it removes the resource from state so
// Terraform knows to recreate it.
func (r *ItemResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var state itemResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	item, err := r.client.GetItem(ctx, state.ID.ValueString())
	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read item", err.Error())
		return
	}

	r.itemToState(ctx, item, &state, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// Update is called when an existing resource has a configuration diff. It
// sends the updated values to the API and saves the response into state.
func (r *ItemResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	var plan itemResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The ID comes from the prior state, not the plan, because it is Computed.
	var state itemResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var tags []string
	resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	updated, err := r.client.UpdateItem(ctx, inventory.Item{
		ID:          state.ID.ValueString(),
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		Quantity:    plan.Quantity.ValueInt64(),
		Tags:        tags,
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to update item", err.Error())
		return
	}

	r.itemToState(ctx, updated, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// Delete removes the resource from the API and from Terraform state.
func (r *ItemResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var state itemResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.DeleteItem(ctx, state.ID.ValueString()); err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Failed to delete item", err.Error())
	}
}

// ImportState enables `terraform import inventory_item.<name> <id>`. The
// framework then calls Read to populate the rest of state.
func (r *ItemResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// itemToState copies an inventory.Item into an itemResourceModel. Tags are
// converted from []string to types.Set so the framework can compare them
// correctly (set semantics: order does not matter).
func (r *ItemResource) itemToState(
	ctx context.Context,
	item inventory.Item,
	model *itemResourceModel,
	diags *diag.Diagnostics,
) {
	model.ID = types.StringValue(item.ID)
	model.Name = types.StringValue(item.Name)
	model.Description = types.StringValue(item.Description)
	model.Quantity = types.Int64Value(item.Quantity)

	tagsSet, d := types.SetValueFrom(ctx, types.StringType, item.Tags)
	diags.Append(d...)
	model.Tags = tagsSet
}

// isNotFound reports whether err wraps inventory.ErrNotFound.
func isNotFound(err error) bool {
	return errors.Is(err, inventory.ErrNotFound)
}
```

### Exercise 4: The Data Source

Create `internal/provider/item_datasource.go`:

```go
// internal/provider/item_datasource.go
package provider

import (
	"context"
	"fmt"

	"github.com/example/terraform-provider-inventory/internal/inventory"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &ItemDataSource{}
	_ datasource.DataSourceWithConfigure = &ItemDataSource{}
)

// ItemDataSource reads an existing inventory item by name.
type ItemDataSource struct {
	client *inventory.Client
}

func NewItemDataSource() datasource.DataSource {
	return &ItemDataSource{}
}

type itemDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Quantity    types.Int64  `tfsdk:"quantity"`
	Tags        types.Set    `tfsdk:"tags"`
}

func (d *ItemDataSource) Metadata(
	_ context.Context,
	req datasource.MetadataRequest,
	resp *datasource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_item"
}

func (d *ItemDataSource) Schema(
	_ context.Context,
	_ datasource.SchemaRequest,
	resp *datasource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		Description: "Reads an existing inventory item by name.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Server-assigned unique identifier.",
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Name to look up.",
			},
			"description": schema.StringAttribute{
				Computed:    true,
				Description: "Item description.",
			},
			"quantity": schema.Int64Attribute{
				Computed:    true,
				Description: "Current stock count.",
			},
			"tags": schema.SetAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Labels attached to this item.",
			},
		},
	}
}

func (d *ItemDataSource) Configure(
	_ context.Context,
	req datasource.ConfigureRequest,
	resp *datasource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*inventory.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *inventory.Client, got %T", req.ProviderData),
		)
		return
	}
	d.client = client
}

func (d *ItemDataSource) Read(
	ctx context.Context,
	req datasource.ReadRequest,
	resp *datasource.ReadResponse,
) {
	var cfg itemDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	item, err := d.client.GetItemByName(ctx, cfg.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read item", err.Error())
		return
	}

	cfg.ID = types.StringValue(item.ID)
	cfg.Description = types.StringValue(item.Description)
	cfg.Quantity = types.Int64Value(item.Quantity)
	tagsSet, diags := types.SetValueFrom(ctx, types.StringType, item.Tags)
	resp.Diagnostics.Append(diags...)
	cfg.Tags = tagsSet

	resp.Diagnostics.Append(resp.State.Set(ctx, cfg)...)
}
```

### Exercise 5: main.go and Acceptance Test

Create `main.go`. This is the binary entry point; `providerserver.Serve` connects it to Terraform's gRPC plugin protocol:

```go
// main.go
package main

import (
	"context"
	"flag"
	"log"

	"github.com/example/terraform-provider-inventory/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

// version is overridden at build time with:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with debug support")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/example/inventory",
		Debug:   debug,
	}
	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err)
	}
}
```

Create `internal/provider/provider_test.go`. Acceptance tests run only when `TF_ACC=1` is set:

```go
// internal/provider/provider_test.go
package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/example/terraform-provider-inventory/internal/inventory"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	inventoryprovider "github.com/example/terraform-provider-inventory/internal/provider"
)

// testAccProtoV6ProviderFactories is the provider factory map required by the
// terraform-plugin-testing framework to wire a test provider.
func testAccProtoV6ProviderFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"inventory": providerserver.NewProtocol6WithError(inventoryprovider.New("test")()),
	}
}

// newAccMockServer starts an httptest.Server that simulates the Inventory API.
// The returned URL is injected into provider configuration.
func newAccMockServer(t *testing.T) string {
	t.Helper()

	mu := sync.Mutex{}
	items := map[string]inventory.Item{}
	next := 0

	mux := http.NewServeMux()

	mux.HandleFunc("POST /items", func(w http.ResponseWriter, r *http.Request) {
		var item inventory.Item
		json.NewDecoder(r.Body).Decode(&item)
		mu.Lock()
		next++
		item.ID = fmt.Sprintf("%d", next)
		items[item.ID] = item
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(item)
	})
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		mu.Lock()
		item, ok := items[id]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(item)
	})
	mux.HandleFunc("PUT /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var updated inventory.Item
		json.NewDecoder(r.Body).Decode(&updated)
		mu.Lock()
		if _, ok := items[id]; !ok {
			mu.Unlock()
			http.NotFound(w, r)
			return
		}
		updated.ID = id
		items[id] = updated
		mu.Unlock()
		json.NewEncoder(w).Encode(updated)
	})
	mux.HandleFunc("DELETE /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		mu.Lock()
		_, ok := items[id]
		if ok {
			delete(items, id)
		}
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /items", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		mu.Lock()
		defer mu.Unlock()
		for _, item := range items {
			if item.Name == name {
				json.NewEncoder(w).Encode(item)
				return
			}
		}
		http.NotFound(w, r)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestAccItemResource_FullLifecycle runs the Terraform plan/apply/refresh/
// update/destroy lifecycle against the in-process mock server.
func TestAccItemResource_FullLifecycle(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("set TF_ACC=1 to run acceptance tests")
	}

	baseURL := newAccMockServer(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		CheckDestroy:             checkDestroy(baseURL),
		Steps: []resource.TestStep{
			// Create and read.
			{
				Config: testAccItemConfig(baseURL, "widget", "a small widget", 10),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("inventory_item.test", "id"),
					resource.TestCheckResourceAttr("inventory_item.test", "name", "widget"),
					resource.TestCheckResourceAttr("inventory_item.test", "quantity", "10"),
				),
			},
			// ImportState: terraform import inventory_item.test <id>
			{
				ResourceName:      "inventory_item.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			// Update quantity.
			{
				Config: testAccItemConfig(baseURL, "widget", "a small widget", 20),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("inventory_item.test", "quantity", "20"),
				),
			},
		},
	})
}

// testAccItemConfig returns HCL for a single inventory_item resource.
func testAccItemConfig(baseURL, name, description string, quantity int) string {
	return fmt.Sprintf(`
provider "inventory" {
  base_url = %q
}

resource "inventory_item" "test" {
  name        = %q
  description = %q
  quantity    = %d
  tags        = ["env:test"]
}
`, baseURL, name, description, quantity)
}

// TestAccItemDataSource checks the data source can read an item by name.
func TestAccItemDataSource(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("set TF_ACC=1 to run acceptance tests")
	}
	baseURL := newAccMockServer(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccDataSourceConfig(baseURL),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.inventory_item.lookup", "name", "sprocket"),
					resource.TestCheckResourceAttrSet(
						"data.inventory_item.lookup", "id"),
				),
			},
		},
	})
}

func testAccDataSourceConfig(baseURL string) string {
	return fmt.Sprintf(`
provider "inventory" {
  base_url = %q
}

resource "inventory_item" "seed" {
  name     = "sprocket"
  quantity = 3
}

data "inventory_item" "lookup" {
  name       = inventory_item.seed.name
  depends_on = [inventory_item.seed]
}
`, baseURL)
}

// checkDestroy verifies the item was actually removed from the API.
func checkDestroy(baseURL string) resource.TestCheckDestroyFunc {
	return func(s *terraform.State) error {
		client := inventory.New(baseURL, "")
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "inventory_item" {
				continue
			}
			_, err := client.GetItem(context.Background(), rs.Primary.ID)
			if err == nil {
				return fmt.Errorf("inventory_item %s still exists", rs.Primary.ID)
			}
		}
		return nil
	}
}
```

The import block above is complete as shown: `fmt` backs the `fmt.Sprintf` config builders, `context` and `terraform` back `checkDestroy`, which is wired into the lifecycle `TestCase` via `CheckDestroy` so the destroy leg is actually asserted.

Your turn: add a `TestClientDeleteNonExistent` test in `client_test.go` that calls `client.DeleteItem` with an ID that was never created and asserts `errors.Is(err, ErrNotFound)`.

## Common Mistakes

### Importing the Wrong Schema Package

Wrong: using `resource/schema` types in a provider-level schema or `datasource/schema` in a resource.

```go
// Wrong: resource/schema imported for a provider attribute
import "github.com/hashicorp/terraform-plugin-framework/resource/schema"

func (p *InventoryProvider) Schema(_ context.Context, _ provider.SchemaRequest,
	resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{ /* this is resource/schema.Schema, not provider/schema.Schema */ }
}
```

What happens: the compiler rejects the assignment because `provider.SchemaResponse.Schema` expects `provider/schema.Schema`.

Fix: each layer has its own schema package. Import `provider/schema` for providers, `resource/schema` for resources, `datasource/schema` for data sources.

### Forgetting `UseStateForUnknown` on Computed ID

Wrong: declaring `id` as `Computed: true` without a plan modifier.

What happens: every `terraform plan` after the first apply shows `id = (known after apply)` even though the ID is already in state and will not change.

Fix: add `stringplanmodifier.UseStateForUnknown()` to the `PlanModifiers` of the `id` attribute. This tells the framework to copy the prior state value into the plan when the attribute is already known.

### Using Plain Go Types in the Model Struct

Wrong:

```go
type itemResourceModel struct {
	ID       string `tfsdk:"id"`
	Quantity int64  `tfsdk:"quantity"`
}
```

What happens: the framework cannot represent null or unknown values with plain Go types. A null `string` and an empty string `""` are different in Terraform's type system; the framework panics or misrepresents the value.

Fix: use `types.String` and `types.Int64` (or the other `types.*` wrappers). Access the underlying value with `.ValueString()` / `.ValueInt64()` only after confirming the value is not null or unknown.

### Ignoring the nil Check in Configure

Wrong:

```go
func (r *ItemResource) Configure(_ context.Context,
	req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = req.ProviderData.(*inventory.Client) // panics if ProviderData is nil
}
```

What happens: Terraform calls `Configure` on resources during the validation pass before the provider is fully configured, at which point `req.ProviderData` is nil. The type assertion panics.

Fix: guard with `if req.ProviderData == nil { return }` before the type assertion.

### Not Calling `resp.State.RemoveResource` on 404

Wrong: returning an error in `Read` when the API returns 404.

What happens: Terraform reports an error and stops instead of marking the resource as needing recreation.

Fix: when `Read` receives a not-found error, call `resp.State.RemoveResource(ctx)` and return without adding a diagnostic error. Terraform will then plan a Create on the next apply.

## Verification

After `go get`:

```bash
cd ~/go-exercises/terraform-provider-inventory

# Format check (offline, no framework needed for the check itself).
test -z "$(gofmt -l .)"

# Vet and build (requires the downloaded module).
go vet ./...
go build ./...

# Unit tests for the inventory client (offline-testable, no TF_ACC needed).
go test -count=1 -race ./internal/inventory/...

# Acceptance tests (require TF_ACC=1 and the module).
TF_ACC=1 go test -count=1 -v ./internal/provider/...
```

The acceptance tests stand up an in-process `httptest.Server`, configure the provider against it, and exercise create/read/update/delete/import without any external service.

To install the provider locally for manual testing with a real Terraform workspace:

```bash
go install .
# Then configure ~/.terraformrc to use the local binary:
# provider_installation { dev_overrides { "registry.terraform.io/example/inventory" = "<GOPATH>/bin" } }
```

## Summary

- `terraform-plugin-framework` is the current HashiCorp-recommended library for writing providers; the older SDK v2 remains supported but should not be used for new providers.
- The provider's `Configure` method builds a shared client and deposits it in `resp.ResourceData` / `resp.DataSourceData`; each resource and data source retrieves it in its own `Configure` method.
- Schema types (`types.String`, `types.Int64`, `types.Set`) are nullable wrappers; use `.ValueString()` and `.ValueInt64()` to extract Go values, and `types.SetValueFrom` to convert `[]string` to `types.Set`.
- Computed attributes that will not change after creation need `UseStateForUnknown()` to avoid spurious `(known after apply)` diffs.
- `Read` must call `resp.State.RemoveResource(ctx)` on a 404 so Terraform recreates the resource rather than erroring.
- `ImportState` delegates to `resource.ImportStatePassthroughID` for the common case of importing by ID, after which the framework calls `Read` to populate state.
- Acceptance tests use `httptest.NewServer` to stand up the backing API in-process; they are gated by `os.Getenv("TF_ACC") != ""` so normal `go test` does not run them.

## What's Next

Next: [Container Health Checks](../09-container-health-checks/09-container-health-checks.md).

## Resources

- [terraform-plugin-framework: Providers](https://developer.hashicorp.com/terraform/plugin/framework/providers)
- [terraform-plugin-framework: Resources](https://developer.hashicorp.com/terraform/plugin/framework/resources)
- [terraform-plugin-framework: Resource Import](https://developer.hashicorp.com/terraform/plugin/framework/resources/import)
- [terraform-plugin-framework: Set attribute handling](https://developer.hashicorp.com/terraform/plugin/framework/handling-data/attributes/set)
- [pkg.go.dev: github.com/hashicorp/terraform-plugin-framework](https://pkg.go.dev/github.com/hashicorp/terraform-plugin-framework)
