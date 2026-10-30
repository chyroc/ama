package channeldao

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/infra/cryptobox"
	"github.com/chyroc/ama/server/weixin"
)

func eachWeixinStore(t *testing.T, run func(t *testing.T, s weixin.Store)) {
	t.Run("mem", func(t *testing.T) { run(t, weixin.NewMemStore()) })
	t.Run("pg", func(t *testing.T) {
		key := make([]byte, 32)
		_, _ = rand.Read(key)
		box, err := cryptobox.New(base64.StdEncoding.EncodeToString(key))
		if err != nil {
			t.Fatal(err)
		}
		s, err := NewWeixinPGStore(context.Background(), newTestClient(t), box)
		if err != nil {
			t.Fatal(err)
		}
		run(t, s)
	})
}

func TestWeixinAccounts(t *testing.T) {
	eachWeixinStore(t, func(t *testing.T, s weixin.Store) {
		ctx := context.Background()
		rec := weixin.AccountRecord{
			ID: "wxa_1", WorkspaceID: "ws1", BotID: "bot1", BotToken: "tok-1", BaseURL: "https://api.example.com",
			OwnerUserID: "owner", CreatedBy: "a@example.com", CreatedAt: "2026-10-01T00:00:00.000Z",
			UpdatedAt: "2026-10-01T00:00:00.000Z",
		}
		got, err := s.UpsertAccount(ctx, rec)
		if err != nil || got.BotToken != "tok-1" || got.Status != apimodel.WeixinAccountActive {
			t.Fatalf("insert: %+v %v", got, err)
		}
		_ = s.SetAccountAgent(ctx, "wxa_1", "agent_a", "2026-10-01T00:01:00.000Z")
		_ = s.SetAccountCursor(ctx, "wxa_1", "cur")
		_ = s.SetAccountStatus(ctx, "wxa_1", apimodel.WeixinAccountExpired, "dead", "2026-10-01T00:02:00.000Z")
		if list, _ := s.ListActiveAccounts(ctx); len(list) != 0 {
			t.Fatal("expired accounts are not active")
		}

		// Scanning again in the same workspace: same row, new token, active again; binding and cursor kept.
		again := rec
		again.ID, again.BotToken, again.UpdatedAt = "wxa_2", "tok-2", "2026-10-02T00:00:00.000Z"
		got, err = s.UpsertAccount(ctx, again)
		if err != nil || got.ID != "wxa_1" || got.BotToken != "tok-2" || got.Status != apimodel.WeixinAccountActive ||
			got.AgentID != "agent_a" || got.SyncCursor != "cur" || got.LastError != "" {
			t.Fatalf("re-login: %+v %v", got, err)
		}
		// Another workspace: refused.
		other := rec
		other.ID, other.WorkspaceID = "wxa_3", "ws2"
		if _, err := s.UpsertAccount(ctx, other); !errors.Is(err, weixin.ErrBotInOtherSpace) {
			t.Fatalf("other workspace: %v", err)
		}

		second := rec
		second.ID, second.BotID, second.BotToken, second.CreatedAt = "wxa_4", "bot2", "tok-3", "2026-10-03T00:00:00.000Z"
		_, _ = s.UpsertAccount(ctx, second)
		list, _ := s.ListAccounts(ctx, "ws1")
		if len(list) != 2 || list[0].ID != "wxa_1" || list[1].ID != "wxa_4" {
			t.Fatalf("list order: %+v", list)
		}
		tokens, _ := s.RecentBotTokens(ctx, 10)
		if len(tokens) != 2 || tokens[0] != "tok-3" {
			t.Fatalf("tokens newest first: %v", tokens)
		}
		_ = s.TouchAccountInbound(ctx, "wxa_4", "2026-10-04T00:00:00.000Z")
		if a, _ := s.GetAccount(ctx, "wxa_4"); a.LastInboundAt != "2026-10-04T00:00:00.000Z" {
			t.Fatalf("touch: %+v", a)
		}
		if err := s.DeleteAccount(ctx, "wxa_4"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetAccount(ctx, "wxa_4"); !errors.Is(err, weixin.ErrAccountNotFound) {
			t.Fatalf("deleted: %v", err)
		}
		if err := s.SetAccountCursor(ctx, "nope", "x"); !errors.Is(err, weixin.ErrAccountNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})
}

func TestWeixinTokenIsEncryptedInPG(t *testing.T) {
	client := newTestClient(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, _ := cryptobox.New(base64.StdEncoding.EncodeToString(key))
	s, err := NewWeixinPGStore(context.Background(), client, box)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpsertAccount(context.Background(), weixin.AccountRecord{
		ID: "wxa_1", WorkspaceID: "ws", BotID: "b", BotToken: "secret-token", BaseURL: "u",
		CreatedAt: "2026-10-01T00:00:00.000Z", UpdatedAt: "2026-10-01T00:00:00.000Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := client.QueryRow(context.Background(), `select bot_token from weixin_accounts`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "secret-token" {
		t.Fatal("the token must not be stored as plain text when a key is configured")
	}
}

func TestWeixinLogins(t *testing.T) {
	eachWeixinStore(t, func(t *testing.T, s weixin.Store) {
		ctx := context.Background()
		now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
		rec := weixin.LoginRecord{
			ID: "wxl_1", WorkspaceID: "ws1", CreatedBy: "a@example.com", QRCode: "qr", QRContent: "https://example.com/q",
			PollBaseURL: "https://login.example.com", Status: apimodel.WeixinLoginWaiting, RefreshCount: 1,
			CreatedAt: now, ExpiresAt: now.Add(8 * time.Minute),
		}
		if err := s.CreateLogin(ctx, rec); err != nil {
			t.Fatal(err)
		}
		rec.Status, rec.PendingVerifyCode, rec.RefreshCount, rec.AccountID, rec.Message =
			apimodel.WeixinLoginConfirmed, "12", 2, "wxa_1", "ok"
		if err := s.UpdateLogin(ctx, rec); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetLogin(ctx, "wxl_1")
		if err != nil || got.Status != apimodel.WeixinLoginConfirmed || got.PendingVerifyCode != "12" ||
			got.RefreshCount != 2 || got.AccountID != "wxa_1" || !got.ExpiresAt.Equal(rec.ExpiresAt) {
			t.Fatalf("got %+v %v", got, err)
		}
		_ = s.DeleteExpiredLogins(ctx, now.Add(time.Minute))
		if _, err := s.GetLogin(ctx, "wxl_1"); err != nil {
			t.Fatal("not expired yet")
		}
		_ = s.DeleteExpiredLogins(ctx, now.Add(9*time.Minute))
		if _, err := s.GetLogin(ctx, "wxl_1"); !errors.Is(err, weixin.ErrLoginNotFound) {
			t.Fatalf("expired: %v", err)
		}
		if err := s.UpdateLogin(ctx, rec); !errors.Is(err, weixin.ErrLoginNotFound) {
			t.Fatalf("update missing: %v", err)
		}
	})
}
