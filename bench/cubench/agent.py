"""The agent loop: one model, one tool interface, one task, a full transcript.

Providers (Python stdlib + urllib only):
  anthropic  ANTHROPIC_API_KEY, model default claude-sonnet-5-5. Plain tool use
             with image blocks in tool results (not the computer-use tool), so
             every provider sees exactly the same tools.
  openai     OPENAI_BASE_URL / OPENAI_API_KEY / OPENAI_MODEL, any
             OpenAI-compatible /chat/completions endpoint with vision
             (e.g. Z.AI GLM-V at https://api.z.ai/api/paas/v4).
  scripted   a fake model for --dry-run and the self-tests.

Tool modes (the report only compares like with like):
  generic    screenshot, click, double_click, drag, scroll, type, key, wait, done:
             all done from outside the VM over QMP. The headline column.
  atspi      generic + cu_tree / cu_find / cu_act with panda-cu's schemas, run
             over ssh by tasks/assets/cu_atspi.py on the same AT-SPI walker as
             panda-cu: equal accessibility access on every OS.
  panda      generic + every PandaOS panda-cu tool, schemas loaded at run start
             from `panda-cu tools`, each run over ssh inside the graphical
             session as `panda-cu call <tool> '<json-args>'`. Contract: one JSON
             value on stdout and exit 0, or non-zero with the error on stderr. A
             result with "png_base64" (or a "path" to a .png in the guest) is
             forwarded to the model as an image. PandaOS-only, so its rows are
             compared with atspi rows, never with generic ones.

After every turn that acted, the last tool result carries a fresh screenshot
(QMP), whatever tools the turn used; read-only calls (screenshot, cu_tree,
cu_find, ...) do not trigger one. Keyboard-only tasks drop the mouse tools,
the pointer cu_* tools and cu_act's click/press actions.
"""
import base64
import hashlib
import json
import os
import shlex
import time
import urllib.error
import urllib.request

SETTLE_S = 0.8  # wait after an action before the screenshot that shows its effect
KEEP_IMAGES = 3  # OpenAI-compatible: screenshots kept in context (older ones dropped)
MAX_TOOL_TEXT = 20000

# USD per million tokens: input, output, cache write, cache read.
PRICES = {
    "claude-sonnet-5-5": (2.0, 10.0, 2.5, 0.2),
    "claude-opus-5-5": (4.0, 20.0, 5.0, 0.2),
    "claude-haiku-5-5": (0.1, 0.5, 0.125, 0.01),
}

GENERIC_TOOLS = [
    ("screenshot", "Take a screenshot of the whole screen.", {}, []),
    ("click", "Move the mouse to pixel (x, y) and click.",
     {"x": {"type": "integer"}, "y": {"type": "integer"},
      "button": {"type": "string", "enum": ["left", "right", "middle"]}}, ["x", "y"]),
    ("double_click", "Double-click the left button at pixel (x, y).",
     {"x": {"type": "integer"}, "y": {"type": "integer"}}, ["x", "y"]),
    ("drag", "Press the left button at (x1, y1), move to (x2, y2), release.",
     {"x1": {"type": "integer"}, "y1": {"type": "integer"},
      "x2": {"type": "integer"}, "y2": {"type": "integer"}}, ["x1", "y1", "x2", "y2"]),
    ("scroll", "Scroll the mouse wheel at (x, y). amount is wheel clicks (default 3).",
     {"x": {"type": "integer"}, "y": {"type": "integer"},
      "direction": {"type": "string", "enum": ["up", "down"]},
      "amount": {"type": "integer"}}, ["x", "y", "direction"]),
    ("type", "Type text on the keyboard (printable ASCII, \\n for Enter, \\t for Tab).",
     {"text": {"type": "string"}}, ["text"]),
    ("key", "Press a key or combination, e.g. Return, Escape, ctrl+s, ctrl+shift+n, alt+F4, super.",
     {"combo": {"type": "string"}}, ["combo"]),
    ("wait", "Wait some seconds (max 30), then take a screenshot.",
     {"seconds": {"type": "number"}}, ["seconds"]),
    ("done", "Call once the task is finished, or if you are sure it cannot be done.",
     {"success": {"type": "boolean"}, "summary": {"type": "string"}}, ["success"]),
]
MOUSE_TOOLS = {"click", "double_click", "drag", "scroll"}
MODES = ("generic", "atspi", "panda")
POINTER_CU = {"cu_click", "cu_drag", "cu_move", "cu_scroll"}
READ_ONLY = {"screenshot", "cu_tree", "cu_find", "cu_windows", "cu_wait", "cu_clipboard_get", "cu_screenshot"}

# cu_tree / cu_find / cu_act exactly as `panda-cu tools` prints them
# (components/computer-use/toolsjson.go), minus cu_act's `screenshot` flag:
# the end-of-turn screenshot covers it in every mode.
_EL = {"type": "string", "description": "An id from the latest cu_tree, cu_find or cu_screenshot{marks} result; "
                                        "the pointer goes to the centre of its box."}
ATSPI_TOOLS = [
    ("cu_tree", "Compact AT-SPI accessibility tree of the running apps: [{id,d(depth),role,name,bbox:[x,y,w,h],"
     "states,actions,text}]. ids are child-index paths, valid for this snapshot (cu_act refuses an id whose "
     "element changed). Hidden subtrees are pruned.",
     {"type": "object", "properties": {
         "max_depth": {"type": "integer", "minimum": 1, "maximum": 50, "default": 30,
                       "description": "Depth limit below each application."},
         "app": {"type": "string", "description": "Only applications whose name or window title contains this "
                                                  "(case-insensitive), e.g. mousepad."},
         "role": {"type": "string", "description": "Only elements with this role (e.g. \"push button\", \"text\", "
                                                   "\"entry\", \"menu item\"); apps and windows are always kept."},
         "limit": {"type": "integer", "minimum": 1, "maximum": 2000, "default": 400,
                   "description": "Maximum elements returned."}}}),
    ("cu_find", "Find accessibility elements whose name or text contains text (case-insensitive) and/or with this "
     "role. Returns elements with ids usable by cu_act.",
     {"type": "object", "properties": {
         "text": {"type": "string", "description": "Substring of the name or text."},
         "role": {"type": "string", "description": "Exact role, e.g. \"push button\"."},
         "app": {"type": "string", "description": "Only this application."},
         "limit": {"type": "integer", "minimum": 1, "maximum": 2000, "default": 50,
                   "description": "Maximum matches."}}}),
    ("cu_act", "Act on an element through AT-SPI, without pixels: click/press (runs its action), focus, set_text "
     "(replaces an editable field's whole contents, up to 100000 characters). Works even when the element is "
     "covered or off screen.",
     {"type": "object", "required": ["element_id", "action"], "properties": {
         "element_id": _EL,
         "action": {"type": "string", "enum": ["click", "focus", "set_text", "press"], "description": "What to do."},
         "text": {"type": "string", "description": "For set_text: the new contents."}}}),
]
# Used only when `panda-cu tools` cannot be read: names, loose schemas.
PANDA_FALLBACK = ["cu_screenshot", "cu_click", "cu_move", "cu_drag", "cu_scroll", "cu_type", "cu_key", "cu_tree",
                  "cu_find", "cu_act", "cu_wait", "cu_windows", "cu_focus", "cu_launch", "cu_open",
                  "cu_clipboard_get", "cu_clipboard_set"]


def parse_panda_tools(text):
    """`panda-cu tools` output (a JSON list of MCP tools) -> [(name, description, schema)]."""
    specs = [(t["name"], t.get("description", ""), t.get("inputSchema") or {"type": "object"})
             for t in json.loads(text)]
    if not specs or not all(n.startswith("cu_") for n, _, _ in specs):
        raise ValueError("unexpected `panda-cu tools` output")
    return specs


def keyboard_only_act(schema):
    """cu_act's schema without the click/press actions."""
    s = json.loads(json.dumps(schema))
    action = s.get("properties", {}).get("action")
    if action is not None:
        action["enum"] = [a for a in action.get("enum", ["focus", "set_text"]) if a not in ("click", "press")]
    return s


def tool_specs(mode, exclude=(), panda_specs=None):
    """[(name, description, JSON schema)] for this mode. A task excluding the mouse
    tools (keyboard only) also loses the pointer cu_* tools and cu_act click/press."""
    if mode not in MODES:
        raise ValueError(f"unknown tool mode {mode!r}")
    exclude = set(exclude)
    keyboard_only = bool(exclude & MOUSE_TOOLS)
    specs = [(n, d, {"type": "object", "properties": p, "required": r})
             for n, d, p, r in GENERIC_TOOLS if n not in exclude]
    extra = []
    if mode == "atspi":
        extra = ATSPI_TOOLS
    elif mode == "panda":
        extra = panda_specs or [(n, "PandaOS panda-cu tool (schema unavailable: pass its JSON arguments).",
                                 {"type": "object", "additionalProperties": True}) for n in PANDA_FALLBACK]
    for n, d, s in extra:
        if keyboard_only and n in POINTER_CU:
            continue
        specs.append((n, d, keyboard_only_act(s) if keyboard_only and n == "cu_act" else s))
    return specs


def system_prompt(os_profile, size, mode, gui_only=False):
    w, h = size
    lines = list(os_profile["hints"]) + ([] if gui_only or not os_profile.get("terminal_hint")
                                         else [os_profile["terminal_hint"]])
    hints = "\n".join(f"- {line}" for line in lines)
    extra = {"panda": "\nYou also have PandaOS's panda-cu tools (cu_*), which read the accessibility tree and act "
                      "on elements directly.",
             "atspi": "\nYou also have accessibility tools (cu_tree, cu_find, cu_act) that read the AT-SPI tree "
                      "and act on elements directly."}.get(mode, "")
    return (
        f"You operate a Linux desktop computer through tools. The screen is {w}x{h} pixels; "
        "coordinates are pixels from the top-left corner of the latest screenshot. After each "
        "turn in which you act, the last tool result includes a new screenshot.\n"
        f"About this computer:\n{hints}\n"
        "The home directory is ~ and paths like ~/bench/x.txt are inside it. Keyboard "
        "layout: US. Nobody will answer questions: work until the task is done, check the "
        f"result on screen, then call done.{extra}"
    )


# -- HTTP ----------------------------------------------------------------------
def http_post(url, headers, body, timeout=300, attempts=4):
    data = json.dumps(body).encode()
    for i in range(attempts):
        req = urllib.request.Request(url, data=data, headers={**headers, "content-type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return json.loads(r.read())
        except urllib.error.HTTPError as e:
            detail = e.read().decode(errors="replace")[:2000]
            if e.code not in (408, 409, 429, 500, 502, 503, 504, 529) or i == attempts - 1:
                raise RuntimeError(f"HTTP {e.code} from {url}: {detail}") from None
            wait = float(e.headers.get("retry-after") or 5 * 2 ** i)
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            if i == attempts - 1:
                raise RuntimeError(f"{url}: {e}") from None
            wait = 5 * 2 ** i
        time.sleep(min(wait, 120))


def b64(png):
    return base64.b64encode(png).decode()


class Usage(dict):
    def add(self, **kw):
        for k, v in kw.items():
            self[k] = self.get(k, 0) + (v or 0)


# -- providers -------------------------------------------------------------------
class Anthropic:
    """Messages API, raw HTTP. History is append-only (thinking blocks are passed
    back unchanged) and auto-cached, so screenshots are never pruned."""

    def __init__(self, model, specs, effort=None, max_tokens=16000):
        self.key = os.environ.get("ANTHROPIC_API_KEY") or ""
        if not self.key:
            raise RuntimeError("ANTHROPIC_API_KEY is not set")
        self.url = os.environ.get("ANTHROPIC_BASE_URL", "https://api.anthropic.com").rstrip("/") + "/v1/messages"
        self.model, self.effort, self.max_tokens = model, effort, max_tokens
        self.tools = [{"name": n, "description": d, "input_schema": s} for n, d, s in specs]
        self.usage = Usage()

    def start(self, system, text, png):
        self.system = system
        self.messages = [{"role": "user", "content": [
            {"type": "text", "text": text},
            {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": b64(png)}}]}]

    def step(self):
        body = {"model": self.model, "max_tokens": self.max_tokens, "system": self.system,
                "tools": self.tools, "messages": self.messages,
                "cache_control": {"type": "ephemeral"}}
        if self.effort:
            body["output_config"] = {"effort": self.effort}
        r = http_post(self.url, {"x-api-key": self.key, "anthropic-version": "2023-06-01"}, body)
        u = r.get("usage", {})
        self.usage.add(input=u.get("input_tokens"), output=u.get("output_tokens"),
                       cache_write=u.get("cache_creation_input_tokens"),
                       cache_read=u.get("cache_read_input_tokens"), requests=1)
        if r.get("stop_reason") == "refusal":
            raise RuntimeError(f"model refused: {r.get('stop_details')}")
        content = r.get("content", [])
        self.messages.append({"role": "assistant", "content": content})
        calls = [(b["id"], b["name"], b.get("input") or {}) for b in content if b.get("type") == "tool_use"]
        text = "\n".join(b.get("text", "") for b in content if b.get("type") == "text")
        return calls, text

    def add_results(self, results):
        blocks = []
        for cid, text, png, is_err in results:
            c = [{"type": "text", "text": text}]
            if png:
                c.append({"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": b64(png)}})
            blocks.append({"type": "tool_result", "tool_use_id": cid, "content": c, "is_error": is_err})
        self.messages.append({"role": "user", "content": blocks})


class OpenAICompat:
    def __init__(self, model, specs, seed=None, max_tokens=4096):
        self.base = (os.environ.get("OPENAI_BASE_URL") or "https://api.openai.com/v1").rstrip("/")
        self.key = os.environ.get("OPENAI_API_KEY") or ""
        if not self.key:
            raise RuntimeError("OPENAI_API_KEY is not set")
        self.model, self.seed, self.max_tokens = model, seed, max_tokens
        self.tools = [{"type": "function", "function": {"name": n, "description": d, "parameters": s}}
                      for n, d, s in specs]
        self.usage = Usage()

    @staticmethod
    def _image_msg(text, png):
        return {"role": "user", "content": [
            {"type": "text", "text": text},
            {"type": "image_url", "image_url": {"url": "data:image/png;base64," + b64(png)}}]}

    def start(self, system, text, png):
        self.messages = [{"role": "system", "content": system}, self._image_msg(text, png)]

    def _prune(self):
        seen = 0
        for m in reversed(self.messages):
            if m["role"] == "user" and isinstance(m["content"], list):
                if any(p.get("type") == "image_url" for p in m["content"]):
                    seen += 1
                    if seen > KEEP_IMAGES:
                        m["content"] = [p if p.get("type") != "image_url" else
                                        {"type": "text", "text": "[older screenshot removed]"} for p in m["content"]]

    def step(self):
        self._prune()
        body = {"model": self.model, "messages": self.messages, "tools": self.tools,
                "max_tokens": self.max_tokens}
        if self.seed is not None:
            body["seed"] = self.seed
        r = http_post(self.base + "/chat/completions", {"authorization": f"Bearer {self.key}"}, body)
        u = r.get("usage") or {}
        self.usage.add(input=u.get("prompt_tokens"), output=u.get("completion_tokens"), requests=1)
        msg = r["choices"][0]["message"]
        keep = {"role": "assistant", "content": msg.get("content") or ""}
        if msg.get("tool_calls"):
            keep["tool_calls"] = msg["tool_calls"]
        self.messages.append(keep)
        calls = []
        for tc in msg.get("tool_calls") or []:
            try:
                args = json.loads(tc["function"].get("arguments") or "{}")
            except json.JSONDecodeError:
                args = {"_invalid_json": tc["function"].get("arguments")}
            calls.append((tc["id"], tc["function"]["name"], args))
        return calls, keep["content"]

    def add_results(self, results):
        png_last = None
        for cid, text, png, is_err in results:
            self.messages.append({"role": "tool", "tool_call_id": cid,
                                  "content": ("ERROR: " if is_err else "") + text})
            png_last = png or png_last
        if png_last:
            self.messages.append(self._image_msg("Screenshot after the last action:", png_last))


class Scripted:
    """Fake model: replays a fixed list of tool calls, then calls done."""

    SCRIPT = [("screenshot", {}), ("click", {"x": 640, "y": 400}), ("double_click", {"x": 600, "y": 300}),
              ("drag", {"x1": 100, "y1": 100, "x2": 300, "y2": 200}),
              ("scroll", {"x": 640, "y": 400, "direction": "down", "amount": 2}),
              ("type", {"text": "Hello, World! ~"}), ("key", {"combo": "ctrl+s"}),
              ("wait", {"seconds": 0}), ("done", {"success": True, "summary": "scripted"})]

    def __init__(self, specs, script=None):
        self.names = {n for n, _, _ in specs}
        self.script = [tuple(c) for c in script or self.SCRIPT]
        self.usage = Usage()

    def start(self, system, text, png):
        self.i = 0
        self.seen = [png]

    def step(self):
        self.usage.add(requests=1)
        while self.i < len(self.script) and self.script[self.i][0] not in self.names:
            self.i += 1
        if self.i >= len(self.script):
            return [], "out of script"
        name, args = self.script[self.i]
        self.i += 1
        return [(f"call{self.i}", name, args)], ""

    def add_results(self, results):
        self.seen += [png for _, _, png, _ in results if png]


def make_model(provider, model, specs, seed=None, effort=None, script=None):
    if provider == "anthropic":
        return Anthropic(model or "claude-sonnet-5-5", specs, effort=effort)
    if provider == "openai":
        return OpenAICompat(model or os.environ.get("OPENAI_MODEL") or "", specs, seed=seed)
    if provider == "scripted":
        return Scripted(specs, script)
    raise ValueError(f"unknown provider {provider!r}")


def cost_usd(model, usage, price=None):
    p = price or PRICES.get(model)
    if not p:
        return None
    pin, pout = p[0], p[1]
    pw, pr = (p[2], p[3]) if len(p) > 3 else (pin, pin)
    return round((usage.get("input", 0) * pin + usage.get("output", 0) * pout
                  + usage.get("cache_write", 0) * pw + usage.get("cache_read", 0) * pr) / 1e6, 4)


# -- panda-cu over MCP stdio ---------------------------------------------------------
class MCPSession:
    """One `panda-cu mcp` process (JSON-RPC lines on stdio) for a whole attempt:
    panda-cu's element ids live as long as its process, so a cu_find id is still
    valid in the next cu_act (each `panda-cu call` would be a new process)."""

    def __init__(self, argv, timeout=120):
        import subprocess
        import queue
        import threading
        self.timeout, self.n, self.lines = timeout, 0, queue.Queue()
        self.proc = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.DEVNULL, text=True, bufsize=1)

        def pump():  # a thread, because select() cannot see lines already in Python's buffer
            for line in self.proc.stdout:
                self.lines.put(line)
            self.lines.put(None)
        threading.Thread(target=pump, daemon=True).start()
        self._req("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                 "clientInfo": {"name": "cubench", "version": "1"}})
        self._send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def _send(self, msg):
        self.proc.stdin.write(json.dumps(msg) + "\n")
        self.proc.stdin.flush()

    def _req(self, method, params):
        import queue
        self.n += 1
        self._send({"jsonrpc": "2.0", "id": self.n, "method": method, "params": params})
        end = time.monotonic() + self.timeout
        while True:
            try:
                line = self.lines.get(timeout=max(0.0, end - time.monotonic()))
            except queue.Empty:
                raise TimeoutError(f"panda-cu mcp: no reply to {method} in {self.timeout} s") from None
            if line is None:
                raise RuntimeError("panda-cu mcp exited")
            msg = json.loads(line)
            if msg.get("id") == self.n:
                if "error" in msg:
                    raise RuntimeError(f"panda-cu mcp {method}: {msg['error'].get('message', msg['error'])}")
                return msg.get("result", {})

    def tools(self):
        return [(t["name"], t.get("description", ""), t.get("inputSchema") or {"type": "object"})
                for t in self._req("tools/list", {}).get("tools", [])]

    def call(self, name, args):
        r = self._req("tools/call", {"name": name, "arguments": args})
        texts = [c.get("text", "") for c in r.get("content", []) if c.get("type") == "text"]
        imgs = [c["data"] for c in r.get("content", []) if c.get("type") == "image" and c.get("data")]
        if r.get("isError"):
            raise RuntimeError("\n".join(texts)[:2000] or f"{name} failed")
        return "\n".join(texts)[:MAX_TOOL_TEXT], (base64.b64decode(imgs[0]) if imgs else None)

    def close(self):
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(5)
            except Exception:  # noqa: BLE001
                self.proc.kill()


# -- environment -------------------------------------------------------------------
class Env:
    """What the tools act on: a QMP connection plus a shell inside the guest session.

    shell(script, timeout) -> (rc, stdout, stderr), run in the session prelude.
    """

    CU = {"panda": "panda-cu call", "atspi": 'python3 "$A/cu_atspi.py"'}

    def __init__(self, qmp, shell, shot_dir, mode="generic", mcp=None):
        self.qmp, self.shell, self.dir, self.n, self.mcp = qmp, shell, shot_dir, 0, mcp
        self.cu_cmd = self.CU.get(mode, "panda-cu call")
        os.makedirs(shot_dir, exist_ok=True)

    def screenshot(self):
        self.n += 1
        path = os.path.join(self.dir, f"{self.n:03d}.png")
        self.qmp.screendump(path)
        with open(path, "rb") as f:
            return path, f.read()

    def act(self, name, a):
        q = self.qmp
        num = lambda k, d=None: int(round(float(a.get(k, d))))  # noqa: E731
        if name == "click":
            q.click(num("x"), num("y"), a.get("button") or "left")
        elif name == "double_click":
            q.click(num("x"), num("y"), count=2)
        elif name == "drag":
            q.drag(num("x1"), num("y1"), num("x2"), num("y2"))
        elif name == "scroll":
            q.scroll(num("x"), num("y"), a.get("direction", "down"), num("amount", 3))
        elif name == "type":
            q.type(str(a["text"]))
        elif name == "key":
            q.key(str(a["combo"]))
        elif name == "wait":
            time.sleep(max(0.0, min(float(a.get("seconds", 1)), 30.0)))
        elif name != "screenshot":
            raise ValueError(f"unknown tool {name}")
        return "ok"

    def panda(self, name, args):
        if self.mcp is not None:  # the panda column: one panda-cu mcp per attempt
            return self.mcp.call(name, args)
        rc, out, err = self.shell(f"{self.cu_cmd} {shlex.quote(name)} {shlex.quote(json.dumps(args))}", 120)
        if rc != 0:
            raise RuntimeError(f"{name} exited {rc}: {(err or out).strip()[:2000]}")
        try:
            res = json.loads(out)
        except json.JSONDecodeError:
            raise RuntimeError(f"{name} printed non-JSON: {out[:500]}") from None
        png = None
        if isinstance(res, dict):
            if "png_base64" in res:
                png = base64.b64decode(res.pop("png_base64"))
            elif str(res.get("path", "")).endswith(".png"):
                rc2, data, _ = self.shell(f"base64 < {shlex.quote(res['path'])}", 60)
                if rc2 == 0:
                    png = base64.b64decode(data)
        return json.dumps(res)[:MAX_TOOL_TEXT], png


def run_episode(model, env, task, os_profile, mode, log_path, specs=None):
    """Run one task attempt. Returns the agent-side record (verify runs elsewhere).

    Time is split: api_seconds is spent waiting for the model, env_seconds is
    everything else (tools, settling, screenshots). timeout_s budgets env time
    only, so a slow API does not fail a task.
    """
    specs = specs or tool_specs(mode, task.get("tools_exclude", ()))
    names = {n for n, _, _ in specs}
    keyboard_only = bool(set(task.get("tools_exclude", ())) & MOUSE_TOOLS)
    t0 = time.monotonic()
    rec = {"steps": 0, "clicks": 0, "noop_clicks": 0, "agent_done": False, "agent_success": None,
           "out_of_budget": False, "error": None, "api_seconds": 0.0}
    log = open(log_path, "w")

    def write(**kw):
        log.write(json.dumps({"t": round(time.monotonic() - t0, 2), **kw}) + "\n")
        log.flush()

    def env_s():
        return time.monotonic() - t0 - rec["api_seconds"]

    try:
        shot, png = env.screenshot()
        last_hash = hashlib.sha256(png).hexdigest()
        system = system_prompt(os_profile, env.qmp.size, mode, task.get("gui_only", False))
        text = "Task: " + task["instruction"]
        if keyboard_only:
            text += "\n(For this task you have no mouse or pointer tools: use the keyboard only.)"
        model.start(system, text, png)
        write(event="start", system=system, user=text, tools=sorted(names), screenshot=os.path.basename(shot))
        while not rec["agent_done"]:
            if rec["steps"] >= task["max_steps"] or env_s() > task["timeout_s"]:
                rec["out_of_budget"] = True
                break
            ta = time.monotonic()
            calls, said = model.step()
            rec["api_seconds"] += time.monotonic() - ta
            write(event="model", text=said, calls=[{"id": c, "name": n, "input": a} for c, n, a in calls],
                  usage=dict(model.usage), api_s=round(time.monotonic() - ta, 2))
            if not calls:  # the model ended its turn without acting: treat as finished
                rec["agent_done"] = True
                break
            results, acted = [], False
            for i, (cid, name, args) in enumerate(calls):
                rec["steps"] += 1
                png_out, err = None, False
                try:
                    if name not in names:
                        raise ValueError(f"tool {name} is not available")
                    if name == "done":
                        rec["agent_done"], rec["agent_success"] = True, bool(args.get("success"))
                        out = "recorded"
                    else:
                        if keyboard_only and name == "cu_act" and args.get("action") in ("click", "press"):
                            raise ValueError("keyboard-only task: cu_act click/press is not available")
                        if name.startswith("cu_"):
                            out, png_out = env.panda(name, args)
                        else:
                            out = env.act(name, args)
                        if name not in READ_ONLY:
                            acted = True
                            time.sleep(SETTLE_S)
                        if name in READ_ONLY and png_out is None and name == "screenshot":
                            shot, png_out = env.screenshot()
                        elif name not in READ_ONLY:
                            shot, shot_png = env.screenshot()
                            h = hashlib.sha256(shot_png).hexdigest()
                            if name in ("click", "double_click", "cu_click"):
                                rec["clicks"] += 1
                                rec["noop_clicks"] += h == last_hash
                            last_hash = h
                            png_out = png_out or shot_png
                        if i < len(calls) - 1 and name not in ("screenshot", "cu_screenshot"):
                            png_out = None  # only the last result of a turn carries the screenshot
                except Exception as e:  # tool errors go back to the model
                    out, err = f"{type(e).__name__}: {e}", True
                write(event="tool", id=cid, name=name, input=args, result=out, error=err,
                      screenshot=os.path.basename(shot) if png_out else None)
                results.append([cid, out, png_out, err])
                if rec["agent_done"]:
                    # Answer the remaining calls so the transcript stays well-formed.
                    results += [[c, "skipped: done was called", None, False] for c, _, _ in calls[i + 1:]]
                    break
            if acted and not rec["agent_done"] and results[-1][2] is None:
                shot, results[-1][2] = env.screenshot()  # a failed last call still gets the screen
            model.add_results([tuple(r) for r in results])
    except Exception as e:
        rec["error"] = f"{type(e).__name__}: {e}"
        write(event="crash", error=rec["error"])
    finally:
        rec["seconds"] = round(time.monotonic() - t0, 1)
        rec["api_seconds"] = round(rec["api_seconds"], 1)
        rec["env_seconds"] = round(rec["seconds"] - rec["api_seconds"], 1)
        rec["usage"] = dict(model.usage)
        write(event="end", **{k: v for k, v in rec.items() if k != "usage"})
        log.close()
    return rec
