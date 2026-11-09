#!/usr/bin/env python3
"""Regenerate the /api/v1 (boat.dev compatibility) part of openapi.json.

Every compat operation is documented with boat.dev's own schemas, copied from
its published spec (https://docs.boat.dev/openapi/boat-v1.yaml) under a "BD"
prefix. The OpenAPI drift test then checks each compat answer against
boat.dev's schema: a field boat.dev requires and we leave out, a wrong enum
value or a bx_ id that breaks boat.dev's pattern fails `go test`.

    curl -sSLo /tmp/boat-v1.yaml https://docs.boat.dev/openapi/boat-v1.yaml
    python3 -c 'import json,sys,yaml; json.dump(yaml.safe_load(open(sys.argv[1])), open(sys.argv[2],"w"))' \
        /tmp/boat-v1.yaml /tmp/boat-v1.json
    python3 tools/compat_openapi.py /tmp/boat-v1.json openapi.json

Request bodies list only the fields this server accepts (ACCEPT below; the
drift test compares them with the handler's struct). Errors are the
CompatError envelope (boat.dev's ErrorEnvelope).
"""
import copy, json, re, sys

# (method, our path, boat.dev path, scope, accepted request fields or None = no body, summary)
ROUTES = [
    ("POST", "/api/v1/sandboxes", "/sandboxes", "vms:write",
     ["name", "type", "ttlSeconds", "labels", "env", "environment", "noEnv", "failFast", "count", "from",
      "snapshots", "monitoring", "visibility", "org", "teamId", "setupScript", "hostedPorts", "files"], "create (POST /v1/vms)"),
    ("GET", "/api/v1/sandboxes", "/sandboxes", "vms:read", None, "list (GET /v1/vms)"),
    ("GET", "/api/v1/sandboxes/{id}", "/sandboxes/{sandboxId}", "vms:read", None, "get (GET /v1/vms/{id})"),
    ("PATCH", "/api/v1/sandboxes/{id}", "/sandboxes/{sandboxId}", "vms:write", ["name", "ttlSeconds"], "rename / auto-stop (PATCH /v1/vms/{id})"),
    ("DELETE", "/api/v1/sandboxes/{id}", "/sandboxes/{sandboxId}", "vms:write", None, "delete (DELETE /v1/vms/{id})"),
    ("GET", "/api/v1/deletion-operations/{opid}", "/deletion-operations/{operationId}", "vms:read", None, "deletion operation (in memory)"),
    ("POST", "/api/v1/sandboxes/{id}/stop", "/sandboxes/{sandboxId}/stop", "vms:write", ["force"], "stop (POST /v1/vms/{id}/stop; done when it answers)"),
    ("GET", "/api/v1/sandboxes/{id}/stops/{stopid}", "/sandboxes/{sandboxId}/stops/{stopId}", "vms:read", None, "stop operation (in memory)"),
    ("POST", "/api/v1/sandboxes/{id}/resume", "/sandboxes/{sandboxId}/resume", "vms:write",
     ["failFast", "ttlSeconds", "type", "env", "environment", "noEnv"], "resume (POST /v1/vms/{id}/start)"),
    ("POST", "/api/v1/sandboxes/{id}/fork", "/sandboxes/{sandboxId}/fork", "vms:write",
     ["name", "failFast", "ttlSeconds", "monitoring", "labels", "env", "environment", "noEnv", "type"], "fork (POST /v1/vms/{id}/fork)"),
    ("GET", "/api/v1/limits", "/limits", "vms:read", None, "limits"),
    ("GET", "/api/v1/me", "/me", "", None, "me (GET /v1/me)"),
    ("GET", "/api/v1/sandboxes/{id}/usage", "/sandboxes/{sandboxId}/usage", "vms:read", None, "usage (GET /v1/vms/{id}/usage)"),
    # prompts
    ("POST", "/api/v1/sandboxes/{id}/prompt", "/sandboxes/{sandboxId}/prompt", "exec",
     ["provider", "prompt", "model", "reasoningEffort", "fast", "new", "conversationId"], "prompt (POST /v1/vms/{id}/prompts)"),
    ("GET", "/api/v1/sandboxes/{id}/prompts/{pid}", "/sandboxes/{sandboxId}/prompts/{promptId}", "exec", None, "prompt run (GET /v1/vms/{id}/prompts/{pid})"),
    ("GET", "/api/v1/sandboxes/{id}/events", "/sandboxes/{sandboxId}/events", "exec", None, "events (prompt runs and their output)"),
    ("GET", "/api/v1/sandboxes/{id}/conversations", "/sandboxes/{sandboxId}/conversations", "exec", None, "conversations (GET /v1/vms/{id}/conversations)"),
    ("POST", "/api/v1/sandboxes/{id}/steer", "/sandboxes/{sandboxId}/steer", "exec", ["message", "conversation"], "steer (POST /v1/vms/{id}/prompts/{pid}/steer)"),
    ("POST", "/api/v1/sandboxes/{id}/interrupt", "/sandboxes/{sandboxId}/interrupt", "exec", None, "interrupt (POST /v1/vms/{id}/prompts/{pid}/interrupt)"),
    # commands
    ("POST", "/api/v1/sandboxes/{id}/commands", "/sandboxes/{sandboxId}/commands", "exec",
     ["command", "cwd", "timeoutSeconds", "detached", "stream"], "command (exec, or POST /v1/vms/{id}/commands when detached)"),
    ("GET", "/api/v1/sandboxes/{id}/commands/{pid}", "/sandboxes/{sandboxId}/commands/{processId}", "exec", None, "detached command status"),
    ("DELETE", "/api/v1/sandboxes/{id}/commands/{pid}", "/sandboxes/{sandboxId}/commands/{processId}", "exec", None, "stop a detached command"),
    ("GET", "/api/v1/sandboxes/{id}/command-history", "/sandboxes/{sandboxId}/command-history", "exec", None, "command history"),
    ("GET", "/api/v1/sandboxes/{id}/command-history/{cid}", "/sandboxes/{sandboxId}/command-history/{commandId}", "exec", None, "one command"),
    ("GET", "/api/v1/sandboxes/{id}/command-history/{cid}/logs", "/sandboxes/{sandboxId}/command-history/{commandId}/logs", "exec", None, "command output"),
    ("GET", "/api/v1/sandboxes/{id}/processes", "/sandboxes/{sandboxId}/processes", "exec", None, "processes (ps)"),
    # files
    ("GET", "/api/v1/sandboxes/{id}/files", "/sandboxes/{sandboxId}/files", "exec", None, "read a file (up to 5 MiB)"),
    ("PUT", "/api/v1/sandboxes/{id}/files", "/sandboxes/{sandboxId}/files", "exec", ["path", "content", "encoding"], "write a file (up to 5 MiB)"),
    ("DELETE", "/api/v1/sandboxes/{id}/files", "/sandboxes/{sandboxId}/files", "exec", None, "delete a file"),
    ("GET", "/api/v1/sandboxes/{id}/files/list", "/sandboxes/{sandboxId}/files/list", "exec", None, "list a folder"),
    ("GET", "/api/v1/sandboxes/{id}/files/info", "/sandboxes/{sandboxId}/files/info", "exec", None, "stat"),
    ("GET", "/api/v1/sandboxes/{id}/files/search", "/sandboxes/{sandboxId}/files/search", "exec", None, "search file names"),
    ("POST", "/api/v1/sandboxes/{id}/files/folder", "/sandboxes/{sandboxId}/files/folder", "exec", ["path", "mode"], "mkdir -p"),
    ("POST", "/api/v1/sandboxes/{id}/files/move", "/sandboxes/{sandboxId}/files/move", "exec", ["source", "destination", "overwrite"], "move"),
    ("POST", "/api/v1/sandboxes/{id}/files/permissions", "/sandboxes/{sandboxId}/files/permissions", "exec", ["path", "mode"], "chmod"),
    # saved snapshots
    ("GET", "/api/v1/saved-snapshots", "/saved-snapshots", "vms:read", None, "saved snapshots (GET /v1/snapshots)"),
    ("POST", "/api/v1/saved-snapshots", "/saved-snapshots", "vms:write", ["sandboxId", "name"], "save (POST /v1/vms/{id}/snapshots)"),
    ("GET", "/api/v1/saved-snapshots/{name}", "/saved-snapshots/{name}", "vms:read", None, "one saved snapshot"),
    ("DELETE", "/api/v1/saved-snapshots/{name}", "/saved-snapshots/{name}", "vms:write", None, "delete (DELETE /v1/snapshots/{sid})"),
    # webhooks (boat.dev event names and payloads)
    ("GET", "/api/v1/webhooks", "/webhooks", "admin", None, "webhooks registered here"),
    ("POST", "/api/v1/webhooks", "/webhooks", "admin", ["name", "url", "events"], "register (boat.dev payloads)"),
    ("GET", "/api/v1/webhooks/{id}", "/webhooks/{webhookId}", "admin", None, "one webhook"),
    ("PATCH", "/api/v1/webhooks/{id}", "/webhooks/{webhookId}", "admin", ["name", "url", "events"], "edit"),
    ("DELETE", "/api/v1/webhooks/{id}", "/webhooks/{webhookId}", "admin", None, "remove"),
    ("POST", "/api/v1/webhooks/{id}/rotate", "/webhooks/{webhookId}/rotate", "admin", None, "rotate the signing secret"),
    # API keys: actions are this server's scopes (compat_admin.go)
    ("GET", "/api/v1/api-keys", "/api-keys", "", None, "keys you can manage (GET /v1/keys)"),
    ("POST", "/api/v1/api-keys/scoped", "/api-keys/scoped", "",
     ["name", "ttl", "preset", "actions", "sandboxIds", "environmentIds"], "create (POST /v1/keys)"),
    ("DELETE", "/api/v1/api-keys/{id}", "/api-keys/{apiKeyId}", "", None, "revoke (DELETE /v1/keys/{id})"),
    ("POST", "/api/v1/api-keys/{id}/rotate", "/api-keys/{apiKeyId}/rotate", "", None, "rotate, no overlap"),
    ("GET", "/api/v1/api-keys/{id}/usage", "/api-keys/{apiKeyId}/usage", "", None, "requests since this process started"),
    # environments: variables and secret selection (compat_admin.go)
    ("GET", "/api/v1/environments", "/environments", "vms:read", None, "environments (GET /v1/environments)"),
    ("POST", "/api/v1/environments", "/environments", "admin", ["name"], "create (POST /v1/environments)"),
    ("PUT", "/api/v1/environments/{id}", "/environments/{environmentId}", "admin",
     ["name", "isDefault", "envContents", "passSecrets"], "update (PUT /v1/environments/{id})"),
    ("DELETE", "/api/v1/environments/{id}", "/environments/{environmentId}", "admin", None, "delete"),
]

PARAM_RENAME = {"sandboxId": "id", "operationId": "opid", "stopId": "stopid", "promptId": "pid", "processId": "pid",
                "commandId": "cid", "webhookId": "id",
                "apiKeyId": "id", "environmentId": "id"}


def main(src, dst):
    bd = json.load(open(src))
    spec = json.load(open(dst))
    schemas = {}

    def pull(name):
        if name in schemas:
            return
        schemas[name] = None
        schemas[name] = rewrite(bd["components"]["schemas"][name])

    def rewrite(x):
        if isinstance(x, dict):
            out = {}
            for k, v in x.items():
                if k in ("examples", "example", "description", "x-stainless", "deprecated"):
                    continue
                if k == "$ref":
                    name = v.rsplit("/", 1)[1]
                    pull(name)
                    out[k] = "#/components/schemas/BD" + name
                else:
                    out[k] = rewrite(v)
            return out
        if isinstance(x, list):
            return [rewrite(v) for v in x]
        return x

    def deref_body(schema):
        while "$ref" in schema:
            schema = bd["components"]["schemas"][schema["$ref"].rsplit("/", 1)[1]]
        return schema

    paths = {p: v for p, v in spec["paths"].items() if not p.startswith("/api/v1/")}
    for method, ours, theirs, scope, accept, summary in ROUTES:
        op = bd["paths"][theirs][method.lower()]
        out = {"summary": "boat.dev " + op.get("operationId", "") + ": " + summary, "operationId": "compat_" + op.get("operationId", ""),
               "x-boat-scope": scope, "tags": ["boat.dev compatibility"]}
        params = []
        for prm in op.get("parameters", []):
            prm = copy.deepcopy(prm)
            if "$ref" in prm:
                prm = copy.deepcopy(bd["components"]["parameters"][prm["$ref"].rsplit("/", 1)[1]])
            if prm.get("in") == "path":
                prm["name"] = PARAM_RENAME.get(prm["name"], prm["name"])
            prm.pop("examples", None)
            prm.pop("example", None)
            params.append(rewrite(prm))
        if params:
            out["parameters"] = params
        if accept is not None:
            body = op.get("requestBody", {}).get("content", {}).get("application/json", {}).get("schema", {"type": "object"})
            body = copy.deepcopy(deref_body(body))
            props = {}
            for k in accept:
                props[k] = rewrite(body.get("properties", {}).get(k, {}))
            out["requestBody"] = {"required": False, "content": {"application/json": {"schema": {
                "type": "object", "properties": props}}}}
        resps = {}
        for code, r in op["responses"].items():
            if not code.startswith("2"):
                continue
            if "$ref" in r:
                r = bd["components"]["responses"][r["$ref"].rsplit("/", 1)[1]]
            r = copy.deepcopy(r)
            for ct in list(r.get("content", {})):
                r["content"][ct].pop("examples", None)
                r["content"][ct].pop("example", None)
            resps[code] = {"description": "boat.dev " + code, **({"content": rewrite(r["content"])} if r.get("content") else {})}
        resps["default"] = {"description": "boat.dev error envelope",
                            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CompatError"}}}}
        out["responses"] = resps
        paths.setdefault(ours, {})[method.lower()] = out
    spec["paths"] = paths
    keep = {k: v for k, v in spec["components"]["schemas"].items() if not k.startswith("BD")}
    for name, s in sorted(schemas.items()):
        keep["BD" + name] = s
    spec["components"]["schemas"] = keep
    with open(dst, "w") as f:
        json.dump(spec, f, indent=1)
        f.write("\n")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
