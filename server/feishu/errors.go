// Sentinel errors of this package.
//
// handler uses errors.Is to map these to HTTP status codes and user-readable messages; see handler.go.

package feishu

import "errors"

var (
	// ErrInvalidArgument means an argument is invalid, e.g. workspaceID is empty.
	ErrInvalidArgument = errors.New("feishu: 参数不合法")

	// ErrAccessDenied means the caller has no permission.
	//
	// Also used for "polling someone else's registration session when you are not the initiator". In that case we do not distinguish
	// "session does not exist" from "no access", so as not to reveal whether the session exists.
	ErrAccessDenied = errors.New("feishu: 无权访问")

	// ErrSessionLost means the registration session does not exist or has expired.
	ErrSessionLost = errors.New("feishu: 注册会话已失效")

	// ErrAppNotFound means the app does not exist.
	ErrAppNotFound = errors.New("feishu: 应用不存在")

	// ErrProgressNotFound means the progress delivery record does not exist.
	ErrProgressNotFound = errors.New("feishu: 进度投递不存在")

	// ErrProgressLeaseLost means the lease no longer belongs to us.
	//
	// On getting it you must **stop immediately**: it means this instance was stuck too long, the lease expired and someone else took over,
	// and writing further would overwrite the new holder's progress, or have two instances pushing events into one CoT at once.
	ErrProgressLeaseLost = errors.New("feishu: 进度投递的租约已失效")

	// ErrAppExists means this Feishu app has already been connected.
	//
	// Store uses it to say "hit the appID unique key". On receiving it the business layer reads back the existing row: if it
	// still belongs to the same person, treat it as a successful reconnect and do not surface this error to the user.
	ErrAppExists = errors.New("feishu: 应用已接入")

	// ErrAppOwnedByOther means this Feishu app has already been connected by someone else.
	//
	// This is the only "already taken" error that is really reported to the user.
	ErrAppOwnedByOther = errors.New("feishu: 应用已被他人接入")

	// ErrScopeNotBound means this session scope has not been bound to an Ark session yet.
	//
	// **It is a normal state, not a failure**: it is always the case the first time the bot is @-mentioned in a chat. When the inbound path gets it,
	// it creates a new session and binds it (see ensureScopedRun in inbound.go); it should not be logged as an error.
	ErrScopeNotBound = errors.New("feishu: 会话作用域尚未绑定")

	// ErrInboundMessageNotFound means there is no dedup record for this inbound message.
	//
	// Only GetInboundMessage returns it, and that is a pure debugging method (no caller on the business path).
	// Old records get cleaned up, so "not found" may just mean it expired; it does not mean the message was never handled.
	ErrInboundMessageNotFound = errors.New("feishu: 入站消息记录不存在")
)
