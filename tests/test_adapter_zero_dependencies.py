"""The adapters' zero-dependency property, enforced rather than remembered (task t18).

Every bridge in adapters/ declares `dependencies = []` and imports nothing but
the standard library. That is not decoration: a bridge is installed beside
somebody else's toolchain on a host this repo does not control, so a PyPI
dependency graph is one more thing that host has to reconcile before an actor
can dial in. The dial-in transport is where the property gets tested in
practice -- a websocket client was considered for it and rejected partly
BECAUSE it would have broken this -- and that argument was, until now, held in
memory instead of in a test.

Two halves, because either alone reports green over a broken repo:

  1. The DECLARATION. `scripts/check-zero-runtime-deps.sh` used to load exactly
     one file, the root pyproject, so an adapter could gain a runtime
     dependency and nothing would fail. It now checks every manifest, and the
     negative case below proves it rejects one that gained a dependency.

  2. The CODE. A manifest declaring `[]` while a module says `import websockets`
     is the actual failure mode -- the manifest is a promise, the import is the
     fact. The scan is an AST walk rather than a grep because these modules
     carry long prose comments; `test_the_import_scan_reads_code_not_prose`
     pins that a docstring sentence starting "from a websocket library" is not
     mistaken for an import.

Byte-identity of the shared dial-in module across the five adapters is the
third property, and it lives in Go with the other shape guards:
tests/lint/dialintransport_test.go.
"""

import ast
import subprocess  # nosec B404 - runs an in-repo guard script, no external input
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
GUARD = ROOT / "scripts" / "check-zero-runtime-deps.sh"

# The bridges shipped today. The lists below are DISCOVERED, not hard-coded, so
# a sixth adapter is guarded the day it lands; this count only refuses a
# vacuous pass if the discovery ever stops finding anything.
EXPECTED_ADAPTERS = 5


def adapter_manifests():
    """Every adapters/<name>/pyproject.toml, sorted."""
    return sorted((ROOT / "adapters").glob("*/pyproject.toml"))


def adapter_packages():
    """Every adapters/<name>/src/<package> directory, sorted."""
    return sorted(path for path in (ROOT / "adapters").glob("*/src/*") if path.is_dir())


def run_guard(*args):
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        [str(GUARD), *(str(arg) for arg in args)], text=True, capture_output=True
    )


def third_party_imports(module: Path, package: str):
    """Imports in `module` that are neither stdlib nor `package`'s own.

    Relative imports are skipped: they cannot reach outside the package by
    construction.
    """
    found = []
    for node in ast.walk(ast.parse(module.read_text())):
        if isinstance(node, ast.Import):
            roots = [alias.name.split(".")[0] for alias in node.names]
        elif isinstance(node, ast.ImportFrom):
            if node.level:
                continue
            roots = [(node.module or "").split(".")[0]]
        else:
            continue
        for root in roots:
            if root in sys.stdlib_module_names or root == package:
                continue
            found.append(f"{module}:{node.lineno}: {root}")
    return found


def test_the_gate_checks_the_root_and_every_adapter_manifest():
    """The gate's own reach: the bug t18 closes was a checker that read one file."""
    listed = [Path(line) for line in run_guard("--list").stdout.split()]
    assert listed == [ROOT / "pyproject.toml"] + adapter_manifests()
    assert len(adapter_manifests()) >= EXPECTED_ADAPTERS


def test_every_adapter_declares_no_runtime_dependencies():
    result = run_guard(ROOT / "pyproject.toml", *adapter_manifests())
    assert result.returncode == 0, result.stderr
    for manifest in adapter_manifests():
        assert str(manifest) in result.stdout


def test_the_gate_rejects_an_adapter_that_gained_a_runtime_dependency(tmp_path):
    """The half that makes the guard real: watch it go red on a violating manifest."""
    source = (ROOT / "adapters" / "codex" / "pyproject.toml").read_text()
    violating = source.replace("dependencies = []", 'dependencies = ["websockets>=13"]', 1)
    assert violating != source, "the codex manifest no longer declares dependencies = []"
    manifest = tmp_path / "pyproject.toml"
    manifest.write_text(violating)

    result = run_guard(manifest)
    assert result.returncode != 0
    assert "websockets" in result.stderr


def test_the_dial_in_client_imports_only_the_standard_library():
    """Criterion 2: no third-party import anywhere in the dial-in path."""
    packages = adapter_packages()
    assert len(packages) >= EXPECTED_ADAPTERS, packages
    violations = []
    for package in packages:
        dialin = package / "dialin.py"
        assert dialin.exists(), f"{package} ships no dialin.py"
        violations += third_party_imports(dialin, package.name)
    assert not violations, (
        "the dial-in transport reached outside the standard library: a bridge is installed "
        "beside somebody else's toolchain, and a transport dependency is that host's problem "
        "before it is ours.\n" + "\n".join(violations)
    )


def test_no_adapter_module_imports_a_third_party_package():
    """`dependencies = []` is a promise; the import list is the fact behind it."""
    violations = []
    for package in adapter_packages():
        for module in sorted(package.rglob("*.py")):
            violations += third_party_imports(module, package.name)
    assert not violations, "\n".join(violations)


def test_the_import_scan_reads_code_not_prose(tmp_path):
    """Proof the scanner catches what it claims to -- and only that.

    A grep for `^from ` would flag the docstring's second sentence. These
    modules are heavy with exactly that kind of prose, so a scanner that cried
    wolf would be turned off within a week.
    """
    module = tmp_path / "dialin.py"
    module.write_text(
        '"""Reverse transport.\n\n'
        "This used to read frames from a websockets client, and json is\n"
        'imported below for the same reason it always was."""\n\n'
        "import json\n"
        "import websockets\n"
        "from anyio import sleep\n"
        "from codex_bridge import config\n"
        "from . import mapping\n"
    )
    found = third_party_imports(module, "codex_bridge")
    assert [line.rsplit(": ", 1)[1] for line in found] == ["websockets", "anyio"]
