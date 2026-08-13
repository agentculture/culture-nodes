import { useEffect, useState, type RefObject } from "react";

/**
 * Observer-gated animation (task t31, the MeshCanvas craft idiom ported to
 * CSS-animated DOM): `true` only while the given element is actually on
 * screen AND the tab is visible. Consumers map it onto a `data-motion`
 * attribute whose `paused` value freezes every CSS animation via
 * `animation-play-state` — the CSS-animation equivalent of MeshCanvas's
 * IntersectionObserver + visibilitychange rAF pause.
 *
 * Environments without IntersectionObserver (jsdom) simply skip the
 * viewport half and gate on tab visibility alone — the honest fallback is
 * "animate", never a permanently-frozen view.
 */
export function useAnimationGate(ref: RefObject<Element | null>): boolean {
  const [onScreen, setOnScreen] = useState(true);
  const [tabVisible, setTabVisible] = useState(
    () =>
      typeof document === "undefined" || document.visibilityState !== "hidden",
  );

  useEffect(() => {
    const element = ref.current;
    if (!element || typeof IntersectionObserver === "undefined") return;
    const observer = new IntersectionObserver((entries) => {
      for (const entry of entries) setOnScreen(entry.isIntersecting);
    });
    observer.observe(element);
    return () => observer.disconnect();
  }, [ref]);

  useEffect(() => {
    const onVisibility = () =>
      setTabVisible(document.visibilityState !== "hidden");
    document.addEventListener("visibilitychange", onVisibility);
    return () =>
      document.removeEventListener("visibilitychange", onVisibility);
  }, []);

  return onScreen && tabVisible;
}
