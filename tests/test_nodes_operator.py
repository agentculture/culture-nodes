import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / ".claude/skills/nodes-operator/scripts/nodes-op.sh"
TEMPLATE = ROOT / ".claude/skills/nodes-operator/templates/assign.workflow.yaml"


class NodesOperatorTest(unittest.TestCase):
    def test_assign_threads_optional_base_ref_without_dangling_binding(self):
        script = SCRIPT.read_text(encoding="utf-8")
        template = TEMPLATE.read_text(encoding="utf-8")
        self.assertIn("base_ref:\n            type: string", template)
        self.assertIn("base_ref: /run/input/base_ref", template)
        self.assertIn('--base-ref) base_ref="$2"; shift 2;;', script)
        self.assertIn('[ -n "$base_ref" ] || sed -i', script)
        self.assertIn('payload["base_ref"]', script)

    def run_operator(self, verb, responses):
        with tempfile.TemporaryDirectory() as directory:
            fake_bin = Path(directory)
            curl = fake_bin / "curl"
            curl.write_text(
                "#!/usr/bin/env python3\n"
                "import json, os, sys\n"
                "responses = json.loads(os.environ['FAKE_RESPONSES'])\n"
                "url = sys.argv[-1]\n"
                "for suffix, response in responses.items():\n"
                "    if url.endswith(suffix):\n"
                "        print(json.dumps(response))\n"
                "        raise SystemExit(0)\n"
                "print('unexpected URL: ' + url, file=sys.stderr)\n"
                "raise SystemExit(22)\n"
            )
            curl.chmod(0o755)
            env = os.environ.copy()
            env["PATH"] = str(fake_bin) + os.pathsep + env["PATH"]
            env["NODES_API_URL"] = "http://example.test"
            env["FAKE_RESPONSES"] = json.dumps(responses)
            return subprocess.run(
                ["bash", str(SCRIPT), verb],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )

    def test_ledger_prints_entire_claim(self):
        report = "qualifying half: " + "x" * 4000
        with tempfile.TemporaryDirectory() as directory:
            fake_bin = Path(directory)
            curl = fake_bin / "curl"
            payload = {
                "items": [
                    {
                        "authority": "proposed",
                        "record_type": "claim",
                        "origin": {"actor_id": "codex"},
                        "data": {"report": report},
                    }
                ]
            }
            curl.write_text("#!/bin/sh\nprintf '%s\\n' \"$FAKE_LEDGER\"\n")
            curl.chmod(0o755)
            env = os.environ.copy()
            env.update(
                PATH=str(fake_bin) + os.pathsep + env["PATH"],
                NODES_API_URL="http://example.test",
                FAKE_LEDGER=json.dumps(payload),
            )
            result = subprocess.run(
                ["bash", str(SCRIPT), "ledger", "run-1"],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(report, result.stdout)

    def test_running_composes_list_and_run_detail(self):
        result = self.run_operator(
            "running",
            {
                "/v1alpha1/runs?state=running&limit=500": {"items": [{"id": "run-1"}]},
                "/v1alpha1/runs/run-1": {
                    "run": {
                        "id": "run-1",
                        "name": "close backlog",
                        "category": "build",
                        "description": "Make runs explain themselves",
                        "input": {"instruction": "fix t29"},
                    },
                    "node_runs": [
                        {
                            "node_id": "task",
                            "state": "running",
                            "outcome": None,
                            "attempts": [{"actor_id": "codex-thor", "status": "running"}],
                        }
                    ],
                },
            },
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        for expected in (
            "running runs: 1",
            "run-1",
            "Make runs explain themselves",
            "fix t29",
            "task: running",
            "codex-thor:running",
        ):
            self.assertIn(expected, result.stdout)


class DevagueCustodyTest(unittest.TestCase):
    """t13 (#199 / #230, frame q1): `.devague/` custody is declared on the
    developer lane and nowhere else, and the assign verb is where a
    `.devague/` write is refused for every other lane — BEFORE the billable
    guard and before any HTTP call, so the refusal costs nothing and
    reaches nobody."""

    CUSTODY_CHECKOUT = "/home/culture-claude/git/culture-nodes-developer"

    def run_assign(self, actor, *opts, responses=None):
        with tempfile.TemporaryDirectory() as directory:
            fake_bin = Path(directory)
            calls = fake_bin / "calls.jsonl"
            curl = fake_bin / "curl"
            curl.write_text(
                "#!/usr/bin/env python3\n"
                "import json, os, sys\n"
                "argv = sys.argv[1:]\n"
                "body = argv[argv.index('-d') + 1] if '-d' in argv else None\n"
                "with open(os.environ['FAKE_CALLS'], 'a') as f:\n"
                "    f.write(json.dumps({'url': argv[-1], 'body': body}) + '\\n')\n"
                "responses = json.loads(os.environ['FAKE_RESPONSES'])\n"
                "for suffix, response in responses.items():\n"
                "    if argv[-1].endswith(suffix):\n"
                "        print(json.dumps(response))\n"
                "        raise SystemExit(0)\n"
                "print('unexpected URL: ' + argv[-1], file=sys.stderr)\n"
                "raise SystemExit(22)\n"
            )
            curl.chmod(0o755)
            env = os.environ.copy()
            env["PATH"] = str(fake_bin) + os.pathsep + env["PATH"]
            env["NODES_API_URL"] = "http://example.test"
            env["FAKE_CALLS"] = str(calls)
            env["FAKE_RESPONSES"] = json.dumps(responses or {})
            env.pop("NODES_OP_YES", None)
            result = subprocess.run(
                [
                    "bash",
                    str(SCRIPT),
                    "assign",
                    actor,
                    "run the /think leg for SCRUM-9",
                    *opts,
                ],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            recorded = []
            if calls.exists():
                recorded = [json.loads(line) for line in calls.read_text().splitlines()]
            return result, recorded

    def test_every_other_lane_is_refused_a_devague_write_before_any_call(self):
        for actor in (
            "codex-thor",
            "codex-orin",
            "planner",
            "verifier",
            "intake",
            "qwen-developer",
        ):
            with self.subTest(actor=actor):
                result, calls = self.run_assign(actor, "--devague-write", "--yes")
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn("refusing", result.stderr)
                self.assertIn(".devague/", result.stderr)
                self.assertIn("developer", result.stderr)
                self.assertEqual(calls, [], "a refused custody request must never reach the API")

    def test_refusal_does_not_wait_for_the_billable_guard(self):
        # Without --yes the script would also refuse — the custody refusal
        # must come first and say so, not hide behind "re-run with --yes".
        result, calls = self.run_assign("codex-thor", "--devague-write")
        self.assertEqual(result.returncode, 1)
        self.assertIn(".devague/", result.stderr)
        self.assertNotIn("--yes", result.stderr)
        self.assertEqual(calls, [])

    def test_custody_is_checkout_bound_even_on_the_developer_lane(self):
        result, calls = self.run_assign(
            "developer", "--devague-write", "--repo", "/tmp/somewhere-else", "--yes"
        )
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn(self.CUSTODY_CHECKOUT, result.stderr)
        self.assertEqual(calls, [])

    def test_developer_lane_dispatch_declares_the_write_in_the_run_input(self):
        result, calls = self.run_assign(
            "developer",
            "--devague-write",
            "--no-watch",
            "--yes",
            responses={
                "/v1alpha1/workflows": {"digest": "sha256:feed"},
                "/v1alpha1/runs": {"id": "run-9", "state": "created"},
            },
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("run=run-9", result.stdout)
        self.assertIn("devague_write=true", result.stdout)
        publish = next(c for c in calls if c["url"].endswith("/v1alpha1/workflows"))
        rendered = json.loads(publish["body"])["source"]
        self.assertIn("devague_write: /run/input/devague_write", rendered)
        create = next(c for c in calls if c["url"].endswith("/v1alpha1/runs"))
        payload = json.loads(create["body"])["input"]
        self.assertIs(payload["devague_write"], True)
        self.assertEqual(payload["repo"], self.CUSTODY_CHECKOUT)

    def test_a_dispatch_that_does_not_ask_carries_no_binding_and_no_flag(self):
        result, calls = self.run_assign(
            "developer",
            "--no-watch",
            "--yes",
            responses={
                "/v1alpha1/workflows": {"digest": "sha256:feed"},
                "/v1alpha1/runs": {"id": "run-10", "state": "created"},
            },
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        publish = next(c for c in calls if c["url"].endswith("/v1alpha1/workflows"))
        self.assertNotIn(
            "devague_write: /run/input/devague_write", json.loads(publish["body"])["source"]
        )
        create = next(c for c in calls if c["url"].endswith("/v1alpha1/runs"))
        self.assertNotIn("devague_write", json.loads(create["body"])["input"])


if __name__ == "__main__":
    unittest.main()
