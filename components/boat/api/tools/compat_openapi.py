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
    ("GET", "/api/v1/sandboxes/{id}/commands/{pid}/stream", "/sandboxes/{sandboxId}/commands/{processId}/stream", "exec", None,
     "attach to a detached command (NDJSON; one combined log, served as stdout)"),
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
    # host, desktop, ssh key (compat_host.go)
    ("POST", "/api/v1/sandboxes/{id}/host", "/sandboxes/{sandboxId}/host", "vms:write", ["port", "public", "localhost", "title"],
     "host a port (POST /v1/vms/{id}/ports + GET …/ports/{port}/url; localhost refused)"),
    ("POST", "/api/v1/sandboxes/{id}/desktop", "/sandboxes/{sandboxId}/desktop", "exec", ["publicAccess"],
     "desktop link (GET /v1/vms/{id}/desktop; vnc mode, publicAccess refused)"),
    ("POST", "/api/v1/sandboxes/{id}/sshkey", "/sandboxes/{sandboxId}/sshkey", "exec", ["key"], "authorize an ssh key (POST /v1/vms/{id}/ssh)"),
    # snapshots (compat_snaps.go)
    ("GET", "/api/v1/snapshots", "/snapshots", "vms:read", None, "team snapshots (GET /v1/snapshots; one page)"),
    ("DELETE", "/api/v1/snapshots/{sid}", "/snapshots/{snapshotId}", "vms:write", None, "delete one (DELETE /v1/snapshots/{sid})"),
    ("GET", "/api/v1/sandboxes/{id}/snapshots", "/sandboxes/{sandboxId}/snapshots", "vms:read", None, "a sandbox's snapshots (GET /v1/vms/{id}/snapshots)"),
    ("DELETE", "/api/v1/sandboxes/{id}/snapshots", "/sandboxes/{sandboxId}/snapshots", "vms:write", None,
     "delete a sandbox's captures (its unnamed snapshots; saved ones stay)"),
    ("GET", "/api/v1/sandboxes/{id}/snapshots/latest", "/sandboxes/{sandboxId}/snapshots/latest", "vms:read", None, "newest snapshot or null"),
    ("GET", "/api/v1/named-snapshots", "/named-snapshots", "vms:read", None, "named snapshots (= saved snapshots)"),
    ("POST", "/api/v1/named-snapshots", "/named-snapshots", "vms:write", ["sandboxId", "name"], "save (= POST /saved-snapshots)"),
    ("GET", "/api/v1/named-snapshots/{name}", "/named-snapshots/{name}", "vms:read", None, "one named snapshot"),
    ("DELETE", "/api/v1/named-snapshots/{name}", "/named-snapshots/{name}", "vms:write", None, "delete a named snapshot"),
    ("GET", "/api/v1/saved-snapshots/{name}/shares", "/saved-snapshots/{name}/shares", "admin", None,
     "share codes (GET /v1/vms/{id}/snapshots/{sid}/shares)"),
    ("POST", "/api/v1/saved-snapshots/{name}/shares", "/saved-snapshots/{name}/shares", "admin", None,
     "new share code (POST …/shares; valid 30 days)"),
    ("DELETE", "/api/v1/saved-snapshots/{name}/shares/{share}", "/saved-snapshots/{name}/shares/{shareId}", "admin", None, "revoke a share code"),
    # secrets and the AI proxy switch (compat_secrets.go)
    ("GET", "/api/v1/secrets", "/secrets", "admin", None, "default-injected team secrets (names only)"),
    ("POST", "/api/v1/secrets", "/secrets", "admin", ["envContents", "secretFiles"],
     "set default-injected team secrets (secretFiles refused)"),
    ("GET", "/api/v1/ai-proxy", "/ai-proxy", "vms:read", None, "AI credentials proxy switch (GET /v1/ai/proxy)"),
    ("PUT", "/api/v1/ai-proxy", "/ai-proxy", "admin", ["enabled"], "turn it on or off (PUT /v1/ai/proxy)"),
    # find and download URLs (compat_download.go)
    ("GET", "/api/v1/sandboxes/{id}/files/find", "/sandboxes/{sandboxId}/files/find", "exec", None, "find text in files (ripgrep)"),
    ("POST", "/api/v1/sandboxes/{id}/files/download-url", "/sandboxes/{sandboxId}/files/download-url", "exec", ["path", "ttlSeconds"],
     "signed short-lived download URL (/api/v1/file-downloads/{token})"),
    # council round 3 (compat_extra.go)
    ("GET", "/api/v1/telemetry", "/telemetry", "admin", None, "team OTLP export (GET /v1/telemetry)"),
    ("PUT", "/api/v1/telemetry", "/telemetry", "admin", ["endpoint", "headers"], "set OTLP export (PUT /v1/telemetry; no test export is sent)"),
    ("DELETE", "/api/v1/telemetry", "/telemetry", "admin", None, "stop OTLP export (DELETE /v1/telemetry)"),
    ("GET", "/api/v1/ai-usage", "/ai-usage", "vms:read", None, "AI proxy usage (GET /v1/ai/usage; rows dated by last use)"),
    ("GET", "/api/v1/sandboxes/{id}/ai-usage", "/sandboxes/{sandboxId}/ai-usage", "vms:read", None, "AI proxy usage of one sandbox (GET /v1/vms/{id}/ai-usage)"),
    ("PUT", "/api/v1/sandboxes/{id}/ai-access", "/sandboxes/{sandboxId}/ai-access", "admin", ["blocked"], "block AI at the proxy (PUT /v1/vms/{id}/ai-access)"),
    ("PUT", "/api/v1/environments/{id}/vars/{key}", "/environments/{environmentId}/vars/{key}", "admin", ["value"], "set one variable (PUT /v1/environments/{id})"),
    ("DELETE", "/api/v1/environments/{id}/vars/{key}", "/environments/{environmentId}/vars/{key}", "admin", None, "remove one variable (PUT /v1/environments/{id})"),
    ("POST", "/api/v1/sandboxes/{id}/files/replace", "/sandboxes/{sandboxId}/files/replace", "exec", ["paths", "pattern", "newValue"], "replace text (read + write each file, up to 5 MiB)"),
    ("POST", "/api/v1/sandboxes/{id}/files/upload-url", "/sandboxes/{sandboxId}/files/upload-url", "exec", ["path", "ttlSeconds"], "signed short-lived upload URL (/api/v1/file-uploads/{token})"),
    ("GET", "/api/v1/sandboxes/{id}/artifacts", "/sandboxes/{sandboxId}/artifacts", "exec", None, "download a file up to 50 MiB"),
    ("POST", "/api/v1/sandboxes/{id}/share", "/sandboxes/{sandboxId}/share", "vms:write", None, "share with the team (a no-op: team sandboxes are shared)"),
    ("POST", "/api/v1/sandboxes/{id}/unshare", "/sandboxes/{sandboxId}/unshare", "vms:write", None, "make private: 501 not_supported (no private sandboxes here)"),
    ("POST", "/api/v1/sandboxes/{id}/recover", "/sandboxes/{sandboxId}/recover", "vms:write", None, "power-cycle (forced stop, then start; the disk is kept)"),
    # credential profiles: the team's one "default" profile over /v1/ai/credentials (compat_profiles.go)
    ("GET", "/api/v1/credential-profiles", "/credential-profiles", "admin", None, "the team's one profile (GET /v1/ai/credentials)"),
    ("POST", "/api/v1/credential-profiles", "/credential-profiles", "admin", None, "more profiles: 501 not_supported"),
    ("PUT", "/api/v1/credential-profiles/{profile}", "/credential-profiles/{profile}", "admin", ["name", "isDefault", "access"],
     "update: already the default; renaming or access is 501 not_supported"),
    ("DELETE", "/api/v1/credential-profiles/{profile}", "/credential-profiles/{profile}", "admin", None, "delete: always 409 is_default"),
    ("PUT", "/api/v1/credential-profiles/{profile}/keys/{credential}", "/credential-profiles/{profile}/keys/{credential}", "admin",
     ["value", "keepCurrent"], "set an API key (PUT /v1/ai/credentials/{provider})"),
    ("DELETE", "/api/v1/credential-profiles/{profile}/keys/{credential}", "/credential-profiles/{profile}/keys/{credential}", "admin", None,
     "remove an API key (DELETE /v1/ai/credentials/{provider})"),
    ("GET", "/api/v1/credential-profiles/{profile}/accounts/{credential}", "/credential-profiles/{profile}/accounts/{credential}", "admin", None,
     "the credential's one account"),
    ("POST", "/api/v1/credential-profiles/{profile}/accounts/{credential}", "/credential-profiles/{profile}/accounts/{credential}", "admin",
     ["value", "keepCurrent"], "set the first key; a second account is 501 not_supported"),
    ("PUT", "/api/v1/credential-profiles/{profile}/accounts/{credential}/{account}/active",
     "/credential-profiles/{profile}/accounts/{credential}/{account}/active", "admin", None, "switch: only to the one account (a no-op)"),
    ("DELETE", "/api/v1/credential-profiles/{profile}/accounts/{credential}/{account}",
     "/credential-profiles/{profile}/accounts/{credential}/{account}", "admin", None, "forget the account (DELETE /v1/ai/credentials/{provider})"),
    ("PUT", "/api/v1/credential-profiles/{profile}/custom-apis/{name}", "/credential-profiles/{profile}/custom-apis/{name}", "admin", None,
     "custom APIs: 501 not_supported"),
    ("DELETE", "/api/v1/credential-profiles/{profile}/custom-apis/{name}", "/credential-profiles/{profile}/custom-apis/{name}", "admin", None,
     "custom APIs: 501 not_supported"),
    ("GET", "/api/v1/snapshots/{sid}/download", "/snapshots/{snapshotId}/download", "exec", None,
     "the snapshot archive itself (GET /v1/vms/{id}/snapshots/{sid}/download), not signed chunk URLs"),
] + [
    (m, "/api/v1" + p.replace("{environmentId}", "{id}").replace("{snapshotId}", "{sid}"), p, "", None, what + ": 501 not_supported")
    for m, p, what in [
        ("GET", "/orgs", "organizations"), ("PATCH", "/orgs/active", "organizations"), ("PATCH", "/orgs/{org}", "organizations"),
        ("GET", "/account/data-retention", "data retention"), ("PATCH", "/account/data-retention", "data retention"),
        ("GET", "/repos", "repositories"), ("POST", "/repos", "repositories"),
        ("GET", "/ci", "boat CI"), ("POST", "/ci/connect", "boat CI"), ("POST", "/ci/public", "boat CI"),
        ("GET", "/ci/jobs/{jobId}", "boat CI"), ("GET", "/ci/jobs/{jobId}/logs", "boat CI"),
        ("POST", "/environments/{environmentId}/upgrade", "environment upgrades"),
        ("PUT", "/environments/{environmentId}/secret-files", "environment secret files"),
        ("DELETE", "/environments/{environmentId}/secret-files", "environment secret files"),
        ("POST", "/environments/{environmentId}/repos", "environment repositories"),
        ("DELETE", "/environments/{environmentId}/repos/{repositoryId}", "environment repositories"),
        ("GET", "/snapshots/{snapshotId}/tree", "snapshot trees"), ("GET", "/snapshots/{snapshotId}/files", "snapshot files"),
    ]
]

# Operations whose 2xx answer differs in kind from boat.dev's.
BINARY_200 = {("GET", "/api/v1/snapshots/{sid}/download")}

# Paths under /api/v1 that are this server's own, not boat.dev operations.
EXTRA = {
    "/api/v1/file-downloads/{token}": {"get": {
        "summary": "Download a file through a signed URL from POST /api/v1/sandboxes/{id}/files/download-url (no API key)",
        "operationId": "compat_fileDownload", "security": [], "tags": ["boat.dev compatibility"],
        "parameters": [{"name": "token", "in": "path", "required": True, "schema": {"type": "string"}}],
        "responses": {
            "200": {"description": "The file's bytes", "content": {"application/octet-stream": {"schema": {"type": "string", "format": "binary"}}}},
            "default": {"description": "boat.dev error envelope",
                        "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CompatError"}}}},
        }}},
}

EXTRA["/api/v1/file-uploads/{token}"] = {"put": {
    "summary": "Upload a file through a signed URL from POST /api/v1/sandboxes/{id}/files/upload-url (no API key)",
    "operationId": "compat_fileUpload", "security": [], "tags": ["boat.dev compatibility"],
    "parameters": [{"name": "token", "in": "path", "required": True, "schema": {"type": "string"}}],
    "requestBody": {"required": True, "content": {"application/octet-stream": {"schema": {"type": "string", "format": "binary"}}}},
    "responses": {
        "200": {"description": "Written", "content": {"application/json": {"schema": {"type": "object",
            "properties": {"ok": {"type": "boolean"}, "path": {"type": "string"}, "bytes": {"type": "integer"}}}}}},
        "default": {"description": "boat.dev error envelope",
                    "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CompatError"}}}},
    }}}

EXTRA["/api/v1/provider-models"] = {"get": {
    "summary": "Agent harnesses and the models the operator listed for each (-provider-models); boat.dev documents this path for prompt model and reasoningEffort choices",
    "operationId": "compat_providerModels", "tags": ["boat.dev compatibility"],
    "responses": {
        "200": {"description": "Providers", "content": {"application/json": {"schema": {"type": "object",
            "required": ["ok", "type", "providers"],
            "properties": {"ok": {"type": "boolean"}, "type": {"type": "string"}, "providers": {"type": "array", "items": {
                "type": "object", "required": ["provider", "harness", "models"],
                "properties": {"provider": {"type": "string"}, "harness": {"type": "string"}, "models": {"type": "array", "items": {
                    "type": "object", "required": ["id"],
                    "properties": {"id": {"type": "string"}, "name": {"type": "string"},
                                   "reasoningEfforts": {"type": "array", "items": {"type": "string"}},
                                   "fastMode": {"type": "boolean"}}}}}}}}}}}},
        "default": {"description": "boat.dev error envelope",
                    "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CompatError"}}}},
    }}}

PARAM_RENAME = {"sandboxId": "id", "operationId": "opid", "snapshotId": "sid", "shareId": "share", "stopId": "stopid", "promptId": "pid", "processId": "pid",
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
        if (method, ours) in BINARY_200:
            resps = {"200": {"description": "The snapshot archive (tar.zst)",
                             "content": {"application/octet-stream": {"schema": {"type": "string", "format": "binary"}}}}}
        resps["default"] = {"description": "boat.dev error envelope",
                            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CompatError"}}}}
        out["responses"] = resps
        paths.setdefault(ours, {})[method.lower()] = out
    paths.update(copy.deepcopy(EXTRA))
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
