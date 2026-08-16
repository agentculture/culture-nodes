"""Repository identity -> a checkout on THIS host (task t2, issue #125).

One module, carried verbatim by every bridge that executes inside a git
checkout — the same arrangement `preflight.py`, `deployment.py` and
`dialin.py` are in, and for the same reason: three adapters each inlining
their own copy of one rule is how `resolve_actor_row_id` shipped as the
same bug in three deploy lanes. Byte-identity is enforced in Go
(`tests/lint/repositoryidentity_test.go`), so change one copy and copy it
to the rest verbatim — never run a formatter per-adapter.

## What this replaces

A trigger-created run's `input` *is* the event payload (task t17b), and a
payload carries no checkout path. Every triggered pr-upkeep dispatch
therefore fell through to `Config.only_allowed_repo()`, which infers the
repository from the allowlist holding *exactly one* entry — and returns
None the moment it holds two. spark's developer bridge legitimately
allowlists more than one, so every triggered run failed closed on
`input.repo is required` (issue #125, attempt 01M04Q276TPTA9GT8ME7PFVRFY).

Cardinality was the wrong signal. An allowlist is a PERMISSION surface: it
answers "may this bridge touch that directory", and it is allowed to hold
many entries. Which repository an actor's lane *is* is a different fact, and
it now lives where the other per-actor deployment facts already live — the
control plane's own registry, beside `metadata.handover_remote`. The engine
sends it as a NAME (`internal/actors.RepositoryIdentityKey`), never a path,
because a path chosen on the control-plane host need not exist on the
actor's (the t16 decision, issue #74). Resolving that name to a directory is
this module's whole job, and only this host can do it.

## How a name becomes a path

Two mechanisms, tried in that order:

1. **Declared.** `Config.repo_identities` maps an identity to a path the
   operator wrote down. This is the deterministic answer, and it is the one
   a host needs when its checkout directories are not named after the
   repository — spark's `.worktrees.culture-nodes/<lane>` worktrees are
   exactly that shape.
2. **Inferred.** Otherwise the identity's repository segment
   (`agentculture/culture-nodes` -> `culture-nodes`, the slug shape
   api/actor-protocol/README.md documents) is matched against the final
   component of every path this bridge is permitted to work in: each
   `repo_allowlist` entry, and `<root>/<name>` for each
   `repo_allowlist_prefixes` root that actually has such a directory. This
   is what keeps the ordinary deployment — one allowlist entry named after
   its repository — needing no bridge-config edit at all.

Inference cannot widen what the bridge may touch: every candidate is drawn
from the permitted surface, and the single survivor is put through
`repo_allowed` regardless. A declaration cannot widen it either — a
declared path outside the allowlist is refused, because a declaration is
the operator saying *which* repository, not saying *may*.

## Why both refusals are named

`only_allowed_repo()` already fails closed on ambiguity, and this module
mirrors that shape deliberately (claim c51): `repo_allowed` accepts an exact
entry OR a strict child of a scoped prefix, so one name really can reach two
permitted paths. Picking the first would silently check out a lane nobody
named — the failure that is indistinguishable from success until a commit
lands in the wrong worktree. Two candidates refuse; zero candidates refuse;
each says which identity, and each carries a hint naming the one config key
that fixes it.

The hint rides INSIDE the `error` string as well as in its own field: task
t3 carries a bridge's `error` and `class` into the attempt result, and a
refusal whose remediation was dropped on the way to the run view is the
diagnosis-by-reproduction that #125 already cost once.

Standard library only, like every other module in these adapters.
"""

from __future__ import annotations

import os
from collections.abc import Callable, Iterable, Mapping
from dataclasses import dataclass
from pathlib import Path

#: The §13.1 `input` key the control plane sends the identity under. Must
#: equal `internal/actors.RepositoryIdentityKey`; the Go guard checks it.
#:
#: Deliberately NOT `repo`, which is the checkout PATH a workflow author may
#: bind explicitly and every bridge validates against its allowlist. The two
#: answer different questions, and merging them would make an identity
#: indistinguishable from a path the moment a bridge read either.
INPUT_KEY = "repository_identity"

#: §13.1 error classes. Same literals as each bridge's
#: `mapping.CLASS_ACTOR_REJECTED_INPUT` and the policy class server.py
#: already answers a disallowed `input.repo` with — restated here rather
#: than imported so this module stays free of any per-bridge import.
CLASS_REJECTED_INPUT = "actor_rejected_input"
CLASS_AUTH_OR_POLICY = "auth_or_policy"

#: The named refusals. A name is part of the wire contract: an operator
#: greps for it, and task t3 puts it in the run view.
ERROR_INVALID = "repository_identity_invalid"
ERROR_UNKNOWN = "repository_identity_unknown"
ERROR_AMBIGUOUS = "repository_identity_ambiguous"
ERROR_NOT_PERMITTED = "repository_identity_not_permitted"

#: How many colliding paths an ambiguity refusal names before it stops.
#: The paths come from this bridge's own configuration, not from a caller,
#: so this is legibility rather than a safety bound.
MAX_NAMED_CANDIDATES = 8


@dataclass(frozen=True)
class Refusal:
    """A named, hinted refusal, ready to become an HTTP response body."""

    status: int
    name: str
    error_class: str
    detail: str
    hint: str

    @property
    def body(self) -> dict[str, str]:
        """The §13.1 error body. `error` carries the name and the hint too,
        because that is the field the attempt result keeps."""
        return {
            "error": f"{self.name}: {self.detail} (hint: {self.hint})",
            "class": self.error_class,
            "hint": self.hint,
        }


@dataclass(frozen=True)
class Resolution:
    """`repo` set means resolved; `refusal` set means refused; neither set
    means no identity was supplied and the caller's own fallback applies."""

    repo: str | None = None
    refusal: Refusal | None = None


def resolve_for_input(cfg, raw_input: Mapping[str, object]) -> Resolution:
    """Resolve `input.repository_identity` against *cfg*'s permitted surface.

    The one call site a bridge needs. *cfg* is the adapter's own `Config`;
    every field read here carries the same name in all of them.
    """
    return resolve(
        raw_input.get(INPUT_KEY),
        allowlist=cfg.repo_allowlist,
        prefixes=cfg.repo_allowlist_prefixes,
        declared=cfg.repo_identities,
        is_allowed=cfg.repo_allowed,
    )


def resolve(
    raw_identity: object,
    *,
    allowlist: Iterable[str],
    prefixes: Iterable[str],
    declared: Mapping[str, str] | None,
    is_allowed: Callable[[str], bool],
) -> Resolution:
    """Map *raw_identity* to one permitted checkout, or refuse by name."""
    if raw_identity is None:
        return Resolution()
    if not isinstance(raw_identity, str):
        return Resolution(
            refusal=Refusal(
                status=400,
                name=ERROR_INVALID,
                error_class=CLASS_REJECTED_INPUT,
                detail=(
                    f"input.{INPUT_KEY} must be a string naming a repository, "
                    f"got {type(raw_identity).__name__}"
                ),
                hint=(
                    "the control plane sends this key from the actor's "
                    "metadata.repository_identity, so a non-string value means the "
                    "registration holds one — re-register the actor with a name"
                ),
            )
        )

    identity = raw_identity.strip()
    if not identity:
        # An absent identity is not an error: an actor that declares none
        # dispatches exactly as it did before this key existed.
        return Resolution()

    declared_path = (declared or {}).get(identity)
    if declared_path:
        candidates = [_normalized(declared_path)]
    else:
        candidates = sorted(_infer(identity, allowlist, prefixes))

    if not candidates:
        return Resolution(refusal=_unknown(identity))
    if len(candidates) > 1:
        return Resolution(refusal=_ambiguous(identity, candidates))

    repo = candidates[0]
    if not is_allowed(repo):
        return Resolution(refusal=_not_permitted(identity, repo))
    return Resolution(repo=repo)


def _infer(identity: str, allowlist: Iterable[str], prefixes: Iterable[str]) -> set[str]:
    """Every permitted path whose final component is *identity*'s repository
    segment. An empty set is a miss, more than one is a collision."""
    name = identity.rsplit("/", 1)[-1]
    if not _is_plain_directory_name(name):
        return set()

    found: set[str] = set()
    for entry in allowlist:
        path = _normalized(entry)
        if Path(path).name == name:
            found.add(path)
    for root in prefixes:
        candidate = _normalized(os.path.join(_normalized(root), name))
        # Existence is the test on purpose: a prefix root is a directory this
        # host mints worktrees into, so `<root>/<name>` is a candidate only
        # when it is actually there. Without that, every identity would
        # "resolve" and the miss would resurface as a checkout failure deeper
        # in, where it reads as an engine fault rather than a naming one.
        if Path(candidate).is_dir():
            found.add(candidate)
    return found


def _is_plain_directory_name(name: str) -> bool:
    """A single directory component, and not a traversal.

    `repo_allowed` is still the last word, but a name that could be joined
    onto a prefix root is refused before the join rather than after it: a
    refusal naming the identity is a better diagnostic than one naming a
    path the operator never wrote.
    """
    if name in ("", ".", ".."):
        return False
    if os.sep in name or (os.altsep and os.altsep in name):
        return False
    return not Path(name).is_absolute()


def _normalized(path: str) -> str:
    """Symlinks and `..` collapsed, `~` expanded — the same normalization
    `Config._normalize_allowlist` and `repo_allowed` apply, so a comparison
    between them is a plain string equality."""
    try:
        return str(Path(path).expanduser().resolve())
    except OSError:
        return str(path)


def _unknown(identity: str) -> Refusal:
    return Refusal(
        status=400,
        name=ERROR_UNKNOWN,
        error_class=CLASS_REJECTED_INPUT,
        detail=(
            f"repository identity {identity!r} does not name any checkout this bridge "
            "is permitted to work in"
        ),
        hint=(
            f"map it in this bridge's repo_identities config "
            f'({{{identity!r}: "/path/to/checkout"}}), or add a checkout named after '
            "it to repo_allowlist — the identity is a name, and only this host knows "
            "which directory it means"
        ),
    )


def _ambiguous(identity: str, candidates: list[str]) -> Refusal:
    shown = candidates[:MAX_NAMED_CANDIDATES]
    listed = ", ".join(shown)
    if len(candidates) > len(shown):
        listed += f", ... ({len(candidates) - len(shown)} more)"
    return Refusal(
        status=400,
        name=ERROR_AMBIGUOUS,
        error_class=CLASS_REJECTED_INPUT,
        detail=(
            f"repository identity {identity!r} names {len(candidates)} checkouts this "
            f"bridge may work in ({listed}); this bridge will not pick one for you"
        ),
        hint=(
            "declare the one this actor's lane is in under repo_identities in this "
            "bridge's config, so the choice is recorded rather than guessed"
        ),
    )


def _not_permitted(identity: str, repo: str) -> Refusal:
    return Refusal(
        status=403,
        name=ERROR_NOT_PERMITTED,
        error_class=CLASS_AUTH_OR_POLICY,
        detail=(
            f"repository identity {identity!r} maps to {repo!r}, which is not in this "
            "bridge's configured allowlist"
        ),
        hint=(
            "a repo_identities declaration says WHICH repository, never that the bridge "
            "may touch it — add that path to repo_allowlist (or a scoped "
            "repo_allowlist_prefixes root), or point the declaration at a permitted checkout"
        ),
    )
