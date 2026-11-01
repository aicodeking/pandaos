// VM list, create form and the lifecycle buttons the detail page shares.

import { errorText, isMissing } from "./api.js";
import { createVMSpec, fmtIP, fmtSize, fmtTTL, vmActions } from "./format.js";
import { askDialog, badge, button, field, h, run, table, toast } from "./ui.js";

const POLL_MS = 10000;
const SIZES = ["small", "default", "large", "xlarge"];

export function vmButtons(ctx, vm, refresh) {
  const allowed = vmActions(vm.state);
  const label = vm.name || vm.id;
  const act = (text, enabled, handler, props = {}) =>
    button(text, (e) => run(handler, e.currentTarget), { disabled: !enabled, "aria-label": `${text} ${label}`, ...props });

  const start = async () => {
    await ctx.api.startVM(vm.id);
    toast(`Starting ${label}`);
    await refresh();
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
    const child = await ctx.api.forkVM(vm.id, answer.name.trim());
    toast(`Forked to ${child.name || child.id}`);
    ctx.navigate(`#/vms/${encodeURIComponent(child.id)}`);
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
    act("Start", allowed.start, start),
    act("Stop", allowed.stop, stop),
    act("Fork", allowed.fork, fork),
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
          { label: "State", cell: (vm) => badge(vm.state) },
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
  const poll = () => {
    if (holder.contains(document.activeElement) || document.querySelector("dialog[open]")) return;
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

// The list of team secrets decides which are offered. All checked means "omit the field", which is the API's all-secrets default.
function secretPicker(names) {
  const boxes = names.map((name) => ({ name, el: h("input", { type: "checkbox", checked: true }) }));
  const el = h(
    "fieldset",
    { class: "field" },
    h("legend", {}, "Secrets to inject"),
    boxes.length ? boxes.map((b) => h("label", { class: "check" }, b.el, b.name)) : h("p", { class: "muted" }, "This team has no secrets."),
  );
  const value = () => (boxes.every((b) => b.el.checked) ? undefined : boxes.filter((b) => b.el.checked).map((b) => b.name));
  return { el, value };
}

export function createVMScreen(ctx) {
  const error = h("p", { class: "error", role: "alert", hidden: true });
  const name = h("input", { type: "text", maxlength: "64", autocomplete: "off" });
  const size = h("input", { type: "text", list: "vm-sizes", autocomplete: "off" });
  const image = h("select", {}, h("option", { value: "" }, "Server default"));
  const snapshot = h("input", { type: "text", autocomplete: "off" });
  const environment = h("select", {}, h("option", { value: "" }, "None"));
  const environmentField = field("Environment template", environment, "Optional");
  environmentField.hidden = true;
  const ttl = h("input", { type: "number", min: "0", inputmode: "numeric" });
  const idle = h("input", { type: "number", min: "0", inputmode: "numeric" });
  const labels = h("textarea", { rows: "2", spellcheck: "false" });
  const env = h("textarea", { rows: "2", spellcheck: "false" });
  const secretsHolder = h("div");
  let picker = secretPicker([]);
  secretsHolder.append(picker.el);

  // Both lists are conveniences: the form works without them.
  ctx.api.images().then((images) => {
    image.append(...images.map((i) => h("option", { value: i.id }, i.default ? `${i.id} (default)` : i.id)));
  }).catch((err) => { if (!isMissing(err)) toast(errorText(err), true); });
  ctx.api.listSecrets().then((secrets) => {
    picker = secretPicker(secrets.map((x) => x.name));
    secretsHolder.replaceChildren(picker.el);
  }).catch((err) => { if (!isMissing(err)) toast(errorText(err), true); });

  // Newer endpoint: the field stays hidden on a server without it.
  ctx.api.listEnvironments().then((list) => {
    environment.append(...list.map((x) => h("option", { value: x.name }, x.name)));
    environmentField.hidden = false;
  }).catch((err) => { if (!isMissing(err)) toast(errorText(err), true); });

  const onsubmit = async (e) => {
    e.preventDefault();
    error.hidden = true;
    const submit = e.currentTarget.querySelector("button[type=submit]");
    submit.disabled = true;
    try {
      const vm = await ctx.api.createVM(createVMSpec({
        name: name.value, size: size.value, image: image.value, snapshot: snapshot.value, environment: environment.value,
        ttl_s: ttl.value, idle_stop_s: idle.value, labels: labels.value, env: env.value, secrets: picker.value(),
      }));
      toast(`Created ${vm.name || vm.id}`);
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
      field("TTL (seconds)", ttl, "Blank uses the server default"),
      field("Idle stop (seconds)", idle, "Stop after this long with no activity"),
      field("Labels", labels, "One KEY=value per line"),
      field("Environment", env, "One KEY=value per line. Use team secrets for anything sensitive."),
      secretsHolder,
      h("div", { class: "row" }, h("button", { type: "submit", class: "primary" }, "Create VM"), h("a", { class: "button", href: "#/vms" }, "Cancel")),
    ),
  ];
}
