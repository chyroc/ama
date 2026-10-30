// selfhosted-bootstrap creates an Ark Environment of type self_hosted. It is the first step in
// connecting AMA's worker (tools run in Docker containers) as a self-hosted runtime environment.
//
//	export ARK_API_KEY=...                       # long-lived API key from the Ark console
//	go run ./cmd/selfhosted-bootstrap
//
//	# optional
//	export ARK_ENV_NAME=ama-self-hosted          # environment name, default below
//	export ARK_ENV_ID=env_xxx                    # if set, only check it, do not create
//
// # Why this tool exists
//
// In the self_hosted setup, the Environment is the anchor the worker pulls work from: all 8
// self-hosted Worker endpoints live under /environments/{env_id}/work, so nothing works without
// an Environment. Creating one has an OpenAPI (`shared/infra/ma` already supports the
// EnvSelfHosted enum); this tool wraps it so nobody has to hand-build curl commands.
//
// Auth is a single long-lived Ark API key, used by both the worker-side and control-side
// endpoints; there is no separate per-environment credential.
//
// The environment itself uses no resources (it is just a piece of config), so this tool **does
// not delete** it: once created, it is meant to be used from then on.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/chyroc/ama/server/shared/infra/ma"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/environment"
)

const defaultEnvName = "ama-self-hosted"

func main() {
	apiKey := os.Getenv("ARK_API_KEY")
	if apiKey == "" {
		log.Fatal("请先设置 ARK_API_KEY（方舟控制台 → API Key 管理，长效 key）")
	}

	ins, err := ma.New(apiKey)
	if err != nil {
		log.Fatalf("初始化方舟客户端失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	envs := ins.Environments()

	// If an environment already exists, only check it, do not create again: environments are
	// long-lived, and creating them repeatedly only leaves a pile of same-named junk.
	if id := strings.TrimSpace(os.Getenv("ARK_ENV_ID")); id != "" {
		env, err := envs.Get(ctx, id)
		if err != nil {
			log.Fatalf("查询环境 %s 失败: %v", id, err)
		}
		report(env)
		return
	}

	name := os.Getenv("ARK_ENV_NAME")
	if name == "" {
		name = defaultEnvName
	}

	env, err := envs.Create(ctx, ma.EnvironmentSpec{
		Name: name,
		Type: ma.EnvSelfHosted,
		// No Networking: self_hosted does not accept this field (sending it gives 400);
		// outbound network is handled by the executor (the network the session container is on).
		Description: "AMA：工具在自建执行端（Docker 容器 / 宿主机）里执行的 Worker 环境",
	})
	if err != nil {
		if ma.IsAuthError(err) {
			log.Fatalf("鉴权失败，检查 ARK_API_KEY 是否有效: %v", err)
		}
		log.Fatalf("创建环境失败（request_id=%s）: %v", ma.RequestID(err), err)
	}

	fmt.Printf("已创建 self_hosted 环境：%s\n\n", env.ID)
	report(env)
}

// report prints the environment info and "what to do manually next".
func report(env *environment.Environment) {
	fmt.Printf("  id          %s\n", env.ID)
	fmt.Printf("  name        %s\n", env.Name)

	envType := "(未返回)"
	if cfg, ok := env.Config.Get(); ok {
		envType = string(cfg.Type)
	}
	fmt.Printf("  config.type %s\n", envType)

	if envType != ma.EnvSelfHosted {
		fmt.Printf("\n⚠ 这个环境不是 self_hosted（是 %s），自建 Worker 那套接口对它不适用。\n", envType)
		return
	}

	fmt.Print(`
接下来把 amad 跑起来，它里面常驻的 worker 会 poll 这个环境、把工具调用下发到
会话容器（Docker）里执行：

   export ARK_ENVIRONMENT_ID=` + env.ID + `
   export ARK_API_KEY=<同一把，就是你刚用的这个>
   ./dev.sh                     # 或 docker compose up -d --build

注意：要先有 Session 派到这个环境上才会有 work 可拉——创建 Session 时把
environment_id 指到上面这个 id（shared/infra/ma 的 Sessions().Create）。
否则 worker 会一直空转，看起来像「连不上」其实是队列里没活。
`)
}
