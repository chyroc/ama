/**
 * Folds the **flat path list** of skill attachments into a directory tree.
 *
 * # Why it is needed
 *
 * The backend gives flat relative paths (`references/client.md`, `scripts/init.sh`); listed one
 * by one,
 * every row repeats the same prefix:
 *
 * ```
 * references/guide.md             ← 7 of 8 entries start with references/,
 * references/client.md              this prefix takes half the row width and carries no information
 * references/component.md
 * ```
 *
 * Once folded into a tree the prefix appears once, and each row only has the part of the file
 * name that tells them apart.
 *
 * # It must be recursive, not just fold one level
 *
 * Path depth has no limit (the backend only limits total length to 200 bytes; `scripts/foo/bar.sh`
 * is the example in its comment),
 * so "cut once at the first `/`" is not enough: that would show `a/b/c.md` as a file named
 * `b/c.md`
 * under `a`, **no error, just wrong**. This kind of gap, "the backend supports N levels, the
 * frontend only handles one",
 * gives zero errors, so this is written recursively from the start.
 *
 * # A node's key is still the full path
 *
 * Display uses `name` (the leaf name), but `path` is always **the full relative path**: it is the
 * argument of `readSkillFile(skillId, path)` and also the identity of the selected state. Mixing
 * the two would make opening a
 * file in a second-level directory get "this file is not in the skill" from the backend.
 */

/** A node in the tree. Directories are only for grouping; they are not clickable and have no content. */
export type SkillFileNode =
  | {
      kind: "dir";
      /** Directory name (leaf name, without the parent prefix). */
      name: string;
      /** Full relative path, like `references`. Only used as a React key, never sent to any API. */
      path: string;
      children: SkillFileNode[];
    }
  | {
      kind: "file";
      /** File name (leaf name). */
      name: string;
      /** **Full** relative path, like `references/client.md`. This is the API argument. */
      path: string;
      size: number;
    };

/** Intermediate shape while building: directories store children in a map so a directory name appears only once. */
type DirBuilder = {
  dirs: Map<string, DirBuilder>;
  files: { name: string; path: string; size: number }[];
};

function newDir(): DirBuilder {
  return { dirs: new Map(), files: [] };
}

/**
 * Fold `[{path, size}]` into a directory tree.
 *
 * Sort rule: **within one level, directories first, then files, each by name ascending**.
 * Directories first is the usual
 * file-browser convention (and makes "which groups of material this skill has" visible at a
 * glance); name ascending matches the backend's `order by path`,
 * so the same data renders in the same order every time.
 *
 * An empty `path`, and a path made only of separators, are skipped: they cannot get into
 * `skill_files`
 * (the backend's `CleanSkillPath` rejects them), but the frontend should not fail to draw the
 * whole tree because of one bad row.
 */
export function buildSkillFileTree(
  files: { path: string; size: number }[],
): SkillFileNode[] {
  const root = newDir();

  for (const f of files) {
    // Split and drop empty segments: `a//b` and `a/b` should give the same tree.
    const segs = f.path.split("/").filter((s) => s.length > 0);
    if (segs.length === 0) continue;

    let cur = root;
    // Every segment except the last is a directory; descend level by level (**the recursive half is
    // in this loop**:
    // any depth is walked fully, not just one cut).
    for (let i = 0; i < segs.length - 1; i++) {
      const seg = segs[i];
      let next = cur.dirs.get(seg);
      if (!next) {
        next = newDir();
        cur.dirs.set(seg, next);
      }
      cur = next;
    }
    cur.files.push({
      name: segs[segs.length - 1],
      // ⚠️ Use the **original path**, not `segs.join("/")`: the API argument must match the row in
      // the backend database
      // exactly. They are equal for normal data, but once upstream sends an extra `//`,
      // the rebuilt one would not be found.
      path: f.path,
      size: f.size,
    });
  }

  return flatten(root, "");
}

function flatten(dir: DirBuilder, prefix: string): SkillFileNode[] {
  const dirs: SkillFileNode[] = [...dir.dirs.entries()]
    .map(([name, child]) => {
      const path = prefix ? `${prefix}/${name}` : name;
      return {
        kind: "dir" as const,
        name,
        path,
        children: flatten(child, path),
      };
    })
    .sort((a, b) => a.name.localeCompare(b.name));

  const files: SkillFileNode[] = dir.files
    .map((f) => ({ kind: "file" as const, name: f.name, path: f.path, size: f.size }))
    .sort((a, b) => a.name.localeCompare(b.name));

  // Directories first, then files.
  return [...dirs, ...files];
}
