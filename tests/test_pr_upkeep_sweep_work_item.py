"""The sweep carries the work item (plan loop-closure-claude-codex t2; spec
c2 / c16 / c41 / c42, honesty h10 / h28; issue #310).

Every ``pr-upkeep.pr`` fact names the work item it belongs to in its PAYLOAD
under ``work_item``, so the engine's trigger stamps the minted run's
``work_item`` column from it (internal/engine/trigger.go
``workItemFromPayload``) and ``GET /v1alpha1/runs?work_item=KEY`` finds the
run without a second write. The value has exactly two shapes:

- the correlated Jira key (branch ref first, then PR body, narrowed to the
  configured ``jira_project`` when one is set) — ``SCRUM-9``;
- otherwise the transient ``gh:<owner>/<repo>#<n>`` form, which never
  survives intake: an orphan ticket is created and the item re-keyed (t4).

It is never empty, and it is NOT ``subject``: a pr-upkeep.pr fact still
carries no subject, because subject re-enters the one-active-run-per-subject
guard #268 removed (docs/operations/pr-upkeep-lane.md).

Split from test_pr_upkeep_sweep.py to keep that file under the 1000-line
hard limit (tests/lint filelength guard).
"""

import importlib
import json

import pytest
import yaml

from tests.test_pr_upkeep_sweep import (  # noqa: F401
    EXAMPLE_DIR,
    FIXTURES,
    REPOSITORY_GRANT,
    _stub_sweep,
    sweep,
)

emit = importlib.import_module("pr_upkeep_emit")

REPOSITORY = "agentculture/culture-nodes"
DOCS_DIR = EXAMPLE_DIR.parents[1] / "docs"


@pytest.fixture(autouse=True)
def repository_grant(monkeypatch):
    monkeypatch.setenv("PR_UPKEEP_REPOSITORIES", REPOSITORY_GRANT)


@pytest.fixture(scope="module")
def sonar_payload():
    return json.loads((FIXTURES / "sonarcloud-issues.json").read_text())


class TestWorkItemForPull:
    def test_branch_key_wins(self):
        pull = {"number": 9, "head": {"ref": "SCRUM-9/loop-closure"}, "body": "also EX-4"}
        assert emit.work_item_for_pull(pull, REPOSITORY) == "SCRUM-9"

    def test_body_key_when_the_branch_has_none(self):
        pull = {"number": 9, "head": {"ref": "loop/t2"}, "body": "Delivers SCRUM-9"}
        assert emit.work_item_for_pull(pull, REPOSITORY) == "SCRUM-9"

    def test_narrowed_to_the_configured_project(self):
        pull = {"number": 9, "head": {"ref": "adr/ADR-0002"}, "body": "see SCRUM-9"}
        assert emit.work_item_for_pull(pull, REPOSITORY, "SCRUM") == "SCRUM-9"
        assert emit.work_item_for_pull(pull, REPOSITORY, "EX") == "gh:agentculture/culture-nodes#9"

    def test_no_key_yields_the_transient_gh_form(self):
        pull = {"number": 307, "head": {"ref": "review/qodo-split"}, "body": ""}
        assert emit.work_item_for_pull(pull, REPOSITORY) == "gh:agentculture/culture-nodes#307"

    def test_a_bare_listing_entry_still_yields_a_non_empty_item(self):
        # fetch_open_pulls' minimal shape: number + head_sha, no ref, no body.
        assert emit.work_item_for_pull({"number": 307, "head_sha": "sha307"}, REPOSITORY) == (
            "gh:agentculture/culture-nodes#307"
        )


class TestUpkeepPrFact:
    def test_carries_work_item_and_no_subject(self):
        pull = {"number": 9, "head_sha": "sha9", "head": {"ref": "SCRUM-9/x"}}
        fact = emit.upkeep_pr_fact(pull, REPOSITORY, [{"id": "pr9-qodo-1"}])
        assert fact == {
            "source": "github_pr",
            "repository": REPOSITORY,
            "number": 9,
            "head_sha": "sha9",
            "findings": [{"id": "pr9-qodo-1"}],
            "work_item": "SCRUM-9",
        }
        assert "subject" not in fact
        assert "category" not in fact


def _emitted_pr_facts(monkeypatch, calls):
    """Re-wrap the stubbed raise_event so the kwargs it was called with are kept."""
    facts = []

    def fake_raise(name, payload, source_key, watermark, **kwargs):
        calls["events"].append((name, payload, source_key, watermark))
        facts.append((name, payload, kwargs))
        return {"event": {"id": f"event-{len(calls['events'])}"}}

    monkeypatch.setattr(sweep, "raise_event", fake_raise)
    return facts


class TestSweepEmitsTheWorkItem:
    def test_a_pr_whose_branch_names_scrum_9_emits_work_item_scrum_9(
        self, monkeypatch, capsys, sonar_payload
    ):
        pull = {"number": 9, "head_sha": "sha9", "head": {"ref": "SCRUM-9/loop-t2"}, "body": ""}
        calls = _stub_sweep(monkeypatch, pulls=[pull], sonar_main=sonar_payload)
        facts = _emitted_pr_facts(monkeypatch, calls)
        assert sweep.main() == 0
        assert json.loads(capsys.readouterr().out)["emitted"] == 1
        ((name, payload, kwargs),) = facts
        assert name == "pr-upkeep.pr"
        assert payload["work_item"] == "SCRUM-9"
        assert "subject" not in payload
        assert "subject" not in kwargs  # raise_event is not handed one either (#268)

    def test_a_pr_with_no_key_emits_the_transient_gh_form(self, monkeypatch, capsys, sonar_payload):
        pull = {"number": 307, "head_sha": "sha307", "head": {"ref": "review/qodo"}, "body": ""}
        calls = _stub_sweep(monkeypatch, pulls=[pull], sonar_main=sonar_payload)
        facts = _emitted_pr_facts(monkeypatch, calls)
        assert sweep.main() == 0
        capsys.readouterr()
        ((_name, payload, kwargs),) = facts
        assert payload["work_item"] == "gh:agentculture/culture-nodes#307"
        assert "subject" not in payload
        assert "subject" not in kwargs

    def test_the_configured_jira_project_narrows_the_correlation(self, monkeypatch, capsys):
        monkeypatch.setenv(
            "PR_UPKEEP_REPOSITORIES",
            json.dumps(
                {
                    "repositories": [
                        {
                            "github_repo": "owner/repo",
                            "sonar_component": "owner_repo",
                            "jira_site": "team.example.com",
                            "jira_project": "SCRUM",
                        }
                    ]
                }
            ),
        )
        monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", "robot@example.com")
        monkeypatch.setenv("JIRA_API_TOKEN", "fixture-token")
        sonar = json.loads((FIXTURES / "sonarcloud-issues.json").read_text())
        pull = {"number": 12, "head_sha": "sha12", "head": {"ref": "ADR-0002/notes"}, "body": ""}
        calls = _stub_sweep(monkeypatch, pulls=[pull], sonar_main=sonar)
        monkeypatch.setattr(sweep, "fetch_jira_issues", lambda *_args: {"issues": []})
        facts = _emitted_pr_facts(monkeypatch, calls)
        assert sweep.main() == 0
        capsys.readouterr()
        upkeep = [p for n, p, _k in facts if n == "pr-upkeep.pr"]
        assert [p["work_item"] for p in upkeep] == ["gh:owner/repo#12"]


class TestFetchOpenPullsCarriesTheCorrelationFields:
    def test_head_ref_and_body_ride_along(self, monkeypatch):
        pulls = [
            {
                "number": 9,
                "head": {"sha": "sha9", "ref": "SCRUM-9/loop-t2"},
                "body": "Delivers SCRUM-9",
            },
            {"number": 7},
        ]
        monkeypatch.setattr(sweep, "_get_json", lambda url, token=None, **_: pulls)
        listed = sweep.fetch_open_pulls(None, REPOSITORY)
        assert listed[0]["head"] == {"ref": "SCRUM-9/loop-t2"}
        assert listed[0]["body"] == "Delivers SCRUM-9"
        assert listed[1]["head"] == {"ref": ""}
        assert listed[1]["body"] == ""
        # The correlation the emitter runs sees the same answer the fact will.
        assert emit.work_item_for_pull(listed[0], REPOSITORY) == "SCRUM-9"
        assert emit.work_item_for_pull(listed[1], REPOSITORY) == "gh:agentculture/culture-nodes#7"


class TestWorkflowInputContractAdmitsWorkItem:
    """The published contract is additionalProperties:false, so a fact carrying
    work_item is REJECTED by the trigger until the contract admits it (c16)."""

    @staticmethod
    def _input_schema():
        document = yaml.safe_load((EXAMPLE_DIR / "workflow.yaml").read_text())
        return document["spec"]["contract"]["input"]["schema"]

    def test_work_item_is_a_required_non_empty_string(self):
        schema = self._input_schema()
        assert schema["additionalProperties"] is False
        assert "work_item" in schema["required"]
        assert schema["properties"]["work_item"] == {"type": "string", "minLength": 1}

    def test_the_contract_names_no_subject_and_no_category(self):
        schema = self._input_schema()
        assert "subject" not in schema["properties"]
        assert "category" not in schema["properties"]

    def test_an_emitted_fact_has_exactly_the_contracts_keys(self):
        schema = self._input_schema()
        fact = emit.upkeep_pr_fact(
            {"number": 9, "head_sha": "sha9", "head": {"ref": "SCRUM-9/x"}},
            REPOSITORY,
            [{"id": "pr9-qodo-1"}],
        )
        assert set(fact) == set(schema["required"]) == set(schema["properties"])


class TestTheOrphanIdempotencyLimitIsDocumented:
    """The `gh:` form never surviving intake is a claim about the GRAPH, not a
    guarantee about the board: the ticket is created before the body is
    stamped, so a run whose `intake-orphan` succeeded and whose `stamp-pr`
    did not leaves a ticket that exists and a PR that still reads as orphaned
    — and the next fact for that PR opens a second one. Both docs used to
    state the idempotency flatly; a person on the board who then found two
    tickets had nothing to read that explained it (Qodo High, PR #326)."""

    DOCS = (
        DOCS_DIR / "drive-from-jira.md",
        DOCS_DIR / "operations" / "pr-upkeep-lane.md",
    )

    def test_the_ticket_is_created_before_the_body_is_stamped(self):
        document = yaml.safe_load((EXAMPLE_DIR / "workflow.yaml").read_text())
        spec = document["spec"]
        assert {"from": "intake-orphan.issue_created", "to": "stamp-pr"} in spec["edges"]
        # Neither node may retry: a retried create is a second ticket, and a
        # retried stamp is a second PATCH. So a failed stamp is terminal for
        # the run, and the ticket it left behind is not withdrawn.
        assert spec["nodes"]["intake-orphan"]["policy"]["retry"]["maxAttempts"] == 1
        assert spec["nodes"]["stamp-pr"]["policy"]["retry"]["maxAttempts"] == 1

    @pytest.mark.parametrize("doc", DOCS, ids=lambda path: path.name)
    def test_the_doc_does_not_claim_a_second_ticket_is_impossible(self, doc):
        text = doc.read_text(encoding="utf-8")
        assert "second orphan ticket for the same PR is not created" not in text, doc.name

    @pytest.mark.parametrize("doc", DOCS, ids=lambda path: path.name)
    def test_the_doc_names_the_case_that_opens_a_second_ticket(self, doc):
        text = doc.read_text(encoding="utf-8").lower()
        assert "second" in text and "orphan" in text, doc.name
        # What the reader needs is the CAUSE: the key is not on the PR body,
        # either because the write failed or because it was edited away.
        assert "edited out" in text or "edits out" in text, doc.name
        assert "failed" in text, doc.name
