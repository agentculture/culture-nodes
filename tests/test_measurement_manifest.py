"""The measurement manifest: schema, canonical digest, validator (task t7).

`examples/harness-compare/measurements/manifest.py` is a zero-dependency
python3 module that validates a manifest against `schema.json`, canonicalises
it, and digests it. This is the module's own test coverage — it does not run
under `culture_nodes`'s package import path, so these tests import it
directly from its file path.

Three properties matter, and each has its own test group below:

1. **Validation** accepts a well-formed manifest and rejects a malformed one,
   naming the failing field path (unknown category, duplicate rule id, bad
   check kind, missing field, extra field).
2. **The digest is canonical**: stable across source key order and
   whitespace, but it changes the instant any rule field's value changes.
3. **`basic.json`** (the shipped basic three-rule set) has exactly one rule
   per category, and — when PyYAML happens to be importable in this
   interpreter — `basic.yaml` canonicalises to the same digest as
   `basic.json`.
"""

from __future__ import annotations

import copy
import importlib.util
import json
import sys
from pathlib import Path
from types import ModuleType

import pytest

ROOT = Path(__file__).resolve().parents[1]
MEASUREMENTS_DIR = ROOT / "examples" / "harness-compare" / "measurements"
YAML_FIXTURE = Path(__file__).resolve().parent / "fixtures" / "measurements" / "basic.yaml"
SCHEMA_PATH = MEASUREMENTS_DIR / "schema.json"
BASIC_JSON_PATH = MEASUREMENTS_DIR / "basic.json"
BASIC_YAML_PATH = YAML_FIXTURE


def _load_manifest_module() -> ModuleType:
    spec = importlib.util.spec_from_file_location(
        "harness_compare_measurement_manifest", MEASUREMENTS_DIR / "manifest.py"
    )
    assert spec is not None
    assert spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


manifest = _load_manifest_module()


def _yaml_importable() -> bool:
    try:
        import yaml  # noqa: F401
    except Exception:  # noqa: BLE001
        return False
    return True


def _basic_data() -> dict:
    return json.loads(BASIC_JSON_PATH.read_text(encoding="utf-8"))


def _schema() -> dict:
    return json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))


# --------------------------------------------------------------------------
# Validation: the basic manifest passes.
# --------------------------------------------------------------------------


def test_basic_manifest_validates() -> None:
    manifest.validate_manifest(_basic_data())


def test_basic_manifest_has_exactly_one_rule_per_category() -> None:
    data = _basic_data()
    categories = [rule["category"] for rule in data["rules"]]
    assert sorted(categories) == ["explain", "locate", "review"]


def test_basic_manifest_sandbox_and_runs_per_actor() -> None:
    data = _basic_data()
    for rule in data["rules"]:
        assert rule["sandbox"] == "read-only"
        assert rule["runs_per_actor"] == 2


# --------------------------------------------------------------------------
# Validation: each malformed construct is rejected, naming the failing path.
# --------------------------------------------------------------------------


def test_unknown_category_is_rejected_naming_the_path() -> None:
    data = _basic_data()
    data["rules"][0]["category"] = "not-a-real-category"
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.rules[0].category"


def test_duplicate_rule_id_is_rejected_naming_the_path() -> None:
    data = _basic_data()
    data["rules"][1]["id"] = data["rules"][0]["id"]
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.rules[1].id"
    assert data["rules"][0]["id"] in str(exc_info.value)


def test_bad_check_kind_is_rejected_naming_the_path() -> None:
    data = _basic_data()
    data["rules"][0]["check"]["kind"] = "not-a-real-kind"
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.rules[0].check.kind"


def test_missing_field_is_rejected_naming_the_path() -> None:
    data = _basic_data()
    del data["rules"][0]["instruction"]
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.rules[0]"
    assert "instruction" in str(exc_info.value)


def test_extra_field_is_rejected_naming_the_path() -> None:
    data = _basic_data()
    data["rules"][0]["unexpected_field"] = "surprise"
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.rules[0]"
    assert "unexpected_field" in str(exc_info.value)


def test_extra_top_level_field_is_rejected() -> None:
    data = _basic_data()
    data["unexpected_top_level"] = True
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$"


def test_wrong_version_is_rejected() -> None:
    data = _basic_data()
    data["version"] = 2
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.version"


def test_empty_actors_list_is_rejected() -> None:
    data = _basic_data()
    data["actors"] = []
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.actors"


def test_bad_rule_id_pattern_is_rejected() -> None:
    data = _basic_data()
    data["rules"][0]["id"] = "Not Valid! id"
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.rules[0].id"


def test_bad_sandbox_value_is_rejected() -> None:
    data = _basic_data()
    data["rules"][0]["sandbox"] = "read-write-and-more"
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.rules[0].sandbox"


def test_runs_per_actor_out_of_range_is_rejected() -> None:
    data = _basic_data()
    data["rules"][0]["runs_per_actor"] = 11
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.rules[0].runs_per_actor"


def test_missing_anchor_key_is_rejected() -> None:
    data = _basic_data()
    del data["rules"][0]["anchors"]["3"]
    with pytest.raises(manifest.ManifestError) as exc_info:
        manifest.validate_manifest(data)
    assert exc_info.value.path == "$.rules[0].anchors"


# --------------------------------------------------------------------------
# Canonical digest: stable across key order/whitespace, sensitive to content.
# --------------------------------------------------------------------------


def test_digest_stable_across_key_order() -> None:
    data = _basic_data()
    reordered = json.loads(
        json.dumps(data, sort_keys=True)
    )  # baseline: canonical form is sort-order independent by construction
    shuffled = {
        "rules": copy.deepcopy(data["rules"]),
        "version": data["version"],
        "actors": copy.deepcopy(data["actors"]),
    }
    assert manifest.digest_manifest(data) == manifest.digest_manifest(shuffled)
    assert manifest.digest_manifest(data) == manifest.digest_manifest(reordered)


def test_digest_stable_across_whitespace() -> None:
    data = _basic_data()
    compact = json.loads(json.dumps(data, separators=(",", ":")))
    spaced = json.loads(json.dumps(data, indent=8))
    assert manifest.digest_manifest(data) == manifest.digest_manifest(compact)
    assert manifest.digest_manifest(data) == manifest.digest_manifest(spaced)


def test_digest_has_sha256_prefix() -> None:
    digest = manifest.digest_manifest(_basic_data())
    assert digest.startswith("sha256:")
    assert len(digest) == len("sha256:") + 64


@pytest.mark.parametrize(
    "mutate",
    [
        lambda d: d["rules"][0].__setitem__("instruction", d["rules"][0]["instruction"] + " "),
        lambda d: d["rules"][0].__setitem__("runs_per_actor", 3),
        lambda d: d["rules"][0]["check"].__setitem__("expect", "something-else"),
        lambda d: d["rules"][0]["anchors"].__setitem__("5", "a different anchor"),
        lambda d: d["actors"].append("company/new-actor"),
        lambda d: d["rules"][0].__setitem__("sandbox", "workspace-write"),
    ],
    ids=[
        "instruction",
        "runs_per_actor",
        "check.expect",
        "anchors.5",
        "actors",
        "sandbox",
    ],
)
def test_digest_changes_when_any_rule_field_changes(mutate) -> None:
    original = _basic_data()
    mutated = _basic_data()
    mutate(mutated)
    assert manifest.digest_manifest(original) != manifest.digest_manifest(mutated)


def test_canonical_json_round_trips_to_same_data() -> None:
    data = _basic_data()
    canonical = manifest.canonical_json(data)
    assert json.loads(canonical) == data


# --------------------------------------------------------------------------
# YAML: authoring sugar over the JSON canonical form.
# --------------------------------------------------------------------------


@pytest.mark.skipif(not _yaml_importable(), reason="PyYAML is not importable in this interpreter")
def test_basic_yaml_canonicalises_to_the_same_digest_as_basic_json() -> None:
    json_data = manifest.load_manifest(BASIC_JSON_PATH)
    yaml_data = manifest.load_manifest(BASIC_YAML_PATH)
    assert manifest.digest_manifest(json_data) == manifest.digest_manifest(yaml_data)


def test_yaml_manifest_without_pyyaml_exits_with_hint(monkeypatch) -> None:
    monkeypatch.setattr(manifest, "_yaml_module", lambda: None)
    with pytest.raises(manifest.YamlUnavailableError):
        manifest.load_manifest(BASIC_YAML_PATH)


# --------------------------------------------------------------------------
# CLI surface.
# --------------------------------------------------------------------------


def test_cli_validate_exits_zero_for_basic_json(capsys) -> None:
    code = manifest.main(["validate", str(BASIC_JSON_PATH)])
    assert code == 0
    captured = capsys.readouterr()
    assert "ok:" in captured.out


def test_cli_digest_prints_sha256_prefixed_value(capsys) -> None:
    code = manifest.main(["digest", str(BASIC_JSON_PATH)])
    assert code == 0
    captured = capsys.readouterr()
    assert captured.out.strip().startswith("sha256:")


def test_cli_validate_exits_one_and_names_field_on_invalid_manifest(tmp_path, capsys) -> None:
    data = _basic_data()
    data["rules"][0]["category"] = "not-a-real-category"
    broken = tmp_path / "broken.json"
    broken.write_text(json.dumps(data), encoding="utf-8")
    code = manifest.main(["validate", str(broken)])
    assert code == 1
    captured = capsys.readouterr()
    assert "error:" in captured.err
    assert "$.rules[0].category" in captured.err


def test_cli_missing_file_exits_one() -> None:
    code = manifest.main(["validate", "/nonexistent/manifest.json"])
    assert code == 1


def test_cli_unsupported_extension_exits_one(tmp_path) -> None:
    bogus = tmp_path / "manifest.txt"
    bogus.write_text("{}", encoding="utf-8")
    code = manifest.main(["validate", str(bogus)])
    assert code == 1


def test_cli_yaml_without_pyyaml_exits_two(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr(manifest, "_yaml_module", lambda: None)
    yaml_file = tmp_path / "manifest.yaml"
    yaml_file.write_text("version: 1\n", encoding="utf-8")
    code = manifest.main(["validate", str(yaml_file)])
    assert code == 2


# --------------------------------------------------------------------------
# Schema sanity: schema.json itself is well-formed 2020-12 JSON Schema shape.
# --------------------------------------------------------------------------


def test_schema_declares_2020_12_dialect() -> None:
    schema = _schema()
    assert schema["$schema"] == "https://json-schema.org/draft/2020-12/schema"


def test_schema_root_is_additional_properties_false_object() -> None:
    schema = _schema()
    assert schema["type"] == "object"
    assert schema["additionalProperties"] is False
    assert set(schema["required"]) == {"version", "actors", "rules"}


def test_schema_rule_object_is_additional_properties_false() -> None:
    schema = _schema()
    rule_schema = schema["properties"]["rules"]["items"]
    assert rule_schema["additionalProperties"] is False
    assert rule_schema["properties"]["category"]["enum"] == ["locate", "review", "explain"]
    assert rule_schema["properties"]["check"]["properties"]["kind"]["enum"] == [
        "grep-cites-file-line",
        "seeded-defect-named",
        "tests-named",
    ]


def test_boolean_version_is_not_the_numeric_const() -> None:
    """Python's True == 1 must not let ``"version": true`` pass ``const: 1``."""
    data = json.loads(BASIC_JSON_PATH.read_text())
    data["version"] = True
    with pytest.raises(manifest.ManifestError) as excinfo:
        manifest.validate_manifest(data)
    assert "version" in str(excinfo.value)
