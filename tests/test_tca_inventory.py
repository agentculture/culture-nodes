"""Contract checks for the generated TCA migration inventory."""

import pathlib
import subprocess
import tempfile
import unittest

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


if __name__ == "__main__":
    unittest.main()
