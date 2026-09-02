"""JIRA_API_BASE — the REST base every bridge verb builds its URL from.

The system's Jira identity is a Cloud service account whose SCOPED API token
is accepted only at the Atlassian gateway; the site URL answers 401 for it.
So the four verbs need a REST base that is not derivable from `JIRA_SITE`,
and the deployment supplies it the way it supplies the credential — through
the bridge process environment, never through a config file.

Every test here is offline: `opener` is a stub, so nothing is contacted.
"""

import json
from pathlib import Path

import pytest
from jira_bridge import client, create_issue, read_issue, rest, transition_issue
from jira_bridge.config import Config, ConfigError

GATEWAY = "https://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c"
SITE = "team.example.com"
FIXTURES = Path(__file__).parent / "fixtures"


class Response:
    def __init__(self, status, data=b""):
        self.status = status
        self.data = data

    def read(self):
        return self.data

    def __enter__(self):
        return self

    def __exit__(self, *_):
        pass


def recorder(status, data=b"{}"):
    """An opener that records every request URL it is handed."""
    seen = []

    def open_(request, timeout):
        seen.append(request.full_url)
        return Response(status, data)

    return seen, open_


# --- configuration --------------------------------------------------------


def test_api_base_is_environment_only_and_defaults_to_unset():
    assert Config.load(env={}).api_base == ""
    assert Config.load(env={"JIRA_API_BASE": GATEWAY}).api_base == GATEWAY


def test_an_empty_grant_means_the_site_url(tmp_path):
    # install-secrets grants the NAME with an empty value on a deployment
    # that has no gateway base; empty must mean "unset", not "malformed".
    assert Config.load(env={"JIRA_API_BASE": ""}).api_base == ""


@pytest.mark.parametrize("suffix", ["/", "//", "  ", "/  "])
def test_trailing_slashes_and_whitespace_are_trimmed(suffix):
    assert Config.load(env={"JIRA_API_BASE": GATEWAY + suffix}).api_base == GATEWAY


@pytest.mark.parametrize(
    "value",
    [
        "http://api.atlassian.com/ex/jira/abc",
        "api.atlassian.com/ex/jira/abc",
        "https:///ex/jira/abc",
        "https://user:placeholder@example.com/ex/jira/abc",
        "https://api.atlassian.com/ex/jira/abc?x=1",
        "https://api.atlassian.com/ex/jira/abc#f",
    ],
)
def test_a_malformed_base_is_refused_when_the_process_starts(value):
    # Parse time, not request time: a typo here is a configuration failure
    # with a name, never an unattributable 401 from four different verbs.
    with pytest.raises(ConfigError, match="JIRA_API_BASE"):
        Config.load(env={"JIRA_API_BASE": value})


def test_a_config_file_naming_the_base_is_refused_like_a_credential(tmp_path):
    # The base decides where the credential is SENT, so it belongs with the
    # credential in the process environment — not in a file the control plane
    # or a runner could carry.
    path = tmp_path / "config.json"
    path.write_text(json.dumps({"api_base": GATEWAY}), encoding="utf-8")
    with pytest.raises(ConfigError, match="unknown bridge config key"):
        Config.load(str(path), env={})


def test_the_site_url_is_the_root_when_nothing_is_granted():
    assert rest.api_root(SITE, "") == "https://team.example.com"
    assert rest.api_root(SITE, GATEWAY) == GATEWAY
    # A non-host JIRA_SITE is still refused — but only when it is what the
    # request would be built from.
    assert rest.api_root("https://team.example.com", "") is None
    assert rest.api_root("", GATEWAY) == GATEWAY


# --- the four verbs -------------------------------------------------------


def test_post_comment_uses_the_gateway_when_granted():
    seen, open_ = recorder(201, (FIXTURES / "comment-response.json").read_bytes())
    result = client.post_comment(
        SITE, "SCRUM-17", "Shipped", "robot@example.com", "secret", opener=open_, api_base=GATEWAY
    )
    assert result.ok
    assert seen == [GATEWAY + "/rest/api/3/issue/SCRUM-17/comment"]


def test_post_comment_uses_the_site_url_when_not_granted():
    seen, open_ = recorder(201, (FIXTURES / "comment-response.json").read_bytes())
    assert client.post_comment(
        SITE, "SCRUM-17", "Shipped", "robot@example.com", "secret", opener=open_
    ).ok
    assert seen == ["https://team.example.com/rest/api/3/issue/SCRUM-17/comment"]


def test_transition_uses_the_gateway_when_granted():
    seen, open_ = recorder(200, b'{"transitions":[{"id":"31","name":"Done"}]}')
    result = transition_issue.transition(
        SITE, "SCRUM-17", "Done", "robot@example.com", "secret", opener=open_, api_base=GATEWAY
    )
    assert result.ok
    assert seen == [GATEWAY + "/rest/api/3/issue/SCRUM-17/transitions"] * 2


def test_transition_uses_the_site_url_when_not_granted():
    seen, open_ = recorder(200, b'{"transitions":[{"id":"31","name":"Done"}]}')
    assert transition_issue.transition(
        SITE, "SCRUM-17", "Done", "robot@example.com", "secret", opener=open_
    ).ok
    assert seen == ["https://team.example.com/rest/api/3/issue/SCRUM-17/transitions"] * 2


def _create_request():
    parsed, error = create_issue.parse(
        {"verb": "create_issue", "project": "SCRUM", "summary": "Wire the lane"},
        allowed_projects=("SCRUM",),
    )
    assert error is None
    return parsed


def test_create_uses_the_gateway_when_granted():
    seen, open_ = recorder(201, (FIXTURES / "create-issue-response.json").read_bytes())
    result = create_issue.create(
        SITE, _create_request(), "robot@example.com", "secret", opener=open_, api_base=GATEWAY
    )
    assert result.ok
    assert seen == [GATEWAY + "/rest/api/3/issue"]


def test_create_uses_the_site_url_when_not_granted():
    seen, open_ = recorder(201, (FIXTURES / "create-issue-response.json").read_bytes())
    assert create_issue.create(
        SITE, _create_request(), "robot@example.com", "secret", opener=open_
    ).ok
    assert seen == ["https://team.example.com/rest/api/3/issue"]


def _read_request():
    parsed, error = read_issue.parse(
        {"verb": "read_issue", "issue": "SCRUM-17"}, project_prefix="SCRUM-", comment_limit=2
    )
    assert error is None
    return parsed


READ_QUERY = "?fields=summary,description,status,comment,issuelinks&expand=renderedFields"


def test_read_uses_the_gateway_when_granted():
    seen, open_ = recorder(200, (FIXTURES / "read-issue-response.json").read_bytes())
    result = read_issue.read(
        SITE, _read_request(), "robot@example.com", "secret", opener=open_, api_base=GATEWAY
    )
    assert result.ok
    assert seen == [GATEWAY + "/rest/api/3/issue/SCRUM-17" + READ_QUERY]


def test_read_uses_the_site_url_when_not_granted():
    seen, open_ = recorder(200, (FIXTURES / "read-issue-response.json").read_bytes())
    assert read_issue.read(SITE, _read_request(), "robot@example.com", "secret", opener=open_).ok
    assert seen == ["https://team.example.com/rest/api/3/issue/SCRUM-17" + READ_QUERY]


@pytest.mark.parametrize(
    "call",
    [
        lambda site, opener: client.post_comment(
            site, "SCRUM-17", "x", "robot@example.com", "secret", opener=opener
        ),
        lambda site, opener: transition_issue.transition(
            site, "SCRUM-17", "Done", "robot@example.com", "secret", opener=opener
        ),
        lambda site, opener: create_issue.create(
            site, _create_request(), "robot@example.com", "secret", opener=opener
        ),
        lambda site, opener: read_issue.read(
            site, _read_request(), "robot@example.com", "secret", opener=opener
        ),
    ],
)
def test_a_non_host_jira_site_is_still_refused_when_no_base_is_granted(call):
    seen, open_ = recorder(200)
    result = call("https://team.example.com/", open_)
    assert not result.ok
    assert result.error == "JIRA_SITE must be a host name"
    assert seen == []
