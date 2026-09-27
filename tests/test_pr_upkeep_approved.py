"""REST review approvals use the same event identity as the webhook."""

import json

from tests.test_pr_upkeep_sweep import _stub_sweep, sweep

PULL = {
    "number": 17,
    "title": "Fix it",
    "html_url": "https://github.com/acme/widgets/pull/17",
    "user": {"login": "alice"},
    "head": {"sha": "sha17", "ref": "fix"},
}
REVIEW = {
    "id": 412,
    "state": "APPROVED",
    "user": {"login": "bob"},
    "submitted_at": "2026-09-27T10:00:00Z",
}
PAYLOAD = {
    "source": "github",
    "repository": "acme/widgets",
    "id": "17",
    "title": "Fix it",
    "url": PULL["html_url"],
    "author": "alice",
    "reviewer": "bob",
}
SOURCE_KEY = "github:acme/widgets:pr:17:review:412:approved"
WATERMARK = {"review_id": "412"}


def test_approved_poll_event_matches_webhook_contract():
    assert sweep.approved_pr_event(PULL, REVIEW, "acme/widgets") == (
        "github.pr.approved",
        PAYLOAD,
        SOURCE_KEY,
        WATERMARK,
    )
    assert sweep.approved_pr_event(PULL, {**REVIEW, "state": "COMMENTED"}, "acme/widgets") is None


def test_fetch_open_pulls_preserves_rest_payload_fields(monkeypatch):
    monkeypatch.setattr(sweep, "_get_json", lambda *_args: [PULL])
    assert sweep.fetch_open_pulls(None, "acme/widgets") == [
        {
            "number": 17,
            "head_sha": "sha17",
            "head": {"ref": "fix"},
            "body": "",
            "title": "Fix it",
            "html_url": PULL["html_url"],
            "user": {"login": "alice"},
        }
    ]


def test_fetch_reviews_reads_rest_pages(monkeypatch):
    urls = []

    def get(url, token):
        urls.append(url)
        return [REVIEW] * 100 if url.endswith("&page=1") else [{**REVIEW, "id": 413}]

    monkeypatch.setattr(sweep, "_get_json", get)
    reviews = sweep.fetch_pr_reviews("token", "acme/widgets", 17)
    assert len(reviews) == 101
    assert urls == [
        f"{sweep.GITHUB_API}/repos/acme/widgets/pulls/17/reviews?per_page=100&page=1",
        f"{sweep.GITHUB_API}/repos/acme/widgets/pulls/17/reviews?per_page=100&page=2",
    ]


def test_sweep_emits_each_approved_review_additively(monkeypatch):
    monkeypatch.setenv(
        "PR_UPKEEP_REPOSITORIES",
        json.dumps(
            {"repositories": [{"github_repo": "acme/widgets", "sonar_component": "widgets"}]}
        ),
    )
    pulls = [
        {
            "number": 17,
            "title": "Fix it",
            "html_url": PULL["html_url"],
            "user": {"login": "alice"},
            "head_sha": "sha17",
            "head": {"ref": "fix"},
            "body": "",
        }
    ]
    calls = _stub_sweep(monkeypatch, pulls=pulls, sonar_main={"issues": []})
    monkeypatch.setattr(
        sweep,
        "fetch_pr_reviews",
        lambda *_: [
            REVIEW,
            {**REVIEW, "id": 413},
            {**REVIEW, "id": 414, "state": "COMMENTED"},
        ],
    )

    assert sweep.main() == 0
    approvals = [event for event in calls["events"] if event[0] == "github.pr.approved"]
    assert len(approvals) == 2
    assert approvals[0][:4] == ("github.pr.approved", PAYLOAD, SOURCE_KEY, WATERMARK)
    assert approvals[1][2:4] == (
        "github:acme/widgets:pr:17:review:413:approved",
        {"review_id": "413"},
    )
