"""Diagnostics and credential handling for the pr-upkeep sweep.

Split out of `test_pr_upkeep_sweep.py` when that module crossed the repo's
1000-line hard limit (`tests/lint/filelength_test.go`). The split is by
subject rather than by size: everything here is about how the sweep REPORTS
— which surface failed, and how a source credential is presented — as opposed
to what it finds, which is the other module's subject.
"""

import importlib.util
import json
import urllib.error
from pathlib import Path

EXAMPLE_DIR = Path(__file__).resolve().parents[1] / "examples" / "pr-upkeep"

REPOSITORY_GRANT = json.dumps(
    {
        "cycle": 0,
        "repositories": [
            {
                "github_repo": "agentculture/culture-nodes",
                "sonar_component": "agentculture_culture-nodes",
            }
        ],
    }
)


def _load_sweep():
    spec = importlib.util.spec_from_file_location("pr_upkeep_sweep", EXAMPLE_DIR / "sweep.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_a_sweep_failure_names_the_surface_that_failed(monkeypatch, capsys):
    """Every source surface used to fail with the same unattributable line.

    Before this, all four surfaces — the repository grant, GitHub, SonarCloud,
    Jira — reported through one boundary as::

        sweep failed: Expecting value: line 1 column 1 (char 0)

    That is what a JSON decoder says about an empty body, and an empty body is
    what a wrong token, a rate limit, an outage, an SPA catch-all and a
    malformed environment variable all look like from here. Diagnosing one
    instance took a monkey-patched ``json.loads`` to find that the culprit was
    a malformed ``PR_UPKEEP_REPOSITORIES``.

    The sweep is meant to run unattended (#107), so this matters more than it
    looks: an always-on emitter whose failures name nothing is one an operator
    stops reading, and a sweep nobody reads is a sweep that has silently
    stopped.
    """
    sweep = _load_sweep()

    monkeypatch.setenv("PR_UPKEEP_REPOSITORIES", "agentculture/culture-nodes")  # not JSON
    assert sweep.main() == 1
    message = capsys.readouterr().err
    assert "PR_UPKEEP_REPOSITORIES" in message, message
    assert "JSONDecodeError" in message, message

    # A different surface must name itself differently — otherwise the stage
    # is decoration rather than diagnosis.
    monkeypatch.setenv("PR_UPKEEP_REPOSITORIES", REPOSITORY_GRANT)
    monkeypatch.setattr(
        sweep,
        "fetch_open_pulls",
        lambda *a, **k: (_ for _ in ()).throw(
            urllib.error.HTTPError("https://api.github.com", 401, "Unauthorized", None, None)
        ),
    )
    assert sweep.main() == 1
    message = capsys.readouterr().err
    assert "GitHub" in message, message
    assert "agentculture/culture-nodes" in message, message
    assert "PR_UPKEEP_REPOSITORIES" not in message, message


def test_a_failure_outside_every_attempting_block_says_it_is_unattributed(monkeypatch, capsys):
    """The gap must be visible rather than dressed up as a named stage.

    `attempting` blocks are added by hand, so a step can be missed — the pure
    transforms between the fetches (`qodo_review_bodies`, `prioritise`) sit
    outside them today. When an untagged step fails, the report says so
    instead of implying a surface was identified, which is what would make the
    next reader trust a stage that was never assigned.
    """
    sweep = _load_sweep()
    monkeypatch.setenv("PR_UPKEEP_REPOSITORIES", REPOSITORY_GRANT)
    monkeypatch.setattr(
        sweep, "fetch_open_pulls", lambda *a, **k: [{"number": 1, "head_sha": "abc"}]
    )
    monkeypatch.setattr(sweep, "fetch_pr_comments", lambda *a, **k: [])
    # A pure transform between two fetches — inside main's try, inside no
    # `attempting` block.
    monkeypatch.setattr(
        sweep, "qodo_review_bodies", lambda *a, **k: (_ for _ in ()).throw(ValueError("boom"))
    )

    assert sweep.main() == 1
    message = capsys.readouterr().err
    assert "unattributed" in message, message
    assert "boom" in message, message


def test_the_sonar_token_is_sent_as_basic_not_bearer(monkeypatch):
    """SonarCloud takes the token as the BASIC username with an empty password.

    Passing it as a bearer authenticates as nobody: SonarCloud ignores the
    header and answers as it would anonymously. Against a PUBLIC project that
    looks like success — the same issues come back — so a bearer-shaped bug
    here is invisible until the project turns private or starts rate-limiting,
    at which point the sweep begins failing for a reason nothing recorded.

    The token is optional by design: both queries succeed anonymously against
    a public project, measured on `agentculture_culture-nodes`.
    """
    sweep = _load_sweep()
    seen = {}

    def capture(url, token=None, *, basic=None):
        seen["url"], seen["token"], seen["basic"] = url, token, basic
        return {"issues": []}

    monkeypatch.setattr(sweep, "_get_json", capture)

    monkeypatch.delenv("SONAR_TOKEN", raising=False)
    sweep.fetch_sonar_issues("comp")
    assert seen["basic"] is None, "no token configured must stay anonymous"

    monkeypatch.setenv("SONAR_TOKEN", "sq-secret")
    sweep.fetch_sonar_issues("comp")
    assert seen["basic"] == ("sq-secret", ""), seen
    assert seen["token"] is None, "a bearer would authenticate as nobody"

    # The PR-scoped query is the one that regressed unnoticed before, because
    # it is the second call site and easy to update only the first.
    sweep.fetch_sonar_issues("comp", pr=7)
    assert seen["basic"] == ("sq-secret", ""), seen
    assert "pullRequest=7" in seen["url"] or "pullRequest%3D7" in seen["url"], seen["url"]

    # An empty value is not a credential.
    monkeypatch.setenv("SONAR_TOKEN", "")
    sweep.fetch_sonar_issues("comp")
    assert seen["basic"] is None, "an empty SONAR_TOKEN must not send an empty credential"
