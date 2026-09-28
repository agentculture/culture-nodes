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
        """Every graph node, edge, trigger, cap and affinity rule maps to the chain.

        A graph node maps to one declaration per predecessor path (t31b); a
        graph edge maps to the links whose successor starts on the
        predecessor's landing node. The structural chain rules themselves
        (reaction triggers, must closure) are pinned in
        internal/api/declaration_examples_test.go.
        """
        for workflow, files in {
            "pr-upkeep": ("workflow.yaml", "sweep-cycle.workflow.yaml"),
            "jira-intake": ("workflow.yaml",),
        }.items():
            base = ROOT / "examples" / workflow
            manifest = json.loads((base / "declarations" / "manifest.json").read_text())
            by_name = {}
            for path in manifest["declarations"]:
                source = json.loads((base / "declarations" / path).read_text())
                self.assertEqual(workflow + "-" + path.removesuffix(".json"), source["name"])
                by_name[source["name"]] = source
            links = {(link["from"], link["to"]): link for link in manifest["links"]}
            for link in manifest["links"]:
                self.assertIn(link["kind"], ("must", "can"))
                self.assertTrue(link["why"].strip(), link)
            for file in files:
                graph = yaml.safe_load((base / file).read_text())["spec"]
                steps = [step for step in manifest["steps"] if step["graph_workflow"] == file]
                self.assertEqual(set(graph["nodes"]), {step["graph_node"] for step in steps})
                for step in steps:
                    self.assertTrue(step["declarations"], step)
                    for name in step["declarations"]:
                        action = by_name[name]["action"]
                        self.assertEqual(file, action["with"]["graph_workflow"])
                        self.assertEqual(step["graph_node"], action["with"]["graph_node"])
                edges = [edge for edge in manifest["edges"] if edge["graph_workflow"] == file]
                self.assertEqual(len(graph["edges"]), len(edges))
                for edge in graph["edges"]:
                    source, outcome = edge["from"].split(".", 1)
                    realized = next(
                        item
                        for item in edges
                        if (item["from"], item["outcome"], item["to"])
                        == (source, outcome, edge["to"])
                    )
                    self.assertEqual(edge.get("when"), realized["when"])
                    self.assertTrue(realized["links"], realized)
                    for pair in realized["links"]:
                        self.assertIn((pair["from"], pair["to"]), links)
                        successor, predecessor = by_name[pair["from"]], by_name[pair["to"]]
                        self.assertEqual(
                            predecessor["landing_node"]["name"], successor["start_node"]["name"]
                        )
                        self.assertEqual(edge["to"], successor["action"]["with"]["graph_node"])
                        self.assertEqual(source, predecessor["action"]["with"]["graph_node"])
                entry_node = graph["entry"]
                entry = next(e for e in manifest["entries"] if e["graph_workflow"] == file)
                entry_source = by_name[entry["declaration"]]
                self.assertEqual(entry_node, entry_source["action"]["with"]["graph_node"])
                self.assertEqual("root", entry_source["start_node"]["name"])
                self.assertEqual(graph["triggers"][0], entry["legacy_trigger"])
                self.assertNotIn("with", entry_source["trigger"])
                guard = graph["triggers"][0].get("when")
                if guard:
                    # The engine's event variables are the payload itself.
                    translated = guard.replace("event.payload.", "event.")
                    self.assertIn(translated, entry_source["condition"])
                if "maxConcurrentSubjectRuns" in graph["limits"]:
                    self.assertEqual(
                        graph["limits"]["maxConcurrentSubjectRuns"],
                        entry_source["trigger"]["max_concurrent_subject"],
                    )
                if "affinity" in graph:
                    for name in next(s for s in steps if s["graph_node"] == "fix")["declarations"]:
                        self.assertEqual(
                            graph["affinity"], by_name[name]["action"]["with"]["actor_selection"]
                        )


if __name__ == "__main__":
    unittest.main()
