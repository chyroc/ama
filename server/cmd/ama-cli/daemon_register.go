// `ama daemon register`: registers this machine with the user's AMA_TOKEN, getting a host_token.
//
// Done in one go: probe the local environment → read/generate host_uuid → POST register → save
// ~/.ama/host.json. Idempotent: the same user registering the same uuid again hits the same machine
// (the token is only issued the first time; when a token is already stored locally and the server
// returns none, that also counts as success, and the local one keeps being used).
//
// Day-to-day setup does not need to type it separately: `ama daemon install` adds this step
// automatically when it finds the machine is not registered, so the user only needs one command.
// register stays on its own for the "register only, no system service" case (for example only
// wanting to run `ama daemon run` in the foreground).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/chyroc/ama/server/shared/hostproto"
)

// daemonRegister runs registration (the register subcommand entry). userToken is read from the
// AMA_TOKEN environment variable (**never from command line arguments**: argv lands in the shell
// history and the process list).
func daemonRegister() error {
	if err := registerHost(); err != nil {
		return err
	}
	fmt.Println("下一步：ama daemon install（开机自启并立即运行），或 ama daemon run（前台调试）")
	return nil
}

// registerHost is the actual registration flow, also reused by install on its first run.
func registerHost() error {
	userToken := strings.TrimSpace(os.Getenv(envToken))
	if userToken == "" {
		return fmt.Errorf("注册需要环境变量 %s（在 AMA 网页的全局设置里创建一把 API Key）", envToken)
	}
	base := normalizeBaseURL(firstNonEmpty(os.Getenv(envBaseURL), defaultBaseURL))

	cfg, err := loadDaemonConfig()
	if err != nil && !isNotExist(err) {
		return err
	}
	if isNotExist(err) {
		cfg = daemonConfig{BaseURL: base}
	}
	// host_uuid is generated on the client's first registration and persisted right away: register
	// may run repeatedly (install failed, network changed, re-registration after upgrade), and
	// generating a new uuid each time would register the same physical machine as several hosts. The
	// uuid is the identity of "this user on this machine", independent of the server's response, so
	// persist first, then send the request.
	if cfg.HostUUID == "" {
		u, err := newHostUUID()
		if err != nil {
			return fmt.Errorf("生成本机标识失败: %w", err)
		}
		cfg.HostUUID = u
		cfg.BaseURL = base
		if err := cfg.save(); err != nil {
			return fmt.Errorf("保存本机配置失败: %w", err)
		}
	}
	cfg.BaseURL = base

	req := hostproto.RegisterRequest{
		HostUUID:      cfg.HostUUID,
		Name:          hostname(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		DaemonVersion: version,
		// The new daemon runs the six tools natively in Go and no longer depends on brew GNU tools.
		NativeTools: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), daemonHTTPTimeout)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := newRawRequest(ctx, "POST", base+"/api/host-daemon/register", body, userToken)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("连不上服务端 %s: %w（公司网络记得走代理；检查 %s）", base, err, envBaseURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return describeRegisterError(resp.StatusCode)
	}
	var reg hostproto.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return fmt.Errorf("解析注册响应失败: %w", err)
	}
	cfg.HostID = reg.HostID
	if reg.Token != "" {
		cfg.Token = reg.Token
	}
	if cfg.Token == "" {
		return fmt.Errorf("服务端没有下发 host_token，且本地也没有存量 token；请在全局设置里吊销这台机器后重新注册")
	}
	if err := cfg.save(); err != nil {
		return err
	}

	fmt.Printf("已注册宿主机 %s（%s）\n", reg.HostID, hostname())
	return nil
}

func describeRegisterError(status int) error {
	switch status {
	case 401:
		return fmt.Errorf("AMA_TOKEN 无效或已过期（401），请在全局设置重新创建 API Key")
	case 403:
		return fmt.Errorf("当前身份无权注册宿主机（403）")
	default:
		return fmt.Errorf("注册失败，服务端返回 %d", status)
	}
}
