# Two callers arrive here with different things in scope. deploy.sh `source`s
# this lane with `set -euo pipefail` already on and with `say` and
# backup_env_file already defined. The re-grant hand-turn runs it standalone --
# `bash deploy/prod/lanes/runner-env-write.sh`, step 5 of
# docs/operations/jira-service-account.md -- where neither is true: both helper
# calls below were `command not found`, which skipped the timestamped backup
# the whole of env-backup.sh exists to take, and left 127 as the exit status of
# a run that had in fact written runner.env. A documented step that reports
# failure on success is a step an operator learns to ignore, and the operator
# it teaches to ignore it is the one who then misses a real refusal.
#
# So the standalone entry supplies exactly what deploy.sh would have: the same
# shell options, the real backup helper, and deploy.sh's own `say`. Each is
# guarded on absence, so a caller that already has them keeps its own.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	set -euo pipefail
fi
if ! declare -F backup_env_file >/dev/null 2>&1; then
	# shellcheck source=deploy/prod/lanes/env-backup.sh
	. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/env-backup.sh"
fi
if ! declare -F say >/dev/null 2>&1; then
	say() { printf '==> %s\n' "$*"; }
fi

# RUNNER_ENV_WRITE_START -- tests/test_deploy_runner_env.py executes this real
# block against a fake host. Keep the marker at the first statement needed by
# the block and its mate after the atomic write.
# Read before deciding anything and before opening runner.env for writing. A
# deploy is allowed to override either grant from its own environment; absent
# an override, retain the complete old line so systemd sees byte-identical
# values across repeated deploys (including PR_UPKEEP_REPOSITORIES quoting).
existing_runner_env=$(ssh "$HOST" 'if [ -f ~/.culture-nodes/runner.env ]; then cat ~/.culture-nodes/runner.env; fi')
# A FIRST deploy has no runner.env at all, and that state is different from a
# runner.env that has lost a line (task t1's colleague review): the refusals
# below exist to stop a re-deploy from silently DROPPING a grant the host
# already has, and on a fresh host there is nothing to drop. Distinguished by
# the file's existence, not by the content being empty.
runner_env_exists=$(ssh "$HOST" 'if [ -f ~/.culture-nodes/runner.env ]; then echo yes; else echo no; fi')
if [ -n "${NODES_API_URL:-}" ]; then
	NODES_API_URL_LINE="NODES_API_URL=$NODES_API_URL"
else
	NODES_API_URL_LINE=$(printf '%s\n' "$existing_runner_env" | sed -n '/^NODES_API_URL=/p' | tail -n 1)
fi
if [ -z "$NODES_API_URL_LINE" ]; then
	if [ "$runner_env_exists" = yes ]; then
		echo "refusing: NODES_API_URL is absent from both the shell and existing runner.env; runner.env was not touched" >&2
	else
		echo "refusing: NODES_API_URL is not set in the shell and this is a first deploy (no runner.env on $HOST to retain it from); nothing was written" >&2
		echo "hint: export NODES_API_URL=http://<thor-address>:18080 (the control-plane URL the runner on $HOST calls back to) and re-run" >&2
	fi
	exit 1
fi

if [ -n "${PR_UPKEEP_REPOSITORIES:-}" ]; then
	PR_UPKEEP_REPOSITORIES_LINE="PR_UPKEEP_REPOSITORIES='$PR_UPKEEP_REPOSITORIES'"
else
	PR_UPKEEP_REPOSITORIES_LINE=$(printf '%s\n' "$existing_runner_env" | sed -n '/^PR_UPKEEP_REPOSITORIES=/p' | tail -n 1)
	PR_UPKEEP_REPOSITORIES=${PR_UPKEEP_REPOSITORIES_LINE#PR_UPKEEP_REPOSITORIES=}
	case "$PR_UPKEEP_REPOSITORIES" in
		\'*\') PR_UPKEEP_REPOSITORIES=${PR_UPKEEP_REPOSITORIES#\'}; PR_UPKEEP_REPOSITORIES=${PR_UPKEEP_REPOSITORIES%\'} ;;
	esac
fi
if [ -z "$PR_UPKEEP_REPOSITORIES_LINE" ] && [ "$runner_env_exists" = no ]; then
	# The well-known jira-less default a first deploy always got before task
	# t1 — the closed repository set for this repo with no Jira pair. A Jira
	# pair is only ever ADDED by the operator (shell override) and from then
	# on retained by the branch above; it can never be reintroduced by this
	# default, because this default is reachable only when no file exists.
	PR_UPKEEP_REPOSITORIES='{"cycle":0,"repositories":[{"github_repo":"agentculture/culture-nodes","sonar_component":"agentculture_culture-nodes"}]}'
	PR_UPKEEP_REPOSITORIES_LINE="PR_UPKEEP_REPOSITORIES='$PR_UPKEEP_REPOSITORIES'"
	say "first deploy on $HOST (no runner.env): granting the default jira-less PR_UPKEEP_REPOSITORIES; add a Jira pair by exporting PR_UPKEEP_REPOSITORIES on a later deploy"
fi
if [ -z "$PR_UPKEEP_REPOSITORIES_LINE" ]; then
	echo "refusing: PR_UPKEEP_REPOSITORIES is absent from both the shell and existing runner.env; runner.env was not touched" >&2
	exit 1
fi

# examples/pr-upkeep's sweep node names its script source as a granted
# environment value rather than baking a URL into the graph (task t16), so
# WHOSE code that node runs is a property of this deployment. The runner
# boundary resolves environment_refs from its OWN process environment
# (internal/runners/headspace/bridge.go), which for a systemd unit means
# runner.env -- and the block above REWRITES that file on every deploy, so
# these two have to be re-granted here rather than hand-added once.
#
# Defaults name the exact immutable revision whose archive was shipped above.
# `git show` reads sweep.py from that same object, so this remains correct when
# the revision is a squash merge and its pre-merge commits are unreachable.
# Either value remains explicitly overridable: running somebody else's granted
# copy is intentional, and its URL and digest can be supplied by the operator.
PR_UPKEEP_SWEEP_SOURCE_URL=${PR_UPKEEP_SWEEP_SOURCE_URL:-"https://raw.githubusercontent.com/agentculture/culture-nodes/$REVISION/examples/pr-upkeep/sweep.py"}
PR_UPKEEP_SWEEP_SOURCE_SHA256=${PR_UPKEEP_SWEEP_SOURCE_SHA256:-$(git show "$REVISION:examples/pr-upkeep/sweep.py" | sha256sum | cut -d' ' -f1)}
PR_UPKEEP_SWEEP_JIRA_SOURCE_URL=${PR_UPKEEP_SWEEP_JIRA_SOURCE_URL:-"https://raw.githubusercontent.com/agentculture/culture-nodes/$REVISION/examples/pr-upkeep/pr_upkeep_jira.py"}
PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256=${PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256:-$(git show "$REVISION:examples/pr-upkeep/pr_upkeep_jira.py" | sha256sum | cut -d' ' -f1)}
PR_UPKEEP_SWEEP_EMIT_SOURCE_URL=${PR_UPKEEP_SWEEP_EMIT_SOURCE_URL:-"https://raw.githubusercontent.com/agentculture/culture-nodes/$REVISION/examples/pr-upkeep/pr_upkeep_emit.py"}
PR_UPKEEP_SWEEP_EMIT_SOURCE_SHA256=${PR_UPKEEP_SWEEP_EMIT_SOURCE_SHA256:-$(git show "$REVISION:examples/pr-upkeep/pr_upkeep_emit.py" | sha256sum | cut -d' ' -f1)}
# systemd's EnvironmentFile parser is shell-LIKE: it processes backslash
# escapes in an unquoted value. Measured on thor, unquoted:
#
#   {"x":"a\"b","path":"c\\d"}  ->  {"x":"a"b","path":"c\d"}   (invalid JSON)
#   {"t":"line\nbreak"}          ->  {"t":"linebreak"}           (escape eaten)
#
# PR_UPKEEP_REPOSITORIES is JSON, and JSON string escapes are backslashes, so
# any repo name, sonar component or jira_site containing a quote or backslash
# silently reshapes the config the sweep reads. Today's default value happens
# to contain neither, which is why this worked at all.
#
# Single-quoting suppresses escape processing entirely (measured: the same
# JSON round-trips byte-exact), so the value is single-quoted here, with any
# literal single quote escaped the POSIX way. Found by pr-upkeep itself, on
# the PR that introduced it.
case "$PR_UPKEEP_REPOSITORIES" in
	*"'"*)
		echo "refusing: PR_UPKEEP_REPOSITORIES contains a literal single quote." >&2
		echo "systemd EnvironmentFile is shell-LIKE, not shell: it cannot represent one inside a" >&2
		echo "single-quoted value and does not honour the POSIX escape idiom (measured on thor)." >&2
		echo "Writing it unquoted would let the runner read the config back reshaped." >&2
		exit 1
		;;
esac
if [ -n "$PR_UPKEEP_SWEEP_SOURCE_URL" ] && [ -n "$PR_UPKEEP_SWEEP_SOURCE_SHA256" ] && [ -n "$PR_UPKEEP_SWEEP_JIRA_SOURCE_URL" ] && [ -n "$PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256" ] && [ -n "$PR_UPKEEP_SWEEP_EMIT_SOURCE_URL" ] && [ -n "$PR_UPKEEP_SWEEP_EMIT_SOURCE_SHA256" ]; then
	# Piped over stdin rather than built into the ssh command string: the
	# repositories value is single-quoted (see above) and interpolating quotes
	# into a double-quoted remote command is how you get a value that is
	# correct locally and reshaped remotely.
	# One replacement means a failure cannot leave the fixed runner settings
	# written but its grants missing. The file is written by `cat` on the
	# target, which expands NOTHING, and EnvironmentFile expands neither $HOME
	# nor systemd's %h -- so the target's home is resolved here and written as
	# an absolute path. (t7 measured the alternative: a literal $HOME in the
	# file left nodes-runner in activating/auto-restart on thor, exit 2.)
	target_home=$(ssh "$HOST" 'printf %s "$HOME"')
	[ -n "$target_home" ] || { echo "refusing: could not resolve \$HOME on $HOST; runner.env was not touched" >&2; exit 1; }
	# Last thing before the replacement, and after every refusal above: the
	# bytes this deploy is about to replace stay on the host, and the deploy
	# log carries the command that puts them back (task t5, issue #253).
	# A backup taken before a refusal would be a copy of a file nothing
	# changed.
	backup_env_file "$HOST" runner.env
	{ printf '%s\n' \
		'NODES_RUNNER_LISTEN=:17070' \
		"NODES_RUNNER_SECRET_FILE=$target_home/.culture-nodes/runner.secret" \
		"NODES_RUNNER_STATE_DIR=$target_home/.culture-nodes/runner-state" \
		'NODES_RUNNER_HEADSPACE_PROFILES=sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de=python3.12' \
		"NODES_RUNNER_HEADSPACE_BIN=$target_home/.local/bin/headspace" \
		"$NODES_API_URL_LINE" \
		"PR_UPKEEP_SWEEP_SOURCE_URL=$PR_UPKEEP_SWEEP_SOURCE_URL" \
		"PR_UPKEEP_SWEEP_SOURCE_SHA256=$PR_UPKEEP_SWEEP_SOURCE_SHA256" \
		"PR_UPKEEP_SWEEP_JIRA_SOURCE_URL=$PR_UPKEEP_SWEEP_JIRA_SOURCE_URL" \
		"PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256=$PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256" \
		"PR_UPKEEP_SWEEP_EMIT_SOURCE_URL=$PR_UPKEEP_SWEEP_EMIT_SOURCE_URL" \
		"PR_UPKEEP_SWEEP_EMIT_SOURCE_SHA256=$PR_UPKEEP_SWEEP_EMIT_SOURCE_SHA256" \
		"$PR_UPKEEP_REPOSITORIES_LINE"
	} | ssh "$HOST" 'umask 077; mkdir -p ~/.culture-nodes/bin ~/.culture-nodes/runner-state; tmp=~/.culture-nodes/runner.env.new; trap '\''rm -f "$tmp"'\'' EXIT; cat > "$tmp"; mv -f "$tmp" ~/.culture-nodes/runner.env; trap - EXIT'
	say "granted the pr-upkeep sweep source and closed repository set to the runner on $HOST"
else
	say "a PR_UPKEEP_SWEEP source URL/digest pair is empty: pr-upkeep's sweep is not configured on $HOST (see examples/pr-upkeep/README.md)"
fi
# RUNNER_ENV_WRITE_END
