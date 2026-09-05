"""The runner's DEFAULT manifest has to be one the shipped graph can serve.

``run.py`` used to default to ``basic.json``, which names all four thor/orin
actors. ``examples/harness-compare/workflow.yaml`` pins slot ``pi`` to
``actor://company/pi-thor`` and slot ``qwen`` to ``actor://company/qwen-thor``
and nothing in a run input redirects a slot, so those four actors collapse
onto two slots and ``fleet.refuse_slot_collisions`` aborts the pass *before*
the first dispatch (issue #304). A no-flag ``run.py`` could therefore never
get past argument handling, and the README's copy-pasteable runner command
selected the same unusable file.

The refusal is correct — running pi-thor twice and labelling one result
pi-orin would be a fabricated measurement. What was wrong is that it was the
*default*. So these tests pin the property the default has to have, in terms
of the graph rather than in terms of a filename:

1. every actor in the default manifest maps to a DISTINCT slot, so
   ``refuse_slot_collisions`` passes;
2. every slot it maps to is a node the graph actually declares;
3. the default manifest is a valid manifest by ``schema.json``;
4. the two shipped manifests carry the same rule list (they differ only in
   ``actors``), so editing a rule in one and not the other is caught;
5. every ``run.py --manifest <path>`` command in the measurements README
   names a manifest that satisfies (1) — the documented procedure and the
   default cannot drift apart from what the graph can run.

When #304 gives the graph a slot per registered actor, (1) starts holding
for ``basic.json`` too and the default can move back to it; nothing here has
to be relaxed for that to happen.
"""

from __future__ import annotations

import importlib.util
import json
import re
import sys
from pathlib import Path
from types import ModuleType

import pytest

ROOT = Path(__file__).resolve().parents[1]
HARNESS_DIR = ROOT / "examples" / "harness-compare"
MEASUREMENTS_DIR = HARNESS_DIR / "measurements"
WORKFLOW_PATH = HARNESS_DIR / "workflow.yaml"
README_PATH = MEASUREMENTS_DIR / "README.md"


def _load(name: str) -> ModuleType:
    spec = importlib.util.spec_from_file_location(
        f"harness_compare_default_manifest_{name}", MEASUREMENTS_DIR / f"{name}.py"
    )
    assert spec is not None
    assert spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


runner = _load("run")
fleet = _load("fleet")
manifest_mod = _load("manifest")


def _resolved_slots(manifest_path: Path) -> list[dict[str, str]]:
    """What ``resolve_actors`` would compute, without the registry lookup.

    The registry half needs a live control plane; the slot half is pure and
    is the half that decides whether a pass can start at all.
    """
    actors = json.loads(manifest_path.read_text(encoding="utf-8"))["actors"]
    return [{"actor_key": key, "slot": fleet.slot_for(key, {})} for key in actors]


def _graph_slot_nodes() -> set[str]:
    """The agent slots ``workflow.yaml`` declares, read from the graph itself."""
    yaml = pytest.importorskip("yaml", reason="workflow.yaml is parsed to read its slots")
    graph = yaml.safe_load(WORKFLOW_PATH.read_text(encoding="utf-8"))
    nodes = graph["spec"]["nodes"]
    return {name for name, node in nodes.items() if node.get("kind") == "agent"}


def test_the_default_manifest_exists_and_is_the_thor_set() -> None:
    assert runner.DEFAULT_MANIFEST.is_file()
    assert runner.DEFAULT_MANIFEST.parent == MEASUREMENTS_DIR


def test_the_default_manifests_actors_do_not_collide_on_one_slot() -> None:
    """The pass can reach dispatch: this is the whole point of the default."""
    fleet.refuse_slot_collisions(_resolved_slots(runner.DEFAULT_MANIFEST))


def test_every_slot_the_default_names_is_a_node_in_the_graph() -> None:
    """A slot the graph does not declare would never fan out."""
    declared = _graph_slot_nodes()
    for actor in _resolved_slots(runner.DEFAULT_MANIFEST):
        assert actor["slot"] in declared, (
            f"{actor['actor_key']} maps to slot {actor['slot']}, "
            f"which workflow.yaml does not declare (has: {sorted(declared)})"
        )


def test_the_default_manifest_is_a_valid_manifest() -> None:
    loaded = manifest_mod.load_manifest(runner.DEFAULT_MANIFEST)
    manifest_mod.validate_manifest(loaded)


def test_the_four_actor_manifest_is_still_refused_by_this_graph() -> None:
    """Why the default is not ``basic.json`` — asserted, not asserted-in-prose.

    If #304 lands a slot per registered actor this stops raising, which is
    the signal to move ``DEFAULT_MANIFEST`` back to the four-actor set.
    """
    four_actor_slots = _resolved_slots(MEASUREMENTS_DIR / "basic.json")
    with pytest.raises(fleet.RunnerError) as excinfo:
        fleet.refuse_slot_collisions(four_actor_slots)
    assert "map to one workflow slot" in str(excinfo.value)


def test_both_shipped_manifests_carry_the_same_rules() -> None:
    """They differ only in ``actors``; a rule edit must land in both."""
    thor = json.loads((MEASUREMENTS_DIR / "basic-thor.json").read_text(encoding="utf-8"))
    full = json.loads((MEASUREMENTS_DIR / "basic.json").read_text(encoding="utf-8"))
    assert thor["rules"] == full["rules"]
    assert set(thor["actors"]) < set(full["actors"])


def test_every_documented_runner_command_names_a_runnable_manifest() -> None:
    """The README's copy-pasteable commands, checked against the graph.

    The original defect was half a documentation one: two `run.py` blocks in
    this README selected the manifest the runner refuses.
    """
    readme = README_PATH.read_text(encoding="utf-8")
    commands = re.findall(r"run\.py\s*\\\s*\n\s*--manifest\s+(\S+)", readme)
    assert commands, "no `run.py --manifest ...` command found in the README"
    for path in commands:
        manifest_path = ROOT / path
        assert manifest_path.is_file(), f"README names a missing manifest: {path}"
        fleet.refuse_slot_collisions(_resolved_slots(manifest_path))
