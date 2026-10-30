// End-to-end example: runs the full device-code flow and really registers a Feishu app.
//
//	go run ./shared/infra/lark/example
//
// No credentials needed. That is the whole point of the device-code flow. The example prints an
// authorization URL; you open it in a browser and click approve, then it polls until it gets
// the credentials and prints the app ID.
//
//	export LARK_REGION=feishu   # optional: feishu (default, China) or lark (international)
//
// This file is compilable usage documentation, and also the only real connectivity check for
// this package: all unit tests in the package are offline and never touch the network.
// **It really creates an app under your Feishu tenant.** Remember to delete it on the open
// platform when you are done.
//
// The AppSecret is printed to the terminal. Do not paste the output anywhere public.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	region := lark.NormalizeRegion(os.Getenv("LARK_REGION"))
	client, err := lark.New(lark.Config{Region: region})
	if err != nil {
		return err
	}

	// 1. Start: get the device code and authorization URL. This step needs no credentials.
	challenge, err := client.Begin(context.Background(), lark.BeginSpec{
		AppName: "AMA 示例应用",
		Source:  "ama-example",
		// Also request "receive group messages without @", which shows the addons encoding too.
		TenantScopes: []string{lark.ScopeGroupMessage},
		Callbacks:    lark.BaseCallbacks,
	})
	if err != nil {
		return err
	}

	fmt.Printf("账号域：%s\n", region)
	fmt.Printf("有效期：%d 秒，轮询间隔：%d 秒\n\n", challenge.ExpiresIn, challenge.Interval)
	fmt.Println("请在浏览器里打开下面的地址，点「同意」：")
	fmt.Printf("\n  %s\n\n", challenge.VerificationURL)
	fmt.Println("等你授权……（Ctrl-C 可中断）")

	// 2. Poll. Give ctx a generous timeout: the user has to act in a browser, which is not fast.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	interval := time.Duration(challenge.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		// Wait first, then ask: asking right after starting is always pending, a wasted call.
		select {
		case <-ctx.Done():
			return errors.New("等待授权超时")
		case <-time.After(interval):
		}

		ticket, err := client.Poll(ctx, challenge.DeviceCode)
		if err != nil {
			return err
		}
		switch ticket.Status {
		case lark.PollOK:
			if ticket.AppID == "" {
				// The protocol reports no error, but the credentials are not issued yet. In real
				// use this is also a sign that "the domain may be guessed wrong"; see the
				// two-domain correction in server/feishu.
				fmt.Println("  已授权，凭证下发中……")
				continue
			}
			fmt.Printf("\n注册成功\n")
			fmt.Printf("  AppID       %s\n", ticket.AppID)
			fmt.Printf("  AppSecret   %s\n", mask(ticket.AppSecret))
			fmt.Printf("  租户归属    %s\n", ticket.TenantBrand)
			return inspect(ctx, region, ticket)

		case lark.PollPending:
			fmt.Println("  还没授权……")

		case lark.PollSlowDown:
			interval += 5 * time.Second
			fmt.Printf("  轮询过快，间隔加到 %s\n", interval)

		default:
			// Expired / denied / invalid device code are all final states; no point waiting.
			return fmt.Errorf("授权失败（%s）：%s", ticket.Status, ticket.ErrorDescription)
		}
	}
}

// inspect checks the app once with the credentials just obtained, to show "what is still
// missing after install".
func inspect(ctx context.Context, region lark.Region, ticket *lark.Ticket) error {
	// The tenant's real domain may differ from our first guess; rebuild the client for it.
	if brand := lark.NormalizeRegion(ticket.TenantBrand); brand != region {
		fmt.Printf("\n租户其实在 %s 域，换过去巡检\n", brand)
		region = brand
	}
	client, err := lark.New(lark.Config{Region: region})
	if err != nil {
		return err
	}

	token, err := client.TenantAccessToken(ctx, ticket.AppID, ticket.AppSecret)
	if err != nil {
		return err
	}
	cfg, err := client.InspectApp(ctx, token)
	if err != nil {
		return err
	}

	fmt.Printf("\n巡检结果\n")
	fmt.Printf("  能读到应用详情  %v\n", cfg.Inspectable)
	fmt.Printf("  机器人已启用    %v\n", cfg.BotEnabled)
	fmt.Printf("  版本已发布      %v\n", cfg.Published)
	fmt.Printf("  已授权权限      %d 个\n", len(cfg.GrantedScopes))
	fmt.Printf("  等管理员批的    %v\n", cfg.UnapprovedScopes)
	fmt.Printf("  还缺的权限      %v\n", lark.MissingValues(lark.BaseScopes, cfg.GrantedScopes))
	if cfg.Inspectable {
		fmt.Printf("  还缺的事件      %v\n", lark.MissingValues(lark.BaseEvents, cfg.SubscribedEvents))
	}
	fmt.Println("\n记得去开放平台把这个示例应用删掉。")
	return nil
}

// mask hides most of the secret, so the full key is not left in the terminal history.
func mask(secret string) string {
	if len(secret) <= 8 {
		return strings.Repeat("*", len(secret))
	}
	return secret[:4] + strings.Repeat("*", 8) + secret[len(secret)-4:]
}
