// VM list, create form and the lifecycle buttons the detail page shares.

import { errorText, errorVMIds, isMissing } from "./api.js";
import { createVMSpec, fmtIP, fmtSize, fmtTTL, resizeVMSpec, shouldDeferPoll, vmActions } from "./format.js";
import { askDialog, badge, envBadge, button, field, h, run, table, toast } from "./ui.js";

const POLL_MS = 10000;
const SIZES = ["small", "default", "large", "xlarge"];

export function vmButtons(ctx, vm, refresh) {
  const allowed = vmActions(vm.state, vm.env_error);
  const retry = vm.state === "running" && Boolean(String(vm.env_error ?? "").trim());
  const label = vm.name || vm.id;
  const act = (text, enabled, handler, props = {}) =>
    button(text, (e) => run(handler, e.currentTarget), { disabled: !enabled, "aria-label": `${text} ${label}`, ...props });

  const start = async () => {
    // Refresh after every attempt: a failed start may still have changed the VM (up, env not delivered).
    try {
      const started = await ctx.api.startVM(vm.id);
      if (String(started?.env_error ?? "").trim()) toast(`${label} started but its environment was not delivered: ${started.env_error}`, true);
      else toast(`Starting ${label}`);
    } finally {
      await refresh().catch(() => {});
    }
  };
  const stop = async () => {
    const ok = await askDialog({ title: `Stop ${label}?`, message: "Running processes pause and any open connections drop. The disk is kept.", confirm: "Stop VM", danger: true });
    if (!ok) return;
    await ctx.api.stopVM(vm.id);
    toast(`Stopping ${label}`);
    await refresh();
  };
  const fork = async () => {
    const answer = await askDialog({
      title: `Fork ${label}?`,
      message: "Creates a new VM from this VM's disk. It keeps a copy of everything on that disk, including secrets written there.",
      confirm: "Fork VM",
      fields: [{ name: "name", label: "Name of the new VM", hint: "Optional" }],
    });
    if (!answer) return;
    let child;
    try {
      child = await ctx.api.forkVM(vm.id, answer.name.trim());
    } catch (err) {
      // A failed fork may keep stopped VMs (named in the error text): show them.
      if (errorVMIds(err).length) await refresh().catch(() => {});
      throw err;
    }
    const forkEnvErr = String(child?.env_error ?? "").trim();
    if (forkEnvErr) toast(`Forked to ${child.name || child.id} but its environment was not delivered: ${forkEnvErr}`, true);
    else toast(`Forked to ${child.name || child.id}`);
    ctx.navigate(`#/vms/${encodeURIComponent(child.id)}`);
  };
  const resize = async () => {
    const answer = await askDialog({
      title: `Resize ${label}`,
      message: vm.state === "running"
        ? "Growing the disk works live. Changing vCPU/RAM size requires stopping the VM first."
        : "Change vCPU/RAM size and/or grow the disk. Discard the memory snapshot if this stopped VM holds one.",
      confirm: "Resize VM",
      fields: [
        { name: "size", label: "Size", value: vm.state === "stopped" ? (vm.size ?? "") : "", placeholder: vm.size || "default", options: SIZES, hint: "Optional. Leave blank to keep current size." },
        { name: "disk_gib", label: "Disk (GiB)", type: "number", min: "1", hint: "Optional. Grow-only." },
        { name: "discard_memory", label: "Discard saved memory snapshot (cold boot on next start)", type: "checkbox", checked: false },
      ],
    });
    if (!answer) return;
    const spec = resizeVMSpec(answer);
    const updated = await ctx.api.resizeVM(vm.id, spec);
    toast(`Resized ${label} to ${fmtSize(updated)}`);
    await refresh();
  };
  const remove = async () => {
    const ok = await askDialog({ title: `Delete ${label}?`, message: "This deletes the VM and its disk for good. It cannot be undone.", confirm: "Delete VM", danger: true });
    if (!ok) return;
    await ctx.api.deleteVM(vm.id);
    toast(`Deleted ${label}`);
    await ctx.afterDelete();
  };

  return h(
    "div",
    { class: "row" },
    act(retry ? "Retry environment" : "Start", allowed.start, start),
    act("Stop", allowed.stop, stop),
    act("Fork", allowed.fork, fork),
    act("Resize", allowed.resize, resize),
    act("Delete", allowed.remove, remove, { class: "danger-text" }),
  );
}

export function vmListScreen(ctx) {
  const holder = h("div", { "aria-live": "polite" }, h("p", { class: "muted" }, "Loading…"));
  let lastSeen = null;

  const load = async () => {
    const vms = await ctx.api.listVMs();
    const seen = JSON.stringify(vms);
    if (seen === lastSeen) return;
    lastSeen = seen;
    const screenCtx = { ...ctx, afterDelete: load };
    const rows = [...vms].sort((a, b) => String(b.created_at).localeCompare(String(a.created_at)));
    holder.replaceChildren(
      table(
        "Your VMs",
        [
          { label: "Name", cell: (vm) => h("a", { href: `#/vms/${encodeURIComponent(vm.id)}` }, vm.name || vm.id) },
          { label: "State", cell: (vm) => h("span", {}, badge(vm.state), " ", envBadge(vm)) },
          { label: "Size", cell: fmtSize },
          { label: "IP", cell: fmtIP },
          { label: "TTL", cell: (vm) => fmtTTL(vm) },
          { label: "Actions", cell: (vm) => vmButtons(screenCtx, vm, load) },
        ],
        rows,
        "No VMs yet. Create one to get started.",
      ),
    );
  };

  // Re-rendering under the keyboard would lose focus, so a poll waits while focus is in the table.
  let lastFocusAt = 0;
  holder.addEventListener("focusin", () => { lastFocusAt = Date.now(); });
  const poll = () => {
    if (shouldDeferPoll({ focusInside: holder.contains(document.activeElement), dialogOpen: Boolean(document.querySelector("dialog[open]")), lastFocusAt })) return;
    load().catch((err) => {
      if (!document.hidden) toast(errorText(err), true);
    });
  };
  const timer = setInterval(poll, POLL_MS);
  ctx.onCleanup(() => clearInterval(timer));
  load().catch((err) => holder.replaceChildren(h("p", { class: "error", role: "alert" }, errorText(err))));

  return [
    h("div", { class: "row spread" }, h("h1", { tabindex: "-1" }, "VMs"), h("a", { class: "button primary", href: "#/vms/new" }, "Create VM")),
    holder,
  ];
}

// Secrets baseline for the chosen environment. A blank choice means the team's default environment.
// null/undefined secrets mean @default (the inject-true set), [] none, a list exactly those.
export function environmentSecrets(envList, selected) {
  const envs = envList ?? [];
  const env = selected ? envs.find((x) => x.id === selected) ?? envs.find((x) => x.name === selected) : envs.find((x) => x.is_default);
  return Array.isArray(env?.secrets) ? env.secrets : undefined;
}

// The list of team secrets decides which are offered. value() is undefined (field omitted, so the server applies
// the environment's selection or @default) until the user changes the checked set from its baseline.
// reset(names) re-baselines the boxes, e.g. when another environment is chosen.
// carry ({noEnv, secrets}, from a previous picker's state()) restores the user's choices when the picker is rebuilt.
export function secretPicker(secrets, initial, carry) {
  const items = (secrets ?? []).map((item) => (typeof item === "string" ? { name: item, inject: true } : item));
  const defaults = items.filter((s) => (typeof s.inject === "boolean" ? s.inject : true)).map((s) => s.name);
  let baseline = Array.isArray(initial) ? [...initial] : defaults;
  const boxes = items.map((s) => ({ name: s.name, el: h("input", { type: "checkbox", checked: baseline.includes(s.name) }) }));
  const noEnvBox = h("input", { type: "checkbox", checked: Boolean(carry?.noEnv) });
  if (Array.isArray(carry?.secrets)) for (const b of boxes) b.el.checked = carry.secrets.includes(b.name);
  const el = h(
    "fieldset",
    { class: "field" },
    h("legend", {}, "Secrets to inject"),
    boxes.length ? boxes.map((b) => h("label", { class: "check" }, b.el, b.name)) : h("p", { class: "muted" }, "This team has no secrets."),
  );
  const noEnvEl = h("label", { class: "check" }, noEnvBox, "No secrets (start without any environment)");
  const checked = () => boxes.filter((b) => b.el.checked).map((b) => b.name);
  const same = (a, b) => a.length === b.length && a.every((x) => b.includes(x));
  const value = () => (boxes.length === 0 || same(checked(), baseline) ? undefined : checked());
  const reset = (names) => {
    baseline = Array.isArray(names) ? [...names] : defaults;
    for (const b of boxes) b.el.checked = baseline.includes(b.name);
  };
  const state = () => ({ noEnv: Boolean(noEnvBox.checked), secrets: value() });
  return { el, noEnvEl, value, reset, state, noEnv: () => Boolean(noEnvBox.checked), noEnvInput: noEnvBox };
}

export function createVMScreen(ctx) {
  const error = h("p", { class: "error", role: "alert", hidden: true });
  const name = h("input", { type: "text", maxlength: "120", autocomplete: "off" });
  const size = h("input", { type: "text", list: "vm-sizes", autocomplete: "off" });
  const image = h("select", {}, h("option", { value: "" }, "Server default"));
  const snapshot = h("input", { type: "text", autocomplete: "off" });
  const environment = h("select", {}, h("option", { value: "" }, "Team default"));
  const environmentField = field("Environment template", environment, "Optional");
  environmentField.hidden = true;
  const ttl = h("input", { type: "number", min: "0", inputmode: "numeric" });
  const idle = h("input", { type: "number", min: "-1", inputmode: "numeric" });
  const labels = h("textarea", { rows: "2", spellcheck: "false" });
  const env = h("textarea", { rows: "2", spellcheck: "false" });
  const secretsHolder = h("div");
  let picker = secretPicker([]);
  secretsHolder.append(picker.el, picker.noEnvEl);
  let secretList = [];
  let envList = [];
  const selectedSecrets = () => environmentSecrets(envList, environment.value);
  // keepSecrets: list reloads keep the user's checked set; an environment change re-baselines it. No-secrets always carries over.
  const mountPicker = (keepSecrets = true) => {
    const prev = picker.state();
    picker = secretPicker(secretList, selectedSecrets(), { noEnv: prev.noEnv, secrets: keepSecrets ? prev.secrets : undefined });
    picker.noEnvInput.addEventListener?.("change", syncNoEnv);
    secretsHolder.replaceChildren(picker.el, picker.noEnvEl);
    syncNoEnv();
  };
  const syncNoEnv = () => { environment.disabled = picker.noEnv(); };
  environment.addEventListener?.("change", () => mountPicker(false));

  // Both lists are conveniences: the form works without them.
  ctx.api.images().then((images) => {
    image.append(...images.map((i) => h("option", { value: i.id }, i.default ? `${i.id} (default)` : i.id)));
  }).catch((err) => { if (!isMissing(err)) toast(errorText(err), true); });
  ctx.api.listSecrets().then((secrets) => {
    secretList = secrets;
    mountPicker();
  }).catch((err) => { if (!isMissing(err)) toast(errorText(err), true); });

  // Newer endpoint: the field stays hidden on a server without it.
  ctx.api.listEnvironments().then((list) => {
    envList = list;
    environment.append(...list.map((x) => h("option", { value: x.id ?? x.name }, x.name || x.id)));
    environmentField.hidden = false;
    mountPicker();
  }).catch((err) => { if (!isMissing(err)) toast(errorText(err), true); });

  const onsubmit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    const submit = e.currentTarget.querySelector("button[type=submit]");
    submit.disabled = true;
    try {
      const vm = await ctx.api.createVM(createVMSpec({
        name: name.value, size: size.value, image: image.value, snapshot: snapshot.value, environment: environment.value,
        ttl_s: ttl.value, idle_stop_s: idle.value, labels: labels.value, env: env.value, secrets: picker.value(), no_env: picker.noEnv(),
      }));
      const envErr = String(vm?.env_error ?? "").trim();
      if (envErr) toast(`Created ${vm.name || vm.id} but its environment was not delivered: ${envErr}`, true);
      else toast(`Created ${vm.name || vm.id}`);
      ctx.navigate(`#/vms/${encodeURIComponent(vm.id)}`);
    } catch (err) {
      error.textContent = errorText(err);
      error.hidden = false;
      error.focus?.();
    } finally {
      submit.disabled = false;
    }
  };

  return [
    h("h1", { tabindex: "-1" }, "Create VM"),
    h(
      "form",
      { class: "card form", onsubmit },
      error,
      field("Name", name, "Optional"),
      field("Size", size, "Blank uses the server default. The server's config decides which names exist."),
      h("datalist", { id: "vm-sizes" }, SIZES.map((v) => h("option", { value: v }))),
      field("Image", image),
      environmentField,
      field("From snapshot", snapshot, "Optional snapshot id"),
      field("TTL (seconds)", ttl, "Blank = 1 hour (server default); 0 = no auto-stop"),
      field("Idle stop (seconds)", idle, "Stop after this long with no activity (-1 = never)"),
      field("Labels", labels, "One KEY=value per line"),
      field("Environment", env, "One KEY=value per line. Use team secrets for anything sensitive."),
      secretsHolder,
      h("div", { class: "row" }, h("button", { type: "submit", class: "primary" }, "Create VM"), h("a", { class: "button", href: "#/vms" }, "Cancel")),
    ),
  ];
}
