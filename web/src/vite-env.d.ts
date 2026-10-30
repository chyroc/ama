/// <reference types="vite/client" />

/**
 * The three environment variables the frontend knows about.
 *
 * Without declaring them, `import.meta.env.VITE_XXX` is any in TS (vite/client provides an index
 * signature), so a misspelled variable name causes no error and just silently gets undefined at
 * runtime. Listing them here buys spell checking and completion.
 */
interface ImportMetaEnv {
  /**
   * API prefix, same-origin `/api` by default (forwarded to the backend by server.proxy in
   * vite.config.ts). To connect elsewhere, set it to a full URL without a trailing slash, such as
   * `http://10.0.0.5:8080/api`.
   */
  readonly VITE_API_BASE?: string;
  /** When set to `local`, use LocalApiClient (localStorage + seed data); any other value uses HTTP. */
  readonly VITE_API_MODE?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}

/**
 * Frontend version (short sha of the current commit), replaced at build time by `define` in
 * vite.config.ts.
 *
 * **It is an empty string when unavailable** (no .git / no git installed in the build container),
 * not undefined: define always replaces this identifier. But places that read it still wrap it in
 * try/catch and a typeof check: that is for execution paths that do not go through vite, such as
 * vitest / tsx, where it really does not exist.
 */
declare const __APP_VERSION__: string;
