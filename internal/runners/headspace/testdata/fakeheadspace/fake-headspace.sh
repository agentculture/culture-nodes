#!/usr/bin/env bash
# fake-headspace.sh -- a canned-response stand-in for headspace-cli, used only
# by bridge_test.go. It is NOT the real CLI: every response is a fixed,
# hand-written JSON document selected by environment variables the test sets
# before calling Bridge.Execute (NODES_FAKE_*). See bridge_test.go for the
# scenarios that set them.
#
# It exists so internal/runners/headspace's unit tests can cover the full
# nine-code exit band, and the run/stop process-separation cancellation
# mechanism itself, without Docker.
set -u

record="${NODES_FAKE_RECORD_FILE:-}"
if [ -n "$record" ]; then
  {
    printf 'ARGV:'
    for a in "$@"; do printf ' [%s]' "$a"; done
    printf '\n'
  } >> "$record"
fi

has_flag() {
  local want="$1"
  shift
  for a in "$@"; do
    [ "$a" = "$want" ] && return 0
  done
  return 1
}

ws="${NODES_FAKE_WORKSPACE_ID:-hs-fake0001}"
job="${NODES_FAKE_JOB_ID:-job-fake0001}"
digest="${NODES_FAKE_IMAGE_DIGEST:-sha256:0000000000000000000000000000000000000000000000000000000000aa}"
profile="${NODES_FAKE_PROFILE:-python3.12}"

verb="${1:-}"
shift || true

result_package() {
  # $1=status $2=key_findings(JSON array literal) $3=evidence(JSON array literal)
  # $4=artifacts(JSON array literal) $5=resource_usage(JSON object literal)
  printf '{"outcome_summary": "fake %s", "status": "%s", "key_findings": %s, "evidence": %s, "artifacts": %s, "warnings": [], "resource_usage": %s, "provenance": {"workspace_id": "%s", "job_id": "%s", "profile": "%s", "image_digest": "%s", "started_at": "2026-08-08T23:00:00.000000+00:00", "finished_at": "2026-08-08T23:00:01.000000+00:00", "policy_summary": "network=disabled, memory=536870912, cpu=1.0, pids=128, storage=1073741824, wall_clock=300", "inputs": [], "trace_id": "%s"}, "attention": []}\n' \
    "$1" "$1" "$2" "$3" "$4" "$5" "$ws" "$job" "$profile" "$digest" "$ws"
}

cli_error() {
  # $1=code $2=message $3=remediation $4=category
  printf '{"code": %s, "message": "%s", "remediation": "%s", "category": "%s"}\n' "$1" "$2" "$3" "$4" >&2
}

default_usage='[]'

case "$verb" in
  create)
    exit_code="${NODES_FAKE_CREATE_EXIT:-0}"
    if [ "$exit_code" != "0" ]; then
      cli_error "$exit_code" "fake create refused" "fix the fake scenario" "injected"
      exit "$exit_code"
    fi
    result_package "success" '["lifecycle state: ready"]' '[]' '[]' '{"wall_time_seconds": 0.1, "cpu_seconds": 0.0, "max_memory_bytes": 0, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 0}'
    exit 0
    ;;

  put)
    exit_code="${NODES_FAKE_PUT_EXIT:-0}"
    if [ "$exit_code" != "0" ]; then
      cli_error "$exit_code" "fake put refused" "fix the fake scenario" "injected"
      exit "$exit_code"
    fi
    result_package "success" '["copied 1 file(s)"]' '[]' '[]' '{"wall_time_seconds": 0.05, "cpu_seconds": 0.0, "max_memory_bytes": 0, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 0}'
    exit 0
    ;;

  run)
    mode="${NODES_FAKE_RUN_MODE:-fixed}"
    if [ "$mode" = "await-stop" ]; then
      marker="${NODES_FAKE_STOP_MARKER:?NODES_FAKE_STOP_MARKER required for await-stop mode}"
      i=0
      while [ ! -f "$marker" ]; do
        i=$((i + 1))
        if [ "$i" -gt 200 ]; then
          # Safety valve: never hang the test suite forever if the stop
          # signal never arrives -- exit with something clearly wrong instead.
          cli_error 1 "fake run: stop marker never appeared" "the cancellation goroutine did not call stop" "injected"
          exit 1
        fi
        sleep 0.05
      done
      result_package "cancelled" '["the job reported cancelled and never produced an exit status"]' '[{"label": "captured output", "kind": "excerpt", "source": "'"$job"'", "truncated": false, "excerpt": ""}]' '[]' '{"wall_time_seconds": 0.5, "cpu_seconds": 0.01, "max_memory_bytes": 4194304, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 0}'
      exit 5
    fi

    exit_code="${NODES_FAKE_RUN_EXIT:-0}"
    # NODES_FAKE_RUN_EXCERPT_JSON overrides the exit-0 captured-output
    # excerpt. Its value is spliced verbatim inside the JSON string literal,
    # so it must be pre-escaped JSON string content (e.g. '{\"emitted\": 3}\n').
    # The ${VAR-default} form (no colon) honours a set-but-empty value, so a
    # test can model a process that printed nothing at all.
    excerpt="${NODES_FAKE_RUN_EXCERPT_JSON-hello\\n}"
    case "$exit_code" in
      0)
        result_package "success" '["the command completed with exit status 0"]' '[{"label": "captured output", "kind": "excerpt", "source": "'"$job"'", "truncated": false, "excerpt": "'"$excerpt"'"}]' '[]' '{"wall_time_seconds": 0.3, "cpu_seconds": 0.02, "max_memory_bytes": 5242880, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 6}'
        exit 0
        ;;
      1|2|3|7)
        cli_error "$exit_code" "fake run refused" "fix the fake scenario" "injected"
        exit "$exit_code"
        ;;
      4)
        result_package "timeout" '["the job reported timeout and never produced an exit status", "no exit status means the command was stopped, not that it succeeded"]' '[{"label": "captured output", "kind": "excerpt", "source": "'"$job"'", "truncated": false, "excerpt": ""}]' '[]' '{"wall_time_seconds": 2.0, "cpu_seconds": 0.01, "max_memory_bytes": 4194304, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 0}'
        exit 4
        ;;
      5)
        result_package "cancelled" '["the job reported cancelled and never produced an exit status", "no exit status means the command was stopped, not that it succeeded"]' '[{"label": "captured output", "kind": "excerpt", "source": "'"$job"'", "truncated": false, "excerpt": ""}]' '[]' '{"wall_time_seconds": 1.0, "cpu_seconds": 0.01, "max_memory_bytes": 4194304, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 0}'
        exit 5
        ;;
      6)
        status_code="${NODES_FAKE_EXIT_STATUS:-1}"
        result_package "failure" '["the command completed with exit status '"$status_code"'"]' '[{"label": "captured output", "kind": "excerpt", "source": "'"$job"'", "truncated": false, "excerpt": ""}]' '[]' '{"wall_time_seconds": 0.2, "cpu_seconds": 0.01, "max_memory_bytes": 4194304, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 0}'
        exit 6
        ;;
      8)
        result_package "resource_exhausted" '["the job was killed for exceeding the workspace'"'"'s enforced memory ceiling of 20971520 bytes", "exit status 137 reports that kill, not an answer the command chose"]' '[{"label": "captured output", "kind": "excerpt", "source": "'"$job"'", "truncated": false, "excerpt": ""}]' '[]' '{"wall_time_seconds": 0.3, "cpu_seconds": 0.01, "max_memory_bytes": 5210112, "max_memory_basis": "sampled floor, not a maximum", "storage_bytes": 0, "output_bytes": 0}'
        exit 8
        ;;
      *)
        exit 9
        ;;
    esac
    ;;

  stop)
    marker="${NODES_FAKE_STOP_MARKER:-}"
    if has_flag --apply "$@"; then
      if [ -n "$marker" ]; then
        : > "$marker"
      fi
      result_package "cancelled" '["job stopped"]' '[]' '[]' '{"wall_time_seconds": 0.0, "cpu_seconds": 0.0, "max_memory_bytes": 0, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 0}'
      exit 5
    fi
    result_package "success" '["lifecycle state: running"]' '[]' '[]' '{"wall_time_seconds": 0.0, "cpu_seconds": 0.0, "max_memory_bytes": 0, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 0}'
    exit 0
    ;;

  export)
    exit_code="${NODES_FAKE_EXPORT_EXIT:-0}"
    if [ "$exit_code" != "0" ]; then
      cli_error "$exit_code" "fake export refused" "fix the fake scenario" "injected"
      exit "$exit_code"
    fi
    to=""
    prev=""
    for a in "$@"; do
      if [ "$prev" = "--to" ]; then
        to="$a"
      fi
      prev="$a"
    done
    name="${*: -1}"
    if [ -n "$to" ]; then
      printf 'fake-artifact-bytes' > "$to"
    fi
    result_package "success" '["16 bytes verified"]' '[]' '[{"name": "'"$name"'", "purpose": "declared runner output path", "media_type": "application/octet-stream", "digest": "deadbeef", "size_bytes": 16, "reference": "'"$to"'"}]' '{"wall_time_seconds": 0.0, "cpu_seconds": 0.0, "max_memory_bytes": 0, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 0}'
    exit 0
    ;;

  destroy)
    mode="${NODES_FAKE_DESTROY_MODE:-ok}"
    if [ "$mode" = "refuse-then-force" ] && ! has_flag --force "$@"; then
      cli_error 1 "refusing to destroy workspace $ws: 1 declared artifact(s) were never exported (out.txt)" "export them first, or pass force" "user_error"
      exit 1
    fi
    result_package "success" '["removed: runtime, storage"]' '[]' '[]' '{"wall_time_seconds": 0.0, "cpu_seconds": 0.0, "max_memory_bytes": 0, "max_memory_basis": "sampled peak", "storage_bytes": 0, "output_bytes": 0}'
    exit 0
    ;;

  *)
    cli_error 1 "fake-headspace: unknown verb $verb" "" "user_error"
    exit 1
    ;;
esac
