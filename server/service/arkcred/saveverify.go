// Online check before saving a credential.
//
// All three save levels (account / workspace / agent) go through the same verifyBeforeSave before writing to the database.
// The three outcomes are spelled out in this one function and all three places just call it, so no outcome gets changed in only one place.
//
// # Three outcomes (decided by product; do not change the boundaries)
//
//  1. Key invalid/deleted (401/403) → hard error, **saving is blocked**.
//     Such a key makes the first conversation fail for sure (the "Ark rejected this API Key" kind in feedback),
//     so it is not worth saving, and there is no "save anyway" way out.
//  2. Key valid but the account has not activated Managed Agents (404 + code=ManagedAgentNotOpen)
//     → warning; saving allowed after confirmation. The key itself is right; the user may want to set it up first and activate later;
//     blocking hard would force them to come back and configure again after activating. The first request is not stored and returns the 422 detail code
//     ark_managed_agent_not_open; the frontend lets them choose "go activate / save anyway", and the latter resends with
//     draft.ConfirmNotActivated=true before it is stored.
//  3. Timeout / 5xx / 429 / any unrecognized error → **saving is allowed**, with only a transient warning.
//     This is a red line: our dependency being down must not stop users from saving config. The worst case is just one missing
//     not-activated hint; a valid key must never be locked out.
//
// # Only structured status codes/error codes are used, never Ark's Chinese message text
//
// The real code is LiveArkVerifier.Verify (core/arkverify.go); the classification helpers are in
// shared/infra/ma/errors.go (IsAuthError / IsManagedAgentNotOpen / NotOpenAccountID).
// Ark's Chinese message text can change; the code is the contract.
//
// # Existing credentials are not batch-checked at startup
//
// This only runs on the synchronous "save a new credential" path; there is no background/startup batch check:
// a new check must not make existing config that used to work unusable. It is one synchronous read-only call
// and adds no in-process state (safe across instances).

package arkcred

import (
	"context"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
)

// verifyBeforeSave is the online check before writing to the database.
//
// Returns:
//   - reject non-nil: saving must be blocked (bad key, or not activated and the user has not confirmed yet);
//   - warning non-nil: allowed, but this transient hint must go back to the user with the save response (the check did not complete).
//
// The two are mutually exclusive; when no verifier is wired (local/some tests) both are nil, meaning this check is skipped,
// keeping the old behavior "saving works even without Ark".
func (s *Service) verifyBeforeSave(
	ctx context.Context, plainKey string, region apimodel.ArkRegion, confirmedNotActivated bool,
) (reject *SaveRejectedError, warning *apimodel.ArkSaveWarning) {
	if s.verifier == nil {
		return nil, nil
	}

	res := s.verifier.Verify(ctx, plainKey, region)
	switch res.Status {
	case VerifyStatusInvalidKey:
		msg := "方舟拒绝了这把 API Key：它无效或已被删除。" +
			"请到方舟控制台检查这把 key，或重新创建一把后再保存。"
		if res.RequestID != "" {
			msg += "（方舟 request_id：" + res.RequestID + "，报障时可提供）"
		}
		return NewSaveRejectedError(RejectCodeInvalidKey, msg), nil

	case VerifyStatusNotActivated:
		if !confirmedNotActivated {
			msg := "这把 API Key 本身有效，但它所属的方舟账号还没开通 Managed Agents 服务，" +
				"现在保存了也无法发起对话。请先到方舟控制台开通 Managed Agents；"
			if res.AccountID != "" {
				msg = "这把 API Key 本身有效，但它所属的方舟账号（账号 " +
					res.AccountID + "）还没开通 Managed Agents 服务，现在保存了也无法发起对话。" +
					"请先到方舟控制台开通 Managed Agents；"
			}
			msg += "如果你打算稍后再去开通，也可以点「仍然保存」先把 key 存下来。"
			if res.RequestID != "" {
				msg += "（方舟 request_id：" + res.RequestID + "，报障时可提供）"
			}
			return NewSaveRejectedError(RejectCodeNotActivated, msg), nil
		}
		// The user already confirmed "save even if not activated": allow, no warning (they just made a choice on the confirm bar,
		// showing the same content again is noise; check stays unchecked, so the card does not lie).
		return nil, nil

	case VerifyStatusSkipped:
		// Red line: dependency failures do not block saving. Technical reasons only go to the log.
		if res.Cause != nil {
			s.log().LogAttrs(ctx, slog.LevelWarn, "保存方舟凭证前的在线校验没验成，已放行",
				slog.String("cause", res.Cause.Error()))
		} else {
			s.log().LogAttrs(ctx, slog.LevelWarn, "保存方舟凭证前的在线校验没验成，已放行")
		}
		return nil, &apimodel.ArkSaveWarning{
			Kind: apimodel.ArkWarnVerifyUnavailable,
			Message: "方舟暂时没能完成校验（超时或服务暂不可用），key 已先保存。" +
				"等方舟恢复后可点「验证连通性」复查。",
		}

	default: // VerifyStatusOK
		return nil, nil
	}
}
