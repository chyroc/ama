// Fill in chat names for the chat list.
//
// Inbound messages do not carry the chat name, so feishu_chats.name starts out empty, and the chat dropdown for scheduled tasks
// only showed a list of oc_xxx that users could not tell apart. Here, when **listing chats**, we call
// im/v1/chats/{chat_id} once for each chat without a name and write it back to the store; later listings read the store and do not call Feishu again.
//
// The inbound side also updates it along the way (see the part after prefetchContext in inbound.go),
// so after a chat is renamed, the next time someone @-mentions the bot in that chat, the name here follows.

package feishu

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

const (
	// chatNameFillTimeout is the total time limit for filling chat names within one chat listing.
	//
	// It sits on the HTTP request path, so **better to fill fewer names this time than to leave the dropdown spinning**: unfilled ones
	// still show the chat_id and get filled on the next listing.
	chatNameFillTimeout = 3 * time.Second
	// chatNameFillParallel is the number of concurrent requests to Feishu. A workspace with many chats has dozens at once; doing them serially would time out.
	chatNameFillParallel = 8
)

// fillChatNames fills in chat names for chats in chats that have none (modifies in place), and writes them back to the store.
//
// ⚠️ **Never returns an error; failures are only logged**: a missing name only means the UI shows the chat_id,
// and it must not stop the chat list from opening.
//
// Chats the bot has been removed from fail every time. Such chats cannot receive deliveries either, so a missing name reflects that honestly,
// and the total time is capped by chatNameFillTimeout, so it does not slow down the list.
func (s *Service) fillChatNames(ctx context.Context, workspaceID string, chats []ChatRecord) {
	missing := map[string][]int{} // Feishu app cli_xxx → indexes in chats
	for i, c := range chats {
		if strings.TrimSpace(c.Name) == "" {
			missing[c.LarkAppID] = append(missing[c.LarkAppID], i)
		}
	}
	if len(missing) == 0 {
		return
	}
	log := slog.Default().With("workspaceId", workspaceID)

	ctx, cancel := context.WithTimeout(ctx, chatNameFillTimeout)
	defer cancel()

	apps, err := s.store.ListApps(ctx, workspaceID)
	if err != nil {
		log.Warn("补群名：列应用失败，本次群列表只显示 chat_id", "error", err.Error())
		return
	}
	byLarkID := make(map[string]*App, len(apps))
	for _, app := range apps {
		byLarkID[app.AppID] = app
	}

	var wg sync.WaitGroup
	slots := make(chan struct{}, chatNameFillParallel)
	for larkAppID, idxs := range missing {
		app := byLarkID[larkAppID]
		if app == nil {
			// The app is disconnected but the chat record remains. Without credentials we cannot fetch the name.
			continue
		}
		client, token, err := s.tenantClient(ctx, app)
		if err != nil {
			log.Warn("补群名：取应用 token 失败，这个应用下的群只显示 chat_id",
				"larkAppId", larkAppID, "error", err.Error())
			continue
		}
		for _, i := range idxs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-slots }()

				chatID := chats[i].ChatID
				info, err := client.GetChat(ctx, token, chatID)
				if err != nil {
					// Log only the id and the error, not the chat name (same as logPrefetchMiss).
					log.Info("补群名：取群信息失败，这个群显示 chat_id",
						"chatId", chatID, "error", err.Error())
					return
				}
				name := strings.TrimSpace(info.Name)
				if name == "" {
					return
				}
				// Each goroutine only writes its own index, so no lock is needed.
				chats[i].Name = name
				if err := s.store.SetChatName(ctx, larkAppID, chatID, name); err != nil {
					log.Warn("补群名：写回库失败，下次列群会再取一次",
						"chatId", chatID, "error", err.Error())
				}
			}(i)
		}
	}
	wg.Wait()
}

// tenantClient builds a client for the app's account domain and exchanges a token for the app identity.
func (s *Service) tenantClient(ctx context.Context, app *App) (*lark.Client, string, error) {
	secret, err := s.store.AppSecret(ctx, app.AppID)
	if err != nil {
		return nil, "", err
	}
	client, err := s.larkClient(lark.NormalizeRegion(app.Region))
	if err != nil {
		return nil, "", err
	}
	token, err := client.TenantAccessToken(ctx, app.AppID, secret)
	if err != nil {
		return nil, "", err
	}
	return client, token, nil
}
