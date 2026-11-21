package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/secret"
)

const (
	DefaultNamedCredentialLimit     = 20
	DefaultNamedCredentialNameRunes = 64
)

// NamedCredentialStore is the only storage dependency of a named credential provider.
// Put must not replace PublicID on an existing row: changing that public identity would
// silently retarget all agent/workspace references to another account. The application
// check gives a readable error; the persistent conditional update is the second guard.
type NamedCredentialStore interface {
	List(context.Context, string) ([]NamedCredentialRecord, error)
	Get(context.Context, string) (NamedCredentialRecord, error)
	Put(context.Context, NamedCredentialRecord) error
	Delete(context.Context, string) error
}

// NamedCredentialHooks supplies private platform verification and display policy.
// Verify may use Secret, but its result, errors, and logs must never contain it.
type NamedCredentialHooks struct {
	Store        NamedCredentialStore
	Verify       func(context.Context, NamedCredentialRecord) NamedCredentialVerification
	Provider     string
	NewID        func() string
	Now          func() time.Time
	NowISO       func() string
	MaxPerUser   int
	MaxNameRunes int
	// LegacyEmptyName preserves an old row's empty label when the update only rotates
	// its secret. It does not relax validation for newly created credentials.
	LegacyEmptyName bool
	UnknownReason   string
	Logger          *slog.Logger
}

// NamedCredentialProvider implements collection CRUD and secret retrieval for downstream
// application credentials. Unknown platform verification is allowed to save; an explicit
// invalid result is rejected before storing the credential.
type NamedCredentialProvider struct{ hooks NamedCredentialHooks }

func NewNamedCredentialProvider(hooks NamedCredentialHooks) *NamedCredentialProvider {
	if hooks.Logger == nil {
		hooks.Logger = slog.Default()
	}
	if hooks.MaxPerUser <= 0 {
		hooks.MaxPerUser = DefaultNamedCredentialLimit
	}
	if hooks.MaxNameRunes <= 0 {
		hooks.MaxNameRunes = DefaultNamedCredentialNameRunes
	}
	if hooks.Now == nil {
		hooks.Now = time.Now
	}
	if hooks.NowISO == nil {
		hooks.NowISO = apimodel.NowISO
	}
	if hooks.UnknownReason == "" {
		hooks.UnknownReason = "服务端没有装配验证器，这次没能验证"
	}
	return &NamedCredentialProvider{hooks: hooks}
}

func (p *NamedCredentialProvider) configured() error {
	if p == nil || p.hooks.Store == nil || p.hooks.NewID == nil {
		return fmt.Errorf("%w: %s 服务账号授权还没接入", ErrPrecondition, p.hooks.Provider)
	}
	return nil
}

func (p *NamedCredentialProvider) label(name string) (string, error) {
	label := strings.TrimSpace(name)
	if label == "" {
		return "", fmt.Errorf("%w: 给这条凭据起个名字（比如「只读-巡检」），好在 agent 里认出它", ErrInvalidArgument)
	}
	if len([]rune(label)) > p.hooks.MaxNameRunes {
		return "", fmt.Errorf("%w: 名字太长了（最多 %d 个字）", ErrInvalidArgument, p.hooks.MaxNameRunes)
	}
	return label, nil
}

func (p *NamedCredentialProvider) List(
	ctx context.Context, actor string,
) ([]apimodel.IntegrationCredentialView, error) {
	if err := p.configured(); err != nil {
		return nil, err
	}
	records, err := p.hooks.Store.List(ctx, actor)
	if err != nil {
		return nil, fmt.Errorf("读 %s 凭据失败: %w", p.hooks.Provider, err)
	}
	out := make([]apimodel.IntegrationCredentialView, 0, len(records))
	for _, record := range records {
		out = append(out, record.View(record.StoredVerification()))
	}
	return out, nil
}

// Metadata deliberately has no actor check: an administrator may bind another person's
// application credential after the workspace authorization check. It never returns Secret.
func (p *NamedCredentialProvider) Metadata(
	ctx context.Context, id string,
) (apimodel.IntegrationCredentialView, error) {
	record, err := p.record(ctx, id)
	if err != nil {
		return apimodel.IntegrationCredentialView{}, err
	}
	return record.View(record.StoredVerification()), nil
}

// Secret is only for an already authorized runtime injection path. It must not be compared
// with the current request actor: a workspace can legitimately reference another owner's app.
func (p *NamedCredentialProvider) Secret(ctx context.Context, id string) (string, string, error) {
	record, err := p.record(ctx, id)
	if err != nil {
		return "", "", err
	}
	return record.PublicID, record.Secret, nil
}

func (p *NamedCredentialProvider) record(ctx context.Context, id string) (NamedCredentialRecord, error) {
	var zero NamedCredentialRecord
	if err := p.configured(); err != nil {
		return zero, err
	}
	if strings.TrimSpace(id) == "" {
		return zero, fmt.Errorf("%w: 没有指定要用哪一条凭据", ErrInvalidArgument)
	}
	record, err := p.hooks.Store.Get(ctx, strings.TrimSpace(id))
	if err != nil {
		return zero, err
	}
	return record, nil
}

func (p *NamedCredentialProvider) ownedRecord(
	ctx context.Context, actor, id string,
) (NamedCredentialRecord, error) {
	record, err := p.record(ctx, id)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return NamedCredentialRecord{}, fmt.Errorf("%w: 没有这条凭据", ErrNotFound)
		}
		return NamedCredentialRecord{}, fmt.Errorf("读 %s 凭据失败: %w", p.hooks.Provider, err)
	}
	// Do not distinguish another person's ID from a nonexistent one.
	if record.Actor != actor {
		return NamedCredentialRecord{}, fmt.Errorf("%w: 没有这条凭据", ErrNotFound)
	}
	return record, nil
}

func (p *NamedCredentialProvider) verify(ctx context.Context, record NamedCredentialRecord) NamedCredentialVerification {
	if p.hooks.Verify == nil {
		return NamedCredentialVerification{Unknown: true, Reason: p.hooks.UnknownReason}
	}
	return p.hooks.Verify(ctx, record)
}

func invalidCredentialReason(v NamedCredentialVerification) error {
	reason := strings.TrimSpace(v.Reason)
	if reason == "" {
		reason = "平台拒绝了这组凭据，但没给具体原因"
	}
	return fmt.Errorf("%w: 凭据验证不通过：%s", ErrInvalidArgument, reason)
}

// Add validates before saving, except for an Unknown verification. A temporary upstream
// failure must not make correct credentials impossible to configure.
func (p *NamedCredentialProvider) Add(
	ctx context.Context, actor, name, publicID, plaintextSecret string,
) (apimodel.IntegrationCredentialView, error) {
	var zero apimodel.IntegrationCredentialView
	if err := p.configured(); err != nil {
		return zero, err
	}
	label, err := p.label(name)
	if err != nil {
		return zero, err
	}
	publicID = strings.TrimSpace(publicID)
	plaintextSecret = strings.TrimSpace(plaintextSecret)
	if publicID == "" || plaintextSecret == "" {
		return zero, fmt.Errorf("%w: 公开身份与 Secret 都要填", ErrInvalidArgument)
	}
	existing, err := p.hooks.Store.List(ctx, actor)
	if err != nil {
		return zero, fmt.Errorf("读 %s 凭据失败: %w", p.hooks.Provider, err)
	}
	if len(existing) >= p.hooks.MaxPerUser {
		return zero, fmt.Errorf("%w: 最多只能配 %d 条凭据（现在有 %d 条），先删掉用不上的那几条",
			ErrInvalidArgument, p.hooks.MaxPerUser, len(existing))
	}
	now := p.hooks.Now()
	record := NamedCredentialRecord{
		ID: p.hooks.NewID(), Actor: actor, Name: label, PublicID: publicID,
		Secret: plaintextSecret, SecretMasked: secret.Mask(plaintextSecret, 4),
		CreatedAt: now, UpdatedAt: now,
	}
	verification := p.verify(ctx, record)
	if !verification.Valid && !verification.Unknown {
		return zero, invalidCredentialReason(verification)
	}
	record.LastVerifyLimited = verification.Limited
	if verification.Valid {
		record.LastVerifiedAt = now
	}
	if err := p.hooks.Store.Put(ctx, record); err != nil {
		return zero, fmt.Errorf("存 %s 凭据失败: %w", p.hooks.Provider, err)
	}
	p.hooks.Logger.InfoContext(ctx, "已添加服务账号凭据",
		slog.String("provider", p.hooks.Provider), slog.String("actor", actor),
		slog.String("cred_id", record.ID), slog.String("public_id", record.PublicID),
		slog.Bool("verified", verification.Valid), slog.Bool("limited", verification.Limited))
	return record.View(verification), nil
}

// Update rotates only the secret and optional display name. The public identity is
// immutable because stored references would otherwise point to a different account
// without changing a single ID. An empty secret means rename-only and skips verification.
func (p *NamedCredentialProvider) Update(
	ctx context.Context, actor, id, name, publicID, plaintextSecret string,
) (apimodel.IntegrationCredentialView, error) {
	var zero apimodel.IntegrationCredentialView
	record, err := p.ownedRecord(ctx, actor, id)
	if err != nil {
		return zero, err
	}
	if given := strings.TrimSpace(publicID); given != "" && given != record.PublicID {
		return zero, fmt.Errorf("%w: 公开身份创建后不能修改（当前是 %s）。要换一个账号请新建一条凭据，再删掉这条——直接改会让引用这条凭据的 agent 在不知情的情况下换了身份",
			ErrInvalidArgument, record.PublicID)
	}
	label := strings.TrimSpace(name)
	if label == "" && p.hooks.LegacyEmptyName {
		label = record.Name
	}
	if label, err = p.label(label); err != nil {
		return zero, err
	}
	verification := record.StoredVerification()
	renameOnly := strings.TrimSpace(plaintextSecret) == ""
	if !renameOnly {
		updatedSecret := record
		updatedSecret.Secret = strings.TrimSpace(plaintextSecret)
		verification = p.verify(ctx, updatedSecret)
		if !verification.Valid && !verification.Unknown {
			return zero, invalidCredentialReason(verification)
		}
		record.Secret = updatedSecret.Secret
		record.SecretMasked = secret.Mask(record.Secret, 4)
		record.LastVerifyLimited = verification.Limited
		if verification.Valid {
			record.LastVerifiedAt = p.hooks.Now()
			record.LastVerifyError = ""
		}
	}
	record.Name = label
	record.UpdatedAt = p.hooks.Now()
	if err := p.hooks.Store.Put(ctx, record); err != nil {
		return zero, fmt.Errorf("存 %s 凭据失败: %w", p.hooks.Provider, err)
	}
	p.hooks.Logger.InfoContext(ctx, "已更新服务账号凭据",
		slog.String("provider", p.hooks.Provider), slog.String("actor", actor),
		slog.String("cred_id", record.ID), slog.String("public_id", record.PublicID),
		slog.Bool("rename_only", renameOnly), slog.Bool("verified", verification.Valid))
	return record.View(verification), nil
}

func (p *NamedCredentialProvider) Delete(ctx context.Context, actor, id string) error {
	record, err := p.ownedRecord(ctx, actor, id)
	if err != nil {
		return err
	}
	if err := p.hooks.Store.Delete(ctx, record.ID); err != nil {
		return fmt.Errorf("删 %s 凭据失败: %w", p.hooks.Provider, err)
	}
	p.hooks.Logger.InfoContext(ctx, "已删除服务账号凭据",
		slog.String("provider", p.hooks.Provider), slog.String("actor", actor), slog.String("cred_id", record.ID))
	return nil
}

// Verify rechecks one credential. Unknown does not overwrite a prior result. A metadata
// write failure does not turn an otherwise valid verification into an invalid credential.
func (p *NamedCredentialProvider) Verify(
	ctx context.Context, record NamedCredentialRecord,
) NamedCredentialVerification {
	verification := p.verify(ctx, record)
	if verification.Unknown {
		return verification
	}
	record.UpdatedAt = p.hooks.Now()
	record.LastVerifyLimited = verification.Limited
	if verification.Valid {
		record.LastVerifiedAt = p.hooks.Now()
		record.LastVerifyError = ""
	} else {
		record.LastVerifyError = verification.Reason
	}
	if err := p.hooks.Store.Put(ctx, record); err != nil {
		p.hooks.Logger.WarnContext(ctx, "写回凭据验证结果失败（不影响本次判定）", slog.Any("err", err))
	}
	return verification
}
