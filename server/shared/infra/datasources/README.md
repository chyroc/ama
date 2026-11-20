# shared/infra/datasources — worker-side datasource tools (`query_sql` / `read_object`)

**The agent never holds a MySQL or TOS credential.** Two custom tools are declared to Ark
(`query_sql` / `read_object`) and executed by amad (the worker): the credentials live only in the
amad process, and neither the session container nor the Agent config contains a single character
of them.

```
Ark Agent (custom tool declared)
   │  agent.custom_tool_use {name:"query_sql", input:{sql:"..."}}
   ▼
amad's SessionToolRunner (core/workerhandle.go)
   │  CustomTools[name].Execute(ctx, input)   ← a fresh map per work item
   ▼
shared/infra/datasources
   ├─ sqlguard: read-only validation / LIMIT injection
   ├─ mysql: dedicated connection + SET SESSION transaction_read_only=1 → query → markdown table
   └─ tos: GetObject (Range reads one extra byte to detect truncation) → UTF-8 text
   │
   └─ audit lines go to the amad log (session_id / work_id / duration / rows / status)
```

## Why the worker side

Putting the credentials on the agent side (the Agent env or a config file in the sandbox) makes
three things worse at once:

1. **The agent can read them out.** It has bash: `env` / `cat ~/.my.cnf`, then one `curl` sends the
   credentials anywhere. It does not need to be malicious; a request to "send this data out" is
   enough.
2. **Plaintext ends up in the Agent definition.** Agent config gets copied, exported and read in
   the Ark console, which makes it a shared document with a database password in it.
3. **Queries leave no trace.** With credentials in the agent's hands, "who queried what and when"
   is whatever the agent decides to report.

With the credentials in amad, the model can only see two things: **an account that may only
SELECT** and **an object-store prefix it may only read**. There is nothing to escalate with; it
cannot even switch to another database.

That placement also buys the two properties that actually make these tools better than handing the
agent a MySQL client:

- **The server decides the limits**: rows, bytes, timeout and result size cannot be raised by
  model-supplied parameters (`max_rows` / `max_bytes` can only lower them);
- **One audit line per call**: session / work / SQL / rows / duration / status, in the amad log.

> ⚠️ **The tool layer is not a security boundary.** The boundary is the SELECT-only RDS account and
> the TOS read-only policy. See "Security boundary and known limitations".

## The two tools

| Tool | What it does | Parameters | Limits |
| --- | --- | --- | --- |
| `query_sql` | Runs **one** read-only SQL statement against the configured MySQL datasource and returns a markdown table | `sql` (required), `max_rows` (optional, can only lower) | rows `mysql.maxRows` (default 200, hard cap 1000), total timeout `mysql.timeout` (default 30s), result text hard cap 256KB |
| `read_object` | Reads **one** UTF-8 text object under the configured TOS bucket + prefix | `key` (required), `max_bytes` (optional, can only lower) | bytes `tos.maxBytes` (default 1MiB, hard cap 8MiB), timeout `tos.timeout` (default 20s); the text cap is `max_bytes` itself |

Allowed statements: `SELECT`, `WITH ... SELECT`, `SHOW`, `DESCRIBE` / `DESC`, `EXPLAIN ... SELECT`.
Only `SELECT` / `WITH` get an automatically appended `LIMIT <cap+1>` (the extra row is what makes
an honest "truncated" flag possible); `SHOW` / `DESCRIBE` / `EXPLAIN` do not accept LIMIT, so they
are left untouched.

## Configuration

### YAML (the `dataSources` section of `server/config.example.yaml`)

```yaml
dataSources:
  mysql:
    enabled: true
    host: mysql.example.com                 # hostname or IP
    port: 3306                              # empty = 3306
    database: example_db
    user: ro_user
    password: ""                            # leave empty, use AMA_DATA_MYSQL_PASSWORD
    maxRows: 200                            # empty = 200, hard cap 1000
    timeout: 30s                            # empty = 30s
  tos:
    enabled: true
    endpoint: https://tos-s3-cn-beijing.volces.com   # https:// is added when missing
    region: cn-beijing
    bucket: example-bucket
    prefix: example-prefix/                 # required; a trailing "/" is added when missing
    accessKey: ""                           # leave empty, use AMA_DATA_TOS_ACCESS_KEY
    secretKey: ""                           # leave empty, use AMA_DATA_TOS_SECRET_KEY
    maxBytes: 1048576                       # empty = 1MiB, hard cap 8MiB
    timeout: 20s                            # empty = 20s
    usePathStyle: false                     # empty = false (TOS's S3-compatible domain is virtual-hosted)
```

`enabled: false` (the default) means the two tools are not declared at all: the model cannot see
them, and the Ark Agent is byte-for-byte identical to what it was before this feature.

### Environment variables (higher priority than YAML)

| Variable | Field | Notes |
| --- | --- | --- |
| `AMA_DATA_MYSQL_ENABLED` | `dataSources.mysql.enabled` | boolean |
| `AMA_DATA_MYSQL_HOST` | `...mysql.host` | hostname or IP |
| `AMA_DATA_MYSQL_PORT` | `...mysql.port` | integer |
| `AMA_DATA_MYSQL_DATABASE` | `...mysql.database` | database name |
| `AMA_DATA_MYSQL_USER` | `...mysql.user` | read-only account |
| `AMA_DATA_MYSQL_PASSWORD` | `...mysql.password` | **use this in production**, never the YAML field |
| `AMA_DATA_MYSQL_MAX_ROWS` | `...mysql.maxRows` | integer, clamped to the hard cap 1000 |
| `AMA_DATA_MYSQL_TIMEOUT` | `...mysql.timeout` | Go duration, e.g. `30s` |
| `AMA_DATA_TOS_ENABLED` | `dataSources.tos.enabled` | boolean |
| `AMA_DATA_TOS_ENDPOINT` | `...tos.endpoint` | S3-compatible endpoint |
| `AMA_DATA_TOS_REGION` | `...tos.region` | signing region |
| `AMA_DATA_TOS_BUCKET` | `...tos.bucket` | bucket |
| `AMA_DATA_TOS_PREFIX` | `...tos.prefix` | **required**, every key must start with it |
| `AMA_DATA_TOS_ACCESS_KEY` | `...tos.accessKey` | **use this in production** |
| `AMA_DATA_TOS_SECRET_KEY` | `...tos.secretKey` | **use this in production** |
| `AMA_DATA_TOS_MAX_BYTES` | `...tos.maxBytes` | integer, clamped to the hard cap 8MiB |
| `AMA_DATA_TOS_TIMEOUT` | `...tos.timeout` | Go duration, e.g. `20s` |
| `AMA_DATA_TOS_USE_PATH_STYLE` | `...tos.usePathStyle` | boolean |

The startup log lists the **field names** overridden by environment variables (names only, never
values), so "I changed the YAML and nothing happened" is visible at a glance.

### Validation and failure behavior

| Situation | Behavior |
| --- | --- |
| `enabled: false` with half-written fields left over | Ignored, startup succeeds (that is a leftover, not a mistake to block on) |
| `enabled: true` with a missing field | **Startup fails** with `ErrInvalidConfig` (e.g. `dataSources.mysql.enabled=true 但缺少 user`) |
| `tos.prefix` empty, starting with `/`, or containing `.` / `..` segments | **Startup fails** (fail-closed: no "read the whole bucket" fallback, and a prefix that can never match a valid key is a config error rather than "every call fails") |
| Datasource unreachable / probe failed | Only a `Warn`; amad starts normally and tool calls return a clear error |
| Startup probe found table names / object keys | Written into the tool Description; if the probe failed the wording falls back to generic text |

Startup probes each source once (MySQL `SHOW TABLES`, TOS `ListObjectsV2` with at most 100 keys,
5s timeout each) so the tool description can say what tables / objects exist. **A failed probe
never blocks startup**: an RDS allowlist or a TOS policy may be temporarily wrong, and that must
not take the whole service down.

## Wiring it up on a server (`/opt/ama`)

> Everything below is **for the operator to run**; this repository does not execute any of it.

1. **Create the read-only account** (RDS side): add the server's private (or public, if RDS is
   reached over the internet) IP to the whitelist / security group, then:

   ```sql
   CREATE USER 'ro_user'@'%' IDENTIFIED BY '<strong-password>';
   GRANT SELECT ON example_db.* TO 'ro_user'@'%';
   FLUSH PRIVILEGES;
   ```

2. **Create read-only TOS credentials**: a sub-account with a policy that allows only
   `GetObject` / `ListBucket` on `example-bucket/example-prefix/*`. **Do not use the main
   account's keys.**

3. **Get the values into the amad container.** ⚠️ The repo's `docker-compose.yml` defines the amad
   service with an **explicit `environment:` list and no `env_file:`**, so putting `AMA_DATA_*`
   into `.env` alone does **not** reach the container; `.env` is only used for `${...}`
   interpolation inside compose files. Two ways to fix that:

   **Option A (recommended, does not touch the repo file): a compose override file.**

   ```yaml
   # /opt/ama/docker-compose.datasources.yml
   services:
     amad:
       environment:
         # Empty defaults on purpose: an empty value does not override the YAML file (see
         # config.applyEnv), so leaving a variable unset keeps the YAML value in effect.
         AMA_DATA_MYSQL_ENABLED: ${AMA_DATA_MYSQL_ENABLED:-}
         AMA_DATA_MYSQL_HOST: ${AMA_DATA_MYSQL_HOST:-}
         AMA_DATA_MYSQL_PORT: ${AMA_DATA_MYSQL_PORT:-}
         AMA_DATA_MYSQL_DATABASE: ${AMA_DATA_MYSQL_DATABASE:-}
         AMA_DATA_MYSQL_USER: ${AMA_DATA_MYSQL_USER:-}
         AMA_DATA_MYSQL_PASSWORD: ${AMA_DATA_MYSQL_PASSWORD:-}
         AMA_DATA_MYSQL_MAX_ROWS: ${AMA_DATA_MYSQL_MAX_ROWS:-}
         AMA_DATA_MYSQL_TIMEOUT: ${AMA_DATA_MYSQL_TIMEOUT:-}
         AMA_DATA_TOS_ENABLED: ${AMA_DATA_TOS_ENABLED:-}
         AMA_DATA_TOS_ENDPOINT: ${AMA_DATA_TOS_ENDPOINT:-}
         AMA_DATA_TOS_REGION: ${AMA_DATA_TOS_REGION:-}
         AMA_DATA_TOS_BUCKET: ${AMA_DATA_TOS_BUCKET:-}
         AMA_DATA_TOS_PREFIX: ${AMA_DATA_TOS_PREFIX:-}
         AMA_DATA_TOS_ACCESS_KEY: ${AMA_DATA_TOS_ACCESS_KEY:-}
         AMA_DATA_TOS_SECRET_KEY: ${AMA_DATA_TOS_SECRET_KEY:-}
         AMA_DATA_TOS_MAX_BYTES: ${AMA_DATA_TOS_MAX_BYTES:-}
         AMA_DATA_TOS_TIMEOUT: ${AMA_DATA_TOS_TIMEOUT:-}
         AMA_DATA_TOS_USE_PATH_STYLE: ${AMA_DATA_TOS_USE_PATH_STYLE:-}
   ```

   Put the real values in `/opt/ama/.env`:

   ```bash
   AMA_DATA_MYSQL_ENABLED=true
   AMA_DATA_MYSQL_HOST=mysql.example.com
   AMA_DATA_MYSQL_PORT=3306
   AMA_DATA_MYSQL_DATABASE=example_db
   AMA_DATA_MYSQL_USER=ro_user
   AMA_DATA_MYSQL_PASSWORD='<strong-password>'
   AMA_DATA_MYSQL_MAX_ROWS=200
   AMA_DATA_MYSQL_TIMEOUT=30s

   AMA_DATA_TOS_ENABLED=true
   AMA_DATA_TOS_ENDPOINT=https://tos-s3-cn-beijing.volces.com
   AMA_DATA_TOS_REGION=cn-beijing
   AMA_DATA_TOS_BUCKET=example-bucket
   AMA_DATA_TOS_PREFIX=example-prefix/
   AMA_DATA_TOS_ACCESS_KEY='<AK>'
   AMA_DATA_TOS_SECRET_KEY='<SK>'
   AMA_DATA_TOS_MAX_BYTES=1048576
   AMA_DATA_TOS_TIMEOUT=20s
   ```

   Then include the override when running compose:

   ```bash
   cd /opt/ama
   COMPOSE_FILE=docker-compose.yml:docker-compose.datasources.yml docker compose up -d amad
   # or export it once, in the shell that runs every later compose command:
   export COMPOSE_FILE=docker-compose.yml:docker-compose.datasources.yml
   ```

   **Option B: add the same `AMA_DATA_*: ${AMA_DATA_...}` entries to the amad `environment:` list
   in `/opt/ama/docker-compose.yml`** and put the values in `.env`. Simpler, but the local compose
   file is part of the checked-out tree and may be replaced on update.

4. **Restart amad and read the startup log:**

   ```bash
   cd /opt/ama && docker compose up -d amad
   docker compose logs --tail=200 amad | grep -E "数据源|dataSources|启动阶段"
   ```

   Expected:

   ```
   INFO 已装配 worker 侧数据源工具（凭证只在 amad 进程内，不进会话容器） 工具=[query_sql read_object] mysql=true tos=true
   INFO 数据源 MySQL 已装配 database=example_db 表数量=42 max_rows=200 timeout=30s
   INFO 数据源 TOS 已装配 bucket=example-bucket prefix=example-prefix/ 对象数量=7 max_bytes=1048576 timeout=20s
   ```

   If you see `数据源 MySQL 启动探测失败` / `数据源 TOS 启动探测失败`, the tools still work, but
   their descriptions have no inventory; see "Troubleshooting".

5. **Open a new conversation** (the model's tool list is fixed when the Ark Agent is created /
   updated, so an already-open session keeps the old Agent):

   ```
   you:    how many orders came in per channel today?
   agent: (calls query_sql)
   ```

   Then watch the audit lines:

   ```bash
   docker compose logs -f amad | grep "datasource "
   # INFO datasource query_sql tool=query_sql session_id=sess_xxx work_id=work_xxx duration_ms=12 rows=6 truncated=false sql="SELECT channel, COUNT(*) FROM orders GROUP BY channel" status=ok
   # INFO datasource read_object tool=read_object session_id=sess_xxx work_id=work_xxx duration_ms=34 bytes=1024 truncated=false key=example-prefix/reviews.csv status=ok
   ```

## Security boundary and known limitations

**Boundary**

- **The account and the policy are the real boundary**: SELECT-only on RDS, read-only on one TOS
  prefix. The SQL guard (`sqlguard.go`) is a **guard rail** against a model that writes a write
  statement by mistake, not a sandbox against malicious SQL.
- **Defense in depth**: every query takes a dedicated connection and issues
  `SET SESSION transaction_read_only = 1`, so even a mis-provisioned writable account cannot write
  through it. A dedicated connection is required because the session switch must be on the same
  connection as the query.
- **Rejected**: multiple statements, a non-read-only leading keyword, `INTO OUTFILE` / `DUMPFILE`,
  `LOAD DATA` / `LOAD XML`, `LOAD_FILE`, `FOR UPDATE` / `FOR SHARE` / `LOCK IN SHARE MODE`,
  MySQL executable comments `/*! ... */`, and the `mysql.` / `performance_schema.` / `sys.`
  system schemas (`information_schema.` is allowed: read-only metadata is useful). Validation works
  on MySQL's own lexical rules, so a semicolon inside a string or comment and comment-split words
  like `INTO/**/OUTFILE` are handled by their real meaning.
- **TOS is read-only**: the bucket is fixed in the config and keys must start with `prefix`; empty
  keys, keys starting with `/`, keys containing `..` / `.` segments and `://` are rejected.
  **No write operation exists.**
- **No credential leakage**: tool results and error messages replace the password / secret key
  with `***`; the DSN is never logged; audit lines record the SQL / key / size, never credentials.

**Limitations**

- **One statement per call, no parameter placeholders** (the model writes the SQL directly;
  parameterization means nothing here, and the read-only account is what prevents injection).
- **No `save_as`**: the session workspace is a Docker volume and the worker has no simple write
  channel into it (that would need either a file-transfer path from amad into the container, or
  writing the result back to object storage and having the agent read it, both of which add new
  write permissions). This is left for later; in the meantime the model is told to narrow the range
  with an aggregate query, or to read an existing file with `read_object`.
- **One MySQL and one TOS per config**: multiple datasources would need per-work context, which is
  not implemented.
- **Result caps**: `query_sql` caps rows, 200 characters per cell and 256KB overall text;
  `read_object`'s text cap is `tos.maxBytes` itself. Going over is truncated with a note, not an
  error.
- **Queries with their own `LIMIT`**: the tool does not inject a second one, but after reading the
  cap the driver still drains the rest of the result set before returning the connection, so
  `LIMIT 100000000` transfers the whole result. Do not write huge LIMITs; use `max_rows` instead.
  (The tool description says this too.)
- **Disabling a datasource does not remove the old declaration from Ark for Agents without
  built-in tools.** The ma layer treats `CustomTools == nil` as "leave the tools array unchanged"
  (sending an empty array would clear it, which is a different meaning), so for an Agent with an
  empty `builtin_tools` the stale `query_sql` / `read_object` declaration stays on the Ark side
  after the datasource is turned off. Fix: recreate that Agent, or give it at least one built-in
  tool (`builtin_tools` non-empty) so the tools array is rewritten as "builtin only".
- **`SET SESSION transaction_read_only = 1` needs MySQL 5.7.20+ / 8.0** (older servers use
  `tx_read_only`). On an older server the call fails with an explicit "failed to set a read-only
  session" error; it does not silently lose the protection.
- **Audit lines only go to the amad log**: not to the database, so they are gone after log
  rotation. Long-term audit needs a table (see future work).
- **The probe inventory goes into the tool description**, and the description is part of the Agent
  fingerprint. Each process probes once at startup, so two instances can legitimately compute
  different fingerprints — for example when one instance's probe failed (description degrades to
  the generic wording) or when a table was added during a rolling deploy (old instance saw 41
  tables, new one sees 42). Every conversation that lands on an instance with a different
  fingerprint triggers one `UpdateAgentSpec` (+ `PutArkBinding`), pushing the Ark Agent version up
  by one; two instances updating at the same time can also collide with 409 (that path retries
  once). This is a known and accepted cost of having the inventory in the description (it is very
  useful for demos, and the alternative — a per-process description that never changes — is worse:
  the model would not know what tables exist). It self-heals once every instance runs a release
  with the same inventory. If it ever becomes noisy, move the inventory into the database (or a
  cached, shared description) instead of probing per process.
- **MySQL 5.7.20+ / 8.0 only for the read-only session switch** (see above).
- **Nothing here has been verified against a real RDS or a real TOS**: the unit tests are all
  offline (fake narrow interfaces). See "To verify on a real deployment".

**To verify on a real deployment (not possible offline)**

1. Real MySQL behavior of `SET SESSION transaction_read_only = 1`, and the error text on an older
   server.
2. TOS `Range` / 404 / 403 / 416 behavior — in particular **a zero-byte object**: some
   S3-compatible services answer a `Range: bytes=0-N` request on an empty object with **416**, and
   the tool maps only 404 / NoSuchKey to "object does not exist", so reading an empty text file
   could report an error instead of "0 bytes (complete)". Put an empty object in the bucket and
   call `read_object` once.
3. **8MiB result relayed to Ark**: `read_object`'s hard cap is 8MiB and the result is POSTed back
   as a `user.custom_tool_result` event. Whether the Ark event-body limit accepts that, and how the
   send gate behaves if it does not, cannot be checked offline. Try one ~8MiB text object; if it
   fails, lower the hard cap to 1–2MiB.
4. UTF-8 detection and truncation against real objects.

## Troubleshooting

| Symptom | Check first |
| --- | --- |
| The model never calls the tools | Is `已装配 worker 侧数据源工具` in the startup log? Is `enabled` true? **Was a new conversation opened?** Any `datasource` audit line in `docker compose logs`? Has the Agent caught up (the next conversation updates it by fingerprint)? |
| `数据源不可用：dial tcp ...` | RDS whitelist / security group / port; is the host right (use the private hostname / IP); did the `AMA_DATA_*` variables actually reach the container (`docker compose exec amad env \| grep AMA_DATA_`)? |
| `查询失败：Error 1045 ... Access denied` | Account / password / database; does the account only have SELECT; is the GRANT on the right database? |
| `SQL 被拒绝：...` | The model wrote multiple statements / a write statement / a system schema / a locking read. That is expected; ask it to rewrite (the prompt can state the constraints). |
| `查询超时` | Raise `mysql.timeout`; add an index; have the model narrow the range or aggregate first. |
| Results always truncated | `mysql.maxRows` is too small or the query is too wide; have the model aggregate or select fewer columns. |
| `读取对象失败：403` | Does the TOS read-only policy include that prefix; do the AK/SK belong to that sub-account; do `tos.bucket` / `endpoint` / `region` match? |
| `对象不存在：...` | Is the key exact (case, `example-prefix/` included); see the key list in the tool description. |
| `key 被拒绝：只允许读取前缀 ...` | The key is missing the prefix, or `tos.prefix` is wrong (note the automatic `/`; `example-prefix` does not equal `example-prefix-secret/`). |
| `对象不是 UTF-8 文本` | The object is binary (xlsx, images, ...). The tool only reads text; ask the user to export CSV into the bucket. |
| Startup exits with `dataSources.` | That is config validation failing on purpose; add the missing field. It is fail-fast so that "configured but silently no tool" cannot happen. |

## Tests

All offline: no real database, no real TOS, no network (MySQL is faked through the `sqlExecutor`
narrow interface, TOS through `objectStore`):

```bash
cd server
go test ./shared/infra/datasources/
go test -race ./core/ ./shared/infra/config/ ./shared/infra/datasources/
```

Covered: read-only validation (positive and negative cases, including semicolons inside
strings/comments, `WITH ... DELETE`, `EXPLAIN`, system schemas), LIMIT injection and no
double-injection (including a semicolon followed by a trailing comment), row truncation,
`max_rows` / `max_bytes` clamping, markdown escaping, NULL, timeouts and error mapping,
password / secret-key redaction, audit fields, prefix / key validation, 404 mapping, non-UTF-8,
truncation in the middle of a multi-byte character, config validation and defaults, and the
anti-drift assertion "the declared names equal the implemented map keys".

## Code layout

| File | Contents |
| --- | --- |
| `datasources.go` | `Config` / `Registry` / `New` / `Declarations` / `ToolsFor` / `Close`; config validation and startup probes |
| `sqlguard.go` | Read-only validation, LIMIT injection, markdown rendering, text caps |
| `mysql.go` | `query_sql`: dedicated read-only connection, row scanning, timeouts, audit |
| `tos.go` | `read_object`: aws-sdk s3 `GetObject` + Range, prefix validation, UTF-8 check, audit |
| `*_test.go` | All offline unit tests |

Dependency direction: `config` → `datasources`; `core` only declares the `CustomToolProvider`
interface (it does not import this package) and `cmd/amad` plugs the implementation in.

## Future work

1. **`save_as`**: write the query result into the session workspace or back to object storage, so
   the model can "query once, read in pages". It needs a write channel from amad into the container
   (or a write path into TOS that the agent then reads), which adds new write permissions and needs
   its own design.
2. **Audit to the database**: today it is only a log line; a table plus asynchronous writes would
   make "who queried which tables, when" queryable.
3. **Multiple datasources**: several MySQL / TOS sources per environment, selected by Agent or
   workspace.
4. **Tighten the read-only validation further**: CTE DML legality differs between MySQL versions;
   refine when there is a real need. Also consider rejecting `SELECT ... INTO @var` (allowed today;
   it only writes a session variable, and the pooled connection may later be reused by another work
   item — no privilege gain, so the risk is negligible).
