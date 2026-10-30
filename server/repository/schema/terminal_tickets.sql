-- ┌── Table outline: terminal_tickets ─────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ ticket      text                      not null
-- │ run_id      text                      not null
-- │ actor       text                      not null
-- │ expires_at  timestamp with time zone  not null
-- │ used_at     timestamp with time zone
-- │ created_at  timestamp with time zone  not null default now()
-- │
-- │ primary key terminal_tickets_pkey
-- │      (ticket)
-- │ index terminal_tickets_expires_idx
-- │      (expires_at)
-- └────────────────────────────────────────────────────────────────────────────

-- One-time entry tickets for the terminal WebSocket.
--
-- # Why this table must exist: WebSocket cannot carry credentials
--
-- The terminal uses WebSocket, and the WebSocket entry may not be on the same domain as the main site
-- (some gateways have a separate entry for WebSocket). Then both existing credential channels **fail
-- at once**:
--
--   - **The cookie does not get there.** `ama_session` is set without a `Domain` attribute, so per
--     RFC 6265 it is host-only, and the browser only sends it to the main site's domain.
--   - **The `Authorization` header cannot be used either.** The browser's `new WebSocket()` **does
--     not support custom request headers**; this is a WHATWG spec limit with no switch.
--
-- The only place left is the URL query. And **long-lived credentials must never go into a query**: it
-- ends up in browser history, gateway access logs, and Referer. This repository already set the same
-- rule for the `ama` CLI ("deliberately no --token parameter, because the command line is recorded as
-- is in RunEvent forever").
--
-- So it uses "short-lived one-time tickets": exchange a ticket through a same-origin endpoint (with
-- the cookie, through authz); the ticket can only be redeemed once, is only valid for one run, and
-- expires in tens of seconds. **The cost of a leak is pushed to the minimum**: someone who picks up a
-- used ticket can do nothing with it.
--
-- # Why not widen the cookie's Domain
--
-- Changing to `Domain=.<parent domain>` is one line, but it would send `ama_session` to **every
-- service under the parent domain**. Any subdomain being compromised would leak the AMA login, and it
-- is **a one-way door**: narrowing it again after widening would log out every online session.
-- Trading a ticket that can only open one terminal for "the cookie scope does not change at all" is
-- worth it.
--
-- # Why it must be stored in the database, not in the process
--
-- ⚠️ **The instance issuing the ticket and the instance using it are almost never the same.** The two
-- requests each land on a random instance through the load balancer. In a `map`, the hit rate is about
-- 1/number of instances, showing up as "the terminal sometimes opens and sometimes does not", exactly
-- the kind of failure the opening of CLAUDE.md warns about.
--
-- # Redemption must be atomic
--
-- The "one-time" property **relies entirely on one SQL statement**:
--
--	update terminal_tickets set used_at = now()
--	 where ticket = $1 and used_at is null returning run_id, actor
--
-- Selecting first and then updating would let the same ticket be redeemed twice under concurrency
-- (TOCTOU), which would undercut exactly why this table exists. The check is the concurrency case in
-- core/sandboxws_test.go.

create table if not exists terminal_tickets (
    -- The ticket value: a random, unguessable string (crypto/rand, 32 bytes hex).
    --
    -- A primary key instead of a normal column: the redeeming UPDATE locates by it, and "only one row
    -- per ticket" should be guaranteed by the database, not by the application's self-discipline.
    ticket text primary key,

    -- This ticket can only open the terminal of this one run.
    --
    -- ⚠️ **It must be recorded and checked on the WS side**. Without it, any valid ticket could open
    -- the terminal of **any** run: "one-time" would be right while "authorization scope" is lost, and
    -- the latter is the real security boundary (authz decides per run).
    run_id text not null,

    -- The identity at issue time (lower-cased email, i.e. the Actor).
    --
    -- The WS connection has no cookie, so **this is that connection's only source of identity**. All
    -- later operations go by it, and no identity the client claims is accepted.
    actor text not null,

    -- Expiry time. timestamptz instead of an ISO text string: it **is not in the HTTP contract**, only
    -- used by the database for the `where expires_at > now()` comparison, the second kind in this
    -- repository's "pick the column type by purpose" (see the timestamps section of CLAUDE.md).
    expires_at timestamptz not null,

    -- Redemption time. NULL = not used yet.
    --
    -- ⚠️ **A time instead of a boolean**: when something goes wrong we must be able to answer "when was
    -- this ticket used", while `used bool` only answers "was it used". Storing one more timestamp
    -- costs nothing.
    used_at timestamptz,

    -- Issue time, purely for debugging (together with used_at it shows how long the ticket was held).
    created_at timestamptz not null default now()
);

-- For expiry cleanup.
--
-- Tickets are short-lived rows produced often and void in tens of seconds; without cleanup this table
-- would grow forever. Cleanup runs
-- `delete from terminal_tickets where expires_at < now() - interval '1 hour'`, and that where needs
-- this index, otherwise every cleanup is a full table scan.
--
-- ⚠️ An hour of margin instead of deleting at expiry: `used_at` is useful for debugging ("who used this
-- ticket"), and deleting right at expiry would make exactly the ticket that just had a problem
-- impossible to find.
create index if not exists terminal_tickets_expires_idx on terminal_tickets (expires_at);
