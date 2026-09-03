import type { ReactNode } from "react";

interface SegmentedToggleProps {
  id?: string;
  className?: string;
  label: string;
  children: ReactNode;
}

/** A shared frame for mutually exclusive, aria-pressed view controls. */
export default function SegmentedToggle({
  id,
  className,
  label,
  children,
}: SegmentedToggleProps) {
  return (
    <div
      id={id}
      className={["segmented-toggle", className].filter(Boolean).join(" ")}
      role="group"
      aria-label={label}
    >
      {children}
    </div>
  );
}
