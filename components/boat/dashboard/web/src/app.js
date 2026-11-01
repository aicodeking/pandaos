// Sign-in, navigation and the hash router. Hash routes mean the dashboard works
// under any path behind Caddy with no rewrite rules.

import config from "../config.js";
import { createClient, createSession, errorText, isMissing, validKey } from "./api.js";
import { environmentsScreen, keysScreen, secretsScreen, teamScreen, webhooksScreen } from "./account.js";
import { button, field, h } from "./ui.js";
import { usageScreen } from "./usage.js";
import { vmDetailScreen } from "./vm-detail.js";
import { createVMScreen, vmListScreen } from "./vms.js";

const app = document.getElementById("app");
const session = createSession(window.sessionStorage);
const NAV = [
  { href: "#/vms", label: "VMs" },
  { href: "#/environments", label: "Environments", probe: "listEnvironments" },
  { href: "#/secrets", label: "Secrets" },
  { href: "#/keys", label: "API keys" },
  { href: "#/webhooks", label: "Webhooks" },
  { href: "#/team", label: "Team" },
  { href: "#/usage", label: "Usage" },
];

let cleanups = [];
let renderSeq = 0;
let me = null;

function runCleanups() {
  cleanups.forEach((fn) => fn());
  cleanups = [];
}

function signOut(message) {
  runCleanups();
  session.clear();
  me = null;
  showSignIn(message);
}

const api = createClient({
  base: config.apiBase,
  getKey: () => session.get(),
  // While signing in, the form shows the error itself.
  onUnauthorized: () => {
    if (me) signOut("This key is no longer valid. Sign in again.");
  },
});

function showSignIn(message) {
  const key = h("input", { type: "password", required: true, autocomplete: "off", spellcheck: "false", autocapitalize: "off" });
  const error = h("p", { class: "error", role: "alert", hidden: !message }, message ?? "");
  const submit = h("button", { type: "submit", class: "primary" }, "Sign in");
  const onsubmit = async (e) => {
    e.preventDefault();
    const value = key.value.trim();
    if (!validKey(value)) {
      error.textContent = "That does not look like an API key.";
      error.hidden = false;
      return;
    }
    submit.disabled = true;
    session.set(value);
    try {
      await enter();
    } catch (err) {
      session.clear();
      error.textContent = errorText(err);
      error.hidden = false;
      submit.disabled = false;
    }
  };
  document.title = "Sign in · Boat";
  app.replaceChildren(
    h(
      "main",
      { class: "signin" },
      h(
        "form",
        { class: "card form", onsubmit },
        h("h1", {}, "Boat"),
        field("API key", key, "Kept in this tab's session only. It is gone when the tab closes."),
        error,
        submit,
      ),
    ),
  );
  key.focus();
}

function shell(content) {
  const links = NAV.map((item) => {
    const link = h("a", { href: item.href, hidden: Boolean(item.probe) }, item.label);
    if (item.probe) api[item.probe]().then(() => { link.hidden = false; }).catch(() => {});
    return link;
  });
  const syncCurrent = () => {
    links.forEach((link) => {
      const here = location.hash === link.getAttribute("href") || location.hash.startsWith(`${link.getAttribute("href")}/`);
      if (here) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    });
  };
  const who = me.user?.name ?? me.user?.id ?? "";
  const insecure = location.protocol === "http:" && !["localhost", "127.0.0.1", "[::1]"].includes(location.hostname);
  app.replaceChildren(
    h("a", { class: "skip", href: "#", onclick: (e) => { e.preventDefault(); content.querySelector("h1")?.focus(); } }, "Skip to content"),
    h(
      "header",
      { class: "topbar" },
      h("strong", { class: "brand" }, "Boat"),
      h("nav", { "aria-label": "Main" }, links),
      h("div", { class: "row" }, h("span", { class: "muted who" }, `${who} · ${me.team?.name ?? ""}`), button("Sign out", () => signOut())),
    ),
    ...(insecure ? [h("p", { class: "error banner", role: "alert" }, "This page is on plain http, so your API key crosses the network unencrypted. Serve it over https.")] : []),
    content,
  );
  return syncCurrent;
}

function parseRoute(hash) {
  const parts = hash.replace(/^#\/?/, "").split("/").filter(Boolean);
  const [first = "vms", second] = parts;
  if (first === "vms" && second === "new") return { name: "new" };
  if (first === "vms" && second) return { name: "vm", id: decodeURIComponent(second) };
  return { name: first };
}

const SCREENS = {
  vms: { title: "VMs", render: vmListScreen },
  new: { title: "Create VM", render: createVMScreen },
  environments: { title: "Environments", render: environmentsScreen },
  secrets: { title: "Secrets", render: secretsScreen },
  keys: { title: "API keys", render: keysScreen },
  webhooks: { title: "Webhooks", render: webhooksScreen },
  team: { title: "Team", render: teamScreen },
  usage: { title: "Usage", render: usageScreen },
};

let content = null;
let syncNav = () => {};

async function render() {
  if (!me) return;
  runCleanups();
  renderSeq += 1;
  const seq = renderSeq;
  const route = parseRoute(location.hash);
  const ctx = {
    api,
    me,
    signOut,
    navigate: (hash) => { location.hash = hash; },
    onCleanup: (fn) => cleanups.push(fn),
  };
  syncNav();
  content.replaceChildren(h("p", { class: "muted" }, "Loading…"));
  try {
    let nodes;
    if (route.name === "vm") {
      nodes = await vmDetailScreen(ctx, route.id);
      document.title = "VM · Boat";
    } else {
      const screen = SCREENS[route.name];
      if (!screen) {
        nodes = [h("h1", { tabindex: "-1" }, "Page not found"), h("p", {}, h("a", { href: "#/vms" }, "Go to VMs"))];
        document.title = "Not found · Boat";
      } else {
        nodes = screen.render(ctx);
        document.title = `${screen.title} · Boat`;
      }
    }
    if (seq !== renderSeq) return;
    content.replaceChildren(...nodes);
    content.querySelector("h1")?.focus();
  } catch (err) {
    if (seq !== renderSeq || !me) return;
    const text = isMissing(err) ? "This VM does not exist or belongs to another team." : errorText(err);
    content.replaceChildren(h("h1", { tabindex: "-1" }, "Could not load this page"), h("p", { class: "error", role: "alert" }, text), h("p", {}, h("a", { href: "#/vms" }, "Go to VMs")));
    content.querySelector("h1")?.focus();
  }
}

async function enter() {
  me = await api.me();
  content = h("main", { id: "content" });
  syncNav = shell(content);
  await render();
}

window.addEventListener("hashchange", render);

if (session.get()) {
  enter().catch((err) => {
    if (me) return;
    session.clear();
    showSignIn(errorText(err));
  });
} else {
  showSignIn();
}
