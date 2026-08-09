import { useEffect, useState } from "react";

const QUERY = "(prefers-reduced-motion: reduce)";

/**
 * Whether the viewer asked for reduced motion (PRD §8.4: "reduced-motion
 * mode preserves the same information without animated transitions").
 *
 * The CSS in styles/app.css already guards its keyframes with the same media
 * query; this hook exists so the *markup* can change too — a reduced-motion
 * viewer gets a static "running" badge in place of the pulsing accent, which
 * is information the animation would otherwise carry alone.
 */
export function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState<boolean>(() => {
    if (typeof window === "undefined" || !window.matchMedia) return false;
    return window.matchMedia(QUERY).matches;
  });

  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const list = window.matchMedia(QUERY);
    const onChange = () => setReduced(list.matches);
    setReduced(list.matches);
    if (list.addEventListener) {
      list.addEventListener("change", onChange);
      return () => list.removeEventListener("change", onChange);
    }
    // Safari < 14 and jsdom's older shims.
    list.addListener(onChange);
    return () => list.removeListener(onChange);
  }, []);

  return reduced;
}
