package core

import (
	"context"
	"errors"
	"testing"

	"github.com/volcengine/ark-runtime-go/arkruntime/tools/agenttoolset"
	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"

	"github.com/chyroc/ama/server/model/apimodel"
)

type stubRouterStore struct {
	run apimodel.Run
	err error
}

func (s stubRouterStore) GetRun(context.Context, string) (apimodel.Run, error) {
	return s.run, s.err
}

// stubToolProvider records the name under which its factory was picked.
type stubToolProvider struct{ name string }

func (p *stubToolProvider) ToolsFuncForSession(_ context.Context, _ string,
) func(*agenttoolset.AgentToolContext) (*toolset.Set, error) {
	return func(*agenttoolset.AgentToolContext) (*toolset.Set, error) {
		chosen <- p.name
		return nil, nil
	}
}

var chosen = make(chan string, 1)

// An empty run.host_id (the default for existing rows) must go to the Docker container; a non-empty
// one goes to the host; if looking up the run fails, fall back to the container
// (a failure in the routing piece must not stop all conversations).
func TestToolRouterDispatch(t *testing.T) {
	ctr := &stubToolProvider{name: "container"}
	hostP := &stubToolProvider{name: "host"}
	router := newToolRouter(stubRouterStore{}, ctr, hostP, nil)

	cases := []struct {
		name string
		host sessionToolProvider
		run  apimodel.Run
		err  error
		want string
	}{
		{"默认容器（无 host_id）", nil, apimodel.Run{}, nil, "container"},
		{"host run 走宿主机", hostP, apimodel.Run{HostID: "hst_1"}, nil, "host"},
		{"查 run 失败降级容器", hostP, apimodel.Run{}, errors.New("db down"), "container"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router.host = tc.host
			router.store = stubRouterStore{run: tc.run, err: tc.err}
			fn := router.ToolsFuncForSession(context.Background(), "sesn_x")
			if fn == nil {
				t.Fatal("工厂为 nil")
			}
			_, _ = fn(nil)
			if got := <-chosen; got != tc.want {
				t.Fatalf("分流到 %q，期望 %q", got, tc.want)
			}
		})
	}
}
