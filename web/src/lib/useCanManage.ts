/**
 * "Can I do admin-type actions in the current workspace". Three states: `true` / `false` / `null`
 * (not known yet).
 *
 * # Why a hook, instead of each page computing it from workspace.members
 *
 * If the frontend compares strings itself, sooner or later it drifts from the server's permission
 * rules: the UI gives someone a read-only view
 * **while the server would actually allow it**. That is a wrong decision with zero errors, found
 * only when that person complains.
 *
 * So the role is always asked from the server (`GET /api/workspaces/{id}/my-role`); that endpoint
 * uses
 * the same function used for permission checks (core.RoleOf).
 *
 * # ⚠️ Why three states, not two states where "failure means cannot manage"
 *
 * Two states **flash**: while loading it first renders as false, showing "editing requires admin
 * permission", and 90ms later
 * the data comes back and it flips to a checkbox. Admins would see a "you have no permission"
 * flash every time they open the page,
 * which is more alarming than no hint, and gets reported as a bug in screenshots/recordings.
 *
 * Three states separate "not known yet" from "known, and no": **while not known the whole block is
 * not rendered**,
 * exactly matching this block's existing rule (when `providers === null` the whole block is also
 * not rendered;
 * see the file header of AgentIntegrationsSection). Both requests go out and come back at about
 * the same time,
 * so there is no extra wait.
 *
 * # On failure fall to false (cannot manage), not null
 *
 * This is **the conservative direction on purpose**, the opposite of "hide it if the authorization
 * list cannot be fetched":
 *
 *   authorization list cannot be fetched  → hide that optional config, do not block the main path
 *                                           (creating an agent)
 *   role cannot be determined             → treat as cannot manage, show read-only view + "please
 *                                           contact an admin"
 *
 * If we guessed "can manage", the UI would show editable controls, and the user would get a 403
 * from the server only after clicking save:
 * that error shows up after all their work is done. Guessing "cannot manage" at worst makes a real
 * admin
 * see one extra "admin permission required"; a refresh fixes it, and they also know whom to ask.
 */

import { useCallback } from "react";

import { api } from "@/data/api";
import { useAsyncData } from "@/lib/useAsyncData";

export function useCanManage(workspaceId: string): boolean | null {
  const fetchRole = useCallback(() => api.getMyRole(workspaceId), [workspaceId]);
  // The text is not used (this hook renders no error state; failure falls to false), but
  // useAsyncData requires one.
  const { data, error } = useAsyncData(fetchRole, "");
  if (error) return false;
  if (data === null) return null;
  return data.canManage === true;
}

/**
 * "Can I do write actions in the current workspace" (member and above). Three states, same as
 * useCanManage.
 *
 * # How it differs from useCanManage, and why they must not be mixed
 *
 *	useCanManage  owner / admin      edit members, edit authorization injection, edit credentials
 *	useCanWrite   owner / admin / member  edit agents, skills, schedules, **rollback**
 *
 * ⚠️ **Rollback needs write, not manage.** Guarding the rollback button with useCanManage would
 * make
 * members unable to see that button, while the server would allow it (`POST .../rollback` declares
 * ActionWrite). That is a wrong decision with zero errors: the feature exists, this person just
 * never sees it.
 * The reverse (using write where manage is needed) is worse: it exposes admin actions to ordinary
 * members.
 *
 * The check is on `role`, not `canManage`: the latter only answers the admin question.
 * viewer is the only excluded role; non-members cannot reach this page at all (403 from the Guard).
 */
export function useCanWrite(workspaceId: string): boolean | null {
  const fetchRole = useCallback(() => api.getMyRole(workspaceId), [workspaceId]);
  const { data, error } = useAsyncData(fetchRole, "");
  // Failure falls to false, same direction as useCanManage: if we guessed "can write", the user
  // would get a 403 only after clicking save,
  // and that error shows up after all their work is done.
  if (error) return false;
  if (data === null) return null;
  return data.role !== "" && data.role !== "viewer";
}
