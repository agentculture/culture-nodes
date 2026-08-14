/// <reference types="vite/client" />

/**
 * `ImportMetaEnv`'s built-in members (`MODE`, `BASE_URL`, `DEV`, `PROD`,
 * `SSR`) come from the `vite/client` reference above. The one entry this
 * repo adds is task t26's operator-set preserve-branch forge link template
 * (issue #49, spec claim c32 / honesty h21) — see
 * web/src/components/NodeDetailPanel.tsx and web/README.md for what it
 * does and why a link may only come from this, never a guess from a
 * remote's name.
 */
interface ImportMetaEnv {
  readonly VITE_PRESERVE_BRANCH_URL_TEMPLATE?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
