package core

import (
	"context"
	"fmt"

	"github.com/chyroc/ama/server/shared/infra/ma"
)

// ArkProxyDo resolves the owning resource's credentials before forwarding a raw
// non-streaming Ark request. Request and response bodies are not remapped.
func ArkProxyDo(
	ctx context.Context, req ma.RawRequest, resolve func() (ArkBindingKey, error),
	do func(context.Context, ArkBindingKey, ma.RawRequest) (*ma.RawResponse, error),
	precondition error,
) (*ma.RawResponse, error) {
	if do == nil {
		return nil, fmt.Errorf("%w: 服务端未接入方舟", precondition)
	}
	key, err := resolve()
	if err != nil {
		return nil, err
	}
	return do(ctx, key, req)
}

// ArkProxyStream resolves credentials before opening a raw Ark SSE stream.
func ArkProxyStream(
	ctx context.Context, req ma.RawRequest, resolve func() (ArkBindingKey, error),
	stream func(context.Context, ArkBindingKey, ma.RawRequest) (*ma.RawStream, error),
	precondition error,
) (*ma.RawStream, error) {
	if stream == nil {
		return nil, fmt.Errorf("%w: 服务端未接入方舟", precondition)
	}
	key, err := resolve()
	if err != nil {
		return nil, err
	}
	return stream(ctx, key, req)
}
