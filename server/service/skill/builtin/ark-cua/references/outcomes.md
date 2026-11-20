# Task results

Use `data.outcome` to decide what happens next. Keep the returned invocation/task identifier and follow `next.command` for the same task.

| Outcome | Action |
| --- | --- |
| `in_progress` | Run `next.command`; do not answer from progress text. |
| `needs_input` | Relay `data.input_request.question` verbatim and submit the user's reply. |
| `completed` | Use `data.result.text` as the authoritative final result. |
| `failed` | Explain the failure; retry only when requested and safe. |
| `cancelled` | Report cancellation. |

A timeout is not a task outcome. Continue the same invocation after a retryable connection error; never submit the task again or infer completion from progress text.

`ACTIVE_RUN_CONFLICT` means the new request was rejected before task creation. Stop and tell the user that the desktop is busy.
