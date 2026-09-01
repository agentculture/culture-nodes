"""JIRA_API_BASE — the sweep's REST base for a scoped service-account token.

A sibling of `tests/test_pr_upkeep_sweep_jira.py` rather than an addition to
it: that file is the d1 read/replay suite and is pinned by the plan, and this
is one narrow deployment fact — *where* the REST calls go — which the same
module now resolves. Both files exercise `pr_upkeep_jira`; neither contacts
Jira.

The fact under test: the system's Jira identity is a Cloud service account
whose scoped API token is accepted ONLY at the Atlassian gateway, so the
site URL answers 401. The sweep therefore builds REST URLs from the granted
base when it has one — and keeps building BROWSE links from the site host,
because a browse link is what a person opens and the gateway is not one.
"""

import importlib
import urllib.parse

import pytest

from tests.test_pr_upkeep_sweep import FIXTURES  # noqa: F401 -- shared sys.path setup

jira = importlib.import_module("pr_upkeep_jira")

GATEWAY = "https://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c"


@pytest.fixture
def recorded_urls(monkeypatch):
    """Every URL `fetch_jira_issues` would have requested, in order."""
    seen = []

    def fake_get(url, token=None, *, basic=None):
        seen.append(url)
        if "/changelog?" in url:
            return {"startAt": 1, "maxResults": 100, "total": 2, "values": [{"id": "20000"}]}
        if "/comment?" in url:
            return {"startAt": 1, "maxResults": 100, "total": 2, "comments": [{"id": "30000"}]}
        return {
            "issues": [
                {
                    "id": "10002",
                    "key": "SCRUM-2",
                    "fields": {
                        "comment": {"total": 2, "comments": [{"id": "30001"}]},
                    },
                    "changelog": {"total": 2, "histories": [{"id": "20001"}]},
                }
            ],
            "isLast": True,
        }

    monkeypatch.setattr(jira, "_get_json", fake_get)
    return seen


class TestTheGrantedBaseIsReadLikeTheCredentialPair:
    def test_an_ungranted_base_is_absent_not_an_error(self, monkeypatch):
        monkeypatch.delenv("JIRA_API_BASE", raising=False)
        assert jira.jira_api_base() == ""

    def test_a_granted_base_is_returned_verbatim(self, monkeypatch):
        monkeypatch.setenv("JIRA_API_BASE", GATEWAY)
        assert jira.jira_api_base() == GATEWAY

    @pytest.mark.parametrize("suffix", ["/", "//", "/   ", "   "])
    def test_trailing_slashes_and_whitespace_are_trimmed(self, monkeypatch, suffix):
        monkeypatch.setenv("JIRA_API_BASE", GATEWAY + suffix)
        assert jira.jira_api_base() == GATEWAY

    @pytest.mark.parametrize(
        "value",
        [
            "http://api.atlassian.com/ex/jira/abc",  # not https
            "api.atlassian.com/ex/jira/abc",  # no scheme
            "https:///ex/jira/abc",  # no host
            "https://user:pw@api.atlassian.com/ex/jira/abc",  # credentials in the URL
            "https://api.atlassian.com/ex/jira/abc?x=1",  # a query
            "https://api.atlassian.com/ex/jira/abc#f",  # a fragment
        ],
    )
    def test_a_malformed_base_is_refused_at_parse_time(self, monkeypatch, value):
        # Named configuration failure, not an unattributable 401 an hour later.
        monkeypatch.setenv("JIRA_API_BASE", value)
        with pytest.raises(ValueError, match="JIRA_API_BASE"):
            jira.jira_api_base()

    def test_it_is_a_granted_environment_value_not_run_input(self):
        source = (jira.__file__ and open(jira.__file__).read()) or ""
        assert 'os.environ.get("JIRA_API_BASE")' in source


class TestEveryRestCallUsesTheGrantedBase:
    def test_search_and_pagination_go_to_the_gateway_when_it_is_granted(self, recorded_urls):
        jira.fetch_jira_issues(
            "team.example.com", "EX", "robot@example.com", "fixture-token", GATEWAY
        )
        assert recorded_urls, "no Jira request was made"
        for url in recorded_urls:
            assert url.startswith(GATEWAY + "/rest/api/3/"), url
            assert "team.example.com" not in url
        assert recorded_urls[0].startswith(GATEWAY + "/rest/api/3/search/jql?")
        assert any(url.startswith(GATEWAY + "/rest/api/3/issue/SCRUM-2/") for url in recorded_urls)

    def test_the_site_url_is_used_when_no_base_is_granted(self, recorded_urls):
        jira.fetch_jira_issues("team.example.com", "EX", "robot@example.com", "fixture-token")
        assert recorded_urls
        for url in recorded_urls:
            assert url.startswith("https://team.example.com/rest/api/3/"), url
        assert recorded_urls[0].startswith("https://team.example.com/rest/api/3/search/jql?")

    def test_the_query_the_sweep_sends_is_unchanged_by_the_base(self, recorded_urls):
        jira.fetch_jira_issues(
            "team.example.com", "EX", "robot@example.com", "fixture-token", GATEWAY
        )
        gateway_query = urllib.parse.urlsplit(recorded_urls[0]).query
        recorded_urls.clear()
        jira.fetch_jira_issues("team.example.com", "EX", "robot@example.com", "fixture-token")
        assert urllib.parse.urlsplit(recorded_urls[0]).query == gateway_query


class TestBrowseLinksAlwaysUseTheSiteHost:
    PAYLOAD = {"issues": [{"key": "EX-17", "fields": {"summary": "s"}}]}

    def test_details_url_is_the_site_browse_link_when_a_base_is_granted(self, monkeypatch):
        # A person clicks this. The gateway serves the API, never the board.
        monkeypatch.setenv("JIRA_API_BASE", GATEWAY)
        (item,) = jira.jira_work_items(self.PAYLOAD, site="team.example.com", project="EX")
        assert item["details_url"] == "https://team.example.com/browse/EX-17"

    def test_details_url_is_the_site_browse_link_when_none_is_granted(self, monkeypatch):
        monkeypatch.delenv("JIRA_API_BASE", raising=False)
        (item,) = jira.jira_work_items(self.PAYLOAD, site="team.example.com", project="EX")
        assert item["details_url"] == "https://team.example.com/browse/EX-17"

    def test_the_work_item_shaper_never_reads_the_base_at_all(self):
        # It is handed a site and a project; the base is a transport fact.
        assert "api_base" not in jira.jira_work_items.__code__.co_varnames
