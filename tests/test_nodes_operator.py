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


if __name__ == "__main__":
    unittest.main()
