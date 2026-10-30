// MemoryStore: a persistent memory store shared across Sessions.
// API: /api/v3/memory_stores
//
// For the memory entries inside a store see memories.go.

package ma

import (
	"context"
	"errors"

	"github.com/volcengine/ark-runtime-go/arkruntime"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/memory"
)

// MemoryStoreService manages memory stores and their entries.
// Get it via Instance.MemoryStores().
type MemoryStoreService struct {
	client *arkruntime.Client
}

// Create creates a memory store.
func (s *MemoryStoreService) Create(
	ctx context.Context,
	name, description string,
) (*memory.MemoryStore, error) {
	if name == "" {
		return nil, errors.New("ma: name 必填")
	}
	return s.CreateRaw(ctx, &memory.CreateMemoryStoreRequest{
		Name:        name,
		Description: optMemoryString(description),
	})
}

// CreateRaw creates a memory store using the SDK's own request body (metadata allowed).
func (s *MemoryStoreService) CreateRaw(
	ctx context.Context,
	req *memory.CreateMemoryStoreRequest,
) (*memory.MemoryStore, error) {
	return s.client.CreateMemoryStore(ctx, req)
}

// Get returns memory store details by ID, including the entry count and bytes used.
func (s *MemoryStoreService) Get(
	ctx context.Context,
	memoryStoreID string,
) (*memory.MemoryStore, error) {
	return s.client.GetMemoryStore(ctx, memoryStoreID)
}

// List lists memory stores, page by page.
func (s *MemoryStoreService) List(
	ctx context.Context,
	page Page,
) (*memory.ListMemoryStoresResponse, error) {
	return s.ListRaw(ctx, &memory.MemoryStoresListParams{
		Limit: optMemoryInt32(page.Limit),
		Page:  optMemoryString(page.Page),
	})
}

// ListRaw paginates using the SDK's own query parameters (supports filtering by name and creator).
func (s *MemoryStoreService) ListRaw(
	ctx context.Context,
	params *memory.MemoryStoresListParams,
) (*memory.ListMemoryStoresResponse, error) {
	return s.client.ListMemoryStores(ctx, params)
}

// Update updates the store's name and description. Empty fields keep their current value.
func (s *MemoryStoreService) Update(
	ctx context.Context,
	memoryStoreID, name, description string,
) (*memory.MemoryStore, error) {
	return s.UpdateRaw(ctx, memoryStoreID, &memory.UpdateMemoryStoreRequest{
		Name:        optMemoryString(name),
		Description: optMemoryString(description),
	})
}

// UpdateRaw updates a memory store using the SDK's own request body.
func (s *MemoryStoreService) UpdateRaw(
	ctx context.Context,
	memoryStoreID string,
	req *memory.UpdateMemoryStoreRequest,
) (*memory.MemoryStore, error) {
	return s.client.UpdateMemoryStore(ctx, memoryStoreID, req)
}

// Delete deletes a memory store together with all entries in it.
func (s *MemoryStoreService) Delete(
	ctx context.Context,
	memoryStoreID string,
) (*memory.DeleteMemoryStoreResponse, error) {
	return s.client.DeleteMemoryStore(ctx, memoryStoreID)
}
