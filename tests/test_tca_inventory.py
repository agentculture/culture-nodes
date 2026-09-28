"""Contract checks for the generated TCA migration inventory."""

import json
import pathlib
import subprocess
import tempfile
import unittest

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/tca-inventory.sh"


class InventoryTest(unittest.TestCase):
    def render(self, path):
        return subprocess.run(
            ["bash", str(SCRIPT), "--output", str(path)],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=True,
        ).stdout

    def test_regeneration_is_repeatable(self):
        with tempfile.TemporaryDirectory() as temp:
            output = pathlib.Path(temp) / "inventory.md"
            first_stdout = self.render(output)
            first = output.read_bytes()
            second_stdout = self.render(output)
            self.assertEqual(first, output.read_bytes())
            self.assertEqual(first_stdout, second_stdout)
            self.assertEqual(first, (ROOT / "docs/migration/tca-inventory.md").read_bytes())

    def test_inventory_covers_workflows_configuration_and_consumers(self):
        tracked = subprocess.check_output(["git", "ls-files", "-z"], cwd=ROOT).decode().split("\0")
        workflow = [
            path
            for path in tracked
            if (
                path.startswith("examples/")
                or "/testdata/" in path
                or "/__golden__/" in path
                or path.startswith(("schemas/examples/", "schemas/workflow/"))
            )
            and "workflow" in pathlib.PurePosixPath(path).name
            and pathlib.PurePosixPath(path).suffix in (".yaml", ".yml", ".json")
        ]
        doc = (ROOT / "docs/migration/tca-inventory.md").read_text()
        for path in workflow:
            with self.subTest(path=path):
                self.assertIn("`" + path + "`", doc)
        for item in (
            "schedules",
            "notifier posts",
            "repair routing",
            "hand-turns",
            "affinity",
            "workflow generation",
            "devague plan import",
            "/v1alpha1/runs",
            "/v1alpha1/node-runs",
            "/v1alpha1/human-tasks",
            "/v1alpha1/pending-decisions",
            "/v1alpha1/tickets",
            "scripts/collect-handover.py",
            "nodes-operator",
            "dev.culture.nodes.run.created",
            "dev.culture.nodes.run.completed",
            "dev.culture.nodes.run.failed",
            "dev.culture.nodes.run.cancelled",
            "dev.culture.nodes.run.bounded",
        ):
            with self.subTest(item=item):
                self.assertIn(item, doc)
        self.assertIn("| Target declaration form | Parity status |", doc)

    def test_before_state_is_printed(self):
        with tempfile.TemporaryDirectory() as temp:
            output = self.render(pathlib.Path(temp) / "inventory.md")
        self.assertRegex(output, r"workflow files: \d+")
        self.assertRegex(output, r"node kinds: \d+")
        self.assertRegex(output, r"github\.pr\.approved trigger: (present|absent)")
        self.assertRegex(output, r"GitHub messaging actor: (present|absent)")

    def test_pr_upkeep_and_jira_intake_parity_rows_are_filled(self):
        doc = (ROOT / "docs/migration/tca-inventory.md").read_text()
        for path in (
            "examples/pr-upkeep/workflow.yaml",
            "examples/pr-upkeep/sweep-cycle.workflow.yaml",
            "examples/jira-intake/workflow.yaml",
        ):
            with self.subTest(path=path):
                row = next(
                    line for line in doc.splitlines() if line.startswith("| `" + path + "` |")
                )
                self.assertIn("declarations/manifest.json", row)
                self.assertNotIn("| pending |", row)

    def test_example_declarations_cover_graph_nodes_edges_triggers_and_caps(self):
        for workflow, files in {
            "pr-upkeep": ("workflow.yaml", "sweep-cycle.workflow.yaml"),
            "jira-intake": ("workflow.yaml",),
        }.items():
            base = ROOT / "examples" / workflow
            manifest = json.loads((base / "declarations" / "manifest.json").read_text())
            declarations = {
                path: json.loads((base / "declarations" / path).read_text())
                for path in manifest["declarations"]
            }
            for file in files:
                graph = yaml.safe_load((base / file).read_text())["spec"]
                steps = [step for step in manifest["steps"] if step["graph_workflow"] == file]
                self.assertEqual(set(graph["nodes"]), {step["graph_node"] for step in steps})
                for step in steps:
                    declaration = declarations[step["graph_node"] + ".json"]
                    self.assertEqual(step["declaration"], declaration["name"])
                    self.assertEqual(file, declaration["action"]["with"]["graph_workflow"])
                for edge in graph["edges"]:
                    source, outcome = edge["from"].split(".", 1)
                    source_targets = {
                        item["to"]
                        for item in graph["edges"]
                        if item["from"].split(".", 1)[0] == source
                    }
                    target_sources = {
                        item["from"].split(".", 1)[0]
                        for item in graph["edges"]
                        if item["to"] == edge["to"]
                    }
                    kind = (
                        "can"
                        if edge.get("when") or len(source_targets) > 1 or len(target_sources) > 1
                        else "must"
                    )
                    self.assertIn(
                        {
                            "from": workflow + "-" + edge["to"],
                            "to": workflow + "-" + source,
                            "kind": kind,
                            "outcome": outcome,
                            "when": edge.get("when"),
                        },
                        manifest["edges"],
                    )
                entry = declarations[graph["entry"] + ".json"]
                self.assertEqual(
                    graph["triggers"][0]["onEvent"], entry["trigger"]["with"]["legacy_event"]
                )
                self.assertIn(
                    graph["triggers"][0].get("when", "true"),
                    entry["condition"],
                )
                if "maxConcurrentSubjectRuns" in graph["limits"]:
                    self.assertEqual(
                        graph["limits"]["maxConcurrentSubjectRuns"],
                        entry["trigger"]["max_concurrent_subject"],
                    )
                if "affinity" in graph:
                    self.assertEqual(
                        graph["affinity"],
                        declarations["fix.json"]["action"]["with"]["actor_selection"],
                    )


if __name__ == "__main__":
    unittest.main()
