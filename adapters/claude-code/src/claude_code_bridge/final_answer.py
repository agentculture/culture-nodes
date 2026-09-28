"""Parse a declared outcome only when its JSON object ends the final answer.

This module is copied byte for byte to every agent bridge. The bridge's
mapping selects its provider-specific final-text field before calling it.
"""

import json
import re

_FENCE = re.compile(r"(?:^|\n)```json[ \t]*\n(?P<object>.*?)\n```$", re.DOTALL)
_DECODER = json.JSONDecoder()


def declared_final_answer(text):
    """Return (outcome, output), preserving prose as output.summary if free."""
    if not isinstance(text, str):
        return None
    answer = text.strip()
    fence = _FENCE.search(answer)
    if fence:
        prefix = answer[: fence.start()].strip()
        candidate = fence.group("object").strip()
        try:
            parsed = json.loads(candidate)
        except (TypeError, ValueError):
            return None
    else:
        parsed = None
        prefix = ""
        # A valid outer object consumes the entire suffix. Start at the last
        # opening brace so earlier JSON in the prose cannot win.
        for start in range(answer.rfind("{"), -1, -1):
            if answer[start] != "{":
                continue
            try:
                candidate, end = _DECODER.raw_decode(answer, start)
            except ValueError:
                continue
            if not answer[end:].strip():
                parsed = candidate
                prefix = answer[:start].strip()
                break
        if parsed is None:
            return None

    if not isinstance(parsed, dict):
        return None
    # Two ending declarations would make the answer ambiguous. The prefix
    # may be prose, but it must not itself end in another declared outcome.
    if prefix and declared_final_answer(prefix) is not None:
        return None
    outcome = parsed.get("outcome")
    output = parsed.get("output", {})
    if not isinstance(outcome, str) or not outcome or not isinstance(output, dict):
        return None
    output = output.copy()
    if prefix:
        summary = output.get("summary")
        if isinstance(summary, str) and summary:
            output["summary"] = prefix + "\n\n" + summary
        elif summary:
            output["narrative"] = prefix
        else:
            output["summary"] = prefix
    return outcome, output
