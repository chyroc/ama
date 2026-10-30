-- ┌── Table outline: suggestion_replies ───────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id             text     not null
-- │ suggestion_id  text     not null
-- │ body           text     not null
-- │ author         text     not null
-- │ is_staff       boolean  not null default false
-- │ origin         text     not null default ''::text
-- │ created_at     text     not null
-- │ updated_at     text     not null
-- │ ord            bigint   not null (sequence)
-- │
-- │ primary key suggestion_replies_pkey
-- │      (id)
-- │ index suggestion_replies_sug_idx
-- │      (suggestion_id, ord)
-- └────────────────────────────────────────────────────────────────────────────

create table if not exists suggestion_replies (
    id text primary key,

    suggestion_id text not null,

    body text not null,
    author text not null,

    -- ⚠️ **This is a stored real column, not computed at read time.**
    --
    -- Computing it (comparing author with the current allow list) has a hidden consequence: the allow
    -- list changes; after someone moves to another role and is removed from the list, the replies they
    -- wrote as the platform team would **retroactively** become user replies, and the meaning of the
    -- whole conversation history would drift. At the moment of writing they really were the platform
    -- team; that is a fact, and facts should not change with config.
    --
    -- The same the other way: follow-ups a newly added person wrote as a user before should not
    -- become official answers.
    is_staff boolean not null default false,

    -- Who produced it. Empty string = written by a person (**all of them today**), 'agent' = machine
    -- output.
    --
    -- **Reserved for "let an agent handle it automatically later"**; no non-empty value is written
    -- this time. ⚠️ **A different purpose from issues.origin**: there origin is in a unique index and
    -- decides the dedup scope; here it **takes part in no unique constraint** and is only used in the
    -- UI to tell "this was answered by a bot". Copying the inference from there ("with origin there
    -- should be fingerprint dedup") is wrong; see the file header.
    origin text not null default '',

    created_at text not null,
    -- **Replies can be edited**, so it also has updated_at. The frontend uses it to show "edited".
    updated_at text not null,

    -- Replies are listed in insertion order, **oldest first** (a conversation is read top down), the
    -- opposite of suggestions' newest-first order.
    ord bigserial not null
);

create index if not exists suggestion_replies_sug_idx on suggestion_replies (suggestion_id, ord);
