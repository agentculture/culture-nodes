# GitHub messaging bridge

`github-bridge` implements the actor protocol's `post_comment` and
`reply_to_review_thread` verbs. It accepts only exact repository names in
`GITHUB_REPOSITORIES` (comma separated); an empty list denies all writes.
Set `GITHUB_TOKEN` in the bridge process environment. The optional
`GITHUB_BRIDGE_AUTH_TOKEN` protects the actor HTTP endpoint. The bridge
binds to loopback by default and refuses a non-loopback bind without that
token. `GITHUB_BRIDGE_CONFIG` may point to JSON containing `actor_id`,
`host`, `port`, `auth_token`, and `repositories`; credentials cannot be put
in that file.

The two verbs use the GitHub REST issue-comment and review-comment reply
endpoints. `reply_to_review_thread` takes the anchor review comment's
numeric `comment_id`, matching `land_reply.py`. Successful responses include
the created comment ID and a proposed ledger claim.
