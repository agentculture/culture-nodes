#!/usr/bin/env python3
"""Measurement manifest: schema validation, canonicalisation and digest.

A *measurement manifest* declares a fixed set of comparison rules (locate /
review / explain) run against a fixed list of actors — see ``schema.json``
for the shape and ``README.md`` for how to add or change a rule. This module
is the only place that reads or validates a manifest; the runner (task t11,
``run.py``) consumes an already-validated manifest and does not re-implement
any of this.

Zero third-party dependencies. JSON is the canonical, always-supported
format (CLAUDE.md: "JSON is canonical, YAML is authoring sugar"). YAML is
accepted only when PyYAML happens to be importable in the running
interpreter — this repo's runtime package ships zero dependencies
(``pyproject.toml``'s ``dependencies = []``), so PyYAML is not guaranteed to
be present; when it is not, a ``.yaml``/``.yml`` manifest is refused with a
hint to use the ``.json`` form instead.

The validator below is deliberately **not** a general JSON Schema engine —
it implements exactly the keywords ``schema.json`` uses (``type``, ``const``,
``enum``, ``pattern``, ``minLength``, ``minItems``, ``minimum``, ``maximum``,
``properties``, ``required``, ``additionalProperties``, ``items``) plus one
business rule JSON Schema 2020-12 cannot express directly: rule ``id``
values must be unique across ``rules``.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Any

HERE = Path(__file__).resolve().parent
SCHEMA_PATH = HERE / "schema.json"


class YamlUnavailableError(Exception):
    """Raised instead of :class:`ManifestError` when a ``.yaml``/``.yml``
    manifest is given but no YAML parser is importable.

    Kept as a distinct exception (rather than a flag on ManifestError) so
    the CLI can exit ``2`` (environment error) for this case and ``1`` for
    every other manifest problem — the same load/validate/user-error split
    ``culture_nodes/cli/_errors.py`` uses for its own exit-code policy.
    """


class ManifestError(Exception):
    """A manifest failed validation or could not be loaded.

    ``path`` is the JSON-Schema-style dotted/bracketed field path (e.g.
    ``$.rules[2].check.kind``) that failed, when the failure is
    field-specific; it is ``None`` for load-time failures (bad JSON, missing
    file, unsupported format).
    """

    def __init__(self, message: str, path: str | None = None) -> None:
        self.path = path
        self.message = message
        located = f"{path}: {message}" if path else message
        super().__init__(located)


def _yaml_module() -> Any:
    """Return the ``yaml`` module if importable, else ``None``.

    Never raises: an unrelated import-time error in a third-party package
    would otherwise surface as a confusing traceback for what is, from the
    caller's perspective, "no YAML support here".
    """
    try:
        import yaml  # type: ignore[import-untyped]
    except Exception:  # noqa: BLE001 - "no YAML available" is the only signal we want
        return None
    return yaml


def load_manifest(path: str | Path) -> Any:
    """Load a manifest file (JSON always; YAML only if PyYAML is importable).

    Raises :class:`ManifestError` (no field path) on a missing file, invalid
    JSON, unsupported extension, or an unavailable YAML parser.
    """
    p = Path(path)
    if not p.exists():
        raise ManifestError(f"manifest file not found: {p}")
    suffix = p.suffix.lower()
    text = p.read_text(encoding="utf-8")
    if suffix == ".json":
        try:
            return json.loads(text)
        except json.JSONDecodeError as exc:
            raise ManifestError(f"invalid JSON: {exc}") from exc
    if suffix in (".yaml", ".yml"):
        yaml = _yaml_module()
        if yaml is None:
            raise YamlUnavailableError(
                "PyYAML is not importable in this interpreter; author the "
                "manifest as JSON instead (JSON is canonical, YAML is "
                "authoring sugar), or install a YAML parser"
            )
        try:
            return yaml.safe_load(text)
        except Exception as exc:  # noqa: BLE001 - report as ManifestError uniformly
            raise ManifestError(f"invalid YAML: {exc}") from exc
    raise ManifestError(f"unsupported manifest extension: {p.suffix!r} (use .json or .yaml)")


def canonical_json(data: Any) -> str:
    """Render ``data`` as canonical JSON: sorted keys, compact separators, ASCII-safe.

    Stable across source key order and incidental whitespace, so two
    manifests that mean the same thing digest the same, and any change to a
    rule field changes the digest.
    """
    return json.dumps(data, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def digest_manifest(data: Any) -> str:
    """Return the manifest's content digest as ``sha256:<hex>``."""
    canonical = canonical_json(data).encode("ascii")
    return "sha256:" + hashlib.sha256(canonical).hexdigest()


# --------------------------------------------------------------------------
# Hand-written validator for exactly the schema.json constructs in use.
# --------------------------------------------------------------------------


def _type_name(value: Any) -> str:
    if isinstance(value, bool):
        return "boolean"
    if isinstance(value, int):
        return "integer"
    if isinstance(value, float):
        return "number"
    if isinstance(value, str):
        return "string"
    if isinstance(value, list):
        return "array"
    if isinstance(value, dict):
        return "object"
    if value is None:
        return "null"
    return type(value).__name__


def _validate(instance: Any, schema: dict[str, Any], path: str) -> None:
    if "const" in schema:
        # JSON types: True == 1 in Python, so a bool must not satisfy a numeric const.
        if instance != schema["const"] or isinstance(instance, bool) != isinstance(
            schema["const"], bool
        ):
            raise ManifestError(f"must equal {schema['const']!r}, got {instance!r}", path)
        return

    if "enum" in schema:
        if instance not in schema["enum"]:
            raise ManifestError(f"must be one of {schema['enum']!r}, got {instance!r}", path)
        # fall through: an "enum" schema may still combine with "type" in
        # principle, but schema.json never does, so no further checks apply.
        return

    expected_type = schema.get("type")
    if expected_type is not None:
        actual = _type_name(instance)
        ok = actual == expected_type or (expected_type == "integer" and actual == "integer")
        if not ok:
            raise ManifestError(f"must be of type {expected_type!r}, got {actual!r}", path)

    if expected_type == "string":
        if "minLength" in schema and len(instance) < schema["minLength"]:
            raise ManifestError(f"must have length >= {schema['minLength']}", path)
        if "pattern" in schema and re.fullmatch(schema["pattern"], instance) is None:
            raise ManifestError(f"must match pattern {schema['pattern']!r}, got {instance!r}", path)

    if expected_type == "integer":
        if "minimum" in schema and instance < schema["minimum"]:
            raise ManifestError(f"must be >= {schema['minimum']}", path)
        if "maximum" in schema and instance > schema["maximum"]:
            raise ManifestError(f"must be <= {schema['maximum']}", path)

    if expected_type == "array":
        if "minItems" in schema and len(instance) < schema["minItems"]:
            raise ManifestError(f"must have at least {schema['minItems']} item(s)", path)
        item_schema = schema.get("items")
        if item_schema is not None:
            for i, item in enumerate(instance):
                _validate(item, item_schema, f"{path}[{i}]")

    if expected_type == "object":
        properties: dict[str, Any] = schema.get("properties", {})
        required: list[str] = schema.get("required", [])
        for key in required:
            if key not in instance:
                raise ManifestError(f"missing required property {key!r}", path)
        if schema.get("additionalProperties") is False:
            unknown = sorted(set(instance) - set(properties))
            if unknown:
                raise ManifestError(
                    f"unexpected propert{'y' if len(unknown) == 1 else 'ies'} {unknown!r}", path
                )
        for key, value in instance.items():
            if key in properties:
                _validate(value, properties[key], f"{path}.{key}")


def validate_manifest(data: Any, schema: dict[str, Any] | None = None) -> None:
    """Validate ``data`` against ``schema`` (default: ``schema.json`` on disk).

    Raises :class:`ManifestError` naming the failing field path. Also
    enforces rule-id uniqueness across ``rules``, a business rule JSON
    Schema 2020-12 has no keyword for.
    """
    if schema is None:
        schema = json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))
    _validate(data, schema, "$")

    if isinstance(data, dict) and isinstance(data.get("rules"), list):
        seen: dict[str, int] = {}
        for i, rule in enumerate(data["rules"]):
            if not isinstance(rule, dict) or "id" not in rule:
                continue
            rule_id = rule["id"]
            if rule_id in seen:
                raise ManifestError(
                    f"duplicate rule id {rule_id!r} (also at $.rules[{seen[rule_id]}])",
                    f"$.rules[{i}].id",
                )
            seen[rule_id] = i


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def _load_and_validate(manifest_path: str) -> tuple[Any, int | None]:
    """Load + validate a manifest, returning (data, None) or (None, exit_code).

    Shared by every subcommand so the exit-code split (2 for an unavailable
    YAML parser, 1 for every other manifest problem) is defined exactly
    once.
    """
    try:
        data = load_manifest(manifest_path)
        validate_manifest(data)
    except YamlUnavailableError as exc:
        print(f"error: {exc}", file=sys.stderr)
        print("hint: author the manifest as .json instead", file=sys.stderr)
        return None, 2
    except ManifestError as exc:
        print(f"error: {exc.message}", file=sys.stderr)
        if exc.path:
            print(f"path: {exc.path}", file=sys.stderr)
        return None, 1
    return data, None


def _cmd_validate(args: argparse.Namespace) -> int:
    data, code = _load_and_validate(args.manifest)
    if code is not None:
        return code
    print(f"ok: {args.manifest} is a valid measurement manifest")
    return 0


def _cmd_digest(args: argparse.Namespace) -> int:
    data, code = _load_and_validate(args.manifest)
    if code is not None:
        return code
    print(digest_manifest(data))
    return 0


def _cmd_canonical(args: argparse.Namespace) -> int:
    data, code = _load_and_validate(args.manifest)
    if code is not None:
        return code
    print(canonical_json(data))
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="manifest.py",
        description="Validate, canonicalise and digest a measurement manifest.",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p_validate = sub.add_parser("validate", help="validate a manifest against schema.json")
    p_validate.add_argument("manifest", help="path to a .json or .yaml manifest")
    p_validate.set_defaults(func=_cmd_validate)

    p_digest = sub.add_parser("digest", help="print sha256:<hex> of the canonical manifest")
    p_digest.add_argument("manifest", help="path to a .json or .yaml manifest")
    p_digest.set_defaults(func=_cmd_digest)

    p_canonical = sub.add_parser("canonical", help="print the canonical JSON form")
    p_canonical.add_argument("manifest", help="path to a .json or .yaml manifest")
    p_canonical.set_defaults(func=_cmd_canonical)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    sys.exit(main())
