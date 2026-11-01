// DOM helpers. Every string child becomes a text node, so API data is never parsed
// as HTML. Nothing in the dashboard uses innerHTML.

import { errorText, isMissing, ApiError } from "./api.js";

const SVG_NS = "http://www.w3.org/2000/svg";

function build(el, props, children) {
  for (const [key, value] of Object.entries(props ?? {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key.startsWith("on")) el.addEventListener(key.slice(2), value);
    else if (key === "value") el.value = value;
    else if (key === "checked" || key === "disabled" || key === "open" || key === "hidden") el[key] = Boolean(value);
    else el.setAttribute(key, value === true ? "" : String(value));
  }
  el.append(...children.flat(Infinity).filter((c) => c !== null && c !== undefined && c !== false).map((c) => (c instanceof Node ? c : String(c))));
  return el;
}

export const h = (tag, props, ...children) => build(document.createElement(tag), props, children);
export const s = (tag, props, ...children) => build(document.createElementNS(SVG_NS, tag), props, children);

let fieldSeq = 0;

// A control with a real <label> tied to it, plus an optional hint it describes itself with.
export function field(label, control, hint) {
  fieldSeq += 1;
  const id = `f${fieldSeq}`;
  control.id = id;
  const hintEl = hint ? h("small", { id: `${id}-hint`, class: "hint" }, hint) : null;
  if (hintEl) control.setAttribute("aria-describedby", hintEl.id);
  return h("div", { class: "field" }, h("label", { for: id }, label), control, hintEl);
}

export const button = (label, onclick, props = {}) => h("button", { type: "button", onclick, ...props }, label);

export function toast(message, isError = false) {
  const el = h("div", { class: isError ? "toast toast-error" : "toast" }, message);
  document.getElementById("toasts").append(el);
  setTimeout(() => el.remove(), isError ? 9000 : 4000);
}

// Runs an action from a button: disables it while pending and reports failure.
export async function run(action, btn) {
  if (btn) btn.disabled = true;
  try {
    return await action();
  } catch (err) {
    toast(errorText(err), true);
    return undefined;
  } finally {
    if (btn) btn.disabled = false;
  }
}

function showDialog(content) {
  return new Promise((resolve) => {
    const dialog = h("dialog", { class: "dialog" });
    const finish = (value) => {
      dialog.close();
      dialog.remove();
      resolve(value);
    };
    dialog.addEventListener("cancel", (e) => {
      e.preventDefault();
      finish(null);
    });
    dialog.append(...content(finish));
    document.body.append(dialog);
    dialog.showModal();
  });
}

// Returns field values (or {} with no fields) on confirm, null on cancel.
// A dangerous action starts with focus on Cancel, so Enter does not destroy anything.
export function askDialog({ title, message, confirm = "Confirm", danger = false, fields = [] }) {
  return showDialog((finish) => {
    const inputs = fields.map((f) => ({ ...f, el: h("input", { type: "text", autocomplete: "off", value: f.value ?? "" }) }));
    const onsubmit = (e) => {
      e.preventDefault();
      finish(Object.fromEntries(inputs.map((f) => [f.name, f.el.value])));
    };
    return [
      h(
        "form",
        { onsubmit },
        h("h2", {}, title),
        message ? h("p", {}, message) : null,
        inputs.map((f) => field(f.label, f.el, f.hint)),
        h(
          "div",
          { class: "row end" },
          button("Cancel", () => finish(null), { autofocus: danger || !inputs.length ? true : undefined }),
          h("button", { type: "submit", class: danger ? "danger" : "primary" }, confirm),
        ),
      ),
    ];
  });
}

export async function copyText(value) {
  try {
    await navigator.clipboard.writeText(value);
    return true;
  } catch {
    return false;
  }
}

// Shows a value once, for copying. The dialog is removed from the DOM when it closes.
export function showValueDialog({ title, message, label, value }) {
  return showDialog((finish) => {
    const input = h("input", { type: "text", readonly: true, value, autocomplete: "off", onfocus: (e) => e.target.select() });
    return [
      h("h2", {}, title),
      message ? h("p", {}, message) : null,
      field(label, input),
      h(
        "div",
        { class: "row end" },
        button("Copy", async () => toast((await copyText(value)) ? "Copied" : "Copy failed. Select the text and copy it by hand.", false)),
        button("Done", () => finish(null), { class: "primary", autofocus: true }),
      ),
    ];
  });
}

// Clipboard needs https and a user gesture. When it refuses, the value is shown for copying by hand.
export async function copyOrShow(value, label) {
  if (await copyText(value)) toast(`${label} copied`);
  else await showValueDialog({ title: label, label, value });
}

// A titled, collapsible block that loads itself. An endpoint this server lacks
// hides the whole block; a scope it lacks becomes a note; other errors offer Retry.
export function section(title, load, { open = true } = {}) {
  const body = h("div", { class: "section-body" }, h("p", { class: "muted" }, "Loading…"));
  const el = h("details", { class: "card", open }, h("summary", {}, h("h2", {}, title)), body);
  const refresh = async () => {
    try {
      body.replaceChildren(await load(refresh));
    } catch (err) {
      if (isMissing(err)) el.hidden = true;
      else if (err instanceof ApiError && err.status === 403) body.replaceChildren(h("p", { class: "muted" }, `This key cannot use this section: ${err.message}`));
      else body.replaceChildren(h("p", { class: "error", role: "alert" }, errorText(err)), button("Retry", refresh));
    }
  };
  refresh();
  return el;
}

// Rows become stacked cards at phone width (see style.css), using data-label for the heading.
export function table(caption, columns, rows, emptyText = "Nothing here yet.") {
  if (!rows.length) return h("p", { class: "muted" }, emptyText);
  return h(
    "table",
    { class: "table" },
    h("caption", { class: "sr-only" }, caption),
    h("thead", {}, h("tr", {}, columns.map((c) => h("th", { scope: "col" }, c.label)))),
    h("tbody", {}, rows.map((row) => h("tr", {}, columns.map((c) => h("td", { "data-label": c.label }, c.cell(row)))))),
  );
}

export function badge(state) {
  const text = String(state ?? "unknown");
  const tone = { running: "good", stopped: "idle", error: "bad" }[text] ?? "busy";
  return h("span", { class: `badge badge-${tone}` }, text);
}

export function downloadBlob(blob, filename) {
  const url = URL.createObjectURL(blob);
  const link = h("a", { href: url, download: filename });
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}
