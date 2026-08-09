// culture-design/mark.tsx
//
// Ported from agentculture/org's Mark.astro — pinned commit
// b4d939ba0aa354a5ae53065319a773e0013de698,
// site-astro/src/components/Mark.astro:
//
//   The organization's mark: three agents, two threads — the smallest
//   mesh that is still a culture. Echoes the hero constellation and the
//   favicon.
//
// Geometry (viewBox, path, circle radii/positions) is copied verbatim
// from the Astro source. Colors ride the same CSS custom properties as
// the rest of this layer (--mesh-node / --mesh-thread, defined in
// ./tokens.css) so the mark stays theme-aware with zero JS: light/dark
// follow prefers-color-scheme exactly as tokens.css defines it — there is
// no toggle upstream (see docs/adr/0001-culture-design-source.md).
//
// The placeholder `type FC<P> = (props: P) => any` this file carried under
// task t5 is gone: @types/react is on the dependency tree as of the web
// build-out, so the component is typed against React's own `FC` and
// type-checked by `tsc -b`. Geometry (viewBox, path, circle radii and
// positions) is unchanged from the Astro source.

import type { FC } from "react";

export interface MarkProps {
  /** Rendered width/height in px. Astro source default: 26. */
  size?: number;
  /**
   * Accessible title. The Astro source always renders `aria-hidden="true"`
   * (the mark is decorative next to sited text). Pass a title for
   * standalone/logo use where the mark needs an accessible name — doing so
   * switches the SVG to `role="img"` + `aria-labelledby` and drops
   * `aria-hidden`.
   */
  title?: string;
}

export const Mark: FC<MarkProps> = ({ size = 26, title }: MarkProps) => {
  const titleId = title ? "culture-design-mark-title" : undefined;

  return (
    <svg
      className="mark"
      width={size}
      height={size}
      viewBox="0 0 64 64"
      fill="none"
      aria-hidden={title ? undefined : true}
      role={title ? "img" : undefined}
      aria-labelledby={titleId}
    >
      {title ? <title id={titleId}>{title}</title> : null}
      <path
        d="M14 46 Q 24 38 33 21 M33 21 Q 40 36 51 41"
        stroke="var(--mesh-thread)"
        strokeWidth={2.5}
        strokeLinecap="round"
      />
      <circle cx={14} cy={46} r={5} fill="var(--mesh-node)" />
      <circle cx={33} cy={21} r={6.5} fill="var(--mesh-node)" />
      <circle cx={51} cy={41} r={4.5} fill="var(--mesh-node)" />
    </svg>
  );
};

export default Mark;
