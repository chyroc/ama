package channel

// Fixed replies every channel sends in the same situations. They are user-facing copy (Chinese, like
// the rest of the bot replies) and live here so the platforms cannot drift apart.
const (
	// ReplyNoAnswer is for a turn that ended without any answer text and without a known cause.
	ReplyNoAnswer = "这一轮没有产生回复，可能是执行出了问题。把刚才那句话再发一次试试；" +
		"要是还不行，换个说法或稍后再问。"

	// ReplyRetriesExhausted is for a turn Ark gave up on (TurnResult.FailedUpstream), most likely rate
	// limiting. It is separate from ReplyNoAnswer because the next step differs: resending the same
	// sentence is enough.
	ReplyRetriesExhausted = "这一轮没能完成：模型侧连续失败、重试次数已用尽（多半是被限流了）。" +
		"稍等一会儿把刚才那句话再发一次就好。"

	// ReplyAgentDisabled is sent instead of running a disabled agent, so the chat is not left silent.
	ReplyAgentDisabled = "这个 agent 已停用，暂时不会回答。到 AMA 的 agent 详情页把状态改回「已启用」就能继续用了。"
)

// FallbackAnswer returns what to send when a turn produced no answer text.
func FallbackAnswer(res TurnResult) string {
	if res.FailedUpstream {
		return ReplyRetriesExhausted
	}
	return ReplyNoAnswer
}
