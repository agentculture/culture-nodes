# shellcheck shell=bash
# Spark login-user lane. Source from deploy.sh; never reads the private key.

# The URL-scoped helper is kept here so every account receives identical Git
# configuration. Git calls it at credential lookup time, after token rotation.
GITHUB_APP_GIT_HELPER='!f() { test "$1" = get || exit 0; . ~/.culture-nodes/github-token.env; echo username=x-access-token; echo "password=$GITHUB_TOKEN_WORKER"; }; f'

github_app_token_lane() {
  local account rc=0
  mkdir -p "$HOME/.culture-nodes/bin" "$HOME/.config/systemd/user"
  install -m 700 "$SCRIPT_DIR/github-app-token.sh" "$HOME/.culture-nodes/bin/github-app-token.sh"
  install -m 644 "$SCRIPT_DIR/culture-nodes-github-app-token.service" "$HOME/.config/systemd/user/"
  install -m 644 "$SCRIPT_DIR/culture-nodes-github-app-token.timer" "$HOME/.config/systemd/user/"
  systemctl --user daemon-reload

  for account in ${GITHUB_APP_TOKEN_ACCOUNTS:-culture-claude culture-qwen}; do
    if ! printf '%s\n' "$GITHUB_APP_GIT_HELPER" | ssh "$account@localhost" 'git config --global credential.https://github.com.helper "$(cat)"' >/dev/null 2>&1; then
      say "github-app-token: credential helper install failed in $account"
      rc=1
    fi
  done

  if [[ ! -x "$HOME/.local/bin/grant" ]]; then
    say 'github-app-token: grant missing at ~/.local/bin/grant; install it and grant GITHUB_APP_PRIVATE_KEY, then start the user timer'
    return 0
  fi
  if ! "$HOME/.local/bin/grant" run --inject GITHUB_APP_PRIVATE_KEY=GITHUB_APP_PRIVATE_KEY -- /bin/true >/dev/null 2>&1; then
    say 'github-app-token: GITHUB_APP_PRIVATE_KEY grant missing; add it and start the user timer'
    return 0
  fi
  systemctl --user enable --now culture-nodes-github-app-token.timer
  if systemctl --user start culture-nodes-github-app-token.service; then
    say 'github-app-token: initial mint succeeded'
  else
    say 'github-app-token: initial mint failed; inspect the user service'
    rc=1
  fi
  return "$rc"
}
