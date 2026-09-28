#!/usr/bin/env bash
# Mint a scoped installation token and deliver it to the engine accounts.
set -euo pipefail

: "${GITHUB_APP_PRIVATE_KEY:?GITHUB_APP_PRIVATE_KEY is required}"
APP_ID=${GITHUB_APP_ID:-5109433}
INSTALLATION_ID=${GITHUB_APP_INSTALLATION_ID:-165818005}
ACCOUNTS=${GITHUB_APP_TOKEN_ACCOUNTS:-culture-claude culture-qwen}
PERMISSIONS=${GITHUB_APP_TOKEN_PERMISSIONS:-'{"contents":"write","pull_requests":"write","issues":"write","metadata":"read","checks":"read","statuses":"read","actions":"read"}'}
RUNNER_HOSTS=${GITHUB_APP_RUNNER_HOSTS-"thor orin"}
RUNNER_PERMISSIONS=${GITHUB_APP_RUNNER_PERMISSIONS:-'{"contents":"read","pull_requests":"read","issues":"read","checks":"read","statuses":"read","actions":"read","metadata":"read"}'}

key_file=$(mktemp)
response_file=$(mktemp)
trap 'rm -f "$key_file" "$response_file"' EXIT
chmod 600 "$key_file" "$response_file"
printf '%s\n' "$GITHUB_APP_PRIVATE_KEY" > "$key_file"
b64() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
now=$(date +%s)
header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64)
payload=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((now-60))" "$((now+540))" "$APP_ID" | b64)
if ! signature=$(printf '%s.%s' "$header" "$payload" | openssl dgst -sha256 -sign "$key_file" 2>/dev/null | b64); then
  echo 'github-app-token: JWT signing failed' >&2
  exit 1
fi
jwt="$header.$payload.$signature"
mint_token() { # permissions JSON; prints token and expiration on separate lines
  local body parsed
  if ! body=$(GITHUB_APP_TOKEN_PERMISSIONS="$1" python3 -c 'import json,os; p=json.loads(os.environ["GITHUB_APP_TOKEN_PERMISSIONS"]); assert isinstance(p,dict); print(json.dumps({"permissions":p},separators=(",",":")))' 2>/dev/null); then
    echo 'github-app-token: invalid permissions JSON' >&2
    return 1
  fi
  if ! curl -fsS -X POST -H "Authorization: Bearer $jwt" -H 'Accept: application/vnd.github+json' -H 'Content-Type: application/json' --data "$body" "https://api.github.com/app/installations/$INSTALLATION_ID/access_tokens" > "$response_file" 2>/dev/null; then
    echo 'github-app-token: minting failed' >&2
    return 1
  fi
  if ! parsed=$(python3 - "$response_file" <<'PY' 2>/dev/null
import json, sys
data = json.load(open(sys.argv[1], encoding="utf-8"))
token, expires = data["token"], data["expires_at"]
if not isinstance(token, str) or not token or "\n" in token or "\r" in token:
    raise ValueError("invalid token")
if not isinstance(expires, str) or not expires or "\n" in expires or "\r" in expires:
    raise ValueError("invalid expiration")
print(token)
print(expires)
PY
  ); then
    echo 'github-app-token: invalid mint response' >&2
    return 1
  fi
  printf '%s\n' "$parsed"
}

account_mint=$(mint_token "$PERMISSIONS") || exit 1
token=${account_mint%%$'\n'*}
expires=${account_mint#*$'\n'}
rc=0
for account in $ACCOUNTS; do
  if printf 'GITHUB_TOKEN_WORKER=%s\nGITHUB_TOKEN_EXPIRES_AT=%s\n' "$token" "$expires" \
    | ssh "$account@localhost" 'umask 077; mkdir -p ~/.culture-nodes; cat > ~/.culture-nodes/github-token.env.tmp && chmod 600 ~/.culture-nodes/github-token.env.tmp && mv -f ~/.culture-nodes/github-token.env.tmp ~/.culture-nodes/github-token.env' >/dev/null 2>&1; then
    printf '%s: wrote token (expires %s)\n' "$account" "$expires"
  else
    printf '%s: token write failed\n' "$account" >&2
    rc=1
  fi
done
if [ -n "$RUNNER_HOSTS" ]; then
  if runner_mint=$(mint_token "$RUNNER_PERMISSIONS"); then
    token=${runner_mint%%$'\n'*}
    expires=${runner_mint#*$'\n'}
    for host in $RUNNER_HOSTS; do
      if printf 'GITHUB_TOKEN=%s\nGITHUB_TOKEN_EXPIRES_AT=%s\n' "$token" "$expires" \
        | ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes; cat > ~/.culture-nodes/github-app-runner.env.tmp && chmod 600 ~/.culture-nodes/github-app-runner.env.tmp && mv -f ~/.culture-nodes/github-app-runner.env.tmp ~/.culture-nodes/github-app-runner.env' >/dev/null 2>&1; then
        printf '%s: wrote runner token (expires %s)\n' "$host" "$expires"
      else
        printf '%s: runner token write failed\n' "$host" >&2
        rc=1
      fi
    done
  else
    rc=1
  fi
fi
exit "$rc"
