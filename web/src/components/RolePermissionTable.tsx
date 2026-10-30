/**
 * The "role permissions" collapsible table. Sits at the bottom of the members page in workspace settings, collapsed by default.
 *
 * # The data is not hand-written
 *
 * The table content comes from `@/data/roleCapabilities`, which is **generated** from `server/authz/matrix.go`
 * (`cd server && go run ./cmd/gen-authz-capabilities`).
 *
 * ⚠️ **Do not hand-write a single permission line in this file**. A hand-written permission description always drifts: the next time someone adds
 * a route, the table silently goes out of date, and an outdated permission description is worse than none: users follow it and think they can do
 * something, get a 403, then ask an admin for permission, and the admin, looking at the same table, also thinks they should be able to.
 * Drift is blocked by the reconciliation test on the Go side (`go test ./shared/gopkg/authzdoc/`).
 *
 * This file only handles **how it is shown**: layout, checks and crosses, narrow-screen support, and the placement of the three notes under the table.
 *
 * # Why collapsed by default
 *
 * It is reference material, not the main content of this page. The main content of the members page is "who is in this workspace": nine times out of ten
 * an admin comes here to add people or change roles; the permission notes are only needed when they hesitate between "member or admin".
 * Expanded by default, that 18-row table would sit under the member list, and they would have to scroll past it every time.
 *
 * # ⚠️ Narrow screens use a second layout, not one table with horizontal scrolling
 *
 * **The first version was rejected by a screenshot on a real device**: it was only a table + `overflow-x-auto`; measurements said "the page does not overflow,
 * the container does not overflow", which looked like a pass. The real reason was that the feature-name column was squeezed to 52px, and "View workspace content"
 * was stacked vertically one character per line: an unreadable table. **The numbers said fine; the eyes said broken.**
 *
 * So below sm it switches to "one small card per capability, with the allowed roles as tags in a row". See the comments below.
 */

import { Disclosure } from "@heroui/react";
import { Icon } from "@iconify/react";
import {
  ACCOUNT_SCOPE_NOTE,
  NON_MEMBER_NOTE,
  ROLE_CAPABILITIES,
  ROLE_COLUMNS,
  ROLE_NOTE,
} from "@/data/roleCapabilities";
import type { WorkspaceRole } from "@/types/domain";
import { useT } from "@/prefs/PrefsContext";

export function RolePermissionTable() {
  const t = useT();
  return (
    <Disclosure>
      <Disclosure.Heading>
        <Disclosure.Trigger
          className="flex items-center gap-2 r-row px-2 py-1.5 text-left text-xs text-muted cursor-[var(--cursor-interactive)] hover:bg-surface-secondary hover:text-foreground"
          data-testid="role-permissions-toggle"
        >
          <Disclosure.Indicator>
            <Icon icon="gravity-ui:chevron-right" className="size-3" />
          </Disclosure.Indicator>
          <Icon icon="gravity-ui:lock" className="size-3.5" />
          <span className="font-medium">{t("rolePerm.title")}</span>
        </Disclosure.Trigger>
      </Disclosure.Heading>
      <Disclosure.Content>
        <Disclosure.Body className="px-2 pb-2">
          <div className="flex flex-col gap-3" data-testid="role-permissions-body">
            {/*
              ⚠️⚠️ **Narrow screens cannot use the same table; that was the first version, rejected by a screenshot on a real device.**

              The first version was "`overflow-x-auto` wrapper + table `w-full`" (copied from the table on UsagePage);
              measurements said neither the page nor the container overflowed, which looked like a pass. **But those numbers were false**:
              the real reason nothing overflowed was that **the feature-name column was squeezed to 52px**, so "View workspace content"
              was stacked vertically one character per line, with the four role columns crammed on the right: an unreadable table.

              The lesson: to judge "squeezed" you cannot just look at total width and scrollWidth; you must **measure cell by cell**;
              and the structural answer in this version is: **on narrow screens it should not be a table at all**.
              5 columns (feature + 4 roles) do not fit in 402px, and horizontal scrolling does not fix it:
              the checks are off screen, and the user has to swipe left and right to line up rows, hardly better than unreadable.

              So two layouts:
                sm and up    real table (`hidden sm:table`), scanned at a glance
                below sm     one small card per capability (`sm:hidden`), roles as chips in a row
              Both share the same data, both rendered from ROLE_CAPABILITIES; there is no second source of truth.
            */}
            <div className="overflow-x-auto">
              <table
                className="hidden w-full text-left text-xs sm:table"
                data-testid="role-permissions-table"
              >
                <thead>
                  <tr className="border-b border-separator text-muted">
                    {/*
                      The first column header is deliberately empty: below it are feature names, and the word "feature"
                      adds no information (the whole table is about features). Leaving it empty makes the four role columns stand out.
                    */}
                    <th className="py-2 pr-3 font-medium" />
                    {ROLE_COLUMNS.map((role) => (
                      <th
                        key={role}
                        // Role columns have a fixed narrow width + centered: checks and crosses are symbols, and centered they can be scanned down at a glance.
                        className="w-16 px-1 py-2 text-center font-medium"
                        data-testid={`role-column-${role}`}
                      >
                        {t(`enum.role.${role}`)}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {ROLE_CAPABILITIES.map((cap) => (
                    <tr
                      key={cap.id}
                      className="border-b border-separator/50 last:border-0"
                      data-testid={`role-capability-${cap.id}`}
                    >
                      <td className="py-2 pr-3 align-top">
                        <span className="text-foreground">{cap.label}</span>
                        {/*
                          detail goes on a new line instead of after label: after label it would make this column
                          very wide on narrow screens, pushing the four role columns off screen.
                        */}
                        {cap.detail ? (
                          <span className="mt-0.5 block text-[11px] leading-snug text-muted">
                            {cap.detail}
                          </span>
                        ) : null}
                      </td>
                      {ROLE_COLUMNS.map((role) => (
                        <td key={role} className="px-1 py-2 text-center align-top">
                          <RoleMark allowed={cap.roles.includes(role)} role={role} />
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            {/*
              Narrow-screen version: one small card per capability.
              Roles are no longer four columns but four tags in a row: **only the allowed ones are shown**,
              plus a line "the roles above can" to make the meaning clear, so no crosses are needed and nothing is ambiguous.
            */}
            <ul className="flex flex-col gap-2.5 sm:hidden" data-testid="role-permissions-cards">
              {ROLE_CAPABILITIES.map((cap) => (
                <li
                  key={cap.id}
                  className="flex flex-col gap-1 border-b border-separator/50 pb-2.5 last:border-0 last:pb-0"
                  data-testid={`role-capability-card-${cap.id}`}
                >
                  <span className="text-xs text-foreground">{cap.label}</span>
                  {cap.detail ? (
                    <span className="text-[11px] leading-snug text-muted">{cap.detail}</span>
                  ) : null}
                  <div className="mt-0.5 flex flex-wrap items-center gap-1">
                    <Icon icon="gravity-ui:check" className="size-3 shrink-0 text-success" />
                    {cap.roles.map((role) => (
                      <span
                        key={role}
                        className="rounded-md bg-surface-secondary px-1.5 py-0.5 text-[11px] text-muted"
                      >
                        {t(`enum.role.${role}`)}
                      </span>
                    ))}
                  </div>
                </li>
              ))}
            </ul>

            {/*
              The three notes under the table. **They are not optional extras**; each blocks one specific misunderstanding:

              1. ROLE_NOTE: "owner and admin are exactly the same". When those two columns are identical,
                 users first think "this table is unfinished". Text must say that being the same is correct.
              2. NON_MEMBER_NOTE: non-members. Deliberately not drawn as a fifth column (that would suggest they can get in
                 but cannot do anything), so it has to be explained here.
              3. ACCOUNT_SCOPE_NOTE: why account-level features are not in the table. Otherwise users would look for
                 "authorization management", not find it, and wonder if something is missing.

              All three come from the generated file, the same source as the table. If hand-written, the day the server really splits owner into its own level,
              these lines would not change with it, and then they would be wrong without any test turning red.
            */}
            <ul className="flex flex-col gap-1 text-[11px] leading-relaxed text-muted">
              <li className="flex gap-1.5" data-testid="role-note-owner">
                <Icon
                  icon="gravity-ui:circle-info"
                  className="mt-0.5 size-3 shrink-0 text-muted"
                />
                <span>{ROLE_NOTE}</span>
              </li>
              <li className="flex gap-1.5" data-testid="role-note-non-member">
                <Icon
                  icon="gravity-ui:circle-info"
                  className="mt-0.5 size-3 shrink-0 text-muted"
                />
                <span>{NON_MEMBER_NOTE}</span>
              </li>
              <li className="flex gap-1.5" data-testid="role-note-account-scope">
                <Icon
                  icon="gravity-ui:circle-info"
                  className="mt-0.5 size-3 shrink-0 text-muted"
                />
                <span>{ACCOUNT_SCOPE_NOTE}</span>
              </li>
            </ul>
          </div>
        </Disclosure.Body>
      </Disclosure.Content>
    </Disclosure>
  );
}

/**
 * The check or cross in one cell.
 *
 * # Why color alone is not enough
 *
 * Check uses success and cross uses neutral gray, but **the shapes themselves must be distinguishable** (check vs short dash),
 * otherwise users with color vision deficiency see two identical symbols.
 *
 * # Why "cannot" uses a short dash instead of a cross
 *
 * ✗ reads like "error / forbidden", while the meaning here is just "this role does not have this".
 * A table half full of red crosses looks like a failure report. A short dash is a neutral "none".
 *
 * # aria-label must stay
 *
 * Icons are empty to screen readers, so every cell must say "who + can or cannot".
 * Writing only "can" would make screen reader users hear a string of "can can cannot" without knowing which column it is.
 */
function RoleMark({ allowed, role }: { allowed: boolean; role: WorkspaceRole }) {
  const t = useT();
  const label = t(allowed ? "rolePerm.markAllowed" : "rolePerm.markDenied", {
    role: t(`enum.role.${role}`),
  });
  return allowed ? (
    <Icon
      icon="gravity-ui:check"
      className="inline-block size-3.5 text-success"
      aria-label={label}
    />
  ) : (
    <Icon
      icon="gravity-ui:minus"
      className="inline-block size-3.5 text-muted/40"
      aria-label={label}
    />
  );
}
