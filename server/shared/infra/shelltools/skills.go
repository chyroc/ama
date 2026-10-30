// Writes the files bundled with skill packages into the container.
//
// # Why this step exists
//
// The SDK **also** installs skills itself (arkruntime/selfhosted/envinit), but it installs them
// under `Workdir/skills/` on **the worker machine**. This package replaces all six tools with
// container versions (see provider.go), so the agent's read/glob/bash see the **container's**
// file system. The two places have nothing to do with each other. So the skill files the SDK
// installs are invisible to the agent, with no error at all: the agent follows SKILL.md and
// reads `scripts/init.sh`, the read tool returns "file does not exist", and the agent just
// tries a few other paths and gives up. This step is what really puts those files into the
// container.
//
// # When to inject: the moment the container is created, not when the tool set is built
//
// It hangs off container.ensureRunning (see pause.go), for two reasons:
//
//  1. **It must not hang off ToolsFor.** The SDK calls ToolsFor for every work item it takes,
//     and turns with plain-text replies never touch the container. Injecting there would give
//     back the 20 seconds saved by "lazy container creation"
//     (TestToolsForDoesNotCreateContainer in provider.go pins exactly this).
//  2. **It must also cover the "recreate" branch.** When the container is reclaimed by the
//     platform and recreated, the workspace is reset (the TTL keeps running while paused, see
//     pause.go). Hanging it only on first creation would leave the restored container without
//     skill files, again with zero errors.
//
// # How files are written, and its limit
//
// base64 + shell redirect, the same approach as the write tool (file.go). Files are merged into
// as few commands as possible, because the SDK handles tool calls **serially**: one command per
// file would stack N round trips directly on top of the ~20 seconds of first container creation.
//
// ⚠️ **The constraint is not ARG_MAX but MAX_ARG_STRLEN.** The two differ by 16x, and getting it
// wrong fails silently. The whole script is passed to `bash -lc` as **one** argv element, and
// Linux has a separate limit for a **single** argv element: `MAX_ARG_STRLEN = 32 pages = 128 KiB`
// (the 2 MiB of `ARG_MAX` is the **total** of all arguments plus environment variables, and does
// not limit a single element). Going over it makes execve return `E2BIG` right away, with the
// error `fork/exec ...: argument list too long`, which does not point to skill injection at all.
// The batch budget must be derived from 128 KiB; see maxInjectBatchBytes.
package shelltools

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"
)

// SkillFile is one file to write into the container.
//
// **Declared locally instead of importing core**: infra/ does not depend on core (that would
// create a cycle and tie the access layer to the business model). core implements this shape and
// amad wires them together, the same way as the other interfaces in this package.
type SkillFile struct {
	// Path is the path relative to the skill directory, like scripts/init.sh.
	// **The caller must make sure it has passed the Zip Slip check** (done by core/skillpkg.go).
	// This file still blocks it once more; see safeSkillPath.
	Path string
	// Content is the file content, written as-is.
	Content []byte
}

// SkillBundle is a skill together with its files.
type SkillBundle struct {
	// Name is the skill name ([a-z0-9-]), used as the directory name in the container.
	Name string
	// Files are the files to be **written one by one**. Can be empty.
	//
	// When ArchiveURL is set, this usually holds only a fallback SKILL.md (so that the directory
	// at least exists if the download fails); the real content comes from the archive.
	Files []SkillFile

	// ArchiveURL is the **presigned download link** of this skill's zip package (the container
	// downloads it directly).
	//
	// # Why this is needed when Files exists
	//
	// Files go through base64 + shell redirect and are limited by MAX_ARG_STRLEN (128 KiB). A
	// skill package of tens of MB cannot fit at all (see the top of this file). ArchiveURL lets the
	// bytes go straight from storage to the container, unrelated to command line length.
	//
	// **Having both paths is deliberate**, not a transition state: skills with no attachments
	// (the three built-in skills, hand-written skills) only have a SKILL.md of a few KB. Signing a
	// URL, uploading once, and running an extra curl+unzip in the container for it is much slower
	// than writing it directly via base64, and adds two failure points for nothing.
	// The decision is on the core side (whether there are attachments); see core/skillinject.go.
	//
	// ⚠️ **The URL carries a signature, so it is a time-limited pass.** Only put it into one-off
	// commands, **never into environment variables** (the agent in the container has bash, and
	// `env` shows everything). There is no recorder on the injection path, so it does not end up
	// in RunEvent.
	ArchiveURL string
	// ArchiveBytes is the byte size of the archive, used to check "did the whole thing download".
	//
	// **It must be checked**: when the connection drops, curl leaves an **incomplete but existing**
	// file and returns exit 0 (as long as the server already replied 200). Without the check the
	// symptom is "unzip error" or, worse, "only half the files extracted". The latter is a broken
	// skill, which is worse than a missing one.
	ArchiveBytes int
}

// SkillLoader gets which skills an Ark session should install.
//
// Implemented by core (it knows the session → run → agent → skillIds chain); this package only
// writes files. An empty slice means this session has no skills with attachments, and then no
// command is sent at all.
type SkillLoader interface {
	SkillsFor(ctx context.Context, arkSessionID string) ([]SkillBundle, error)
}

// Skill location: `<container workdir>/skills/<skill name>/`.
//
// # Why this shape: so the agent's relative path matches
//
// The self_hosted side now makes the agent look for skills under **`$workdir/skills`**: it sends
// the relative path `./skills/<skill name>/SKILL.md`. So this location must keep the relation
// "cwd + /skills" with the cwd of bash in the container, otherwise that relative path misses.
//
// The container cwd defaults to `/workspace` (DefaultWorkdir in dockertools, can be overridden by
// `docker.workdir`), so `./skills/x` expands exactly to `/workspace/skills/x`.
//
// ⚠️ **The location is derived from workdir, not hard-coded** (see skillsRootFor). It used to be
// a constant hard-coded to `/workspace/skills`, so the two only matched under the default workdir.
// When someone overrode workdir they would drift apart again, still with zero errors. Since it is
// derived from workdir, "keeping the two values the same" no longer depends on anyone remembering.
//
// Injection still builds **absolute paths**: injection goes through `bash -lc`, and the Cwd that
// runShell passes and the tool-side workdir are two separate entry points. Building absolute
// paths gives "where does the file actually land" only one answer.
//
// # Why it used to be /mnt/skills, and why it changed
//
// It used to be `/mnt/skills`, to **match the Ark platform convention**: the directory permission
// table the cloud sandbox refers to puts the skill directory there, and that convention made it
// into training data, so when nobody tells the model the path it guesses
// `/mnt/skills/<skill name>/`. That judgment was right at the time, and it did fix the even
// earlier `/root/.ama/skills` (a path nobody would ever guess).
//
// What changed is **platform behavior**, not that reasoning: the self_hosted side now explicitly
// looks for skills under `$workdir/skills`, so "the model's prior" is no longer the static
// `/mnt/skills` convention but **follows the cwd**. A real sequence reported by a user shows this
// clearly:
//
//	read ./skills/ark-tls/SKILL.md      → fails
//	bash pwd && ls -la
//	bash find / -name SKILL.md -path '*ark-tls*'    ← searches the whole disk
//	read /mnt/skills/ark-tls/SKILL.md   → only now it reads
//
// It **tries the relative path first**, and only falls back to the old convention when that
// fails. The file was sitting in `/mnt/skills` all along, and the injection log said success. So
// this was not "injection is broken" but **the location and the agent's lookup path were out of
// line**. The cost was one failed read plus one whole-disk find every time a skill was read (slow
// and noisy), and `find /` is expensive on large images.
//
// This mismatch **has been reproduced with a probe**: with cwd pinned to `/workspace` and skills
// still in `/mnt/skills`, `read ./skills/ark-tls/SKILL.md` returns exit=2, exactly as in
// production.
//
// # The platform does not write anything here, so there are no name clashes
//
// The worry that "skills attached to the Agent on the Ark side are also written here by the
// platform" does not hold under self_hosted: the SDK's envinit unpacks skill packages into
// `<Workdir>/skills` (`arkruntime/selfhosted/envinit/initializer.go:59`) with
// `os.MkdirAll`/`os.WriteFile`. That is **the worker machine's disk** (`/tmp/ama-worker-sessions`),
// unrelated to the container (see "The same skill is installed twice" in the README). Before
// injection the directory was measured to be empty, which confirms this.

// skillsDirName is the name of the skill directory under the working directory.
//
// **This name is set by the self_hosted side, not something we can pick**: the relative path the
// agent sends when looking for skills is `./skills/<skill name>/SKILL.md`, so it must be exactly
// `skills`.
const skillsDirName = "skills"

// SkillsRootFor computes "where skills should land under this workdir", which is always
// `<workdir>/skills`.
//
// # Why a function and not a constant
//
// The location must **follow the actual workdir**, and workdir can be overridden by config
// (`docker.workdir` / `AMA_DOCKER_WORKDIR`, see dockertools/provider.go). It used to be a constant
// hard-coded to `/workspace/skills`, so the two only matched under the default workdir:
// **once someone overrode workdir, the agent's `./skills/…` resolved to `<new workdir>/skills`
// while injection still wrote to `/workspace/skills`**. That brought back exactly the failure
// fixed here (one failed read first, then a whole-disk `find /`), and **still with zero errors**.
// Deriving it from workdir with a function makes that branch impossible by construction; no test
// or alert is needed to chase it.
//
// An empty workdir is treated as `/` (same as the empty-workdir behavior of resolvePath). In
// practice dockertools already turns an empty string into DefaultWorkdir, so the production path
// never reaches this branch.
func SkillsRootFor(workdir string) string {
	if strings.TrimSpace(workdir) == "" {
		return "/" + skillsDirName
	}
	return path.Join(workdir, skillsDirName)
}

// maxInjectBatchBytes is the base64 payload limit for one batch of injection commands.
//
// ⚠️ **This value must be derived from MAX_ARG_STRLEN (128 KiB), not from maxWriteBytes
// (512 KiB).** It used to be 384 KiB, with the reason "leave some margin below maxWriteBytes".
// That reference point was wrong, so **this constant was 256 KiB above the real limit, and the
// batching logic never triggered once**.
//
// The real constraint: the whole script is passed to `bash -lc` as **one** argv element, and Linux
// limits a single argv element to `MAX_ARG_STRLEN = 32 pages = 128 KiB`. `ARG_MAX` (2 MiB) is the
// **total** of all arguments plus environment variables and does not limit a single element.
// Using it as the test leads to "a 128 KiB script is fine".
//
// Observed: an agent bound 10 skills with no attachments, with bodies totaling 108 KiB, 144 KiB
// after base64, collected into one 148 KiB script. The backend returned `argument list too long`
// (E2BIG). And `set -e` made this batch fail **as a whole**, so **not a single one of the ten
// skills was written to disk**, and the agent reading `/mnt/skills/<some skill>/SKILL.md` got
// exit=2.
//
// 96 KiB leaves 32 KiB of margin under 128 KiB: the scaffolding (paths, mkdir, quotes) is counted
// at about 100 bytes per file, and it also has to fit overhead outside `bash -lc` that we cannot
// see. Writing right at the limit turns "just enough" into "sometimes over", and the symptom of
// going over is the whole batch silently lost.
const maxInjectBatchBytes = 96 << 10

// maxInjectChunkBytes is the base64 chunk limit a **single file** can carry in one command.
//
// Why it needs its own limit: the upload side allows 256 KiB per file (maxSkillFileBytes in
// core/skillpkg.go), about 341 KiB after base64, which is **much larger than MAX_ARG_STRLEN**. An
// earlier comment said "a single file larger than the batch budget is safe as its own batch".
// That looked true against maxWriteBytes (512 KiB), but not against the real 128 KiB: a batch of
// its own is still one 341 KiB command, and still gets E2BIG. So large files must be **written in
// chunks** (the first chunk `>` truncates, later chunks `>>` append).
//
// ⚠️ **It must be a multiple of 4**: base64 decodes 3 bytes from every 4 characters. Cutting at a
// point that is not a multiple of 4 makes `base64 -d` decode the chunks into shifted bytes, which
// is **silent content corruption**, not an error. It must also be ≤ maxInjectBatchBytes, otherwise
// a single chunk can never fit in an empty batch, and every chunk gets its own batch in a
// loop-like way (still correct, but wasting round trips).
const maxInjectChunkBytes = 64 << 10

// Pin the invariants the two comments above rely on, at compile time.
//
// **These two cannot live only in comments.** Both are constraints of the kind "if violated it
// still compiles and runs, just with occasional silent corruption in production": chunks not
// aligned to 4 make base64 chunk decoding produce shifted bytes (no error), and a chunk larger
// than the batch budget makes every chunk get its own batch (correct but wasting round trips).
// Someone "bumping these constants up a bit" later is quite likely, and then we want a compile
// failure, not another production investigation.
//
// The trick is "make the invalid case compute a negative number, then convert to uint": a
// negative constant converted to uint is a compile error (constant -1 overflows uint).
const (
	// The chunk must fall on a 4-character base64 boundary.
	_ = uint(0 - maxInjectChunkBytes%4)
	// A chunk must not exceed one batch budget, otherwise even an empty batch cannot hold one chunk.
	_ = uint(maxInjectBatchBytes - maxInjectChunkBytes)
)

// InjectSkills writes skill files into the container.
//
// The caller is container.ensureRunning, which **already holds c.mu**, so this must not touch that
// lock again. And because it runs inside the lock, concurrent tool calls cannot start before
// injection finishes.
//
// Failure **does not block** the container from being usable: a returned error is only logged.
// The reason is that "skill files were not written" is lighter than "this session cannot run any
// command": in the first case the agent can still work from the SKILL.md body (the body goes
// through Ark's system prompt and does not depend on files), the second is totally unusable.
// But it **must be logged**, otherwise "why are the skill files missing" cannot be traced.
func InjectSkills(
	ctx context.Context, sh Execer, workdir string, bundles []SkillBundle, log *slog.Logger,
) error {
	if len(bundles) == 0 {
		return nil
	}

	// Derive the location from the **actual workdir**, the same base as the agent's `./skills/…`.
	// With a hard-coded constant, overriding containerWorkdir would make the two drift apart with
	// zero errors (see skillsRootFor).
	root := SkillsRootFor(workdir)

	// Skills with an archive get downloaded first, one each. **This runs before per-file
	// injection**: if a download fails, that skill's fallback SKILL.md is still sent by the batch
	// write below, so the invariant "the directory exists" holds.
	archived, archiveErr := pullSkillArchives(ctx, sh, workdir, root, bundles, log)

	// First flatten all files into "absolute path in container → content", and block unsafe paths
	// along the way.
	type target struct {
		abs     string
		content []byte
	}
	var (
		targets []target
		skipped int
	)
	for _, b := range bundles {
		dir, ok := safeSkillDir(root, b.Name)
		if !ok {
			// The skill name should already be [a-z0-9-] (checked by apimodel.ValidSkillName).
			// Getting here means some path bypassed the check. Better to skip than to put it into
			// shell.
			skipped += len(b.Files)
			log.Warn("技能名不安全，跳过它的附件注入", slog.String("skill", b.Name))
			continue
		}
		// Skills whose archive already landed successfully must not be written again file by file.
		//
		// ⚠️ **This must be skipped**: what is in Files is the **fallback SKILL.md**, while the one
		// in the archive is the real one (a user-uploaded package may carry its own SKILL.md, see
		// core/skillinject.go). Without skipping we would overwrite the real one with the fallback,
		// and the two may differ, with zero errors.
		if archived[b.Name] {
			continue
		}
		for _, f := range b.Files {
			rel, ok := safeSkillPath(f.Path)
			if !ok {
				skipped++
				log.Warn("附件路径不安全，跳过",
					slog.String("skill", b.Name), slog.String("path", f.Path))
				continue
			}
			targets = append(targets, target{abs: path.Join(dir, rel), content: f.Content})
		}
	}
	if len(targets) == 0 {
		if skipped > 0 {
			return fmt.Errorf("shelltools: %d 个附件的路径不安全，全部跳过", skipped)
		}
		// Every skill landed via its archive, so there are no files to write one by one. This is the
		// normal case for large skills.
		return archiveErr
	}

	// Batch up: the total base64 per batch does not exceed maxInjectBatchBytes.
	start := time.Now()
	var (
		batch  []string
		size   int
		writes int
		cmds   int
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		cmds++
		// `set -e` stops right away when any line in the batch fails. Without it, if an earlier file
		// was written badly the later ones are still written, and the final exit code only reflects
		// the last line, so the failure gets swallowed.
		script := "set -e\n" + strings.Join(batch, "\n")
		res, err := runShell(ctx, sh, workdir, script)
		if err != nil {
			return fmt.Errorf("写技能文件失败: %w", err)
		}
		if exitCode(res) != 0 {
			return fmt.Errorf("写技能文件失败（exit=%d）: %s", exitCode(res), stderrOrErr(res))
		}
		batch = batch[:0]
		size = 0
		return nil
	}

	for _, t := range targets {
		encoded := base64.StdEncoding.EncodeToString(t.content)
		// Large files are written in chunks: the first chunk `>` truncates, later chunks `>>`
		// append.
		//
		// **"A batch of its own" does not solve it**: a batch is one command, so a 341 KiB payload
		// in its own batch is still one 341 KiB command, and still cannot pass MAX_ARG_STRLEN.
		//
		// Cut points must fall on multiples of 4 (see maxInjectChunkBytes): base64 works in groups
		// of 4 characters, and cutting out of line makes chunks decode into shifted bytes, **with no
		// error**.
		for off, first := 0, true; off < len(encoded) || first; first = false {
			end := off + maxInjectChunkBytes
			if end > len(encoded) {
				end = len(encoded)
			}
			chunk := encoded[off:end]
			// If it does not fit, send the current batch first. An empty batch can always hold one
			// chunk (maxInjectChunkBytes <= maxInjectBatchBytes), so this never spins.
			if size > 0 && size+len(chunk) > maxInjectBatchBytes {
				if err := flush(); err != nil {
					return err
				}
			}
			// Same command shape as the write tool (file.go): mkdir -p the parent, then base64 -d
			// to disk. Both path and payload go through shQuote: the path comes from a zip entry
			// name, which is untrusted input.
			redirect := ">"
			if !first {
				redirect = ">>"
			}
			batch = append(batch, fmt.Sprintf(
				`p=%s; mkdir -p "$(dirname "$p")" && printf '%%s' %s | base64 -d %s "$p"`,
				shQuote(t.abs), shQuote(chunk), redirect))
			size += len(chunk)
			off = end
		}
		writes++
	}
	if err := flush(); err != nil {
		return err
	}

	log.Info("技能文件已注入容器",
		slog.Int("files", writes),
		slog.Int("commands", cmds),
		slog.Int("archives", len(archived)),
		slog.Int("skipped", skipped),
		slog.String("dir", root),
		slog.Duration("took", time.Since(start).Round(time.Millisecond)))
	if skipped > 0 {
		return fmt.Errorf("shelltools: %d 个附件的路径不安全，已跳过", skipped)
	}
	return archiveErr
}

// archiveDownloadTimeout is the timeout for downloading + unpacking one skill package in the
// container.
//
// Packages are at most 100 MiB (core/skillpkg.go). Measured: 64 KiB including one container round
// trip takes about 1 second, mostly round-trip overhead; 100 MiB within the same data center takes
// only tens of seconds. 5 minutes **leaves margin for the worst case**: a timeout that is too
// tight shows up as "large skills randomly fail to install", and that failure looks like object
// storage being unstable, not pointing at all to "we set our own timeout too small".
const archiveDownloadTimeout = 5 * time.Minute

// archivePullConcurrency is how many skill packages are downloaded at once.
//
// These commands spend 99% of their time waiting on the network, so the gain from concurrency is
// almost linear, until it hits the container's download bandwidth. 4 rather than "all at once":
// an agent can bind at most 20 skills (maxArkSkillsPerAgent in core), and 20 commands hitting the
// same container at once would max out its network and process count, while the gain flattens
// after a few. Same idea as skillBlobFetchConcurrency on the core side.
const archivePullConcurrency = 4

// pullSkillArchives makes the container itself download and unpack skill packages from the
// presigned links.
//
// It returns "which skills already landed via their archive", plus a **non-blocking** error
// (non-nil when some skill failed to download).
//
// # Why one command per skill instead of a batch
//
// The opposite of the base64 path: there, batching saves container round trips (every file takes
// command line budget). Here **every command is short** (just a URL), so batching saves little,
// but it would turn "one skill failed to download" into "this whole batch failed" (`set -e`).
// With one command per skill, a failure stays limited to that one skill.
//
// # Three places that are easy to get wrong, and silent when wrong
//
//  1. **`curl` needs `-f`.** Without it, HTTP 403/404 counts as success: curl writes the error
//     page into the file and exits 0, then unzip says "not a zip file". That message points to a
//     broken package, not to "the signature expired".
//  2. **Check the byte count after download.** When the connection drops, curl leaves an
//     incomplete but existing file and exits 0 (as long as the server already replied 200).
//     Without the check unzip may extract only half the files, and **a broken skill is worse
//     than a missing one** (same as the "starved" rule on the core side).
//  3. **Clear the directory before unpacking.** The container may be restored from a snapshot and
//     may contain leftovers from a previous skill version. Without clearing, old and new files
//     mix, and the agent reads a skill where "some files are the old version".
//
// # Concurrent downloads: one command per skill, but **sent at the same time**
//
// These commands spend 99% of their time waiting on the network (download + unpack), with almost
// no CPU. Sent serially, each skill pays its own ~0.9 second container round trip (measured), so
// N skills means N round trips:
//
//	5 skills with attachments    serial 5.3s → concurrent 1.7s
//	10 skills with attachments   serial 10.6s → concurrent 2.5s
//
// **This is the most cost-effective step for "the user does not feel it is slow"**: it squeezes N
// round trips into 1, and it is **still synchronous**: no background goroutine, no "wait until
// ready" gate, no race with idle/pause.
//
// ⚠️ Why not "background async download": measured, creating a container takes ~20 seconds, while
// downloading 100 MiB takes only ~2.9 seconds (14%). Worse, there is the finding recorded in
// `pause.go`: **pausing freezes background processes, and resuming does not unfreeze them**. So
// "background download + container paused when idle" would leave the skill package stuck halfway
// forever, and a `kill -0` liveness check would say it is still running. Not worth adding that
// failure state for 14%.
//
// ⚠️ Calling `sess.Exec` concurrently is safe: the SDK already calls tools concurrently
// (see the "resume inside the lock, execute outside the lock" part of Exec in pause.go).
//
// # Concurrency has a cap
//
// Not "all at once": an agent can bind at most 20 skills (maxArkSkillsPerAgent in core), and 20
// commands hitting the same container at once would max out its network and process count, while
// the gain flattens after a few (the bottleneck becomes the container's download bandwidth). 4
// follows the same idea as skillBlobFetchConcurrency on the core side: fast enough, without
// overwhelming the downstream.
func pullSkillArchives(
	ctx context.Context, sh Execer, workdir, root string,
	bundles []SkillBundle, log *slog.Logger,
) (map[string]bool, error) {
	// First list "what to download", and block unsafe skill names along the way.
	type job struct {
		name string
		dir  string
		url  string
		size int
	}
	var jobs []job
	for _, b := range bundles {
		if b.ArchiveURL == "" {
			continue
		}
		dir, ok := safeSkillDir(root, b.Name)
		if !ok {
			// When the skill name is unsafe, **do not even put the URL into shell**. The loop above
			// logs a Warn.
			continue
		}
		jobs = append(jobs, job{name: b.Name, dir: dir, url: b.ArchiveURL, size: b.ArchiveBytes})
	}
	if len(jobs) == 0 {
		return map[string]bool{}, nil
	}

	// Write results by index, no lock needed: each goroutine only touches its own slot
	// (same shape as hydrateSkillFiles in core).
	type outcome struct {
		ok  bool
		err error
	}
	results := make([]outcome, len(jobs))

	sem := make(chan struct{}, archivePullConcurrency)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// ⚠️ **The timeout must start counting only after getting the token.**
			//
			// If it were set before `sem <-`, queue waiting would count toward those 5 minutes: with
			// concurrency 4 and 20 skills, the last batch first waits for the four rounds ahead to
			// finish, and by the time it gets a token there is little budget left. So **skills at
			// the back of the queue would time out for "waiting too long"**. That failure has
			// nothing to do with the skill or the network; it is purely caused by queuing, and the
			// error says "download timed out", which does not point to the real cause at all.
			start := time.Now()
			// ⚠️ **Both timeouts must be given.** Only widening the context does not help: runShell
			// hard-codes ExecSpec.Timeout to toolExecTimeout (60 seconds), and that is the limit on
			// the backend side. Downloads longer than 60 seconds get killed by it, so the "5 minutes"
			// here would mean nothing, showing up as "large skills randomly fail to install on slow
			// links". Code review caught this one.
			//
			// The context gets a bit more margin than ExecSpec: let the container side time out first
			// and return a clear error, rather than us giving up first and leaving a command still
			// running.
			cctx, cancel := context.WithTimeout(ctx, archiveDownloadTimeout+30*time.Second)
			res, err := runShellTimeout(
				cctx, sh, workdir, ArchiveScript(j.dir, j.url, j.size), archiveDownloadTimeout)
			cancel()

			switch {
			case err != nil:
				// ⚠️ **Never put the URL in logs**: it carries a signature, so it is a time-limited
				// pass, and logs are kept for a long time and may get pasted into issues.
				results[i] = outcome{err: err}
				log.Warn("技能包下载失败，回落到只写 SKILL.md",
					slog.String("skill", j.name), slog.Any("err", err))
			case exitCode(res) != 0:
				results[i] = outcome{err: fmt.Errorf("exit=%d", exitCode(res))}
				log.Warn("技能包下载失败，回落到只写 SKILL.md",
					slog.String("skill", j.name),
					slog.Int("exit", exitCode(res)),
					slog.String("stderr", stderrOrErr(res)))
			default:
				results[i] = outcome{ok: true}
				log.Info("技能包已下载并解压",
					slog.String("skill", j.name),
					slog.Int("bytes", j.size),
					slog.Duration("took", time.Since(start).Round(time.Millisecond)))
			}
		}(i, j)
	}
	wg.Wait()

	var (
		done   = map[string]bool{}
		failed []string
	)
	for i, r := range results {
		if r.ok {
			done[jobs[i].name] = true
			continue
		}
		failed = append(failed, jobs[i].name)
	}
	if len(failed) > 0 {
		return done, fmt.Errorf("shelltools: %d 个技能包没下成（%s）",
			len(failed), strings.Join(failed, ", "))
	}
	return done, nil
}

// ArchiveScript builds the "download + verify + unpack" command.
//
// The target is `<skill directory>` itself: the zip's top level is `<skill name>/` (guaranteed by
// BuildSkillZip), so unpacking into the **parent directory** gives `<root>/<skill name>/SKILL.md`.
//
// **It is exported for tests and probes**: they need to run "the script production code really
// sends", not a copy. With a copy they would verify the copy, and once the two drift apart they
// would **stay green**, which is the most useless way to fail.
func ArchiveScript(dir, url string, wantBytes int) string {
	parent := path.Dir(dir)
	// tmp lives in /tmp, not in the skill directory: the skill directory is cleared before
	// unpacking, and a package inside it would delete itself. The name includes the basename of
	// the skill directory, so two skills injected concurrently do not overwrite each other.
	tmp := "/tmp/ama-skill-" + path.Base(dir) + ".zip"
	return fmt.Sprintf(`set -e
tmp=%s
# The stderr file is derived from tmp: owned by this script run, so concurrent downloads do not overwrite each other (see the curl comment below).
curl_err="$tmp.err"
dir=%s
url=%s
# ⚠️ curl's stderr is logged by the caller, and the URL carries a signature (a time-limited pass).
# Some curl versions echo the URL in some errors, so **filter the URL out of stderr** before it leaves.
# This is defense in depth: the caller does not log the URL either, but we should not bet on which curl versions echo it.
#
# -f: make curl exit non-zero on HTTP errors. Without it a 403 error page is written to the file with exit 0,
# and then unzip says "not a zip file", which points at a broken package, not at an expired signature.
# -sS: quiet but keep error messages. --retry: retry three times on occasional storage blips.
#
# stderr must not go to a fixed file name under /tmp: with several skill packages downloading at once, or several
# sessions injecting on one machine, a fixed name gets overwritten; at best failure logs get mixed up, at worst
# another download's signed URL (this sed only replaces this run's $url) leaks into the caller's log as is.
if ! curl -fsS --retry 3 --retry-delay 1 -m %d -o "$tmp" "$url" 2>"$curl_err"; then
  sed "s|$url|<已隐去的签名 URL>|g" "$curl_err" >&2 || true
  rm -f "$curl_err" "$tmp"
  exit 1
fi
rm -f "$curl_err"
got=$(wc -c < "$tmp" | tr -d ' ')
if [ "$got" != "%d" ]; then
  echo "技能包字节数不对：下到 $got，应当是 %d（下载被截断了）" >&2
  rm -f "$tmp"
  exit 1
fi
# Clean first, then unzip: the container may come from a snapshot that still has files from an older version.
# Without cleaning, old and new files mix and the agent reads a skill where some files are stale.
rm -rf "$dir"
mkdir -p %s
# -q quiet, -o overwrite. Unzip into the parent dir: the zip's top level is <skill name>/.
unzip -qo "$tmp" -d %s
rm -f "$tmp"`,
		shQuote(tmp), shQuote(dir), shQuote(url),
		int(archiveDownloadTimeout/time.Second),
		wantBytes, wantBytes,
		shQuote(parent), shQuote(parent))
}

// safeSkillDir builds the in-container directory for a skill and checks the skill name.
//
// The skill name becomes a directory name and goes into shell, so only [a-z0-9-] is accepted, the
// same rule as apimodel.ValidSkillName. **Implementing it a second time here** is deliberate: this
// package does not import core (see the file header), and this check is the last gate on the
// injection path, so it cannot rely on the upstream having done it.
func safeSkillDir(root, name string) (string, bool) {
	if name == "" || len(name) > 64 {
		return "", false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return "", false
		}
	}
	return path.Join(root, name), true
}

// safeSkillPath blocks attachment paths once more.
//
// core/skillpkg.go already does the full Zip Slip check; this is **defense in depth**: injection
// is the step that turns untrusted data into real files in the container, so it should be able to
// judge safety on its own, rather than rely on the assumption "the caller always checked". If a
// new caller ever breaks that assumption, the symptom is an extra file under /etc in the
// container, with no error at all.
func safeSkillPath(p string) (string, bool) {
	if p == "" || len(p) > 512 {
		return "", false
	}
	// Treat backslash as a separator too: path.Clean does not know it, so `..\x` would be taken
	// as a normal file name.
	if strings.ContainsRune(p, '\\') {
		return "", false
	}
	if path.IsAbs(p) {
		return "", false
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	// NUL and newline: the first can cut a C string short, the second can start a new command line
	// when put into a script. shQuote already blocks newlines, but a second guard is cheap.
	if strings.ContainsAny(cleaned, "\x00\n") {
		return "", false
	}
	return cleaned, true
}
