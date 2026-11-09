// One VM: exec console, prompt, detached commands, files, ports, snapshots, connect.

import { errorText, eventDecoder, isMissing } from "./api.js";
import { fmtBytes, fmtCmd, fmtIP, fmtSize, fmtTime, fmtTTL, parseOptionalInt, safeHttpUrl, sshCommand } from "./format.js";
import { askDialog, badge, button, copyOrShow, downloadBlob, field, h, run, section, table, toast } from "./ui.js";
import { vmButtons } from "./vms.js";

const POLL_MS = 10000;
const MAX_OUTPUT_CHARS = 2_000_000;
const MAX_PREVIEW_BYTES = 1_000_000;

function outputPane() {
  const el = h("pre", { class: "output", role: "log", tabindex: "0", "aria-label": "Output" });
  const append = (text, isErr) => {
    if (!text) return;
    el.append(isErr ? h("span", { class: "stderr" }, text) : text);
    while (el.textContent.length > MAX_OUTPUT_CHARS && el.firstChild) el.firstChild.remove();
    el.scrollTop = el.scrollHeight;
  };
  return { el, append, clear: () => el.replaceChildren() };
}

// Streams exec or prompt events into a pane. Returns once the stream ends or is cancelled.
async function streamInto(pane, events) {
  const decode = eventDecoder();
  for await (const event of events) {
    pane.append(decode(event), event.stream === "stderr");
    if (event.error) pane.append(`\n${typeof event.error === "string" ? event.error : event.error.message}\n`, true);
    if (event.exit !== undefined) pane.append(`\n[exit ${event.exit}]\n`, event.exit !== 0);
  }
}

// One running stream at a time per form, with Cancel wired to an AbortController.
function streamRunner(ctx, pane, onMissing) {
  let controller = null;
  const cancel = button("Cancel", () => controller?.abort(), { disabled: true });
  ctx.onCleanup(() => controller?.abort());
  const start = async (open, submit) => {
    controller = new AbortController();
    submit.disabled = true;
    cancel.disabled = false;
    pane.clear();
    try {
      await streamInto(pane, open(controller.signal));
    } catch (err) {
      if (err.name === "AbortError") pane.append("\n[cancelled]\n", true);
      else if (isMissing(err) && onMissing) onMissing();
      else pane.append(`\n${errorText(err)}\n`, true);
    } finally {
      submit.disabled = false;
      cancel.disabled = true;
      controller = null;
    }
  };
  return { cancel, start };
}

function consoleSection(ctx, vm) {
  const load = async () => {
    const pane = outputPane();
    const runner = streamRunner(ctx, pane);
    const cmd = h("input", { type: "text", autocomplete: "off", autocapitalize: "off", spellcheck: "false", required: true, placeholder: "uname -a" });
    const cwd = h("input", { type: "text", autocomplete: "off", spellcheck: "false" });
    const timeout = h("input", { type: "number", min: "1", inputmode: "numeric" });
    const submit = h("button", { type: "submit", class: "primary" }, "Run");
    const onsubmit = (e) => {
      e.preventDefault();
      let timeoutS;
      try {
        timeoutS = parseOptionalInt(timeout.value, "Timeout", { min: 1 });
      } catch (err) {
        toast(err.message, true);
        return;
      }
      const spec = { cmd: ["bash", "-lc", cmd.value], cwd: cwd.value || undefined, timeout_s: timeoutS };
      runner.start((signal) => ctx.api.exec(vm.id, spec, signal), submit);
    };
    return h(
      "div",
      {},
      h(
        "form",
        { class: "form", onsubmit },
        field("Command", cmd, "Runs with bash -lc on the VM"),
        h("div", { class: "grid2" }, field("Working directory", cwd), field("Timeout (seconds)", timeout)),
        h("div", { class: "row" }, submit, runner.cancel),
      ),
      pane.el,
    );
  };
  return section("Exec console", load);
}

function promptHistory(ctx, vm) {
  return section("Prompt history", async (refresh) => {
    const prompts = await ctx.api.listPrompts(vm.id);
    const logs = outputPane();
    const showLogs = async (p) => {
      logs.clear();
      logs.append(await ctx.api.promptLogs(vm.id, p.id));
    };
    const interrupt = async (p) => {
      const ok = await askDialog({ title: "Interrupt this prompt?", message: "The agent stops its current turn.", confirm: "Interrupt", danger: true });
      if (!ok) return;
      await ctx.api.interruptPrompt(vm.id, p.id);
      await refresh();
    };
    return h(
      "div",
      {},
      h("div", { class: "row" }, button("Refresh", refresh)),
      table(
        "Prompt history",
        [
          { label: "Prompt", cell: (p) => p.prompt ?? p.id },
          { label: "Agent", cell: (p) => p.agent ?? "—" },
          { label: "State", cell: (p) => badge(p.state) },
          { label: "Started", cell: (p) => fmtTime(p.started_at ?? p.created_at ?? p.created) },
          {
            label: "Actions",
            cell: (p) => h(
              "div",
              { class: "row" },
              button("Logs", (e) => run(() => showLogs(p), e.currentTarget)),
              button("Interrupt", (e) => run(() => interrupt(p), e.currentTarget), { class: "danger-text", disabled: p.state !== "running" }),
            ),
          },
        ],
        prompts,
        "No prompts yet.",
      ),
      logs.el,
    );
  }, { open: false });
}

function promptSection(ctx, vm) {
  const el = section("Prompt an agent", async () => {
    const pane = outputPane();
    const runner = streamRunner(ctx, pane, () => { el.hidden = true; });
    const prompt = h("textarea", { rows: "3", required: true });
    const agent = h("select", {}, ["claude", "codex", "opencode", "pi", "prime", "kimi", "mistral"].map((a) => h("option", { value: a }, a)));
    const model = h("input", { type: "text", autocomplete: "off" });
    const submit = h("button", { type: "submit", class: "primary" }, "Send");
    const onsubmit = (e) => {
      e.preventDefault();
      const spec = { prompt: prompt.value, agent: agent.value, model: model.value || undefined };
      runner.start((signal) => ctx.api.prompt(vm.id, spec, signal), submit);
    };
    return h(
      "div",
      {},
      h(
        "form",
        { class: "form", onsubmit },
        field("Prompt", prompt, "Team secrets are passed to the agent as environment variables"),
        h("div", { class: "grid2" }, field("Agent", agent), field("Model", model, "Optional")),
        h("div", { class: "row" }, submit, runner.cancel),
      ),
      pane.el,
    );
  }, { open: false });
  return el;
}

function commandsSection(ctx, vm) {
  return section("Detached commands", async (refresh) => {
    // Live processes need a running VM. History also answers for a stopped one.
    const commands = await (vm.state === "running" ? ctx.api.listCommands(vm.id) : ctx.api.commandHistory(vm.id));
    const logs = outputPane();
    const cmd = h("input", { type: "text", autocomplete: "off", autocapitalize: "off", spellcheck: "false", required: true, placeholder: "python3 -m http.server 8000" });
    const showLogs = async (c) => {
      logs.clear();
      logs.append(await ctx.api.commandLogs(vm.id, c.id));
    };
    const stop = async (c) => {
      const ok = await askDialog({ title: "Stop this command?", message: fmtCmd(c.cmd), confirm: "Stop command", danger: true });
      if (!ok) return;
      await ctx.api.stopCommand(vm.id, c.id);
      await refresh();
    };
    const onsubmit = (e) => {
      e.preventDefault();
      run(async () => {
        await ctx.api.startCommand(vm.id, { cmd: ["bash", "-lc", cmd.value] });
        toast("Command started");
        await refresh();
      }, e.currentTarget.querySelector("button[type=submit]"));
    };
    return h(
      "div",
      {},
      h(
        "form",
        { class: "form", onsubmit },
        field("Command", cmd, "Keeps running after this page closes"),
        h("div", { class: "row" }, h("button", { type: "submit", class: "primary" }, "Start"), button("Refresh", refresh)),
      ),
      table(
        "Detached commands",
        [
          { label: "Command", cell: (c) => h("code", {}, fmtCmd(c.cmd)) },
          { label: "State", cell: (c) => badge(c.state) },
          { label: "Exit", cell: (c) => c.exit ?? c.signal ?? "—" },
          { label: "Started", cell: (c) => fmtTime(c.started_at ?? c.created_at) },
          {
            label: "Actions",
            cell: (c) => h(
              "div",
              { class: "row" },
              button("Logs", (e) => run(() => showLogs(c), e.currentTarget)),
              button("Stop", (e) => run(() => stop(c), e.currentTarget), { class: "danger-text", disabled: c.state !== "running" }),
            ),
          },
        ],
        commands,
        "No detached commands.",
      ),
      logs.el,
    );
  }, { open: false });
}

// GET and PUT /files are in the base API. The directory listing is newer, so it hides itself on 404.
function filesSection(ctx, vm) {
  return section("Files", async () => {
    const path = h("input", { type: "text", value: "/home", autocomplete: "off", spellcheck: "false", required: true, pattern: "/.*", title: "An absolute path" });
    const preview = outputPane();
    const listing = h("div");
    const upload = h("input", { type: "file" });

    const list = async (dir) => {
      path.value = dir;
      try {
        const entries = await ctx.api.listFiles(vm.id, dir);
        const joined = (name) => `${dir.replace(/\/+$/, "")}/${name}`;
        const isDir = (entry) => entry.type === "dir";
        listing.replaceChildren(table(
          `Files in ${dir}`,
          [
            {
              label: "Name",
              cell: (f) => (isDir(f)
                ? button(`${f.name}/`, () => run(() => list(joined(f.name))), { class: "link" })
                : button(f.name, () => { path.value = joined(f.name); }, { class: "link" })),
            },
            { label: "Size", cell: (f) => (isDir(f) ? "—" : fmtBytes(f.size)) },
            { label: "Modified", cell: (f) => fmtTime(f.mtime) },
          ],
          entries,
          "This directory is empty.",
        ));
      } catch (err) {
        if (isMissing(err)) listing.replaceChildren();
        else throw err;
      }
    };
    const view = async () => {
      const blob = await ctx.api.readFile(vm.id, path.value);
      preview.clear();
      preview.append(await blob.slice(0, MAX_PREVIEW_BYTES).text());
      if (blob.size > MAX_PREVIEW_BYTES) preview.append(`\n[showing the first ${fmtBytes(MAX_PREVIEW_BYTES)} of ${fmtBytes(blob.size)}]\n`, true);
    };
    const download = async () => {
      const blob = await ctx.api.readFile(vm.id, path.value);
      downloadBlob(blob, path.value.split("/").pop() || "download");
    };
    const send = async () => {
      const file = upload.files[0];
      if (!file) throw new Error("Choose a file first");
      if (!path.value.startsWith("/") || path.value.endsWith("/")) throw new Error("Upload path must be an absolute file path");
      await ctx.api.writeFile(vm.id, path.value, file);
      toast(`Uploaded to ${path.value}`);
    };
    const remove = async () => {
      const target = path.value;
      const ok = await askDialog({ title: `Delete ${target}?`, message: "This deletes the file on the VM. It cannot be undone.", confirm: "Delete file", danger: true });
      if (!ok) return;
      await ctx.api.deleteFile(vm.id, target);
      toast(`Deleted ${target}`);
    };
    // A stopped VM or a missing /home is normal here, so this first listing fails quietly.
    list(path.value).catch((err) => listing.replaceChildren(h("p", { class: "muted" }, errorText(err))));

    const act = (label, handler) => button(label, (e) => {
      if (!path.reportValidity()) return;
      run(handler, e.currentTarget);
    });
    return h(
      "div",
      {},
      h(
        "div",
        { class: "form" },
        field("Path", path, "Absolute path on the VM"),
        h("div", { class: "row" }, act("List", () => list(path.value)), act("View", view), act("Download", download), act("Delete", remove)),
        field("Upload to this path", upload),
        h("div", { class: "row" }, act("Upload", send)),
      ),
      listing,
      preview.el,
    );
  }, { open: false });
}

function portsSection(ctx, vm) {
  return section("Ports", async (refresh) => {
    const ports = await ctx.api.listPorts(vm.id);
    const port = h("input", { type: "number", min: "1", max: "65535", required: true, inputmode: "numeric" });
    const auth = h("select", {}, h("option", { value: "private" }, "Private (signed URL)"), h("option", { value: "public" }, "Public (no token)"));
    const onsubmit = (e) => {
      e.preventDefault();
      run(async () => {
        if (auth.value === "public") {
          const ok = await askDialog({ title: `Make port ${port.value} public?`, message: "Anyone on the internet who has the URL can reach this port, with no token. It also opens a raw TCP port.", confirm: "Expose publicly", danger: true });
          if (!ok) return;
        }
        await ctx.api.addPort(vm.id, Number(port.value), auth.value);
        await refresh();
      }, e.currentTarget.querySelector("button[type=submit]"));
    };
    const copyUrl = async (p) => {
      const { url } = await ctx.api.portUrl(vm.id, p.guest_port);
      if (!safeHttpUrl(url)) throw new Error("The server returned an unusable URL");
      await copyOrShow(url, `URL for port ${p.guest_port}`);
    };
    const remove = async (p) => {
      const ok = await askDialog({ title: `Remove port ${p.guest_port}?`, message: "Its URL stops working.", confirm: "Remove port", danger: true });
      if (!ok) return;
      await ctx.api.removePort(vm.id, p.guest_port);
      await refresh();
    };
    return h(
      "div",
      {},
      table(
        "Forwarded ports",
        [
          { label: "Port", cell: (p) => p.guest_port },
          { label: "Access", cell: (p) => p.auth ?? "private" },
          { label: "Hostname", cell: (p) => p.hostname ?? "—" },
          { label: "TCP", cell: (p) => p.host_port ?? "—" },
          {
            label: "Actions",
            cell: (p) => h(
              "div",
              { class: "row" },
              button("Copy URL", (e) => run(() => copyUrl(p), e.currentTarget), { "aria-label": `Copy URL for port ${p.guest_port}` }),
              button("Remove", (e) => run(() => remove(p), e.currentTarget), { class: "danger-text", "aria-label": `Remove port ${p.guest_port}` }),
            ),
          },
        ],
        ports,
        "No ports forwarded.",
      ),
      h(
        "form",
        { class: "form", onsubmit },
        h("div", { class: "grid2" }, field("Port", port), field("Access", auth)),
        h("div", { class: "row" }, h("button", { type: "submit" }, "Forward port")),
      ),
    );
  }, { open: false });
}

function snapshotsSection(ctx, vm) {
  return section("Snapshots", async (refresh) => {
    const snapshots = await ctx.api.listSnapshots(vm.id);
    const name = h("input", { type: "text", autocomplete: "off" });
    const onsubmit = (e) => {
      e.preventDefault();
      run(async () => {
        await ctx.api.createSnapshot(vm.id, name.value.trim());
        toast("Snapshot created");
        await refresh();
      }, e.currentTarget.querySelector("button[type=submit]"));
    };
    const remove = async (snap) => {
      const ok = await askDialog({ title: `Delete snapshot ${snap.name || snap.id}?`, message: "This cannot be undone.", confirm: "Delete snapshot", danger: true });
      if (!ok) return;
      await ctx.api.deleteSnapshot(vm.id, snap.id);
      await refresh();
    };
    return h(
      "div",
      {},
      table(
        "Snapshots",
        [
          { label: "Name", cell: (x) => x.name || "—" },
          { label: "ID", cell: (x) => h("code", {}, x.id) },
          { label: "Created", cell: (x) => fmtTime(x.created_at) },
          { label: "Actions", cell: (x) => button("Delete", (e) => run(() => remove(x), e.currentTarget), { class: "danger-text", "aria-label": `Delete snapshot ${x.name || x.id}` }) },
        ],
        snapshots,
        "No snapshots.",
      ),
      h(
        "form",
        { class: "form", onsubmit },
        field("Snapshot name", name, "Optional"),
        h("div", { class: "row" }, h("button", { type: "submit" }, "Create snapshot")),
      ),
    );
  }, { open: false });
}

function connectSection(ctx, vm) {
  return section("Connect", async () => {
    const desktopHolder = h("div", {});
    const sshHolder = h("div", {});
    const publicKey = h("textarea", { rows: "2", spellcheck: "false" });

    const openDesktop = async () => {
      const { url, expires_at: expires } = await ctx.api.desktop(vm.id);
      const safe = safeHttpUrl(url);
      if (!safe) throw new Error("The server returned an unusable desktop URL");
      desktopHolder.replaceChildren(
        h("a", { href: safe, target: "_blank", rel: "noopener noreferrer" }, "Open desktop in a new tab"),
        h("small", { class: "hint" }, ` Link expires ${fmtTime(expires)}`),
      );
    };
    const showSsh = async () => {
      const info = await ctx.api.ssh(vm.id, publicKey.value.trim());
      const command = sshCommand(info);
      sshHolder.replaceChildren(h("pre", { class: "output short", tabindex: "0" }, command), button("Copy command", () => copyOrShow(command, "ssh command")));
    };

    return h(
      "div",
      { class: "form" },
      h("h3", {}, "Desktop"),
      h("div", { class: "row" }, button("Get desktop link", (e) => run(openDesktop, e.currentTarget))),
      desktopHolder,
      h("h3", {}, "SSH"),
      field("Public key", publicKey, "Optional. Adds this OpenSSH key to the VM's authorized_keys."),
      h("div", { class: "row" }, button("Show ssh command", (e) => run(showSsh, e.currentTarget))),
      sshHolder,
    );
  });
}

function summary(vm, actions) {
  const fact = (term, value) => h("div", {}, h("dt", {}, term), h("dd", {}, value));
  return [
    h("div", { class: "row spread" }, h("h1", { tabindex: "-1" }, vm.name || vm.id), badge(vm.state)),
    vm.error ? h("p", { class: "error", role: "alert" }, vm.error) : null,
    h(
      "dl",
      { class: "facts" },
      fact("ID", h("code", {}, vm.id)),
      fact("Size", fmtSize(vm)),
      fact("IP", fmtIP(vm)),
      fact("TTL", fmtTTL(vm)),
      fact("Image", vm.image || "—"),
      fact("Created", fmtTime(vm.created_at)),
    ),
    actions,
  ];
}

export async function vmDetailScreen(ctx, id) {
  const vm = await ctx.api.getVM(id);
  const screenCtx = { ...ctx, afterDelete: async () => ctx.navigate("#/vms") };
  const header = h("div", { class: "vm-header" });
  let lastSeen = null;

  const refresh = async () => {
    const latest = await ctx.api.getVM(id);
    const seen = JSON.stringify(latest);
    if (seen !== lastSeen) {
      lastSeen = seen;
      header.replaceChildren(...summary(latest, vmButtons(screenCtx, latest, refresh)).filter(Boolean));
    }
  };
  lastSeen = JSON.stringify(vm);
  header.replaceChildren(...summary(vm, vmButtons(screenCtx, vm, refresh)).filter(Boolean));

  // Keeps keyboard focus in the header: a poll waits until focus moves off it or a dialog closes.
  const timer = setInterval(() => {
    if (header.contains(document.activeElement) || document.querySelector("dialog[open]")) return;
    refresh().catch(() => {});
  }, POLL_MS);
  ctx.onCleanup(() => clearInterval(timer));

  return [
    h("p", {}, h("a", { href: "#/vms" }, "← All VMs")),
    header,
    connectSection(ctx, vm),
    consoleSection(ctx, vm),
    promptSection(ctx, vm),
    promptHistory(ctx, vm),
    commandsSection(ctx, vm),
    filesSection(ctx, vm),
    portsSection(ctx, vm),
    snapshotsSection(ctx, vm),
  ];
}
