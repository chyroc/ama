// Memory entries: single memories inside a memory store, organized by path (like a file tree).
// API: /api/v3/memory_stores/{memory_store_id}/memories

package ma

import (
	"context"
	"errors"

	"github.com/volcengine/ark-runtime-go/arkruntime/model/memory"
)

// CreateMemory writes one memory. path looks like a file path (e.g. "user/preferences"),
// and content is the memory body.
func (s *MemoryStoreService) CreateMemory(
	ctx context.Context,
	memoryStoreID, path, content string,
) (*memory.Memory, error) {
	if path == "" {
		return nil, errors.New("ma: path 必填")
	}
	return s.CreateMemoryRaw(ctx, memoryStoreID, &memory.CreateMemoryRequest{
		Path:    path,
		Content: content,
	})
}

// CreateMemoryRaw writes a memory using the SDK's own request body.
func (s *MemoryStoreService) CreateMemoryRaw(
	ctx context.Context,
	memoryStoreID string,
	req *memory.CreateMemoryRequest,
) (*memory.Memory, error) {
	return s.client.CreateMemory(ctx, memoryStoreID, req)
}

// GetMemory returns one memory.
func (s *MemoryStoreService) GetMemory(
	ctx context.Context,
	memoryStoreID, memoryID string,
) (*memory.Memory, error) {
	return s.client.GetMemory(ctx, memoryStoreID, memoryID)
}

// MemoryQuery holds the filters for listing memory entries.
type MemoryQuery struct {
	// Limit is the max number of items per page; 0 uses the server default.
	Limit int32
	// Page is the page cursor; leave empty for the first page.
	Page string
	// PathPrefix lists only memories under this path prefix, e.g. "user/".
	PathPrefix string
	// Depth is the max depth relative to PathPrefix; 0 means no limit.
	Depth int32
}

// ListMemories lists memory entries page by page, optionally filtered by path prefix and depth.
func (s *MemoryStoreService) ListMemories(
	ctx context.Context,
	memoryStoreID string,
	q MemoryQuery,
) (*memory.ListMemoriesResponse, error) {
	return s.ListMemoriesRaw(ctx, memoryStoreID, &memory.MemoriesListParams{
		MemoryStoreId: memoryStoreID,
		Limit:         optMemoryInt32(q.Limit),
		Page:          optMemoryString(q.Page),
		PathPrefix:    optMemoryString(q.PathPrefix),
		Depth:         optMemoryInt32(q.Depth),
	})
}

// ListMemoriesRaw lists memory entries using the SDK's own query parameters.
func (s *MemoryStoreService) ListMemoriesRaw(
	ctx context.Context,
	memoryStoreID string,
	params *memory.MemoriesListParams,
) (*memory.ListMemoriesResponse, error) {
	return s.client.ListMemories(ctx, memoryStoreID, params)
}

// UpdateMemory updates a memory's content or path. Empty fields keep their current value.
func (s *MemoryStoreService) UpdateMemory(
	ctx context.Context,
	memoryStoreID, memoryID, content, path string,
) (*memory.Memory, error) {
	return s.UpdateMemoryRaw(ctx, memoryStoreID, memoryID, &memory.UpdateMemoryRequest{
		Content: optMemoryString(content),
		Path:    optMemoryString(path),
	})
}

// UpdateMemoryRaw updates a memory using the SDK's own request body.
func (s *MemoryStoreService) UpdateMemoryRaw(
	ctx context.Context,
	memoryStoreID, memoryID string,
	req *memory.UpdateMemoryRequest,
) (*memory.Memory, error) {
	return s.client.UpdateMemory(ctx, memoryStoreID, memoryID, req)
}

// DeleteMemory deletes one memory.
func (s *MemoryStoreService) DeleteMemory(
	ctx context.Context,
	memoryStoreID, memoryID string,
) (*memory.DeleteMemoryResponse, error) {
	return s.client.DeleteMemory(ctx, memoryStoreID, memoryID)
}
