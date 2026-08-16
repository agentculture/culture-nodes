"""Which revision of a bridge is actually running (task t32, issue #120 item 4).

**This file is byte-identical in every bridge that ships it**, exactly as
`preflight.py` is, and the same Go lint test
(`tests/lint/preflightsurface_test.go`) fails the build if it stops being. It
is a separate module from `preflight.py` only because that file was at the
repo's 1000-line hard limit; everything the shared-module rule says about
`preflight.py` applies here unchanged. It has no imports outside the stdlib
and no import from its own package.

## What this is for

`deploy/prod/deploy.sh` reinstalls the bridges and nothing reported which
revision a running one is on, so "is the fleet current?" was unanswerable
without inspecting installed source over ssh. That is not a hypothetical:
three dispatches this cycle reported `handover=true`, committed successfully,
and created no handover ref, because the bridges on thor and orin predated the
code that mints them. Nothing reported a problem — `internal/handover`
correctly records nothing when there is no fetchable ref, so a stale bridge and
an honest refusal produce byte-identical evidence. It took a `git for-each-ref`
on the host to notice.

## Why one answer would have been wrong

The two install shapes in this fleet have DIFFERENT answers, and that
difference is the whole design:

* the codex bridges are `uv tool install`ed COPIES under
  ~/.local/share/uv/tools/. Deliberately so — deploy.sh's own comment says
  why: an editable install would keep pointing at the shipped archive and
  break at the next deploy. The cost is that a copy carries no git and goes
  stale silently, so its revision must be STAMPED IN at install time or it is
  genuinely unknown.
* the claude bridges on spark are EDITABLE installs pointing at the repo
  source. They never go stale — and they can be serving uncommitted
  working-tree code, which is its own hazard and is reported as one.

So a design reporting only a git sha would be wrong for the first, and one
reporting only a stamp would be wrong (and stale) for the second. This reports
the revision, WHERE IT WAS LEARNED, and the install shape — and omits the
revision entirely when nothing can establish it, because an absent key reads
as absence where a guess reads as a fact.
"""

from __future__ import annotations

import json
import subprocess
from pathlib import Path
from typing import Any, Callable

#: The file a deploy stamps into the installed package so a copy install can
#: answer. Written by `deploy/prod/deploy.sh` into the shipped tree before
#: `uv tool install` copies it, and carried into the wheel by each adapter's
#: `[tool.hatch.build] artifacts` entry (it is VCS-ignored, so hatchling would
#: otherwise leave it out — which would make this whole mechanism a silent
#: no-op, the exact failure class it exists to close).
REVISION_STAMP_FILE = "_revision.json"

#: How a revision was learned. Reported alongside the revision itself because
#: the two carry different guarantees: a work tree is LIVE and a stamp is a
#: record of one past install, and a reader deciding whether to trust "the
#: fleet is current" needs to know which one they are looking at.
REVISION_FROM_GIT = "git_worktree"
REVISION_FROM_STAMP = "build_stamp"
REVISION_FROM_VCS_METADATA = "pep610_vcs"

#: What shape this installation is.
#:
#: `editable` and `source-tree` differ in how they were installed, not in
#: where the code is: both serve a live work tree, and both are reported
#: separately because only the first is a deliberate deployment choice.
#: `unknown` is not a synonym for `copy` — it means the package directory
#: could not even be located, and a reader must not take it as a statement
#: about staleness.
INSTALL_EDITABLE = "editable"
INSTALL_SOURCE_TREE = "source-tree"
INSTALL_COPY = "copy"
INSTALL_UNKNOWN = "unknown"

#: How long a `git` probe here gets. A deployment measurement must never be
#: the reason a capability request hangs.
GIT_PROBE_TIMEOUT_SECONDS = 5.0


def _full_commit_sha(value: Any) -> str:
    """*value* if it is an unambiguous 40-character lowercase hex commit id,
    else "".

    The same refusal `internal/handover`'s `validateFullSHA` makes, for the
    same reason: `HEAD`, a branch name and an abbreviation each mean something
    different tomorrow, and a deploy record nobody can resolve later is not a
    record. Mixed case is refused too — the same commit spelled two ways
    compares unequal, and this value exists to be compared.
    """
    if not isinstance(value, str) or len(value) != 40:
        return ""
    return value if all(c in "0123456789abcdef" for c in value) else ""


def _git_probe(package_dir: "Path") -> tuple[str, bool] | None:
    """(HEAD sha, working tree is dirty) for the work tree containing
    *package_dir*, or None when it is not in one.

    Both facts come from the same two subprocesses so they cannot describe
    different moments. `--is-inside-work-tree` is asked first and separately:
    a bare repo, an unborn HEAD and a plain directory all fail `rev-parse
    HEAD` identically, and only one of them is a work tree at all.
    """

    def run(*args: str) -> "subprocess.CompletedProcess[str] | None":
        try:
            return subprocess.run(  # noqa: S603,S607 # nosec B603,B607 - fixed binary, constant
                # argv, no shell. B607 (partial path) is deliberate: `git` is
                # resolved from PATH so the deployment chooses its own
                # toolchain, which is how every other git call in this
                # project works. Hardcoding /usr/bin/git would break the
                # hosts that install it elsewhere.
                ["git", "-C", str(package_dir), *args],
                capture_output=True,
                text=True,
                check=False,
                timeout=GIT_PROBE_TIMEOUT_SECONDS,
            )
        except (OSError, subprocess.SubprocessError):
            return None

    inside = run("rev-parse", "--is-inside-work-tree")
    if inside is None or inside.returncode != 0 or inside.stdout.strip() != "true":
        return None
    head = run("rev-parse", "HEAD")
    if head is None or head.returncode != 0:
        return None
    sha = _full_commit_sha(head.stdout.strip())
    if not sha:
        return None
    # Untracked files count. An editable install serves the DIRECTORY, so a
    # new .py file nobody committed is code that is running, and reporting
    # such a tree as clean would be the exact false reassurance this fact
    # exists to prevent. `--porcelain`'s default already respects .gitignore,
    # so a .venv or a __pycache__ does not make every bridge permanently
    # dirty.
    status = run("status", "--porcelain")
    # A status probe that failed is reported as NOT dirty rather than as
    # dirty: this bridge refuses to invent a verdict it did not measure, the
    # same rule `scope_guard` states for an unmeasurable workspace.
    dirty = bool(status and status.returncode == 0 and status.stdout.strip())
    return sha, dirty


def _read_stamp(package_dir: "Path") -> dict[str, Any]:
    """The build stamp a deploy wrote into the installed package, or {}."""
    try:
        raw = (package_dir / REVISION_STAMP_FILE).read_text(encoding="utf-8")
    except OSError:
        return {}
    try:
        stamp = json.loads(raw)
    except ValueError:
        # A stamp nobody can parse is not a revision. Ignored rather than
        # reported: a garbled record must read as "no record", never as a
        # fact.
        return {}
    return stamp if isinstance(stamp, dict) else {}


def _direct_url(distribution: str) -> dict[str, Any]:
    """This distribution's PEP 610 `direct_url.json`, or {}.

    It is the only authoritative answer to "was this installed editable" —
    `pip`/`uv` write `dir_info.editable` there — and for a VCS install it also
    carries the exact `vcs_info.commit_id`. Guessing from the presence of a
    `__editable__` finder, or from where `__file__` happens to point, is how
    an editable install and a copy that lives beside a checkout get confused.
    """
    try:
        from importlib import metadata
    except ImportError:  # pragma: no cover - stdlib since 3.8
        return {}
    try:
        raw = metadata.distribution(distribution).read_text("direct_url.json")
    except Exception:  # noqa: BLE001 - any packaging error means "not knowable"
        return {}
    if not raw:
        return {}
    try:
        parsed = json.loads(raw)
    except ValueError:
        return {}
    return parsed if isinstance(parsed, dict) else {}


def measure_deployment(
    *,
    package_dir: "Path | None",
    distribution: str,
    direct_url: dict[str, Any] | None = None,
    git_probe: "Callable[[Path], tuple[str, bool] | None]" = _git_probe,
) -> dict[str, Any]:
    """What revision this bridge is running, and how that was established.

    Precedence is deliberate and is pinned by
    `test_a_work_tree_outranks_a_stale_stamp`:

    1. **a work tree** — LIVE truth. An editable install serves whatever is in
       the tree right now, so a stamp written at some past install would be a
       record of code that is no longer running.
    2. **a build stamp** — what the deploy shipped. The only thing that can
       answer for a `uv tool install`ed copy, which has no git anywhere near
       it.
    3. **PEP 610 `vcs_info.commit_id`** — the exact commit a VCS install came
       from, recorded by the installer.
    4. **nothing** — the `revision` key is OMITTED, and `staleness` says why.
       Absence reads as absence; a null or an empty string would read as a
       fact about the deployment, which is the rule `HOST_KEYS` states for
       every other fact here.

    Every input is injectable so a test can assert both install shapes rather
    than whichever one happens to be running the suite — neither thor's copy
    nor spark's editable install is the one running pytest.
    """
    metadata_url = _direct_url(distribution) if direct_url is None else direct_url
    dir_info = metadata_url.get("dir_info") if isinstance(metadata_url, dict) else None
    editable = bool(isinstance(dir_info, dict) and dir_info.get("editable"))

    facts: dict[str, Any] = {"package": package_dir.name if package_dir else distribution}
    version = _distribution_version(distribution)
    if version:
        facts["package_version"] = version

    measured = git_probe(package_dir) if package_dir is not None else None
    if measured is not None:
        sha, dirty = measured
        facts["install_mode"] = INSTALL_EDITABLE if editable else INSTALL_SOURCE_TREE
        facts["revision"] = sha
        facts["revision_source"] = REVISION_FROM_GIT
        facts["revision_is_dirty"] = dirty
        facts["staleness"] = (
            "this install serves a live work tree, so it cannot go stale against the source it "
            "points at"
        )
        if dirty:
            facts["staleness"] = (
                "this install serves a live work tree with UNCOMMITTED changes: the revision names "
                "the last commit, and the code actually running is not that commit"
            )
        return facts

    facts["install_mode"] = INSTALL_COPY if package_dir is not None else INSTALL_UNKNOWN

    stamp = _read_stamp(package_dir) if package_dir is not None else {}
    stamped = _full_commit_sha(stamp.get("revision"))
    if stamped:
        facts["revision"] = stamped
        facts["revision_source"] = REVISION_FROM_STAMP
        if isinstance(stamp.get("stamped_at"), str):
            facts["stamped_at"] = stamp["stamped_at"]
        facts["staleness"] = (
            "this is a COPY of the source as it stood when it was installed; it does not track the "
            "repository and goes stale silently until it is reinstalled"
        )
        return facts

    vcs_info = metadata_url.get("vcs_info") if isinstance(metadata_url, dict) else None
    from_vcs = _full_commit_sha(vcs_info.get("commit_id")) if isinstance(vcs_info, dict) else ""
    if from_vcs:
        facts["revision"] = from_vcs
        facts["revision_source"] = REVISION_FROM_VCS_METADATA
        facts["staleness"] = (
            "this is a copy built from a pinned commit; it does not track the repository and goes "
            "stale silently until it is reinstalled"
        )
        return facts

    facts["staleness"] = (
        "this install's revision CANNOT BE ESTABLISHED: it is not in a git work tree, it "
        f"carries no deploy {REVISION_STAMP_FILE} stamp, and its install metadata names no "
        "commit. Treat it as "
        "of unknown age — a bridge whose revision nobody can check is exactly the state issue #120 "
        "was found in"
    )
    return facts


def _distribution_version(distribution: str) -> str:
    try:
        from importlib import metadata

        return metadata.version(distribution)
    except Exception:  # noqa: BLE001 - an uninstalled package has no version, which is not an error
        return ""


def deployment_facts(module: Any, distribution: str) -> dict[str, Any]:
    """`measure_deployment` for an already-imported bridge package.

    The package directory is taken from the module's own `__file__`, which is
    where the code that is RUNNING lives — not from a path derived from
    configuration, which could name a checkout this process never loaded.
    """
    location = getattr(module, "__file__", None)
    package_dir = Path(location).resolve().parent if location else None
    return measure_deployment(package_dir=package_dir, distribution=distribution)
