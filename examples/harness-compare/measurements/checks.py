#!/usr/bin/env python3
"""Mechanical checks and the 5/3/1 anchor rating for a measurement answer.

Split out of ``run.py`` so the dispatch half (revision gate, serial run
creation, grade posting) and the judgement half can each be read on their
own — and so this half is testable with nothing but a string.

Zero third-party dependencies (``re`` and ``pathlib`` only).

WHAT A RATING MEANS, EXACTLY
----------------------------

The manifest's ``anchors`` are prose a *human* grader reads. This module
implements the mechanical floor under them, and it is deliberately a floor:
it reads an actor's summary text, not its understanding.

- ``5`` — the expected fact is present **and** the answer points at
  something specific: a ``path:line`` citation for the expected path, a
  ``line N`` / ``L N`` reference, or a backtick-quoted code span. And the
  answer is not padded (below).
- ``3`` — the expected fact is present but the answer is **uncited** (no
  specific reference at all) **or padded**: it names
  ``PADDING_THRESHOLD`` or more distinct file paths *other than* the
  expected one. Padding is graded down because "here are nine files, one of
  them is right" is a different quality of answer from "it is this one",
  and the manifest's own ``3`` anchors say so ("pads the answer with
  unrelated file references").
- ``1`` — the expected fact is absent, or the run did not produce an answer
  at all (failed, cancelled, timed out, empty summary). ``rate()`` is only
  ever handed a summary; the *run failed* case is the caller's, and
  ``FAILED_RUN`` below is the verdict it uses.

The three check kinds differ only in what counts as a citation:

- ``grep-cites-file-line`` — a ``path:line`` citation whose path contains
  ``expect``. Nothing else counts: the rule asked for a file AND a line.
- ``seeded-defect-named`` — any specific reference (``path:line``,
  ``line N``, or a quoted code span), because the seeded defect is named by
  a token, and pointing at it can mean quoting the changed line as easily
  as citing it.
- ``tests-named`` — a ``path:line`` citation for ``expect``, a quoted code
  span, or a named ``test_*`` function; "which assertions" is what the
  manifest's 5-anchor asks for beyond the bare filename.

FABRICATION IS BEST-EFFORT, AND SAYS SO
---------------------------------------

``fabricated_paths`` returns the file paths an answer cited that do not
exist in a checkout the runner *can* read. It is explicitly not proof: the
actor ran against its own checkout on its own host, which the runner has no
access to (a path is meaningful on exactly one machine — #74). What it
catches is the common, cheap case: a path that does not exist in this
repository at all. A path that exists here but not on the actor's host, or
vice versa, is invisible to it. The flag lands in the grade notes as a
signal for a human reading the grade, never as a rating input.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any, Iterable

#: How many *other* file paths an answer may name before it counts as
#: padded. Three is the smallest number that cannot be reached by an answer
#: that cites the right file and one neighbour.
PADDING_THRESHOLD = 3

#: The verdict a caller records when the run itself never produced an
#: answer. Its rating is 1 by the anchor scale ("or gives no answer").
FAILED_RUN = "no-answer"

#: ``path/to/file.py:123`` — a path with an extension, then a line number.
#: Deliberately narrow: a bare ``foo:12`` (no dot) is prose, not a citation.
_CITATION_RE = re.compile(r"(?<![\w/.-])([A-Za-z0-9_][\w./-]*\.[A-Za-z0-9_]+):(\d+)\b")

#: Any file-ish path with an extension, cited or not — used for the padding
#: count and for the fabrication probe.
_PATH_RE = re.compile(r"(?<![\w/.-])([A-Za-z0-9_][\w./-]*\.[A-Za-z0-9_]+)\b")

#: ``line 179``, ``lines 179-181``, ``L179``.
_LINE_REF_RE = re.compile(r"\b(?:lines?\s+\d+|L\d+)\b", re.IGNORECASE)

#: A backtick-quoted span — the answer showing the thing rather than
#: describing it.
_QUOTED_RE = re.compile(r"`[^`\n]+`")

#: ``test_something`` — a named test function.
_TEST_FUNC_RE = re.compile(r"\btest_[A-Za-z0-9_]+\b")

KINDS = ("grep-cites-file-line", "seeded-defect-named", "tests-named")


class CheckError(Exception):
    """An unknown check kind was requested."""


def citations(text: str) -> list[tuple[str, int]]:
    """Every ``path:line`` citation in *text*, in order of appearance."""
    return [(m.group(1), int(m.group(2))) for m in _CITATION_RE.finditer(text)]


def cited_paths(text: str) -> list[str]:
    """Every distinct file-ish path named in *text*, in order of appearance."""
    seen: list[str] = []
    for match in _PATH_RE.finditer(text):
        path = match.group(1)
        if path not in seen:
            seen.append(path)
    return seen


def _matches(path: str, expect: str) -> bool:
    """Whether *path* is the path *expect* names.

    Substring, both ways and case-insensitively: an actor may cite the
    repo-relative path the rule expects, an absolute path that ends with it,
    or just the basename.
    """
    low_path, low_expect = path.lower(), expect.lower()
    return low_expect in low_path or low_path in low_expect


def citation_for(text: str, expect: str) -> str | None:
    """The first ``path:line`` citation in *text* whose path is *expect*."""
    for path, line in citations(text):
        if _matches(path, expect):
            return f"{path}:{line}"
    return None


def unrelated_paths(text: str, expect: str) -> list[str]:
    """Distinct paths named in *text* that are not the expected one."""
    return [p for p in cited_paths(text) if not _matches(p, expect)]


def is_padded(text: str, expect: str) -> bool:
    """Whether the answer names ``PADDING_THRESHOLD`` or more other paths."""
    return len(unrelated_paths(text, expect)) >= PADDING_THRESHOLD


def _specific_reference(text: str, expect: str, kind: str) -> str | None:
    """The specific thing this answer points at, or ``None`` if it points at
    nothing more precise than a name. What counts is per-kind; see the
    module docstring."""
    cited = citation_for(text, expect)
    if cited:
        return cited
    if kind == "grep-cites-file-line":
        return None
    quoted = _QUOTED_RE.search(text)
    if kind == "seeded-defect-named":
        line_ref = _LINE_REF_RE.search(text)
        if line_ref:
            return line_ref.group(0)
        return quoted.group(0) if quoted else None
    # tests-named. The test-function probe runs over the text with every
    # file path REMOVED: "tests/test_cli.py" contains "test_cli", so
    # searching the raw text would read the filename the answer was asked
    # for as if it were the named assertion it was asked for on top.
    stripped = _PATH_RE.sub(" ", text)
    func = _TEST_FUNC_RE.search(stripped)
    if func:
        return func.group(0)
    return quoted.group(0) if quoted else None


def rate(kind: str, expect: str, summary: str | None) -> dict[str, Any]:
    """Apply the *kind* check with *expect* to *summary* and rate it.

    Returns a dict — the shape the runner writes into both the report and
    the grade notes:

    ``kind``, ``expect``, ``passed`` (the mechanical check), ``rating``
    (5/3/1), ``verdict`` (``cited`` / ``uncited`` / ``padded`` /
    ``absent`` / ``no-answer``), ``citation`` (what it pointed at, if
    anything) and ``reason`` (one sentence, for a human reading the grade).
    """
    if kind not in KINDS:
        raise CheckError(f"unknown check kind {kind!r}; expected one of {', '.join(KINDS)}")
    text = (summary or "").strip()
    if not text:
        return {
            "kind": kind,
            "expect": expect,
            "passed": False,
            "rating": 1,
            "verdict": FAILED_RUN,
            "citation": None,
            "reason": "the run produced no summary text to check",
        }
    if expect.lower() not in text.lower():
        return {
            "kind": kind,
            "expect": expect,
            "passed": False,
            "rating": 1,
            "verdict": "absent",
            "citation": None,
            "reason": f"the expected fact {expect!r} does not appear in the summary",
        }
    reference = _specific_reference(text, expect, kind)
    padded = is_padded(text, expect)
    if reference and not padded:
        return {
            "kind": kind,
            "expect": expect,
            "passed": True,
            "rating": 5,
            "verdict": "cited",
            "citation": reference,
            "reason": f"names {expect!r} and points at {reference}",
        }
    if padded:
        others = unrelated_paths(text, expect)
        return {
            "kind": kind,
            "expect": expect,
            "passed": True,
            "rating": 3,
            "verdict": "padded",
            "citation": reference,
            "reason": (
                f"names {expect!r} but pads the answer with {len(others)} other file "
                f"references ({', '.join(others[:5])})"
            ),
        }
    return {
        "kind": kind,
        "expect": expect,
        "passed": True,
        "rating": 3,
        "verdict": "uncited",
        "citation": None,
        "reason": f"names {expect!r} but points at nothing specific (no citation)",
    }


def fabricated_paths(summary: str | None, roots: Iterable[str | Path]) -> list[str]:
    """Paths cited in *summary* that exist under none of *roots*.

    Best-effort by construction — see the module docstring. An absolute path
    is probed as itself as well as relative to each root, because an actor
    may cite either form.
    """
    text = summary or ""
    root_paths = [Path(r) for r in roots]
    missing: list[str] = []
    for path in cited_paths(text):
        candidate = Path(path)
        if candidate.is_absolute() and candidate.exists():
            continue
        relative = path.lstrip("/")
        if any((root / relative).exists() for root in root_paths):
            continue
        missing.append(path)
    return missing
