// Tool set assembly: registers the six container tools into the SDK's toolset.Set and attaches
// the idle sentinel.
//
// Executor-agnostic: given any Execer, a working directory and an idle callback, it produces a
// set of bash/read/write/edit/glob/grep that Ark can send calls to.
package shelltools

import (
	"fmt"
	"log/slog"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// NewSet builds the six tools running on ex and attaches onIdle to Set.Close().
//
//   - workdir is the working directory inside the executor that the six tools use to resolve
//     **relative paths** (the agent's `./skills/...` is resolved against it);
//   - localWorkdir only satisfies the argument check of SDK toolset.NewDefault: NewDefault runs
//     MkdirAll on it **on the worker's own machine** and starts a local /bin/bash right away,
//     which we kill with set.Close() right after building, then replace all six tools with the
//     executor versions. **Never pass workdir for this argument**: workdir is a path inside the
//     executor (/workspace in the container), which often does not exist on the worker machine,
//     and MkdirAll may also fail on permissions. Callers should pass the local session directory
//     the SDK gives;
//   - onIdle is called when a conversation round ends idle (Set.Close); nil means do nothing when idle;
//   - when log is nil, slog.Default() is used.
func NewSet(
	ex Execer, workdir, localWorkdir string, onIdle func() error, log *slog.Logger,
) (*toolset.Set, error) {
	if log == nil {
		log = slog.Default()
	}

	// First build a default Set with the SDK, then replace **all** six tools with the executor versions.
	//
	// Why not new an empty Set ourselves: the tools field of toolset.Set is private, and there is no
	// empty constructor outside the package, so the only way is NewDefault then Register to override
	// (Register with the same name replaces; see SDK toolset.Set.Register).
	//
	// **All six must be replaced.** If only bash is replaced, the file tools read and write the
	// worker process's local directory, which is not the same as the remote file system bash sees.
	// When the agent writes a file with bash and then reads it with read, it is not found, with no
	// error; the agent just reaches a wrong conclusion and retries again and again.
	set, err := toolset.NewDefault(toolset.Options{Workdir: localWorkdir})
	if err != nil {
		return nil, fmt.Errorf("建默认工具集失败: %w", err)
	}

	// Close the SDK's default Set before overriding. This step cannot be skipped, otherwise **each
	// work item leaks one local bash child process**.
	//
	// NewDefault **starts** a local `/bin/bash` **right away** (NewBashTool → NewBashSession
	// → cmd.Start(), not lazy). Register replaces map entries by name: once the SDK's bash is
	// replaced, it can never be reached again, and the Set.Close() the SDK calls when the work item
	// ends only walks the tools still in the map. So nobody takes care of that child process and its
	// temp directory, and it hangs around until amad exits.
	//
	// Close is safe at this moment: of the six tools only bash implements ClosableTool; the other
	// five are stateless (they only hold a Resolver) and still work after Close. They are replaced
	// right away anyway.
	if err := set.Close(); err != nil {
		// Failing to close only leaks one local process; not worth failing the whole work item.
		log.Warn("关闭 SDK 自带 bash 失败，可能漏一个本地 bash 进程", slog.Any("err", err))
	}

	// All six tools take ex: whichever is called goes through ex first (on the container path that
	// is the dockertools session handle, which makes sure the executor is awake). So no tool needs
	// its own resume logic, and none can be missed.
	//
	// Each one gets a timing wrapper (see timing.go). **Only wrap these six, not the sentinel
	// below**: the decorator hides Close, so wrapping it would mean the idle callback never fires
	// again, with zero symptoms.
	tools := []toolset.Tool{
		NewBashTool(ex, workdir),
		NewReadTool(ex, workdir),
		NewWriteTool(ex, workdir),
		NewEditTool(ex, workdir),
		NewGlobTool(ex, workdir),
		NewGrepTool(ex, workdir),
	}
	return newSetFromTools(localWorkdir, tools, onIdle, log)
}

// newSetFromTools builds the SDK Set, closes its built-in local bash, and registers the given
// tools and the idle sentinel. The shell version (NewSet) and the native version (NewNativeSet)
// share this part; see the comments above in this file for why.
//
// localWorkdir is only used to satisfy NewDefault (it runs MkdirAll on the worker machine and
// starts a local bash, which is closed right after building); **do not pass the workdir inside
// the executor**.
func newSetFromTools(
	localWorkdir string, tools []toolset.Tool, onIdle func() error, log *slog.Logger,
) (*toolset.Set, error) {
	if log == nil {
		log = slog.Default()
	}
	// First build a default Set with the SDK, then replace everything: the tools field of
	// toolset.Set is private, and there is no empty constructor outside the package, so the only
	// way is NewDefault then Register to override.
	set, err := toolset.NewDefault(toolset.Options{Workdir: localWorkdir})
	if err != nil {
		return nil, fmt.Errorf("建默认工具集失败: %w", err)
	}

	// Close the SDK's default Set, otherwise **each work item leaks one local bash child process**
	// (NewDefault starts /bin/bash right away, and after Register replaces it nobody Closes it).
	if err := set.Close(); err != nil {
		log.Warn("关闭 SDK 自带 bash 失败，可能漏一个本地 bash 进程", slog.Any("err", err))
	}

	for _, tool := range tools {
		set.Register(newTimedTool(tool, log))
	}
	// The sentinel uses Set.Close to get the "this round ended idle" signal. ⚠️ Do not wrap it with
	// newTimedTool (that hides Close). When onIdle is nil, the sentinel's Close is a no-op.
	set.Register(newIdleHook(onIdle))
	return set, nil
}
