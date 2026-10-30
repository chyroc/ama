/**
 * Derives a workspace key from the display name.
 *
 * # Why a separate module, instead of keeping it in types/domain.ts
 *
 * The pinyin dictionary is **287 KB** (+113 KB after gzip). `domain.ts` is imported by almost
 * every page;
 * putting the dictionary there means **every user downloads it every time they open any page**,
 * while it is only used in one place: "typing a name
 * when creating a workspace". So it is loaded with dynamic `import()`, Vite splits it into its own
 * chunk,
 * and it is fetched only at the moment a name is really being typed.
 *
 * The cost is that derivation becomes async (see the race note on slugifyName). This is a
 * deliberate tradeoff:
 * one 300 KB on-demand download in exchange for 113 KB off the first screen of the whole site.
 */

/** Character rules for the key: lowercase, non-[a-z0-9] becomes a hyphen, trim both ends, cut to 32. */
export function asciiSlug(value: string): string {
  return value
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 32);
}

/**
 * A run of consecutive Han characters. The three ranges are Extension A, the basic block, and
 * Compatibility Ideographs.
 *
 * **Has g** (slugifyName uses it for replace, segment by segment). `test()` advances lastIndex,
 * but the `replace` right after resets it to zero, so the two calls do not interfere.
 */
const HAN_RUN = /[㐀-䶿一-鿿豈-﫿]+/g;

/** Cache the import() so we do not go through module resolution on every keystroke. */
let pinyinModule: Promise<typeof import("pinyin-pro")> | null = null;

/**
 * Derive a candidate key from the display name; Chinese is turned into pinyin.
 *
 * # Chinese names must be turned into pinyin first, or the result is an empty string
 *
 * **This was a real production bug.** It used to only do `[^a-z0-9] → -`, so all-Chinese names
 * (like "增长中台" or "测试空间") were fully replaced, then the leading/trailing hyphens trimmed,
 * leaving an **empty string**.
 * Users could never submit unless they edited the Key field by hand, while the Key looked like it
 * followed the name automatically, as if
 * it were already filled in. The error "workspace Key cannot be empty" points at the Key field,
 * not at the real cause
 * "your name is Chinese". And Chinese names are exactly the normal case here: the seed workspace
 * itself has a Chinese name.
 *
 * At the time all 16 e2e cases explicitly called `fill("ws-key", ...)`, so **the auto-derive path
 * was never
 * covered once**, and all-green still missed it. The "only fill in a Chinese name" case in
 * 01-workspace fills that gap.
 *
 * # Why pinyin-pro and not the smaller tiny-pinyin
 *
 * We tried tiny-pinyin first (42 KB, an order of magnitude smaller), and **in testing it did not
 * work**: it relies on
 * `Intl.Collator` to binary search a table of 411 representative characters, and under this ICU
 * 5 pairs of representative characters
 * sort opposite to what the table assumes (`陂/奔`, `欻/揣`, `肋/勒`, `嗯/拏`, `悄/癿`), so whole
 * ranges
 * fall into the wrong bucket: **前/钱/桥/强/墙/抢 all read as qie, 电/点/第/底 all read as di**.
 * Node and real Chrome gave the same wrong result, so it is not an environment issue, and a few
 * `patchDict` exceptions cannot fix it
 * (hundreds of characters are affected). pinyin-pro ships its own dictionary, does not depend on
 * Intl sorting, and **resolves polyphones by word**:
 * 重庆→chong-qing, 银行→yin-hang, 重点→zhong-dian are all right.
 *
 * # Only hand Han runs to pinyin()
 *
 * If the whole string is passed in, **ASCII is also split character by character**: "AMA 测试" →
 * "A M A ce shi",
 * and the key becomes `a-m-a-ce-shi`. So only runs of Han characters are replaced; ASCII is left
 * as is.
 * Spaces are added on both sides of a Han run so it does not stick to neighboring ASCII as one word.
 *
 * # If nothing can be derived, fall back to an empty string
 *
 * Korean, emoji and pure symbols cannot become pinyin, so the result is still empty; that is
 * acceptable: **derivation only saves effort,
 * it is not the only way in**. The Key field can always be filled by hand, and an empty string is
 * caught by validateWorkspaceKey's required check.
 * A dictionary load failure (offline, chunk 404) also takes this fallback, instead of breaking the
 * input.
 *
 * # ⚠️ Callers must handle the race themselves
 *
 * This is async, and it hangs on the input's onChange: **with fast typing several promises run at
 * once**,
 * an earlier one may arrive later, and the key gets overwritten by a stale name. After await the
 * caller must check
 * "the current input is still the name from this call" before writing state (WorkspacesPage
 * compares via a ref).
 */
export async function slugifyName(name: string): Promise<string> {
  if (!HAN_RUN.test(name)) return asciiSlug(name);
  try {
    pinyinModule ??= import("pinyin-pro");
    const { pinyin } = await pinyinModule;
    return asciiSlug(name.replace(HAN_RUN, (run) => ` ${pinyin(run, { toneType: "none" })} `));
  } catch {
    // If the dictionary could not be fetched, fall back to the plain ASCII rule, the same as the
    // behavior before pinyin was added.
    return asciiSlug(name);
  }
}
