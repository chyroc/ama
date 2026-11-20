/**
 * Shared reader for deployment-level UI branding (`GET /api/auth/providers` -> `ui`).
 *
 * Cached at module scope so AppShell, LandingPage, and ChatPage share a single request per page
 * load without extra round-trips. When `ui` is absent (default open-source deployment), callers
 * receive `null` and fall back to standard AMA defaults.
 */

import { useEffect, useState } from "react";
import { api, type UIBranding } from "@/data/api";

let cachedBranding: UIBranding | null | undefined;
let inflight: Promise<UIBranding | null> | null = null;

function fetchBrandingOnce(): Promise<UIBranding | null> {
  if (cachedBranding !== undefined) {
    return Promise.resolve(cachedBranding);
  }
  if (!inflight) {
    inflight = api
      .getAuthProviders()
      .then((res) => {
        cachedBranding = res.ui ?? null;
        const customTitle = cachedBranding?.title?.trim();
        if (customTitle && typeof document !== "undefined") {
          document.title = customTitle;
        }
        return cachedBranding;
      })
      .catch(() => {
        // Do not memoize transient network failures permanently; allow retry on next mount.
        inflight = null;
        return null;
      });
  }
  return inflight;
}

export function useUIBranding(): UIBranding | null {
  const [branding, setBranding] = useState<UIBranding | null>(cachedBranding ?? null);

  useEffect(() => {
    let alive = true;
    void fetchBrandingOnce().then((res) => {
      if (alive) {
        setBranding(res);
      }
    });
    return () => {
      alive = false;
    };
  }, []);

  return branding;
}
