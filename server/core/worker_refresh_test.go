// Tests for the worker pool's pass that scans workspaces.
//
// Only one thing matters here: **does a failed store query leave a trace**. Starting a worker
// itself has to call the real Ark service, which is outside this test's scope; refresh does
// `continue` when it can't get the bindings, which is enough to verify.
package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
)

// bindingsFailStore makes ListArkBindings fail every time; everything else behaves like MemStore.
//
// Embed instead of reimplementing the whole Store: Store has over a hundred methods, and a
// hand-written fake would fail to compile every time a method is added, while this test only
// cares about one of them.
type bindingsFailStore struct {
	*coremem.MemStore

	mu     sync.Mutex
	called int
}

func (s *bindingsFailStore) ListArkBindings(
	_ context.Context, _ string,
) ([]persistmodel.ArkBinding, error) {
	s.mu.Lock()
	s.called++
	s.mu.Unlock()
	return nil, errors.New(
		`查询失败（sql=select ... from ark_bindings …）: ` +
			`ERROR: column "whatever" does not exist (SQLSTATE 42703)`)
}

// TestRefreshLogsBindingQueryFailure: when the store has a hiccup, a log must be left; the
// workspace must not be skipped silently.
//
// This is a gap in observability exposed by an earlier incident. The code here used to be
//
//	if err != nil || len(bindings) == 0 { continue }
//
// Two things that mean completely different things were merged into one: **"this workspace has no
// binding" is normal, "the store query failed" is a fault**, and once merged both took the same
// silent continue. The result: the worker couldn't start → every tool call in that workspace got
// stuck in requires_action, **and there was not a single log line the whole time**, so nothing
// pointed at the database during debugging.
func TestRefreshLogsBindingQueryFailure(t *testing.T) {
	mem, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("造 MemStore 失败：%v", err)
	}
	store := &bindingsFailStore{MemStore: mem}
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}

	var buf bytes.Buffer
	pool := &WorkerPool{
		svc: svc,
		log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	pool.refresh(context.Background())

	if store.called == 0 {
		t.Fatal("用例没跑到被测路径：ListArkBindings 一次都没被调用（种子里没有空间？）")
	}

	logged := buf.String()
	if !strings.Contains(logged, "列空间的方舟绑定失败") {
		t.Errorf("查绑定失败必须留日志，否则 worker 起不来时零线索；实际日志：%q", logged)
	}
	// A bare "it failed" is not enough: debugging needs to know which workspace, and what the database actually said.
	if !strings.Contains(logged, "42703") {
		t.Errorf("日志里要带上底层错误（这次是 42703），否则还得再查一轮；实际日志：%q", logged)
	}
	if !strings.Contains(logged, seedWS) {
		t.Errorf("日志里要带上空间 id，否则多空间时不知道是哪个；实际日志：%q", logged)
	}
}

// TestRefreshDoesNotLogWhenNoBindings: having no binding is **normal** and must not flood the log.
//
// Pairs with the previous test: when adding "log on failure", it is easy to also log
// len(bindings)==0 by accident, but that is a path every workspace without Ark hits on every
// cycle, and the real failure log would get buried.
func TestRefreshDoesNotLogWhenNoBindings(t *testing.T) {
	mem, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("造 MemStore 失败：%v", err)
	}
	svc, err := New(Config{Store: mem})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}

	var buf bytes.Buffer
	pool := &WorkerPool{
		svc: svc,
		log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	pool.refresh(context.Background())

	if logged := buf.String(); strings.Contains(logged, "列空间的方舟绑定失败") {
		t.Errorf("没有绑定不是故障，不该打这条日志；实际日志：%q", logged)
	}
}
