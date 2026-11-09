#!/usr/bin/env python3
"""Refresh boatdev-openapi.json: the operations drivers.BoatDev calls, with $refs
inlined, cut from boat.dev's published OpenAPI. Needs PyYAML (dev only; the
tests read the JSON and need nothing).

  curl -fsSLo /tmp/boat-v1.yaml https://docs.boat.dev/openapi/boat-v1.yaml
  python3 -I bench/boatbench/boatdev_openapi_extract.py /tmp/boat-v1.yaml > bench/boatbench/boatdev-openapi.json
"""
import datetime
import hashlib
import json
import sys

import yaml

URL = "https://docs.boat.dev/openapi/boat-v1.yaml"
USED = [("/sandboxes", "post"), ("/sandboxes/{sandboxId}", "get"), ("/sandboxes/{sandboxId}", "delete"),
        ("/sandboxes/{sandboxId}/stop", "post"), ("/sandboxes/{sandboxId}/resume", "post"),
        ("/sandboxes/{sandboxId}/fork", "post"), ("/sandboxes/{sandboxId}/commands", "post"),
        ("/sandboxes/{sandboxId}/files", "put"), ("/saved-snapshots", "post"),
        ("/saved-snapshots/{name}", "get"), ("/saved-snapshots/{name}", "delete")]
DROP = ("description", "summary", "tags", "headers", "operationId")


def resolve(node, root, seen=()):
    if isinstance(node, dict):
        ref = node.get("$ref")
        if ref:
            if ref in seen:
                return {"$ref": ref}
            target = root
            for part in ref.lstrip("#/").split("/"):
                target = target[part]
            return resolve(target, root, seen + (ref,))
        return {k: resolve(v, root, seen) for k, v in node.items() if k not in DROP}
    if isinstance(node, list):
        return [resolve(v, root, seen) for v in node]
    return node


def main(path):
    raw = open(path, "rb").read()
    spec = yaml.safe_load(raw)
    ops = {}
    for p, m in USED:
        item = spec["paths"][p]
        op = dict(item[m])
        op["parameters"] = item.get("parameters", []) + op.get("parameters", [])
        ops[f"{m.upper()} {p}"] = resolve(op, spec)
    json.dump({"source": URL, "sha256": hashlib.sha256(raw).hexdigest(),
               "fetched": datetime.date.today().isoformat(), "servers": spec["servers"],
               "operations": ops}, sys.stdout, indent=1, sort_keys=True)
    print()


if __name__ == "__main__":
    main(sys.argv[1])
