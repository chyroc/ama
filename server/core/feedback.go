// Issue feedback: one-click reporting when a user hits a problem, plus a read endpoint for
// the dev team.
//
// # The two levels differ in "what is collected", not only in "who triggers it"
//
//	auto  saved automatically when an error happens; the user **did not click anything**.
//	      Since there is no user consent, only non-sensitive metadata is collected.
//	user  the user clicked "report this problem". **That click is their consent**, so more
//	      can be collected.
//
// This split is not a formality. Level one happens without the user knowing, so every field
// it collects must be "fine even if the whole world sees it". Level two is handed over by the
// user on purpose, so it can include the identifiers actually needed to find the problem.
// **Collecting the same things at both levels would use level two's consent to justify level
// one's collection.**
//
// # ⚠️ Filtering is an allowlist, not a denylist
//
// This is the most important rule in this file. The request body is **not** stored into the
// payload as is. It is first decoded into a struct with fixed fields (feedbackInput), and then
// sanitizeAuto / sanitizeUser **build a new** map to store. Any extra field the frontend sends
// is already gone at decode time.
//
// A denylist ("drop the token / cookie / authorization keys") will always miss something, in
// two ways, and neither reports an error:
//
//   - a missed key name (Authorization / X-Auth-Token / apiKey / secret / bearer /
//     set-cookie / x-jwt-assertion …; the list never ends, and every new frontend field adds
//     one more);
//   - a credential **embedded** in a value (half a JWT pasted into a message, a URL carrying
//     ?token=).
//
// The allowlist blocks the first: keys not on the list **are never read at all**. scrubText
// adds a layer for the second (see below), but that is only defense in depth. The real
// guarantee is "we only store what we listed ourselves".
//
// # Level one must be rate limited, or this feature DoSes itself
//
// One broken endpoint can make one user produce hundreds of errors in a minute (page
// remounts, SSE reconnects, polling retries), production runs many instances, and there are
// hundreds of users. Without a limit, the first real outage floods the table, and then
// **the read endpoint fails too**: you cannot find any other problem among hundreds of
// thousands of copies of the same error.
//
// The approach: "same actor + same route_pattern + same error_code + same status code + same
// ten-minute bucket" has only one row; a hit does occurrences +1. **A database unique
// constraint** decides, not read-then-write in Go (that is TOCTOU and will miss under
// concurrent instances). See schema_feedback_pg.sql.

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// credentialRedacted is the marker left after redaction.
//
// **A readable phrase on purpose, not an empty string**:
// whoever investigates needs to know "something was here and we removed it", or they will
// look for a field that does not exist.
// No part of the original value is kept (not even its length), because length is itself
// information.
const credentialRedacted = "[已抹除]"

// Regexes that recognize credentials by shape. **Compiled once** (package-level vars); do not
// MustCompile inside a function: reporting runs on every error, so that would recompile four
// regexes for every feedback.
var (
	// jwtPattern matches JWTs: three dot-separated parts, the first starting with eyJ (the
	// base64url of `{"`).
	//
	// Match `eyJ` rather than "any three base64 parts": the latter would treat version
	// numbers, file names and domains as JWTs (a.b.c is too common), and a false hit here wipes
	// out debugging info.
	jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`)

	// apiKeyPattern matches API keys issued by this repo (amak_ prefix, see auth.KeyPrefix).
	//
	// **We do not import auth to get that constant**: core already imports auth
	// (feedbackActor uses ActorFrom), but writing it into the regex literal is clearer, and
	// the shape of this regex (how many characters follow) is this file's own decision anyway.
	apiKeyPattern = regexp.MustCompile(`amak_[A-Za-z0-9_-]{8,}`)

	// sensitiveParamPattern matches sensitive parameters in a URL query, **redacting only the
	// value and keeping the key name**.
	//
	// Keeping the key is deliberate: "this request carried a token parameter" is useful
	// debugging info, while removing the whole thing hides why the request failed.
	//
	// The second capture group is a quote or nothing (both `token="xxx"` and `token=xxx` must
	// be covered).
	sensitiveParamPattern = regexp.MustCompile(
		`(?i)\b(token|access_token|refresh_token|id_token|api_?key|secret|password|passwd|jwt|actor|credential|auth)=(["']?)[^\s&"'<>]+`)

	// bearerPattern matches forms like `Authorization: Bearer xxx` / `Bearer xxx`.
	bearerPattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
)

// FeedbackLevel and FeedbackAuto / FeedbackUser have moved to model/persistmodel.

// feedbackDedupWindow is the dedup time bucket size for level-one automatic records.
//
// Ten minutes: long enough to fold the repeated flood during one outage into one row, and
// short enough that "it came back half an hour later" is not mistaken for the same event.
// **Rounding, not a sliding window**: rounding is stateless, so two instances compute the same
// bucket for the same moment without coordinating. A sliding window must first read "when was
// the last one" before deciding, which is read-then-write again.
const feedbackDedupWindow = 10 * time.Minute

// Length limits for each field.
//
// **They are required**: all these fields come from the frontend, that is, from input the
// caller controls. Without limits, one request could push hundreds of MB into PG. Truncate
// rather than reject: debugging info that is a bit shorter is still useful, while rejecting
// means the user clicks feedback and nothing happens.
const (
	maxFeedbackTextLen   = 500
	maxFeedbackPathLen   = 300
	maxFeedbackDetailLen = 2000
	maxFeedbackListLen   = 10
	maxFeedbackPageSize  = 200
	feedbackDefaultLimit = 50
	maxFeedbackReasonLen = 200
)

// FeedbackRecord has moved to model/persistmodel (a Store row struct, not in HTTP responses).

// FeedbackQuery has moved to model/persistmodel (a Store input).

// FeedbackReaders decides who counts as "the dev team". Wired by cmd/amad from
// config.Feedback.
//
// It is an interface rather than a direct dependency on shared/infra/config: core does not
// import config today, and once that dependency exists, every core test would have to build a
// Config. Same approach as authz.Membership / core.ChatOrigins.
//
// **Left empty (nil) means always deny**; see Service.ListFeedback.
type FeedbackReaders interface {
	// CanRead reports whether this actor can read the feedback list.
	CanRead(actor string) bool
}

// SubmitFeedback accepts one feedback.
//
// The return value is **only an error** on purpose: the caller (the browser) does not need
// the stored id, and returning an id would make "reporting" look like it created a resource
// the user can act on again. Nobody looks at the return value for level one anyway.
func (s *Service) SubmitFeedback(ctx context.Context, in FeedbackInput) error {
	actor, err := feedbackActor(ctx)
	if err != nil {
		return err
	}

	level := persistmodel.FeedbackLevel(strings.TrimSpace(in.Level))
	switch level {
	case persistmodel.FeedbackAuto, persistmodel.FeedbackUser:
	default:
		// Reject unknown levels outright; **do not pick a default**. Defaulting to auto
		// would treat a feedback the user submitted on purpose as an automatic record, so it
		// takes part in dedup and may be merged into an existing record: the user sees
		// "received" while their feedback does not actually exist on its own.
		// Defaulting to user is worse: the whole level-one flood would bypass rate limiting.
		return fmt.Errorf("%w: 反馈级别只能是 auto 或 user", ErrInvalidArgument)
	}

	now := time.Now().UTC()
	rec := persistmodel.FeedbackRecord{
		ID:    s.nextID("fb"),
		Level: level,
		Actor: actor,
		// The backend strips ids again: if the frontend got it wrong, or a caller hits this
		// endpoint directly, a path with ids defeats dedup entirely (every row is a new key)
		// and turns the aggregate query into a table of tens of thousands of rows.
		RoutePattern: stripRouteIDs(truncate(in.RoutePattern, maxFeedbackPathLen)),
		HTTPStatus:   in.HTTPStatus,
		ErrorCode:    truncate(strings.TrimSpace(in.ErrorCode), maxFeedbackReasonLen),
		ErrorKind:    truncate(strings.TrimSpace(in.ErrorKind), maxFeedbackReasonLen),
		UserAgent:    feedbackUserAgent(in),
		CreatedAt:    now,
		LastSeenAt:   now,
		Occurrences:  1,
	}

	// Keep the workspace id only when it **looks like an id**. Sending the key (growth)
	// instead of the id (ws_xxx) is an easy mistake from the frontend, and once a key is
	// stored this column no longer joins with the workspaces table, which shows up as "the
	// workspace in the feedback cannot be found". Better to leave it empty.
	if wsID := strings.TrimSpace(in.WorkspaceID); strings.HasPrefix(wsID, "ws") {
		rec.WorkspaceID = truncate(wsID, maxFeedbackReasonLen)
	}

	// Server logid: **collected at both levels**.
	//
	// It needs no user consent: the string is generated by our own server (httpd's
	// withLogID), contains no user input, and is not a credential. Level one already knows
	// "who reported this" (the actor column stores the employee email), so the logid does not
	// expose anyone's identity further.
	//
	// ⚠️ **It does not matter whether this is set before dedup_key is computed, because
	// feedbackDedupKey does not read it.** That is a hard rule: the logid is unique per
	// request, so once it enters the dedup key every row is new and **rate limiting stops
	// working entirely** (the way this feature DoSes itself, with no error).
	// `TestFeedbackLogIDIsNotInDedupKey` pins this.
	//
	// An empty string is normal, not an anomaly: failed requests like network_error never
	// reached the server, so no logid was ever generated. The Store writes empty as NULL.
	rec.LogID = truncate(strings.TrimSpace(in.LogID), maxFeedbackReasonLen)

	// ⚠️ **The two levels split here**; this is the implementation of the whole collection
	// boundary.
	if level == persistmodel.FeedbackAuto {
		rec.Payload = sanitizeAuto(in)
		// Only level one has a dedup key. Level user leaves it empty → the Store writes NULL →
		// it is not part of the unique constraint.
		rec.DedupKey = feedbackDedupKey(rec, now)
	} else {
		rec.Payload = sanitizeUser(in)
	}

	return s.store.PutFeedback(ctx, rec)
}

// ListFeedback lists feedback, **for the dev team only**.
//
// # The permission check is here; this is the repo's second place where "authorization lives
// in the Service layer"
//
// The first is GetWorkspaceByKey (the workspace comes from a query parameter, which Guard
// patterns cannot express). The reason here is different and stronger: **the authz model is
// "actor + workspace → role", and this endpoint has no workspace at all**. Feedback spans
// workspaces (and includes errors that happen outside any workspace).
// Expressing "is this the dev team" with Guard would need a brand new, platform-level role
// concept, which the authz README explicitly calls the more costly option (c).
//
// So in the matrix it is ActionNone (no workspace to check), and the real gate is here.
// **This is not "forgot to set a level"**; the matrix comment says so clearly, and a test
// pins that regular users get 403.
//
// # Empty means always deny
//
// When readers is nil (not wired), **nobody can read**, rather than everybody. Same reason as
// the feishu side, where an empty Authorizer denies: with an allow fallback, "forgot to wire
// it" means the gate does not exist, with zero errors.
func (s *Service) ListFeedback(ctx context.Context, q persistmodel.FeedbackQuery) (FeedbackPage, error) {
	actor, err := feedbackActor(ctx)
	if err != nil {
		return FeedbackPage{}, err
	}
	// The nil check is separate from CanRead so that "not wired" and "not on the list" can be
	// told apart in logs. To the user both mean the same thing (cannot read), but for
	// debugging they are two different things.
	if s.feedbackReaders == nil {
		s.log().WarnContext(ctx, "有人尝试读反馈列表，但读取白名单没有装配——一律拒绝",
			"actor", actor)
		return FeedbackPage{}, fmt.Errorf("%w: 只有开发团队可以查看问题反馈", ErrAccessDenied)
	}
	if !s.feedbackReaders.CanRead(actor) {
		// Log a Warn and name the caller: this endpoint "should not be hit by anyone", so it is
		// worth knowing when it is. Not an Error: a regular user trying it by hand is not an
		// outage.
		s.log().WarnContext(ctx, "非开发团队成员尝试读反馈列表，已拒绝", "actor", actor)
		return FeedbackPage{}, fmt.Errorf("%w: 只有开发团队可以查看问题反馈", ErrAccessDenied)
	}

	// Paging parameters are normalized here; **do not trust the caller**: limit 0 or negative
	// uses the default, and above the max is cut to the max (not an error; script authors
	// should not have to read our constants before pulling data).
	if q.Limit <= 0 {
		q.Limit = feedbackDefaultLimit
	}
	if q.Limit > maxFeedbackPageSize {
		q.Limit = maxFeedbackPageSize
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	if q.Level != "" && q.Level != persistmodel.FeedbackAuto && q.Level != persistmodel.FeedbackUser {
		return FeedbackPage{}, fmt.Errorf("%w: 反馈级别只能是 auto 或 user", ErrInvalidArgument)
	}

	recs, total, err := s.store.ListFeedback(ctx, q)
	if err != nil {
		return FeedbackPage{}, err
	}
	// A non-nil empty slice: list endpoints never return null (see the file header of
	// core/handler.go).
	reports := make([]FeedbackReport, 0, len(recs))
	for _, rec := range recs {
		reports = append(reports, feedbackReportOf(rec))
	}
	return FeedbackPage{Reports: reports, Total: total, Limit: q.Limit, Offset: q.Offset}, nil
}

// sanitizeAuto builds the **level-one** payload.
//
// # What is collected
//
//	message      the error text shown to the user (a sentence from the backend, not user
//	             input)
//	browser/os   browser and OS summary, e.g. Chrome 120 / macOS
//	appVersion   frontend version (git sha)
//
// # ⚠️ What is deliberately not collected (each has a concrete reason)
//
//	Path          contains ids and can point to one specific conversation / agent. The user
//	              did not consent.
//	Recent        API call trail; it can reconstruct what this person was just doing.
//	ConsoleErrors may carry stack traces and variable values.
//	AgentID       a specific resource identifier inside this workspace.
//	Response body never collected, at any level (there is not even a field; see
//	              FeedbackInput).
//
// **The logid is not on this "not collected" list**: it is a string our own server
// generates, has no user input, and level one already knows who the actor is. SubmitFeedback
// writes it straight into FeedbackRecord.LogID (a first-class column, not in the payload), so
// you do not see it in this function.
//
// In other words: the level-one payload **contains no identifier that points to a specific
// business object**. It only answers "which browser, which version, which route, what error".
//
// Before adding a field here, ask: does it matter if an unrelated person sees this? If you
// cannot answer "it does not matter", it does not belong in level one.
func sanitizeAuto(in FeedbackInput) map[string]any {
	out := map[string]any{}
	// Assign each field explicitly; **no loops, no reflection, no copying the whole map**.
	// Those would let any new field flow in automatically, and the whole value of this
	// function is "only what is listed here gets stored".
	putIfNotEmpty(out, "message", scrubText(truncate(in.Message, maxFeedbackTextLen)))
	putIfNotEmpty(out, "appVersion", truncate(strings.TrimSpace(in.AppVersion), maxFeedbackReasonLen))
	// The endpoint that failed. **Collected at both levels**; see FeedbackInput.APIRoute for
	// the reason: RoutePattern is the page route, and level one does not collect Recent, so
	// before this a level-one record could not show which endpoint failed.
	//
	// ⚠️ **The backend strips ids again** (same rule as route_pattern): this endpoint is
	// public, the caller is not necessarily our frontend, and a path with real ids breaks
	// aggregation.
	putIfNotEmpty(out, "apiRoute",
		stripRouteIDs(stripQuery(truncate(in.APIRoute, maxFeedbackPathLen))))
	putIfNotEmpty(out, "apiMethod", truncate(strings.TrimSpace(in.APIMethod), 10))
	return out
}

// sanitizeUser builds the **level-two** payload: everything in level one, plus what the
// user's click allows.
//
// # What else is collected
//
//	path           full path (with ids): "which conversation had the problem" is where
//	               debugging starts
//	agentId        current agent
//	recent         the last few API calls (method + path pattern + status + code + logid)
//	consoleErrors  first lines of the last few console errors
//
// (The server logid is **collected at both levels** and is a first-class column, not in the
// payload, so it is not in this table.)
//
// # ⚠️ What is still never collected
//
//	token / cookie / API Key / Authorization — the fields do not even exist (not listed in
//	                                           FeedbackInput)
//	API response bodies                      — same as above
//	user message text                        — same as above. **This is deliberate**:
//	  conversation content is user data, and "reporting a load failure" does not need it.
//	  Collecting it would need a separate, clearly stated reason, shown to the user.
//	query parameters                         — identity really was put in ?actor= in the past
func sanitizeUser(in FeedbackInput) map[string]any {
	out := sanitizeAuto(in)

	// Full path: level two only. The query is still cut off (see the comment on stripQuery).
	putIfNotEmpty(out, "path", scrubText(stripQuery(truncate(in.Path, maxFeedbackPathLen))))
	putIfNotEmpty(out, "agentId", truncate(strings.TrimSpace(in.AgentID), maxFeedbackReasonLen))
	// ⚠️ **The top-level logId is not here**: it is a first-class column (FeedbackRecord.LogID),
	// **collected at both levels**, and set by SubmitFeedback before the split. The ones in
	// recent below are **each call's own** logid, not the same thing as the top-level one (the
	// top-level one is for "this error", those are for "the few earlier failures").

	// Recent API calls. **Rebuilt field by field**, not by putting FeedbackCall in directly:
	// that would make any field later added to FeedbackCall get stored automatically.
	if calls := in.Recent; len(calls) > 0 {
		if len(calls) > maxFeedbackListLen {
			// Keep the **last** few: the most recent ones are most related to the failure.
			calls = calls[len(calls)-maxFeedbackListLen:]
		}
		items := make([]map[string]any, 0, len(calls))
		for _, c := range calls {
			item := map[string]any{
				"method": truncate(strings.TrimSpace(c.Method), 10),
				// Strip ids here too: ids in API paths are the same kind of thing as in routes.
				"path":   stripRouteIDs(stripQuery(truncate(c.Path, maxFeedbackPathLen))),
				"status": c.Status,
				"code":   truncate(strings.TrimSpace(c.Code), maxFeedbackReasonLen),
			}
			// Likewise, the logid is only added when non-empty: the calls that hit network
			// errors never had one. Mixing in empty strings would hide "which calls actually
			// reached the server", and that is the whole value of this trail in the
			// network_error case.
			if v := truncate(strings.TrimSpace(c.LogID), maxFeedbackReasonLen); v != "" {
				item["logId"] = v
			}
			items = append(items, item)
		}
		out["recent"] = items
	}

	if errs := in.ConsoleErrors; len(errs) > 0 {
		if len(errs) > maxFeedbackListLen {
			errs = errs[len(errs)-maxFeedbackListLen:]
		}
		items := make([]string, 0, len(errs))
		for _, e := range errs {
			if v := scrubText(truncate(strings.TrimSpace(e), maxFeedbackDetailLen)); v != "" {
				items = append(items, v)
			}
		}
		if len(items) > 0 {
			out["consoleErrors"] = items
		}
	}
	return out
}

// putIfNotEmpty writes the value only when it is non-empty.
//
// Skip empty values instead of writing empty strings: the payload is jsonb, a missing key is
// cleaner than an empty one, and "the frontend did not send this field" vs "it sent an empty
// string" are two different things when debugging.
func putIfNotEmpty(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

// feedbackUserAgent joins the browser and OS summary into one column.
//
// One string rather than two columns: this column is used for `group by` ("this error only
// happens on Safari"), and that judgment needs the combination. Two columns would mean
// grouping by two fields, with no benefit.
func feedbackUserAgent(in FeedbackInput) string {
	browser := truncate(strings.TrimSpace(in.Browser), 60)
	os := truncate(strings.TrimSpace(in.OS), 60)
	switch {
	case browser != "" && os != "":
		return browser + " / " + os
	case browser != "":
		return browser
	default:
		return os
	}
}

// feedbackDedupKey computes the dedup key for level-one automatic records.
//
// key = sha256(actor | route_pattern | error_code | http_status | ten-minute bucket).
//
// # Why hash instead of joining the parts into a plain-text key
//
// A plain-text key would write the email into a **column with a unique index**, making it a
// second place that stores emails (the actor column already does), and it would also show up
// in unique-constraint error messages and slow query logs. After hashing, the column has no
// readable information, and uniqueness is just as strong.
//
// # Why http_status is also in the key
//
// On the same route, "403" and "500" are two different problems. Without it in the key, the
// one that shows up first during an outage would swallow the later one, and the later one may
// be the real cause.
//
// # The bucket comes from rounding, not from "when was the last one"
//
// `unix / 600` is stateless: two instances compute the same bucket for the same moment.
// Reading the last record's time to check "is it inside the window" means read-then-write,
// which will miss under multiple instances (both see "nothing in the window").
// The cost is that a burst crossing a bucket boundary produces 2 rows: bounded, and it does
// not affect the judgment "this error is happening".
func feedbackDedupKey(rec persistmodel.FeedbackRecord, now time.Time) string {
	bucket := now.Unix() / int64(feedbackDedupWindow.Seconds())
	// Use \x00 as the separator: it cannot appear in any part, so ("a|b", "c") and
	// ("a", "b|c") can never collide into the same key.
	raw := strings.Join([]string{
		rec.Actor,
		rec.RoutePattern,
		rec.ErrorCode,
		fmt.Sprint(rec.HTTPStatus),
		fmt.Sprint(bucket),
	}, "\x00")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// feedbackActor returns the current logged-in user.
//
// It returns 400, not 401/403, exactly like onboardingActor / integration: these two routes
// already sit behind auth.Protect, so reaching here without an identity means the caller did
// not come through HTTP (a test or an internal call), which is an argument problem. The real
// "not logged in" case gets a 401 from the middleware further out.
func feedbackActor(ctx context.Context) (string, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return "", fmt.Errorf("%w: 需要先登录", ErrInvalidArgument)
	}
	// Lowercase once more (defense in depth, same as onboardingActor): case mismatch would
	// give the same person's feedback two dedup keys, so per-person rate limiting fails.
	return strings.ToLower(actor), nil
}

// feedbackReportOf converts the stored form into the response form.
//
// **DedupKey is not returned**: it is an implementation detail, and it is predictable.
// Exposing it would tell others how to build a report that collides with an existing record,
// hiding their feedback inside someone else's.
func feedbackReportOf(rec persistmodel.FeedbackRecord) FeedbackReport {
	payload := rec.Payload
	if payload == nil {
		// Make sure it is non-nil: a nil map encodes as null, and the caller iterates it
		// directly.
		payload = map[string]any{}
	}
	return FeedbackReport{
		ID:           rec.ID,
		Level:        rec.Level,
		Actor:        rec.Actor,
		WorkspaceID:  rec.WorkspaceID,
		RoutePattern: rec.RoutePattern,
		HTTPStatus:   rec.HTTPStatus,
		ErrorCode:    rec.ErrorCode,
		ErrorKind:    rec.ErrorKind,
		UserAgent:    rec.UserAgent,
		LogID:        rec.LogID,
		Payload:      payload,
		CreatedAt:    apimodel.FormatISO(rec.CreatedAt),
		LastSeenAt:   apimodel.FormatISO(rec.LastSeenAt),
		Occurrences:  rec.Occurrences,
	}
}

// truncate cuts by **character** (rune), not by byte.
//
// Cutting by byte splits a multi-byte character in half, which is invalid UTF-8 when stored:
// PG text/jsonb columns fail **at write time** with `22021 invalid byte sequence`. That is a
// 500, and the error points to encoding rather than "this text is too long". skill_files hit
// the same kind of problem (see core/README.md).
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// stripQuery cuts the query and fragment off a URL.
//
// **This is required**: the query may contain credentials. This is not hypothetical: this
// repo's SSE subscription once really put identity in `?actor=` (see the comment in
// web/src/data/http.ts), and any feature that "reports the current URL" takes such
// parameters along.
//
// The fragment is cut too: what follows `#` belongs to the frontend and may also carry
// identifiers.
func stripQuery(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return s[:i]
	}
	return s
}

// stripRouteIDs replaces path segments that look like ids with placeholders.
//
// # Why the backend strips them again (the frontend already did)
//
// Three reasons; the third is the hard one:
//
//  1. the frontend may get it wrong (a route level added, a parameter renamed);
//  2. this endpoint is a public HTTP endpoint, and the caller is not necessarily our frontend;
//  3. **a route_pattern with ids silently breaks two things**: the dedup key differs for
//     every row (rate limiting fails), and `group by route_pattern` becomes a table of tens
//     of thousands of rows with count 1 each (aggregation is meaningless).
//     Neither reports an error.
//
// The rule is the **shape of the segment**, not its position: opaque ids with an underscore
// prefix (ws_x / agent_x / run_x / sesn_x / fb_x), all-digit segments, and long hex strings
// that look like hashes.
//
// Short all-letter segments are deliberately **not** stripped: those are route literals
// (agents / settings / chat). Stripping them would make all routes look the same, and the
// column would become useless.
func stripRouteIDs(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(stripQuery(p), "/")
	for i, seg := range parts {
		if looksLikeID(seg) {
			parts[i] = "{id}"
		}
	}
	return strings.Join(parts, "/")
}

// looksLikeID reports whether a path segment is an identifier.
func looksLikeID(seg string) bool {
	if seg == "" {
		return false
	}
	// Leave existing placeholders alone (like {key} / {runId} computed by the frontend).
	if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
		return false
	}
	// Ids in this repo all have the shape `prefix_random` (domain.nextID / Service.nextID).
	// Match the underscore instead of listing prefixes: adding a new resource type does not
	// require coming back here.
	if strings.Contains(seg, "_") {
		return true
	}
	// **Hyphen + long digit run**: session ids look like `sesn-20260101000003-ex003`, with no
	// underscore at all.
	//
	// ⚠️ This hole was found only end to end; it is not tidiness. Without it, level-one
	// reports stored route_pattern with the **real session id**: that leaks "which
	// conversation this person was just in" (and level one is exactly the level without user
	// consent), and it also makes the field useless for aggregation: a different value per
	// conversation, so "which page has the most errors" cannot be answered.
	//
	// Neither unit tests nor e2e catch it, because the mock seed uses ids like `run_1` with an
	// underscore, while production uses ids starting with `sesn-`. **The same field has
	// different shapes in the two environments.**
	//
	// The rule requires "a run of ≥8 digits" (a date or timestamp), not just a hyphen: the
	// latter would also strip **workspace keys and route literals** like `ark-agent` and
	// `skill-hub`, which ruins the field (all routes look the same).
	if strings.Contains(seg, "-") && hasLongDigitRun(seg) {
		return true
	}
	// All digits: page numbers, timestamps, auto-increment ids.
	allDigit := true
	for _, r := range seg {
		if r < '0' || r > '9' {
			allDigit = false
			break
		}
	}
	if allDigit {
		return true
	}
	// Long hex strings (≥ 16 chars): hashes, UUIDs without hyphens.
	// The threshold of 16 avoids hitting real words: no English word has 16 letters all in
	// the a-f range.
	if len(seg) >= 16 && isHexLike(seg) {
		return true
	}
	// Standard UUID form (with hyphens, 36 characters).
	if len(seg) == 36 && strings.Count(seg, "-") == 4 {
		return true
	}
	return false
}

// hasLongDigitRun reports whether the string contains a run of ≥8 consecutive digits (a date
// or timestamp).
//
// Used by looksLikeID to detect "hyphen-style ids": `sesn-20260101000003-ex003` matches,
// while route literals and workspace keys like `ark-agent` / `skill-hub` do not.
func hasLongDigitRun(s string) bool {
	run := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			run++
			if run >= 8 {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// isHexLike reports whether the string consists only of hex characters and hyphens.
func isHexLike(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// scrubText redacts things in text that look like credentials.
//
// # ⚠️ This is defense in depth, not the main line of defense
//
// The main defense is the allowlist (FeedbackInput only lists the fields we want). This layer
// targets **credentials embedded in values**: half a JWT pasted into an error message, a URL
// carrying ?token=xxx, a console error that printed the whole request header.
//
// The idea comes from shared/infra/shelltools/scrub.go, but **that one cannot be reused**:
// it redacts "the token we injected ourselves" (known value, exact replacement), while this
// layer faces "unknown values, can only guess by shape". The rules are completely different,
// and forcing them into one function would blur both.
//
// Guessing by shape will both miss things and hit wrong things, so it is only the last
// layer. The real guarantee is the sentence above:
// **we only store fields we listed ourselves.**
func scrubText(s string) string {
	if s == "" {
		return ""
	}
	out := s
	// JWT: three base64url parts separated by dots, the first starting with eyJ (base64 of
	// {"alg). This shape is very distinctive, so false hits are very unlikely.
	out = jwtPattern.ReplaceAllString(out, credentialRedacted)
	// This repo's own API key prefix (amak_). One reason it exists is "so scanners can
	// recognize it" (see KeyPrefix in auth/apikey.go); this is one use of that.
	out = apiKeyPattern.ReplaceAllString(out, credentialRedacted)
	// Sensitive query parameters in URLs: ?token=xxx / &access_token=xxx / ?actor=xxx.
	// Redact only the value and keep the key: "this request carried a token parameter" is
	// useful debugging info.
	out = sensitiveParamPattern.ReplaceAllString(out, "$1=$2"+credentialRedacted)
	// Values of headers like `Authorization: Bearer xxx`.
	out = bearerPattern.ReplaceAllString(out, "$1 "+credentialRedacted)
	return out
}
