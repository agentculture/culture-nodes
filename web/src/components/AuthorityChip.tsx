import {
  DOTTED,
  LEDGER_AUTHORITY_EDGE_STYLE,
  SOLID,
  type EdgeStyle,
  type LedgerAuthority,
} from "../culture-design/edges";
import type { LedgerAuthorityValue } from "../api/types";

/**
 * Authority -> chip outline, straight off culture-design/edges.ts.
 *
 * `LEDGER_AUTHORITY_EDGE_STYLE` is the load-bearing part: an agent's own
 * unconfirmed claim (`proposed`) draws DASHED; anything a human confirmed or
 * a trusted runner/deterministic validator recorded directly draws SOLID.
 * The same dashed/solid line the canvas uses for edges is reused here so a
 * reader learns the semantic once and it holds everywhere.
 *
 * Two authorities are outside that map because they are not authority levels
 * so much as verdicts on a record:
 *   - `rejected` — a human *did* act, so the record has real authority
 *     behind it; SOLID, distinguished by its icon and label.
 *   - `superseded` — no longer the live record; DOTTED, the "reference /
 *     soft link, no ledger authority" style.
 */
export function edgeStyleForAuthority(
  authority: LedgerAuthorityValue,
): EdgeStyle {
  if (authority === "rejected") return SOLID;
  if (authority === "superseded") return DOTTED;
  return LEDGER_AUTHORITY_EDGE_STYLE[authority as LedgerAuthority];
}

const AUTHORITY_ICON: Record<LedgerAuthorityValue, string> = {
  proposed: "◌",
  confirmed: "✔",
  observed: "◎",
  derived: "ƒ",
  rejected: "✕",
  superseded: "↩",
};

export interface AuthorityChipProps {
  authority: LedgerAuthorityValue;
}

/**
 * A ledger authority chip. Icon + word + outline style — never colour alone
 * (PRD §8.8).
 */
export function AuthorityChip({ authority }: AuthorityChipProps) {
  const style = edgeStyleForAuthority(authority);
  const outline = style.strokeDasharray ? "dashed" : "solid";
  return (
    <span
      className={`authority-chip authority-chip--${authority} authority-chip--${outline}`}
      data-authority={authority}
      data-edge-style={style.name}
      style={{
        borderStyle: outline,
        borderWidth: `${Math.round(style.strokeWidth)}px`,
      }}
      title={style.meaning}
    >
      <span className="authority-chip__icon" aria-hidden="true">
        {AUTHORITY_ICON[authority]}
      </span>
      <span className="authority-chip__label">{authority}</span>
    </span>
  );
}

export default AuthorityChip;
