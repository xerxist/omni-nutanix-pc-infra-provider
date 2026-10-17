#!/usr/bin/env python3
"""Render the portable Deployment; never reads credentials or applies resources."""
import argparse
import json
import os
from pathlib import Path
import re
from string import Template


ROOT = Path(__file__).resolve().parents[1]
TEMPLATE = ROOT / "deploy/kubernetes/deployment.yaml.tmpl"
NAMES = {
    "PROVIDER_DEPLOYMENT_NAME", "PROVIDER_NAMESPACE", "PROVIDER_CONFIGMAP_NAME",
    "PROVIDER_CREDENTIAL_SECRET_NAME", "PROVIDER_CA_CONFIGMAP_NAME",
}
CPU = {"PROVIDER_CPU_REQUEST", "PROVIDER_CPU_LIMIT"}
MEMORY = {"PROVIDER_MEMORY_REQUEST", "PROVIDER_MEMORY_LIMIT", "PROVIDER_TMP_SIZE_LIMIT"}
REQUIRED = NAMES | CPU | MEMORY | {"PROVIDER_IMAGE", "PROVIDER_TERMINATION_GRACE_SECONDS"}


def load_env_file(path):
    values = {}
    for number, line in enumerate(path.read_text().splitlines(), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        name, separator, value = line.partition("=")
        if not separator or name not in REQUIRED:
            raise ValueError(f"Invalid deployment input at line {number}; contents suppressed")
        if name in values:
            raise ValueError(f"Duplicate deployment input: {name}")
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        values[name] = value
    return values


def render(values):
    missing = sorted(name for name in REQUIRED if not values.get(name))
    if missing:
        raise ValueError("Required deployment inputs missing: " + ", ".join(missing))
    encoded = {}
    for name in REQUIRED:
        value = values[name]
        if not isinstance(value, str):
            raise ValueError(f"{name} must be a string input")
        if name in NAMES and (len(value) > 63 or not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]*[a-z0-9])?", value)):
            raise ValueError(f"{name} must be a DNS label")
        if name == "PROVIDER_IMAGE" and (re.search(r"\s", value) or "://" in value or "#" in value or "?" in value):
            raise ValueError("PROVIDER_IMAGE must be an image reference, not a URL or multiline value")
        if name in CPU and not re.fullmatch(r"[0-9]+(?:\.[0-9]+)?m?", value):
            raise ValueError(f"{name} must be a CPU quantity")
        if name in MEMORY and not re.fullmatch(r"[1-9][0-9]*(?:Ki|Mi|Gi|Ti|K|M|G|T)", value):
            raise ValueError(f"{name} must be a positive memory quantity")
        if name == "PROVIDER_TERMINATION_GRACE_SECONDS":
            if not value.isdigit() or not 1 <= int(value) <= 86400:
                raise ValueError("PROVIDER_TERMINATION_GRACE_SECONDS must be between 1 and 86400")
            encoded[name] = str(int(value))
        else:
            encoded[name] = json.dumps(value)  # Quote YAML scalars; prevent injection.
    return Template(TEMPLATE.read_text()).substitute(encoded)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--env-file", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    try:
        values = {name: os.environ.get(name, "") for name in REQUIRED}
        if args.env_file:
            values.update(load_env_file(args.env_file))
        rendered = render(values)
    except ValueError as error:
        parser.error(str(error))
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        with args.output.open("x") as output:  # Never overwrite a user's rendered file.
            output.write(rendered)
    else:
        print(rendered, end="")


if __name__ == "__main__":
    main()
