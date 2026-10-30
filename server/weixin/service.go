package weixin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/chyroc/ama/server/model/apimodel"
	wx "github.com/chyroc/ama/server/shared/infra/weixin"
)

const (
	// defaultLoginTTL is how long a login can stay open. The reference client gives up after eight minutes.
	defaultLoginTTL = 8 * time.Minute
	// defaultPollWait is how long one poll request waits on get_qrcode_status. The server holds that request
	// up to ~35 seconds; we stop earlier so a browser request never hits a gateway timeout, and the page
	// simply polls again.
	defaultPollWait = 20 * time.Second
	// maxQRCodes is how many QR codes one login may issue (the first one included), like the reference.
	maxQRCodes = 3
	// recentTokens is how many known bot tokens go with a QR-code request, like the reference.
	recentTokens = 10
)

// Config wires a Service.
type Config struct {
	Store  Store
	Client *wx.Client
	Agents Agents
	Logger *slog.Logger
	// Now defaults to time.Now; tests inject a clock.
	Now      func() time.Time
	LoginTTL time.Duration
	PollWait time.Duration
}

// Service is the HTTP-facing side of the WeChat channel: logins and connected accounts.
type Service struct {
	cfg Config
	log *slog.Logger
	now func() time.Time
}

// NewService creates a Service.
func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Client == nil || cfg.Agents == nil {
		return nil, fmt.Errorf("%w: Store / Client / Agents 不能为空", ErrInvalidArgument)
	}
	if cfg.LoginTTL <= 0 {
		cfg.LoginTTL = defaultLoginTTL
	}
	if cfg.PollWait <= 0 {
		cfg.PollWait = defaultPollWait
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{cfg: cfg, log: log, now: now}, nil
}

// ---- Logins ----

// StartLogin asks WeChat for a login QR code.
func (s *Service) StartLogin(ctx context.Context, workspaceID, actor string) (apimodel.WeixinLogin, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(actor) == "" {
		return apimodel.WeixinLogin{}, ErrInvalidArgument
	}
	now := s.now()
	if err := s.cfg.Store.DeleteExpiredLogins(ctx, now); err != nil {
		s.log.Warn("清理过期的微信登录失败", "error", err.Error())
	}
	tokens, err := s.cfg.Store.RecentBotTokens(ctx, recentTokens)
	if err != nil {
		s.log.Warn("取已接入的微信 token 失败，按没有处理", "error", err.Error())
		tokens = nil
	}
	qr, err := s.cfg.Client.GetBotQRCode(ctx, "", tokens)
	if err != nil {
		return apimodel.WeixinLogin{}, fmt.Errorf("%w: 向微信要登录二维码失败: %v", ErrUpstream, err)
	}
	if qr.Code == "" || qr.Content == "" {
		return apimodel.WeixinLogin{}, fmt.Errorf("%w: 微信返回的登录二维码是空的", ErrUpstream)
	}
	rec := LoginRecord{
		ID:           newID("wxl_"),
		WorkspaceID:  workspaceID,
		CreatedBy:    actor,
		QRCode:       qr.Code,
		QRContent:    qr.Content,
		PollBaseURL:  s.cfg.Client.LoginBaseURL(),
		Status:       apimodel.WeixinLoginWaiting,
		RefreshCount: 1,
		Message:      "用手机微信扫描二维码，并在手机上确认",
		CreatedAt:    now,
		ExpiresAt:    now.Add(s.cfg.LoginTTL),
	}
	if err := s.cfg.Store.CreateLogin(ctx, rec); err != nil {
		return apimodel.WeixinLogin{}, err
	}
	return loginView(rec), nil
}

// getOwnLogin loads a login that belongs to this workspace and this person. Anything else is "not found":
// the QR code is a credential, and a login id must not be usable by someone else.
func (s *Service) getOwnLogin(ctx context.Context, workspaceID, actor, loginID string) (LoginRecord, error) {
	rec, err := s.cfg.Store.GetLogin(ctx, loginID)
	if err != nil {
		return LoginRecord{}, err
	}
	if rec.WorkspaceID != workspaceID || rec.CreatedBy != actor {
		return LoginRecord{}, ErrLoginNotFound
	}
	return rec, nil
}

// PollLogin advances a login by one status check and returns where it stands.
//
// One call waits up to PollWait on WeChat's long poll. Two pages polling the same login at once are
// harmless: confirming twice upserts the same bot.
func (s *Service) PollLogin(ctx context.Context, workspaceID, actor, loginID string) (apimodel.WeixinLogin, error) {
	rec, err := s.getOwnLogin(ctx, workspaceID, actor, loginID)
	if err != nil {
		return apimodel.WeixinLogin{}, err
	}
	if rec.Status.Done() {
		return loginView(rec), nil
	}
	if s.now().After(rec.ExpiresAt) {
		rec.Status, rec.Message = apimodel.WeixinLoginExpired, "登录超时，请重新扫码"
		return s.saveLogin(ctx, rec)
	}

	st, err := s.cfg.Client.GetQRCodeStatus(ctx, rec.PollBaseURL, rec.QRCode, rec.PendingVerifyCode, s.cfg.PollWait)
	if err != nil {
		// Like the reference client: a failed status check is "still waiting"; the page polls again.
		s.log.Warn("查微信登录状态失败，当作还在等", "login", rec.ID, "error", err.Error())
		return loginView(rec), nil
	}

	switch st.Status {
	case wx.QRWait, "":
	case wx.QRScanned:
		rec.Status, rec.Message, rec.PendingVerifyCode = apimodel.WeixinLoginScanned, "已扫码，请在手机上确认", ""
	case wx.QRScannedRedirect:
		if host := strings.TrimSpace(st.RedirectHost); host != "" {
			rec.PollBaseURL = "https://" + host
		}
		rec.Status, rec.Message = apimodel.WeixinLoginScanned, "已扫码，请在手机上确认"
	case wx.QRNeedVerifyCode:
		rec.Status = apimodel.WeixinLoginNeedVerifyCode
		if rec.PendingVerifyCode != "" {
			// We sent a code and the server still asks: it was wrong. Stop resending it.
			rec.Message = "数字不对，请重新输入手机微信上显示的数字"
		} else {
			rec.Message = "请输入手机微信上显示的数字"
		}
		rec.PendingVerifyCode = ""
	case wx.QRExpired, wx.QRVerifyCodeBlocked:
		reason := "二维码已过期"
		if st.Status == wx.QRVerifyCodeBlocked {
			reason = "验证码输错次数太多"
		}
		if rec.RefreshCount >= maxQRCodes {
			rec.Status, rec.Message = apimodel.WeixinLoginExpired, reason+"，请稍后重新发起"
			break
		}
		if err := s.refreshQRCode(ctx, &rec); err != nil {
			rec.Status, rec.Message = apimodel.WeixinLoginFailed, "刷新二维码失败："+err.Error()
			break
		}
		rec.Message = reason + "，已换一张新的二维码，请重新扫码"
	case wx.QRAlreadyBound:
		rec.Status, rec.Message = apimodel.WeixinLoginAlreadyConnected, "这个微信机器人已经接入过了，无需重复接入"
	case wx.QRConfirmed:
		s.confirmLogin(ctx, &rec, st)
	default:
		s.log.Warn("微信登录返回了不认识的状态，当作还在等", "login", rec.ID, "status", st.Status)
	}
	return s.saveLogin(ctx, rec)
}

// SubmitVerifyCode stores the number the user typed; it is sent with the next polls.
func (s *Service) SubmitVerifyCode(ctx context.Context, workspaceID, actor, loginID, code string) (apimodel.WeixinLogin, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return apimodel.WeixinLogin{}, ErrInvalidArgument
	}
	rec, err := s.getOwnLogin(ctx, workspaceID, actor, loginID)
	if err != nil {
		return apimodel.WeixinLogin{}, err
	}
	if rec.Status != apimodel.WeixinLoginNeedVerifyCode {
		return apimodel.WeixinLogin{}, ErrVerifyCodeNotAsked
	}
	rec.PendingVerifyCode, rec.Message = code, "正在验证"
	return s.saveLogin(ctx, rec)
}

func (s *Service) refreshQRCode(ctx context.Context, rec *LoginRecord) error {
	tokens, _ := s.cfg.Store.RecentBotTokens(ctx, recentTokens)
	qr, err := s.cfg.Client.GetBotQRCode(ctx, "", tokens)
	if err != nil {
		return err
	}
	rec.QRCode, rec.QRContent = qr.Code, qr.Content
	rec.PollBaseURL = s.cfg.Client.LoginBaseURL()
	rec.Status, rec.PendingVerifyCode = apimodel.WeixinLoginWaiting, ""
	rec.RefreshCount++
	return nil
}

// confirmLogin stores the bot the login produced.
func (s *Service) confirmLogin(ctx context.Context, rec *LoginRecord, st wx.QRStatus) {
	if strings.TrimSpace(st.BotID) == "" || strings.TrimSpace(st.BotToken) == "" {
		rec.Status, rec.Message = apimodel.WeixinLoginFailed, "微信确认了登录，但没有返回机器人凭据，请重试"
		return
	}
	baseURL := strings.TrimSpace(st.BaseURL)
	if baseURL == "" {
		baseURL = wx.DefaultBaseURL
	}
	now := apimodel.FormatISO(s.now())
	acc, err := s.cfg.Store.UpsertAccount(ctx, AccountRecord{
		ID:          newID("wxa_"),
		WorkspaceID: rec.WorkspaceID,
		BotID:       st.BotID,
		BotToken:    st.BotToken,
		BaseURL:     baseURL,
		OwnerUserID: st.UserID,
		Status:      apimodel.WeixinAccountActive,
		CreatedBy:   rec.CreatedBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	switch {
	case errors.Is(err, ErrBotInOtherSpace):
		rec.Status, rec.Message = apimodel.WeixinLoginFailed, "这个微信机器人已接入别的空间，要在这里用请先到那个空间断开"
		return
	case err != nil:
		s.log.Error("保存微信账号失败", "login", rec.ID, "error", err.Error())
		rec.Status, rec.Message = apimodel.WeixinLoginFailed, "保存微信账号失败，请重试"
		return
	}
	s.log.Info("微信机器人已接入", "workspace", rec.WorkspaceID, "account", acc.ID, "bot", acc.BotID)
	rec.Status, rec.AccountID, rec.Message = apimodel.WeixinLoginConfirmed, acc.ID, "已接入"
}

func (s *Service) saveLogin(ctx context.Context, rec LoginRecord) (apimodel.WeixinLogin, error) {
	if err := s.cfg.Store.UpdateLogin(ctx, rec); err != nil {
		return apimodel.WeixinLogin{}, err
	}
	return loginView(rec), nil
}

func loginView(rec LoginRecord) apimodel.WeixinLogin {
	v := apimodel.WeixinLogin{
		ID:        rec.ID,
		Status:    rec.Status,
		Message:   rec.Message,
		AccountID: rec.AccountID,
		ExpiresAt: apimodel.FormatISO(rec.ExpiresAt),
	}
	// The QR code is only useful while someone still has to scan it.
	if !rec.Status.Done() {
		v.QRCodeContent = rec.QRContent
		v.QRCodeDataURL = qrDataURL(rec.QRContent)
	}
	return v
}

// qrDataURL renders text as a PNG QR code data URL. "" when it cannot be rendered (the page then shows
// the link instead).
func qrDataURL(text string) string {
	if text == "" {
		return ""
	}
	png, err := qrcode.Encode(text, qrcode.Medium, 256)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// ---- Accounts ----

// ListAccounts lists the WeChat bots of a workspace.
func (s *Service) ListAccounts(ctx context.Context, workspaceID string) ([]apimodel.WeixinAccount, error) {
	recs, err := s.cfg.Store.ListAccounts(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]apimodel.WeixinAccount, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.View())
	}
	return out, nil
}

func (s *Service) getAccountIn(ctx context.Context, workspaceID, accountID string) (AccountRecord, error) {
	acc, err := s.cfg.Store.GetAccount(ctx, accountID)
	if err != nil {
		return AccountRecord{}, err
	}
	if acc.WorkspaceID != workspaceID {
		return AccountRecord{}, ErrAccountNotFound
	}
	return acc, nil
}

// BindAgent binds the bot to an agent of the same workspace; "" unbinds it.
//
// The binding takes effect with the next message (the poller reads the account per message), and the
// conversation starts fresh: the agent id is part of the session scope key.
func (s *Service) BindAgent(ctx context.Context, workspaceID, accountID, agentID string) (apimodel.WeixinAccount, error) {
	acc, err := s.getAccountIn(ctx, workspaceID, accountID)
	if err != nil {
		return apimodel.WeixinAccount{}, err
	}
	agentID = strings.TrimSpace(agentID)
	if agentID != "" {
		ws, err := s.cfg.Agents.AgentWorkspace(ctx, agentID)
		if err != nil {
			return apimodel.WeixinAccount{}, err
		}
		if ws != workspaceID {
			return apimodel.WeixinAccount{}, ErrAgentInOtherSpace
		}
	}
	now := apimodel.FormatISO(s.now())
	if err := s.cfg.Store.SetAccountAgent(ctx, acc.ID, agentID, now); err != nil {
		return apimodel.WeixinAccount{}, err
	}
	acc.AgentID, acc.UpdatedAt = agentID, now
	return acc.View(), nil
}

// DeleteAccount disconnects a bot. Its poller stops at the next reconcile (within a minute).
func (s *Service) DeleteAccount(ctx context.Context, workspaceID, accountID string) error {
	acc, err := s.getAccountIn(ctx, workspaceID, accountID)
	if err != nil {
		return err
	}
	return s.cfg.Store.DeleteAccount(ctx, acc.ID)
}

// newID returns prefix + 16 random lowercase base32 characters.
func newID(prefix string) string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return prefix + strings.ToLower(base32NoPad.EncodeToString(b[:]))
}
