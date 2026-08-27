"""`reap.py`'s unit tests (task t17).

Every test here builds its own throwaway repo and worktrees under pytest's
`tmp_path` and never looks at a path outside it. The two tests that let the
reaper actually remove something remove only a worktree the test itself
minted seconds earlier — nothing in this file can touch a real checkout,
which is the point: a reaper whose test run deletes work is not one anybody
would let near a host.

Covers both halves: `reap.py` decides, `reclaim.py` is the only module
that can change anything.

Mirrored field-for-field by `qwen_bridge`'s and `colleague_bridge`'s copies
(all-backends rule); only the import line differs.
"""

from __future__ import annotations

import os
import subprocess
import time

from qwen_bridge import reap, reclaim


def _git(repo, *args: str) -> str:
    proc = subprocess.run(["git", *args], cwd=repo, check=True, capture_output=True, text=True)
    return proc.stdout.strip()


def _init_repo(repo) -> None:
    repo.mkdir(parents=True, exist_ok=True)
    _git(repo, "init", "-q")
    _git(repo, "config", "user.email", "reap-test@example.com")
    _git(repo, "config", "user.name", "reap test")
    (repo / "README.md").write_text("# scratch\n")
    _git(repo, "add", "README.md")
    _git(repo, "commit", "-q", "-m", "init")


def _age(path, seconds: float) -> None:
    """Backdate every file under *path* so the age floor is satisfied. The
    reaper reads mtimes off the filesystem, so this is how a test says
    "this worktree has been idle for a day" without sleeping for one."""
    old = time.time() - seconds
    for base, _dirs, files in os.walk(path):
        for name in files:
            target = os.path.join(base, name)
            try:
                os.utime(target, (old, old))
            except OSError:
                pass


def _policy(root, **kwargs):
    defaults = {
        "permitted_roots": (str(root),),
        "active_workspaces": (),
        "min_idle_seconds": 3600.0,
    }
    defaults.update(kwargs)
    return reap.ReapPolicy(**defaults)


def _fixture(tmp_path, *, name="writer-a", branch="lane/a"):
    """A main repo plus one minted worktree on its own branch, backdated
    past the idle floor. The ordinary safe case every other test perturbs."""
    repo = tmp_path / "culture-nodes"
    _init_repo(repo)
    root = tmp_path / ".worktrees.culture-nodes"
    root.mkdir()
    worktree = root / name
    _git(repo, "worktree", "add", "-q", "-b", branch, str(worktree))
    (worktree / "work.txt").write_text("real work\n")
    _git(worktree, "add", "work.txt")
    _git(worktree, "commit", "-q", "-m", "the work nobody wants to lose")
    _age(worktree, 7200)
    _age(repo / ".git" / "worktrees" / name, 7200)
    return repo, root, worktree


def _only(repo, worktree, policy):
    plan = reap.plan(str(repo), policy, only=[str(worktree)])
    assert len(plan["decisions"]) == 1, plan
    return plan["decisions"][0]


# --- the safe case, which every refusal below is measured against -------


def test_clean_worktree_with_commits_on_a_branch_is_reapable(tmp_path):
    repo, root, worktree = _fixture(tmp_path)

    decision = _only(repo, worktree, _policy(root))

    assert decision["decision"] == reap.REAP
    assert decision["domain_outcome"] == reap.RECLAIMED
    assert decision["blockers"] == []
    assert decision["holds"] == []
    kinds = {e["kind"] for e in decision["evidence"]}
    assert "branch" in kinds
    # The command is named, not run: this is a plan.
    assert decision["command"] == ["git", "worktree", "remove", str(worktree.resolve())]
    assert worktree.is_dir()


def test_the_branch_evidence_survives_the_removal_it_authorises(tmp_path):
    """The whole safety argument: refs live in the SHARED .git, so removing
    the directory keeps the work. Proven by removing one and reading the
    branch back afterwards — not by citing the manual."""
    repo, root, worktree = _fixture(tmp_path)
    head = _git(worktree, "rev-parse", "HEAD")
    policy = _policy(root)
    decision = reclaim._decision_from_dict(_only(repo, worktree, policy))

    result = reclaim.execute(str(repo), decision, policy, perform=True)

    assert result["performed"] is True, result
    assert result["domain_outcome"] == reap.RECLAIMED
    assert not worktree.exists()
    assert _git(repo, "rev-parse", "refs/heads/lane/a") == head


def test_a_dry_run_is_the_default_and_removes_nothing(tmp_path):
    repo, root, worktree = _fixture(tmp_path)
    policy = _policy(root)
    decision = reclaim._decision_from_dict(_only(repo, worktree, policy))

    result = reclaim.execute(str(repo), decision, policy)

    assert result["performed"] is False
    assert result["command"] == ["git", "worktree", "remove", str(worktree.resolve())]
    assert worktree.is_dir()


# --- refusals ------------------------------------------------------------


def test_a_dirty_worktree_is_refused_and_names_what_is_uncommitted(tmp_path):
    repo, root, worktree = _fixture(tmp_path)
    (worktree / "half-written.py").write_text("def unfinished(:\n")
    (worktree / "README.md").write_text("# edited, never committed\n")

    decision = _only(repo, worktree, _policy(root))

    assert decision["decision"] == reap.REFUSE
    assert decision["domain_outcome"] == reap.RETAINED
    blocker = next(b for b in decision["blockers"] if b["code"] == "uncommitted_work")
    assert "half-written.py" in blocker["detail"]
    assert "README.md" in blocker["detail"]
    assert worktree.is_dir()


def test_a_dirty_worktree_with_no_preserve_ref_is_refused_by_that_case(tmp_path):
    """t17's acceptance bullet names this case specifically: the refusal has
    to be proven by a dirty worktree whose work is NOT preserved, not
    inferred from a clean-worktree test passing."""
    repo = tmp_path / "culture-nodes"
    _init_repo(repo)
    root = tmp_path / ".worktrees.culture-nodes"
    root.mkdir()
    worktree = root / "timed-out-writer"
    # --detach is exactly what `workspace.provision()` mints.
    _git(repo, "worktree", "add", "-q", "--detach", str(worktree))
    (worktree / "in-flight.py").write_text("# the deadline fired mid-edit\n")
    _age(worktree, 7200)

    preserve_refs = _git(repo, "for-each-ref", "--format=%(refname)", "refs/heads/preserve/")
    assert preserve_refs == "", "the fixture must have no preserve ref for this to prove anything"

    decision = _only(repo, worktree, _policy(root))

    assert decision["decision"] == reap.REFUSE
    codes = {b["code"] for b in decision["blockers"]}
    assert "uncommitted_work" in codes
    assert "in-flight.py" in next(
        b["detail"] for b in decision["blockers"] if b["code"] == "uncommitted_work"
    )
    assert (worktree / "in-flight.py").is_file()


def test_git_itself_refuses_the_removal_the_reaper_declined_to_ask_for(tmp_path):
    """The probed fact this module is built on, pinned so a future edit
    cannot quietly reintroduce --force and still look green."""
    repo, root, worktree = _fixture(tmp_path)
    (worktree / "uncommitted.txt").write_text("mine\n")

    proc = subprocess.run(
        ["git", "worktree", "remove", str(worktree)],
        cwd=repo,
        capture_output=True,
        text=True,
    )

    assert proc.returncode != 0
    assert "use --force" in proc.stderr
    assert (worktree / "uncommitted.txt").is_file()


def test_a_worktree_nested_in_another_writers_root_is_refused_by_name(tmp_path):
    """The real `.claude/worktrees/web-ux-quick-wins` shape: clean, on a
    branch, and still not ours — a writer dispatched at the repo root can
    already reach it."""
    repo = tmp_path / "culture-nodes"
    _init_repo(repo)
    nested = repo / ".claude" / "worktrees" / "web-ux-quick-wins"
    _git(repo, "worktree", "add", "-q", "-b", "web/ux-quick-wins", str(nested))
    _age(nested, 7200)

    # contained_by_roots is deliberately EMPTY: the refusal must not depend
    # on anyone having allowlisted the repo root. spark's bridges carry
    # per-lane allowlists that do not name the main checkout at all, and the
    # hazard is a fact about the filesystem either way.
    policy = reap.ReapPolicy(
        permitted_roots=(str(tmp_path / ".worktrees.culture-nodes"),),
        contained_by_roots=(),
        active_workspaces=(),
        min_idle_seconds=3600.0,
    )
    decision = _only(repo, nested, policy)

    assert decision["decision"] == reap.REFUSE
    blocker = next(b for b in decision["blockers"] if b["code"] == "nested_under_allowlisted_root")
    assert str(repo.resolve()) in blocker["detail"]
    assert decision["facts"]["containing_allowlisted_root"] == str(repo.resolve())
    # And it is clean with a branch — so the refusal is about ownership, not
    # about the work being at risk.
    assert any(e["kind"] == "branch" for e in decision["evidence"])
    assert nested.is_dir()


def test_the_main_checkout_and_the_reapers_own_worktree_are_refused(tmp_path):
    repo, root, worktree = _fixture(tmp_path)
    policy = _policy(root)

    main = _only(repo, repo, policy)
    assert main["decision"] == reap.REFUSE
    assert "main_worktree" in {b["code"] for b in main["blockers"]}

    plan = reap.plan(str(repo), policy, only=[str(worktree)])
    running_inside = reap.assess(
        str(repo),
        {
            "worktree": str(worktree),
            "head": plan["decisions"][0]["facts"]["head"],
            "branch": "refs/heads/lane/a",
            "detached": False,
            "bare": False,
            "locked": None,
            "prunable": None,
        },
        policy,
        self_paths=[str(worktree / "some" / "deep" / "cwd")],
    )
    assert running_inside.decision == reap.REFUSE
    assert "reaper_own_worktree" in {b.code for b in running_inside.blockers}


def test_a_worktree_outside_every_permitted_root_is_refused(tmp_path):
    repo, root, worktree = _fixture(tmp_path)

    decision = _only(repo, worktree, _policy(root, permitted_roots=(str(tmp_path / "elsewhere"),)))

    assert decision["decision"] == reap.REFUSE
    assert "outside_permitted_roots" in {b["code"] for b in decision["blockers"]}


def test_a_git_locked_worktree_is_refused(tmp_path):
    repo, root, worktree = _fixture(tmp_path)
    _git(repo, "worktree", "lock", "--reason", "operator is mid-bisect", str(worktree))

    decision = _only(repo, worktree, _policy(root))

    assert decision["decision"] == reap.REFUSE
    blocker = next(b for b in decision["blockers"] if b["code"] == "locked_by_git")
    assert "mid-bisect" in blocker["detail"]


def test_an_unreadable_status_fails_closed_rather_than_reading_as_clean(tmp_path):
    """ "We could not look" and "there is nothing there" must not produce the
    same decision."""
    repo, root, worktree = _fixture(tmp_path)
    decision = reap.assess(
        str(repo),
        {
            "worktree": str(tmp_path / "not-a-repo-at-all"),
            "head": None,
            "branch": None,
            "detached": True,
            "bare": False,
            "locked": None,
            "prunable": None,
        },
        _policy(root),
    )
    assert decision.decision == reap.REFUSE
    assert "worktree_missing" in {b.code for b in decision.blockers}


# --- the dangerous case: clean, real commits, no ref anywhere ------------


def test_commits_on_no_branch_are_secured_before_they_are_reaped(tmp_path):
    repo = tmp_path / "culture-nodes"
    _init_repo(repo)
    root = tmp_path / ".worktrees.culture-nodes"
    root.mkdir()
    worktree = root / "detached-writer"
    _git(repo, "worktree", "add", "-q", "--detach", str(worktree))
    (worktree / "orphan.py").write_text("# committed, but on no branch\n")
    _git(worktree, "add", "orphan.py")
    _git(worktree, "commit", "-q", "-m", "work reachable only from a detached HEAD")
    head = _git(worktree, "rev-parse", "HEAD")
    _age(worktree, 7200)
    _age(repo / ".git" / "worktrees" / "detached-writer", 7200)

    containing = _git(repo, "for-each-ref", "--contains", head, "--format=%(refname)")
    assert containing == "", "no ref may contain the commit, or this is not the dangerous case"

    policy = _policy(root)
    raw = _only(repo, worktree, policy)
    assert raw["decision"] == reap.PRESERVE_THEN_REAP
    assert raw["domain_outcome"] == reap.RECLAIMED
    assert raw["mint_ref"].startswith("preserve/reaped/detached-writer-")
    assert raw["evidence"] == [], "there is no evidence yet — that is why it must be minted"

    decision = reclaim._decision_from_dict(raw)
    secured = reclaim.secure(str(repo), decision)
    assert secured["secured"] is True, secured
    assert _git(repo, "rev-parse", f"refs/heads/{decision.mint_ref}") == head

    # Minting the ref is what converts it into the ordinary safe case — and
    # because it lands under the preserve prefix, it reads back as exactly
    # the evidence kind a timed-out writer's own rescue would have produced.
    after = _only(repo, worktree, policy)
    assert after["decision"] == reap.REAP
    assert "preserve_ref" in {e["kind"] for e in after["evidence"]}


def test_secure_refuses_to_move_a_ref_that_already_exists(tmp_path):
    repo, root, worktree = _fixture(tmp_path)
    head = _git(worktree, "rev-parse", "HEAD")
    decision = reap.Decision(
        path=str(worktree),
        decision=reap.PRESERVE_THEN_REAP,
        domain_outcome=reap.RECLAIMED,
        facts={"head": head},
        mint_ref="lane/a",
    )

    result = reclaim.secure(str(repo), decision)

    assert result["secured"] is False
    assert "already exists" in result["reason"]
    assert result["domain_outcome"] == reap.RETAINED


def test_the_swept_orphan_of_a_cancelled_run_is_reclaimed_with_its_work_kept(tmp_path):
    """t17's sweeper bullet end to end: an orphaned detached worktree is
    secured, then reclaimed, and the commit is still there afterwards."""
    repo = tmp_path / "culture-nodes"
    _init_repo(repo)
    root = tmp_path / ".worktrees.culture-nodes"
    root.mkdir()
    worktree = root / "cancelled-run"
    _git(repo, "worktree", "add", "-q", "--detach", str(worktree))
    (worktree / "salvage.py").write_text("# the cancelled run got this far\n")
    _git(worktree, "add", "salvage.py")
    _git(worktree, "commit", "-q", "-m", "orphaned by a cancel")
    head = _git(worktree, "rev-parse", "HEAD")
    _age(worktree, 7200)
    _age(repo / ".git" / "worktrees" / "cancelled-run", 7200)

    result = reclaim.sweep(str(repo), _policy(root), perform=True)

    steps = [s for s in result["performed"] if s["path"].endswith("cancelled-run")]
    assert any(s["step"] == "secure" and s["secured"] for s in steps), steps
    assert any(s["step"] == "execute" and s["performed"] for s in steps), steps
    assert not worktree.exists()
    assert _git(repo, "cat-file", "-t", head) == "commit"
    assert head in _git(repo, "for-each-ref", "--format=%(objectname)", "refs/heads/preserve/")


# --- idleness ------------------------------------------------------------


def test_without_a_session_registry_snapshot_everything_defers(tmp_path):
    """If the reaper cannot know what is running, it says so and holds."""
    repo, root, worktree = _fixture(tmp_path)

    decision = _only(repo, worktree, _policy(root, active_workspaces=None))

    assert decision["decision"] == reap.DEFER
    assert decision["domain_outcome"] == reap.DEFERRED
    assert "session_liveness_unknown" in {h["code"] for h in decision["holds"]}
    assert decision["facts"]["registered_session"] is None


def test_a_registered_live_session_defers_its_own_worktree(tmp_path):
    repo, root, worktree = _fixture(tmp_path)

    decision = _only(repo, worktree, _policy(root, active_workspaces=(str(worktree),)))

    assert decision["decision"] == reap.DEFER
    assert "session_active" in {h["code"] for h in decision["holds"]}


def test_a_recently_touched_worktree_defers_and_names_the_file(tmp_path):
    repo, root, worktree = _fixture(tmp_path)
    (worktree / "work.txt").write_text("real work\n")  # touched just now

    decision = _only(repo, worktree, _policy(root))

    assert decision["decision"] == reap.DEFER
    hold = next(h for h in decision["holds"] if h["code"] == "too_recently_touched")
    assert "work.txt" in hold["detail"]
    assert decision["facts"]["newest_touch_path"].endswith("work.txt")


def test_a_running_process_inside_the_worktree_defers_it(tmp_path):
    repo, root, worktree = _fixture(tmp_path)
    proc = subprocess.Popen(["sleep", "30"], cwd=worktree)
    try:
        pids, _unreadable = reap.processes_inside(str(worktree))
        assert pids is not None and proc.pid in pids
        decision = _only(repo, worktree, _policy(root))
    finally:
        proc.terminate()
        proc.wait(timeout=10)

    assert decision["decision"] == reap.DEFER
    hold = next(h for h in decision["holds"] if h["code"] == "process_cwd_inside")
    assert str(proc.pid) in hold["detail"]


def test_the_cwd_probe_reports_the_pids_it_could_not_read(tmp_path):
    """The probe's blind spot is recorded on every decision rather than
    left implied — it can only see this uid's processes."""
    repo, root, worktree = _fixture(tmp_path)

    decision = _only(repo, worktree, _policy(root))

    assert isinstance(decision["facts"]["unreadable_pids"], int)
    assert decision["facts"]["processes_inside"] == []


def test_a_refusal_still_reports_the_holds_it_also_found(tmp_path):
    """An operator reading "refused: uncommitted work" also needs to know a
    session is live in there."""
    repo, root, worktree = _fixture(tmp_path)
    (worktree / "dirty.txt").write_text("x\n")

    decision = _only(repo, worktree, _policy(root, active_workspaces=(str(worktree),)))

    assert decision["decision"] == reap.REFUSE
    assert "uncommitted_work" in {b["code"] for b in decision["blockers"]}
    assert "session_active" in {h["code"] for h in decision["holds"]}


# --- failure is a routable outcome, never an exception -------------------


def test_a_repo_that_is_not_a_repo_is_a_domain_outcome_not_a_raise(tmp_path):
    result = reap.plan(str(tmp_path / "nowhere"), reap.ReapPolicy(active_workspaces=()))

    assert result["domain_outcome"] == reap.RETAINED
    assert result["decisions"] == []
    assert "could not be read" in result["error"]


def test_execute_rechecks_live_state_and_retains_a_worktree_dirtied_since_the_plan(tmp_path):
    repo, root, worktree = _fixture(tmp_path)
    policy = _policy(root)
    decision = reclaim._decision_from_dict(_only(repo, worktree, policy))
    assert decision.decision == reap.REAP

    # A writer comes back to life between the sweep and the removal.
    (worktree / "late-edit.py").write_text("# arrived after the plan was made\n")

    result = reclaim.execute(str(repo), decision, policy, perform=True)

    assert result["performed"] is False
    assert result["domain_outcome"] == reap.RETAINED
    assert "changed between the plan and the removal" in result["reason"]
    assert (worktree / "late-edit.py").is_file()


def test_execute_refuses_to_perform_a_refusal(tmp_path):
    repo, root, worktree = _fixture(tmp_path)
    (worktree / "dirty.txt").write_text("x\n")
    policy = _policy(root)
    decision = reclaim._decision_from_dict(_only(repo, worktree, policy))

    result = reclaim.execute(str(repo), decision, policy, perform=True)

    assert result["performed"] is False
    assert "not a performable decision" in result["reason"]
    assert worktree.is_dir()


def test_the_sweep_outcome_is_the_worst_of_its_decisions(tmp_path):
    repo, root, worktree = _fixture(tmp_path)
    (worktree / "dirty.txt").write_text("x\n")

    result = reap.plan(str(repo), _policy(root))

    # The main checkout is refused too, so the aggregate can only be retained.
    assert result["domain_outcome"] == reap.RETAINED
    assert result["counts"][reap.REFUSE] >= 2


# --- parsing and naming --------------------------------------------------


def test_the_porcelain_parser_reads_every_marker_git_emits(tmp_path):
    entries = reap.parse_worktree_list(
        "worktree /main\nHEAD abc\nbranch refs/heads/main\n\n"
        "worktree /w1\nHEAD def\ndetached\nlocked being rebased\n\n"
        "worktree /w2\nHEAD 000\nprunable gitdir file points to non-existent location\n"
    )

    assert [e["worktree"] for e in entries] == ["/main", "/w1", "/w2"]
    assert entries[0]["branch"] == "refs/heads/main"
    assert entries[1]["detached"] is True
    assert entries[1]["locked"] == "being rebased"
    assert entries[2]["prunable"].startswith("gitdir file")


def test_a_metadata_orphan_with_no_directory_left_is_prunable_not_gated(tmp_path):
    decision = reap.assess(
        str(tmp_path),
        {
            "worktree": str(tmp_path / "gone"),
            "head": "abc123",
            "branch": None,
            "detached": True,
            "bare": False,
            "locked": None,
            "prunable": "gitdir file points to non-existent location",
        },
        reap.ReapPolicy(active_workspaces=()),
    )

    assert decision.decision == reap.PRUNE
    assert decision.domain_outcome == reap.RECLAIMED
    assert decision.command == ("git", "worktree", "prune")


def test_the_minted_rescue_ref_is_namespaced_and_ref_safe():
    ref = reap.mint_reap_ref("preserve/", "/roots/writer for node #7", "0123456789abcdef")

    assert ref == "preserve/reaped/writer-for-node-7-0123456789ab"
    assert ".." not in ref and " " not in ref


def test_the_policy_reuses_the_provisioners_own_allowlists():
    class _Cfg:
        repo_allowlist = ("/home/spark/git/culture-nodes",)
        repo_allowlist_prefixes = ("/home/spark/git/.worktrees.culture-nodes",)
        preserve_branch_prefix = "preserve/"
        worktree_reap_min_idle_seconds = 1234.0

    policy = reap.ReapPolicy.from_config(_Cfg())

    assert policy.permitted_roots == ("/home/spark/git/.worktrees.culture-nodes",)
    assert policy.contained_by_roots == ("/home/spark/git/culture-nodes",)
    assert policy.min_idle_seconds == 1234.0
    assert policy.active_workspaces is None, "an unstated registry must default to unknown"
