import { useState } from "react";
import { getDecisionActorID, setDecisionActorID } from "../api/decision-token";

/**
 * The "who is deciding" field, remembered across sittings (task t10,
 * extracted for the ticket page in t18).
 *
 * The token authenticates the DEPLOYMENT, not the person — see
 * api/decision-token.ts. Accountability rides entirely on
 * `decider_actor_id`, which is why it is typed in and never inferred, and
 * why it is remembered in `localStorage` rather than session storage: an
 * operator's own id is not a credential, and re-typing it every tab is how a
 * required field turns into a copy-pasted placeholder.
 */
export function DeciderActorField({
  id,
  value,
  onChange,
}: {
  /** DOM id, so two of these on one page keep distinct labels. */
  id: string;
  value: string;
  onChange: (actorID: string) => void;
}) {
  return (
    <div className="inbox-card__field">
      <label htmlFor={id}>Decider actor id</label>
      <input
        id={id}
        value={value}
        onChange={(event) => {
          onChange(event.target.value);
          setDecisionActorID(event.target.value);
        }}
      />
    </div>
  );
}

/** The remembered decider id as controlled state, seeded from storage. */
export function useDeciderActorID(): [string, (next: string) => void] {
  const [actorID, setActorID] = useState(getDecisionActorID);
  return [actorID, setActorID];
}

export default DeciderActorField;
