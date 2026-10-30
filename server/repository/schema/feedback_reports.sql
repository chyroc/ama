-- ┌── Table outline: feedback_reports ─────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id             text                      not null
-- │ level          text                      not null
-- │ actor          text                      not null
-- │ workspace_id   text
-- │ route_pattern  text                      not null default ''::text
-- │ http_status    integer                   not null default 0
-- │ error_code     text                      not null default ''::text
-- │ error_kind     text                      not null default ''::text
-- │ payload        jsonb                     not null default '{}'::jsonb
-- │ user_agent     text                      not null default ''::text
-- │ created_at     timestamp with time zone  not null
-- │ last_seen_at   timestamp with time zone  not null
-- │ occurrences    integer                   not null default 1
-- │ dedup_key      text
-- │ log_id         text
-- │
-- │ primary key feedback_reports_pkey
-- │      (id)
-- │ index feedback_reports_code_created_idx
-- │      (error_code, created_at DESC)
-- │ index feedback_reports_created_idx
-- │      (created_at DESC)
-- │ unique feedback_reports_dedup_idx
-- │      (dedup_key)
-- │ index feedback_reports_level_created_idx
-- │      (level, created_at DESC)
-- └────────────────────────────────────────────────────────────────────────────

-- Problem feedback.
--
-- # Two levels of feedback live in the same table, told apart by level
--
--   auto  stored **automatically** when an error happens, without the user clicking anything. So only
--         non-sensitive metadata is collected.
--   user  the user clicked "report this problem". **That click is their consent**, so more can be
--         collected.
--
-- Splitting into two tables was thought through and is not worth it: the read endpoint must show on a
-- timeline "this error was recorded automatically 47 times, 2 of which the user also reported", and
-- with two tables it would need a union of two different shapes and then a sort, while level is just
-- an enum column. **The difference in collection boundary is on the write side (the Service's allow
-- list), not in the table structure.**
--
-- # Why payload is jsonb instead of real columns
--
-- The same rule as the runs table: **what can be queried/aggregated becomes real columns, the rest goes
-- into doc**. The read endpoint must filter and page by level / error_code / time (otherwise
-- "automated improvement" cannot pull the data), so those four are real columns + indexes; while the
-- details collected at the second level (the last few endpoint calls, console errors) change shape as
-- the frontend evolves, and **are only read one by one during manual debugging**; no query filters by
-- them.
--
-- ⚠️ The content of payload is **not** "the raw JSON the frontend sent". It is **rebuilt** by the
-- Service from an allow list (sanitize* in core/feedback.go), and extra fields the frontend sent cease
-- to exist the moment it is decoded. See the section "an allow list, not a deny list" at the top of
-- that file.
--
-- # ⚠️ Level-one automatic records must be deduplicated, or this feature DoSes itself
--
-- One broken endpoint can make **one user** produce hundreds of errors in a minute (page remounts, SSE
-- reconnects, polling), and the service has multiple instances and possibly hundreds of users. Without
-- dedup, the first real failure would flood this table, and then not only is the table big, **the read
-- endpoint becomes useless too**: no other problem can be found among hundreds of thousands of the
-- same error.
--
-- Dedup relies on the unique constraint on `dedup_key` + `on conflict do update ... occurrences + 1`,
-- i.e. "unique constraint + ON CONFLICT, let the database decide" from that table in CLAUDE.md. **Do
-- not query and then write in Go**: that is TOCTOU and is bound to miss under multi-instance
-- concurrency.
--
-- dedup_key is computed from (actor, route_pattern, error_code, http_status, time bucket), where the
-- time bucket is `floor(unix / 600)`: **rounding instead of a sliding window**, because rounding is
-- stateless: two instances compute the same bucket for the same moment without coordinating. A
-- sliding window would first read "when was the previous one" and then decide, which brings back
-- query-then-write.
--
-- The cost, stated plainly: a burst across a bucket boundary produces 2 rows (one at 10:09:59 and one
-- at 10:10:01). This cost is **bounded** (at most 1 row per key per bucket), while the precision a
-- sliding window would buy does nothing for the judgment "this error is happening".
--
-- # Level two is deliberately not deduplicated
--
-- Two clicks by the user should be two rows. **Losing the one the user actively submitted is much worse
-- than storing one extra**: they would see "received, thanks" while the database has nothing, and the
-- whole value of this feature is that the feedback really arrived. So the user level's dedup_key is
-- NULL, and NULLs are never equal to each other in a unique index (PG semantics), so it naturally
-- takes no part in dedup, without any branch written for it.

create table if not exists feedback_reports (
    -- The id of this feedback (an opaque string starting with fb_).
    id text primary key,

    -- 'auto' (automatic record) or 'user' (submitted by the user).
    --
    -- **No constraint enum**: the value is validated by the Service (anything unrecognized is
    -- rejected), and a check constraint here would only turn "add a third level later" into a DDL
    -- migration.
    level text not null,

    -- Who hit the problem. **The user's email, i.e. the Actor ID**, already lower-cased.
    --
    -- Not nullable: both routes are behind auth.Protect, and nothing gets here without an identity.
    actor text not null,

    -- The workspace the user was in when the problem happened, **nullable**.
    --
    -- Nullable is required: errors may happen outside workspaces (the workspace list page, preferences,
    -- newcomers who have not joined any workspace). Making it not null would make feedback from those
    -- places **impossible to report at all**, and those are exactly where newcomers most often get stuck.
    --
    -- What is stored is the workspace id (ws_xxx), not the key: the id is an immutable primary key, and
    -- users can change the key.
    workspace_id text,

    -- The route where the problem happened, **a pattern string with ids already removed**, like
    -- /w/{key}/chat/{sessionId}.
    --
    -- ⚠️ **Both levels store the pattern string, not the real path.** Level one because there is no user
    -- consent; at level two the full path (with ids) is stored separately in payload. This split has
    -- two benefits:
    --
    --   * this column has low cardinality, so `group by route_pattern` makes sense, and that is exactly
    --     the first query "automated improvement" wants (which page errors most). With real ids, every
    --     row would be a unique value, and aggregating would give a table of tens of thousands of rows.
    --   * the dedup key uses it, and paths with ids never repeat, so dedup would stop working entirely.
    route_pattern text not null default '',

    -- HTTP status code; 0 when unavailable (e.g. a network failure where the request was never sent).
    --
    -- 0 instead of NULL: this column is part of the dedup key, and NULL is not equal to itself in
    -- equality comparisons, which would count "errors without a status code" as a new row every time.
    http_status int not null default 0,

    -- The backend error code (access_denied / not_found / network_error...); empty string when
    -- unavailable.
    --
    -- **Worth more for finding the cause than http_status**, so it has its own column and index: a 403
    -- has several causes, and code is a stable identifier given explicitly by the backend.
    error_code text not null default '',

    -- The frontend's error category (denied / other), from AsyncError.kind.
    error_kind text not null default '',

    -- The other details. **Rebuilt from an allow list**; see the file header and core/feedback.go.
    --
    -- The field sets of level one and level two here are **deliberately different** (level one only has
    -- a UA summary and the frontend version; level two also has the full path, recent endpoint calls,
    -- console errors).
    payload jsonb not null default '{}'::jsonb,

    -- A **summary** of the browser UA (like Chrome 120 / macOS), not the raw UA string.
    --
    -- The raw UA has fingerprinting value (it can identify a specific device), while debugging only
    -- needs "which browser on which system". It is a real column so it can be grouped by: "this error
    -- only appears on Safari" is a real cause to find.
    user_agent text not null default '',

    -- When this entry was first recorded.
    created_at timestamptz not null,

    -- The last time this entry was hit. Updated on dedup, so the auto level looks at the range
    -- [created_at, last_seen_at]. For the user level the two are equal.
    last_seen_at timestamptz not null,

    -- How many times this entry was hit. 1 when first stored, +1 on dedup.
    --
    -- **With it you save rows without losing the magnitude**: "this error happened 312 times" and "it
    -- happened" are completely different for judging priority, and counting rows after dedup would
    -- always under-report.
    occurrences int not null default 1,

    -- The dedup key, **non-null for the auto level, NULL for the user level** (see "Level two is
    -- deliberately not deduplicated" in the file header).
    dedup_key text,

    -- The logid of the server request (the request id response header). **Collected at both levels**,
    -- nullable.
    --
    -- A real column instead of inside payload: this column is used for "look up the feedback by the
    -- logid a user gave" and, the other way, "search the service logs for that request from a
    -- feedback", both equality lookups. A key inside jsonb cannot do this (it would need ->> to extract
    -- and compare, without an index).
    --
    -- Level one also collects it because it needs no user consent: this string is generated by our own
    -- server, contains none of the user's input, and is not a credential, while the actor column
    -- already stores who it is. Level one needs it most: those records have no user beside them to
    -- explain "what was I doing", and only a logid can tie them back to the scene.
    --
    -- ⚠️ **Nullable, and it must allow empty**: in failures like network_error the request **never
    -- reached the server**, the HTTP middleware that generates the logid never ran, and the server logs
    -- have not a single line of that request. Then the frontend gives an empty string, and storing
    -- NULL is the only honest way to express it.
    --
    -- So **do not give it not null / default**: that would force a fake value, and a logid that finds
    -- nothing wastes more debugging time than no logid (same as LogIDDefault "-" forbidden in
    -- core/observe.go).
    --
    -- ⚠️ It **must never go into dedup_key**: logids are unique per request, so with it in the key
    -- every entry would be a new row, and rate limiting would stop working entirely (with no error; the
    -- table just floods). feedbackDedupKey does not read it. **This matters even more now that level
    -- one also collects it**, because level one is exactly what goes through dedup.
    log_id text
);

-- Dedup is decided by this unique index.
--
-- ⚠️ **It must be UNIQUE**; it is the implementation of the whole rate limiting mechanism:
-- `on conflict (dedup_key)` only works with it. Removing it gives no error; every error just inserts
-- a new row, i.e. rate limiting silently stops working.
--
-- NULLs take no part (PG unique indexes allow several NULLs), so user-level rows are naturally not
-- constrained by it, with no need for a partial index or a branch in Go.
create unique index if not exists feedback_reports_dedup_idx on feedback_reports (dedup_key);

-- The main query of the read endpoint: paging by time descending, optionally filtered by level /
-- error_code.
--
-- The descending order is in the index instead of only (created_at): that query is **always**
-- descending (the newest problems matter most), so it can follow the index order directly without an
-- extra sort.
create index if not exists feedback_reports_created_idx on feedback_reports (created_at desc);

-- After filtering by level it still needs time descending, so a composite index instead of a single
-- column.
--
-- With only (level), "see the last 50 user-submitted feedbacks" would first take all user-level rows
-- and then sort, and that kind is exactly what is viewed most (feedback written by people carries more
-- information than automatic records).
create index if not exists feedback_reports_level_created_idx on feedback_reports (level, created_at desc);

-- Group by error code: "how many times has this error_code appeared recently" is the entry query of
-- automated improvement.
create index if not exists feedback_reports_code_created_idx on feedback_reports (error_code, created_at desc);
