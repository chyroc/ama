"""Rebuild a throwaway schema from the .sql files, then read its catalog.

The question gen-schema-outline.py answers is: **"after this DDL runs, what do the tables
actually look like?"** This module is the only implementation of that question.

# Why we must really run it instead of parsing the .sql text

Only PostgreSQL itself gives the canonical form of defaults, sequences, constraints and
indexes (`format_type`, `pg_get_indexdef`). A hand-written parser will sooner or later
disagree with the real shape. Really running the DDL is the only way that misses nothing:
PostgreSQL itself is the parser.

# ⚠️ Why rebuild every time instead of reading a long-lived schema

A **long-lived** schema only keeps up with the .sql files when someone remembers to reload
it, so **it goes stale silently**: you change the DDL, run the generator, and the outline
still shows the old types, while the script exits normally and reports nothing.

Rebuilding moves "remember to reload" from a person's job to the script's job. The cost is
about 2 extra seconds per run.

# ⚠️ Why not read the production database

Production answers **a different question** ("does production match the code?"), not this
one ("what shape does this DDL produce?"). Reading production also needs network access to
the production database and a read-only account, so **it cannot run in CI**.

# Which database

It connects through `docker exec <container> psql`. By default the container is the one
found by `docker compose ps -q postgres`. Override with environment variables:

  AMA_SCHEMA_PG_CONTAINER  container name (default: the postgres service container in docker compose)
  AMA_SCHEMA_PG_DB         database name (default ama)
  AMA_SCHEMA_PG_USER       user (default ama)

The script only creates tables in the throwaway schema (SCRATCH) and drops it when done.
It never touches public.
"""
import os
import re
import subprocess
import sys
from pathlib import Path

# Name of the throwaway schema used for rebuilding. It is dropped after each run and not
# left in the database: if left behind it becomes a long-lived schema that goes stale
# (exactly the trap described above).
SCRATCH = 'ama_gen_scratch'

def _compose_pg_container():
    # compose does not hard-code container_name (the name carries the project prefix),
    # so look it up by service name.
    try:
        out = subprocess.run(['docker', 'compose', 'ps', '-q', 'postgres'], cwd=REPO,
                             capture_output=True, text=True, check=True).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        out = ''
    if not out:
        sys.exit('找不到 PG 容器：先 docker compose up -d postgres，或设 AMA_SCHEMA_PG_CONTAINER')
    return out.splitlines()[0]


CONTAINER = os.environ.get('AMA_SCHEMA_PG_CONTAINER') or _compose_pg_container()
DB = os.environ.get('AMA_SCHEMA_PG_DB') or 'ama'
USER = os.environ.get('AMA_SCHEMA_PG_USER') or 'ama'

REPO = Path(__file__).resolve().parent.parent
SCHEMA_DIR = REPO / 'server/repository/schema'

# Use 0x1f (Unit Separator) as the field separator: SQL defaults and comments can contain
# any character, so a comma or pipe separator would sooner or later collide with the content.
SEP = '\x1f'


def _psql(args, stdin=None):
    return subprocess.run(
        ['docker', 'exec', '-i', CONTAINER, 'psql', '-U', USER, '-d', DB,
         '-v', 'ON_ERROR_STOP=1'] + args,
        input=stdin, capture_output=True, text=True)


def query(sql):
    """Run one SELECT and return list[list[str]].

    ⚠️ **search_path is deliberately not set here**; every query below spells out the full
    `schema.table`. With `set search_path to x; select ...`, psql also prints the result of
    SET (a line `SET`) into the -At output, so the first line is taken as a malformed data
    row. The symptom is `not enough values to unpack` while unpacking, and the error points
    at the parsing code, not at "there is an extra SET line".
    """
    r = _psql(['-At', '-F', SEP, '-c', sql])
    if r.returncode != 0:
        die(f'查询失败：\n{r.stderr.strip()}')
    return [ln.split(SEP) for ln in r.stdout.strip('\n').split('\n') if ln]


def die(msg):
    print(f'\n✗ {msg}', file=sys.stderr)
    sys.exit(1)


def check_db():
    r = subprocess.run(['docker', 'exec', CONTAINER, 'true'],
                       capture_output=True, text=True)
    if r.returncode != 0:
        die(f'连不上容器 {CONTAINER}。\n'
            f'  起库：在仓库根跑 `docker compose up -d`，等 `docker compose ps` 显示 healthy')


def order():
    """Return all .sql file names in the apply order defined in schema.go.

    ⚠️ **The order must come from schema.go, not from sorted(glob).** There are two real
    cross-file dependencies (workspaces → workspace_members, and the two attachments tables
    → files); sorting by file name fails right away with `relation does not exist`. The
    list in schema.go is the only source of truth for that order.
    """
    src = (SCHEMA_DIR / 'schema.go').read_text()
    names = []
    for var in ('coreOrder', 'authOrder', 'feishuOrder'):
        m = re.search(r'var ' + var + r' = \[\]string\{(.*?)\n\}', src, re.S)
        if not m:
            die(f'schema.go 里找不到 {var}——它的写法变了？本脚本靠正则解析它取下发顺序。')
        names += re.findall(r'"([a-z0-9_]+\.sql)"', m.group(1))

    on_disk = {p.name for p in SCHEMA_DIR.glob('*.sql')}
    missing = [n for n in names if n not in on_disk]
    if missing:
        die(f'schema.go 的顺序列表里有文件不存在：{missing}')
    extra = sorted(on_disk - set(names))
    if extra:
        die(f'这些 .sql 没进 schema.go 的顺序列表（加表要改两处，见该目录 README）：{extra}')
    return names


def rebuild(files=None, passes=1, schema=SCRATCH):
    """Load the .sql files into a brand-new schema.

    When files is None, load all of them (in apply order); when a list is given, load only
    those (used by the "only run create" path). With passes>1 the whole set runs several
    times: **whether re-running is safe can only be tested by running it a second time**.
    """
    check_db()
    names = order() if files is None else files
    r = _psql(['-c', f'drop schema if exists {schema} cascade; create schema {schema}'])
    if r.returncode != 0:
        die(f'重建 schema 失败：\n{r.stderr.strip()}')

    for p in range(passes):
        for name in names:
            body = (SCHEMA_DIR / name).read_text()
            r = _psql(['-q'], stdin=f'set search_path to {schema};\n' + body)
            if r.returncode != 0:
                die(f'第 {p + 1} 遍下发 {name} 失败：\n{r.stderr.strip()}')
    return names


def drop(schema=SCRATCH):
    _psql(['-c', f'drop schema if exists {schema} cascade'])


def columns(table, schema=SCRATCH):
    """Return [(name, type, not-null bool, default, identity)], ordered by attnum.

    The schema prefix in defaults is stripped: a sequence's nextval default looks like
    `nextval('ama_gen_scratch.agents_ord_seq'::regclass)`. Keeping the throwaway schema name
    would make the output differ on every rebuild, and that name means nothing for a real
    database.
    """
    rows = query(f"""
        select a.attname,
               format_type(a.atttypid, a.atttypmod),
               a.attnotnull,
               coalesce(pg_get_expr(d.adbin, d.adrelid), ''),
               a.attidentity
          from pg_attribute a
          left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
         where a.attrelid = '{schema}.{table}'::regclass
           and a.attnum > 0 and not a.attisdropped
         order by a.attnum""")
    out = []
    for name, typ, notnull, dflt, ident in rows:
        out.append((name, typ, notnull == 't',
                    dflt.replace(f'{schema}.', ''), ident))
    return out


def indexes(table, schema=SCRATCH):
    return query(f"""
        select c.relname, i.indisprimary, i.indisunique,
               pg_get_indexdef(i.indexrelid)
          from pg_index i join pg_class c on c.oid = i.indexrelid
         where i.indrelid = '{schema}.{table}'::regclass
         order by i.indisprimary desc, c.relname""")


def constraints(table, schema=SCRATCH):
    """Primary key and unique constraints, used to decide which can be inlined into create table."""
    return query(f"""
        select conname, contype, pg_get_constraintdef(oid),
               array_length(conkey, 1)
          from pg_constraint
         where conrelid = '{schema}.{table}'::regclass
           and contype in ('p', 'u')
         order by contype, conname""")
