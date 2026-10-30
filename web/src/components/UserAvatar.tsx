/**
 * A small round avatar for a person + name on hover. **A generic component, not tied to any specific page.**
 *
 * # Interface design: give an email, the component handles the rest
 *
 * Callers only need to give `email`; `profile` is optional. **Do not let each caller handle empty values on its own**:
 * this repo has already written the "name/avatar may be empty" branch four times (twice in AppShell,
 * SettingsPage), and each place had to think again about "what if there is no avatar".
 * With it all in one place, callers just pass whatever they have.
 *
 * Four call sites were thought through: the creator in the agent list (this change), the member list, the issue author,
 * the creator of a scheduled task. The first three have different data shapes (members carry name/avatarUrl,
 * agents only have an email to look up, issues also only have an email), so the input is designed as
 * "**email required, profile optional**": use it if present, otherwise fall back to the email initial.
 *
 * # ⚠️ All three kinds of "no image" fall back to the initial; never write conditional rendering
 *
 * `<Avatar.Image src>` + `<Avatar.Fallback>` with the initial is enough: **Radix only renders `<img>` after the image
 * has loaded**, so "no such field", "still loading" and "URL broken"
 * all fall back to the initial automatically.
 *
 * **Hand-writing `avatarUrl && <Avatar.Image>` actually leaves an empty circle when loading fails**:
 * this is noted separately in types/domain.ts, AppShell and SettingsPage
 * (all citing the same fix commit); it is a trap this repo has hit again and again. Write it this way; do not invent your own fallback.
 *
 * Likewise **do not add delayMs to Fallback**: that is for preventing flicker when "the image arrives quickly",
 * while here having no avatar is normal (people who never logged in, machine identities never have avatars), so a delay would only show a blank first.
 *
 * # ⭐ The fourth state: **we do not know who it is at all**
 *
 * This is **the only real difference** between this component and those four existing pieces of code, and the reason it must exist.
 *
 * Existing agents have no `createdBy` (it was not recorded then and cannot be traced; see Agent.createdBy).
 * In that case:
 *
 *   - **Do not show a blank**: one missing in a column of avatars looks like broken rendering or data still loading;
 *   - **Do not show the initial of an empty string**: that renders an empty circle with no letter, same as above;
 *   - **Even less guess a person to fill in** (like the workspace owner): a wrong attribution would be read as fact.
 *
 * So when `email` is empty we render a **question-mark avatar**, and the tooltip says "creator not recorded".
 * "Unknown" is a state to **express honestly**, not an error to hide.
 */

import { Avatar, Tooltip } from "@heroui/react";

import type { UserProfile } from "@/types/domain";
import { usePrefs } from "@/prefs/PrefsContext";

/**
 * Gets the initial to display.
 *
 * The email fallback is **deliberate**: people who never logged in have an empty name, and then the email initial is still better than an
 * empty circle (at least the same person always gets the same letter, so you can still tell people apart in a column).
 */
function initialOf(name: string, email: string): string {
  const source = name.trim() || email.trim();
  // Use [...s] instead of s[0]: Chinese names are fine, but surrogate pairs like emoji would be cut in half by s[0]
  // and render as a garbled box. If nothing can be taken, return a placeholder, not an empty string.
  return [...source][0]?.toUpperCase() ?? "?";
}

/**
 * Gets the key for the `profiles` lookup table.
 *
 * ⚠️ **Keys are always lowercase emails**: `useUserProfiles` normalizes with `.trim().toLowerCase()`
 * internally, and the keys returned by the backend are lowercase too. So a direct lookup like `profiles[issue.author]`
 * **silently misses** when the email has uppercase letters, and that looks exactly like "no profile passed at all"
 * (both fall back to the initial), but is harder to find, because the code looks like it already passed the profile.
 *
 * It is one function so that **the correct way is the shortest way**, instead of every caller
 * writing `.toLowerCase()` and hoping nobody forgets.
 *
 * ⚠️ **This `.toLowerCase()` is not guarded by any frontend test**; do not assume it is. Tested: with it deleted,
 * the two new cases in 17-issues are still all green, because the author in the mock is always `MOCK_USER.email`,
 * which is already lowercase; the frontend fixtures cannot produce the "email with uppercase" shape.
 * What really pins this invariant is the backend `TestResolveUserProfilesLowercasesEmails`
 * (`server/core/agentactor_test.go`): it proves **the response keys are always lowercase**,
 * so looking up with the raw email here will miss. Read that test before changing this line.
 */
export function profileKey(email?: string): string {
  return (email ?? "").trim().toLowerCase();
}

/**
 * Gets the display name: the name if there is one, otherwise (that person has not logged in yet) fall back to the email.
 *
 * **Exported for callers that also show the name next to the avatar** (the author on the issue detail page,
 * commenters in the comment section): the visible name there and the name in this component's tooltip must follow the same rule;
 * writing `profile?.name || email` in each place is exactly the "this branch has been written four times" from the top of the file.
 *
 * ⚠️ When both are empty it returns **an empty string** ("we do not know who"). Callers getting an empty string **must not render an
 * empty span**: that looks like broken rendering (for precedent see where platform staff emails are blanked in data/api.ts);
 * on the avatar side this component falls back to the question mark, and on the text side nothing should be rendered.
 */
export function displayNameOf(email?: string, profile?: UserProfile): string {
  return profile?.name?.trim() || (email ?? "").trim();
}

export interface UserAvatarProps {
  /**
   * This person's email. **Empty means "we do not know who"** (existing data did not record the creator),
   * and then a question-mark avatar + a "not recorded" hint are rendered instead of a blank.
   */
  email?: string;
  /**
   * Display name and avatar. **May be omitted**: without it, it falls back to the email initial, fully working.
   *
   * The typical use is `profiles[agent.createdBy]`, and that lookup **may miss anyway**
   * (that person never logged in → not in the users table). So undefined is normal, not an error.
   */
  profile?: UserProfile;
  /**
   * Tailwind class for the avatar size. Default `size-6` (enough in a list row without stealing attention).
   *
   * Matches the three existing sizes: size-6 dense lists / size-7 people picker / size-8 member rows and the account menu.
   */
  sizeClass?: string;
  /**
   * A role note placed before the tooltip, like "Creator".
   *
   * **Not required**: when the same component is used in the member list, "who this is" is all the information,
   * and a prefix would be wordy.
   */
  label?: string;
  /** For e2e. Placed on the Avatar wrapper, not on `<img>`: that img does not exist while the image has not loaded. */
  testId?: string;
}

/**
 * Small round avatar, name on hover.
 *
 * Usage:
 *
 *	<UserAvatar email={a.createdBy} profile={profiles[a.createdBy ?? ""]}
 *	            label={t("agent.createdBy")} />
 */
export function UserAvatar({
  email,
  profile,
  sizeClass = "size-6",
  label,
  testId,
}: UserAvatarProps) {
  const { t } = usePrefs();

  const trimmed = (email ?? "").trim();

  /*
    The "we do not know who" branch is handled separately, **not** treated as "a person with an empty name".
    The two must be distinguishable in the UI: the former is "this information does not exist", the latter is "this person exists but never logged in".
    If merged, users seeing an initial avatar would think such a person really exists.
  */
  if (trimmed === "") {
    return (
      <Tooltip delay={0}>
        <Tooltip.Trigger aria-label={t("user.unknownActor")}>
          {/*
            Question-mark avatar. Uses Fallback instead of wrapping an Icon: shape, size, radius and
            alignment all match a real avatar **pixel for pixel**, so in a mixed column no circle is half a step bigger than the others.
            text-muted makes it step back visually: it is "no information" and should not stand out more than real people.
          */}
          <Avatar className={`${sizeClass} shrink-0`} data-testid={testId}>
            <Avatar.Fallback className="text-xs text-muted">?</Avatar.Fallback>
          </Avatar>
        </Tooltip.Trigger>
        <Tooltip.Content showArrow>
          <Tooltip.Arrow />
          <p className="text-xs">{t("user.unknownActor")}</p>
        </Tooltip.Content>
      </Tooltip>
    );
  }

  // name may be an empty string (that person has not logged in yet); always fall back to the email.
  // The same implementation as displayNameOf: callers that show the name next to the avatar use exactly that.
  const displayName = displayNameOf(trimmed, profile);
  const initial = initialOf(profile?.name ?? "", trimmed);

  return (
    <Tooltip delay={0}>
      {/*
        aria-label gives screen reader users a complete sentence. Visually the tooltip says the same thing,
        but that needs hover to be seen.
      */}
      <Tooltip.Trigger aria-label={label ? `${label}：${displayName}` : displayName}>
        <Avatar className={`${sizeClass} shrink-0`} data-testid={testId}>
          {/*
            alt is an empty string: the name is in the tooltip; having the screen reader read the avatar again is repetition.
            This is the standard way to write a "decorative image", not a missing value; same as AppShell/SettingsPage.
          */}
          <Avatar.Image src={profile?.avatarUrl} alt="" />
          <Avatar.Fallback className="text-xs">{initial}</Avatar.Fallback>
        </Avatar>
      </Tooltip.Trigger>
      <Tooltip.Content showArrow>
        <Tooltip.Arrow />
        {/*
          In the tooltip: a one-size-larger avatar + name + email.

          **The email must be on its own line**; do not show only the name: many people in a company share a name, while the email is
          unique. When the name is empty the two lines would be the same (both fall back to the email); then only one line is drawn:
          the same string twice looks like broken rendering.
        */}
        <div className="flex items-center gap-2 py-0.5">
          <Avatar className="size-8 shrink-0">
            <Avatar.Image src={profile?.avatarUrl} alt="" />
            <Avatar.Fallback className="text-xs">{initial}</Avatar.Fallback>
          </Avatar>
          <div className="flex min-w-0 flex-col">
            {label ? <span className="text-xs text-muted">{label}</span> : null}
            <span className="text-sm font-medium">{displayName}</span>
            {displayName === trimmed ? null : (
              <span className="text-xs text-muted">{trimmed}</span>
            )}
          </div>
        </div>
      </Tooltip.Content>
    </Tooltip>
  );
}
