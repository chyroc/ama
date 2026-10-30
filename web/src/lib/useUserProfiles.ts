/**
 * A hook that "looks up display names and avatars for a list of people".
 *
 * # What it solves is N+1
 *
 * Every row on a list page shows "who created it", and profiles (name + avatar) are in another
 * backend table.
 * Querying once per row is N+1: a page of 20 agents makes 20 calls. This hook takes the emails
 * the whole page needs,
 * **dedupes them and queries once**, and returns a lookup table.
 *
 * # ⚠️ Emails that are not found are missing from the result; that is not an error
 *
 * "This person never logged in" is normal (invited but never logged in, machine identities of a
 * fixed API Key,
 * former employees in old data). So callers just take undefined from `profiles[email]`
 * and hand it to `<UserAvatar>` for the fallback; **do not null-check it, and never treat it as a
 * failure**.
 *
 * # Why it returns no error on failure
 *
 * Deliberately different from useAsyncData: that one is about "failures must be visible, never
 * stuck on the skeleton",
 * because it loads the page body. **What this loads is avatars**: pure decoration; if not found
 * it falls back to
 * initials and the page is fully usable. Making it an error state would turn "the avatar service
 * hiccuped" into
 * "the whole agent list will not open", promoting a minor problem into a major failure.
 *
 * So this hook only returns `profiles`; on failure it is an empty object.
 */

import { useEffect, useState } from "react";

import { api } from "@/data/api";
import type { UserProfile } from "@/types/domain";

/**
 * Look up profiles for a set of emails in one batch.
 *
 * @param emails Emails to look up; may contain empty values and duplicates (cleaned inside).
 *
 * ⚠️ **Do not pass a new array literal on every render**: that would make the effect re-run
 * every frame.
 * The value passed in is first normalized into a stable string key before comparing, so
 * `useUserProfiles(agents.map(a => a.createdBy))` is safe: if the content did not change it does
 * not query again.
 */
export function useUserProfiles(emails: (string | undefined)[]): Record<string, UserProfile> {
  const [profiles, setProfiles] = useState<Record<string, UserProfile>>({});

  /*
    Dedupe + sort + join into one string to use as the dependency.

    **Do not put the array straight into useEffect's deps**: the array is a new reference every
    render,
    and deps are compared with Object.is, so the effect re-runs every frame and sends a request
    every frame,
    which is invisible on screen (the data is right, it just keeps hitting the API).

    Sorting makes "the same people in a different order" hit the same key; reordering a list should
    not trigger a new query.
  */
  const wanted = [
    ...new Set(emails.map((e) => (e ?? "").trim().toLowerCase()).filter((e) => e !== "")),
  ].sort();
  const key = wanted.join(",");

  useEffect(() => {
    if (key === "") {
      // Nothing to look up (e.g. every agent on the page is old data without createdBy).
      // **Clear and return right away; do not send an empty request**.
      setProfiles({});
      return;
    }
      // Race guard: switching workspace / paging can make the previous request's result arrive
      // after this one;
      // without the guard the avatars would flash old data.
    let alive = true;
    api
      .listUserProfiles(key.split(","))
      .then((got) => {
        if (alive) setProfiles(got);
      })
      .catch(() => {
        // Degrade silently; see the file header for why: an avatar not found just falls back to
        // initials,
        // and the caller should not see an error state.
        if (alive) setProfiles({});
      });
    return () => {
      alive = false;
    };
  }, [key]);

  return profiles;
}
