"""GitHub reads the sweep needs beyond the pull listing (#328 t24).

Kept out of sweep.py, which sits at its line limit (tests/lint
filelength_test.go). This is the 4th module the sweep-cycle bootstrap
fetches by URL and pinned sha256 (after sweep.py, pr_upkeep_jira.py and
pr_upkeep_emit.py; PR_UPKEEP_SWEEP_GITHUB_SOURCE_URL / _SHA256). The HTTP
getter is passed in, so this module performs GitHub reads only through that
injected getter and holds no credential and no transport of its own:
sweep.py's authenticated _get_json stays the only GitHub read path.
"""

REVIEWS_PAGE_SIZE = 100


def fetch_pr_reviews(
    get_json, api: str, token: str | None, repository: str, number: int
) -> list[dict]:
    """Every REST review of one pull, across pages (REST pulls carry no reviewDecision)."""
    reviews: list[dict] = []
    page = 1
    while True:
        batch = get_json(
            f"{api}/repos/{repository}/pulls/{number}/reviews"
            f"?per_page={REVIEWS_PAGE_SIZE}&page={page}",
            token,
        )
        reviews.extend(batch)
        if len(batch) < REVIEWS_PAGE_SIZE:
            return reviews
        page += 1


def fetch_open_pulls(get_json, api: str, token: str | None, repository: str) -> list[dict]:
    """Every currently open PR as ``{"number", "head_sha", "head": {"ref"},
    "body"}``, unfiltered. The cap lives with the caller (`main`) so the SAME swept set
    feeds all three per-PR queries — the SonarCloud per-PR query, the Qodo
    comment fetch, and the check-runs fetch below, one request per PR each —
    rather than independently-capped (and possibly diverging) sets.

    The head sha rides along from this ONE list request because the
    check-runs endpoint is keyed by commit: fetching it per PR instead would
    double this source's request cost for nothing. A PR object that arrives
    without a head sha keeps its entry with an empty one; `main` reports it
    rather than dropping it quietly."""
    pulls = get_json(f"{api}/repos/{repository}/pulls?state=open&per_page=50", token)
    open_pulls = []
    for pull in pulls:
        if not isinstance(pull.get("number"), int):
            continue
        head = pull.get("head") or {}
        # `head.ref` + `body` ride along for the work-item correlation (branch,
        # then body); without them every PR falls to the transient gh: form.
        open_pulls.append(
            {"number": pull["number"], "head_sha": head.get("sha") or ""}
            | {"head": {"ref": head.get("ref") or ""}, "body": pull.get("body") or ""}
            | {k: pull[k] for k in ("title", "html_url", "user") if k in pull}  # t24 payload
        )
    return open_pulls
