# What a dispatch can actually execute, by posture

Captured from probe runs, not from what is on disk. The baselines in
`toolchains/*.json` answer *is it installed*; this answers *can a dispatch use
it*, which is a different question and the one that decides where work is
routed.

The two are routinely confused, and the cost is real: three work packages this
cycle were routed to a host on the strength of `go: present`, and two of them
could not run the tests they wrote.

## codex hosts (thor, orin)

| Capability | `read-only` | `workspace-write` |
|---|---|---|
| `go build` / `go vet` | **unusable** | usable |
| `go test`, no database, no listener | **unusable** | usable |
| `go test` against PostgreSQL | unusable | **unusable** |
| `go test ./internal/api/...` (binds a listener) | unusable | **unusable** |
| `uv run` | **unusable** | usable |
| network, loopback or external | unusable | **unusable** |

### Why read-only kills Go outright

Not the module cache — the *work directory*. Under `read-only` nothing is
writable at all, and `go` needs somewhere to build:

```text
$ mktemp -d
mktemp: failed to create directory via template '/tmp/tmp.XXXXXXXXXX': Read-only file system
$ GOCACHE=/home/thor/.cache/go-build GOPROXY=off go build ./internal/ledger/
go: creating work dir: mkdir /tmp/go-build3944234912: read-only file system
```

`$TMPDIR` is unset, `/tmp`, `~/.cache` and the worktree are all read-only, and
`mktemp -d` fails everywhere it was tried. Go and uv are **installed and
identifiable** in this posture and **operationally unusable**. Run
`01M04D3A7G5NWCHKMN3AJWXF5K` on thor.

### Why workspace-write still cannot reach a database

`socket(2)` returns `EPERM` — the syscall that *creates* a socket, not
`connect`. Loopback `127.0.0.1:5432`, loopback `127.0.0.1:18080` and external
`1.1.1.1:443` all fail identically, so a PostgreSQL published on the very host
the dispatch runs on is unreachable from it:

```text
PermissionError: [Errno 1] Operation not permitted
  File "/usr/lib/python3.12/socket.py", line 233, in __init__
    _socket.socket.__init__(self, family, type, proto, fileno)
```

Run `01M04C39ZQK8NHJPQFSG8Z3QNV` on thor. No address, port, credential, unix
socket, or sidecar changes this. Tracked as **#119**.

## claude bridges (spark)

Full toolchain, real PostgreSQL, Docker, network. This is the lane a
database-backed or listener-backed package belongs in.

## Routing rule

**A codex host can author Go. It cannot gate it.** Route the authoring there
freely — it is a different account's session window, and three packages this
cycle proved the authoring is sound. Route the *gate* to a lane with a
database, and say so in the brief so the session declares its unrun checks
instead of guessing. Recorded as deviations d17 and d19.

## A measurement trap, twice paid for

Both the operator and a probe hit this within an hour:

```sh
some-command 2>&1 | tail -20 ; echo "EXIT=$?"   # WRONG: that is tail's status
```

`$?` after a pipeline is the *last* command's status. Without `set -o pipefail`
this reports success for a command that failed. Use `${PIPESTATUS[0]}`, or drop
the pipe. The probe on run `01M04D3A7G5NWCHKMN3AJWXF5K` caught it in its own
brief and re-derived the real answer by counting PASS and SKIP lines instead.

## This table is now a routing input, not only a briefing (task t32)

`internal/repair` reads the same facts mechanically. When a merge gate rejects,
the control plane decides where that failure goes, and this table is what
decides whether a repair is offered at all:

| What the surface says | What the router does |
|---|---|
| the posture grants no `workspace-write` | human — a session there cannot edit a file, so it cannot repair |
| the failing suite's tool is unusable in that posture | human — a repair could not be checked before it was claimed |
| the suite declares a grant the posture lacks (`--requires-grant network-egress`) | human — this row is the `go test` against PostgreSQL line above |
| the actor advertised no surface at all | human — fail closed; unknown is not permission |
| none of the above, and the run is inside its bound | a bounded repair attempt on that lane |

The bound is stated in the record: **2 repair attempts per run, over a 24-hour
window from the run's first gate rejection, and a human node at either
ceiling.**

Two limits worth knowing before trusting it. The tool is identified from the
gate's `argv[0]`, which establishes "this lane can run the binary the suite is
spelled with" — weaker than "this lane can run the suite", because a `go test`
that needs a listener is invisible here. And the routing is a DECISION: nothing
is dispatched, because the write path through the bridges is still unproven
(#18). Both are why the loop is routed rather than unattended.

## Which revision is answering (task t32, issue #120 item 4)

Every fact above is only as current as the bridge that measured it, and until
now nothing said how current that was — three dispatches this cycle reported
`handover=true` and created no ref because the installed bridges predated the
code that mints them. Each bridge now advertises a `deployment` block beside
these facts:

```sh
curl -sH "Authorization: Bearer $TOKEN" http://thor:8086/v1/capabilities \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["preflight"]["host"]["deployment"])'
```

`install_mode` is the part to read first. A `copy` (the `uv tool install`ed
codex and notify bridges) reports the revision the deploy stamped and **goes
stale silently until it is reinstalled**. An `editable` install (the claude
bridges on spark) reports its live work tree and cannot go stale — but check
`revision_is_dirty`, because it serves uncommitted changes too. A bridge
reporting no revision at all has not been deployed by a `deploy.sh` that stamps,
and its age is unknown.

The control plane answers for itself, unauthenticated:

```sh
curl -s http://192.168.1.146:18080/v1alpha1/version
```
