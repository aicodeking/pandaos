// Team-level screens: secrets, API keys, webhooks and delivery log, members, environments.

import { fmtTime, parseKV, parseList } from "./format.js";
import { askDialog, badge, button, field, h, run, section, showValueDialog, table, toast } from "./ui.js";

const SCOPES = ["vms:read", "vms:write", "exec", "admin"];
const SECRET_NAME = "^[A-Z_][A-Z0-9_]*$";

const submitOf = (e) => e.currentTarget.querySelector("button[type=submit]");
const danger = { class: "danger-text" };

export function secretsScreen(ctx) {
  const load = async (refresh) => {
    const secrets = await ctx.api.listSecrets();
    const name = h("input", { type: "text", required: true, pattern: SECRET_NAME, title: "UPPER_SNAKE environment variable name", autocomplete: "off", autocapitalize: "characters", spellcheck: "false" });
    const value = h("input", { type: "password", required: true, autocomplete: "new-password" });
    const save = async () => {
      const result = await ctx.api.putSecret(name.value, value.value);
      value.value = "";
      toast(result.rotated ? `Rotated ${name.value}. Running VMs keep the old value until restarted.` : `Saved ${name.value}`);
      await refresh();
    };
    const remove = async (secret) => {
      const ok = await askDialog({ title: `Delete secret ${secret.name}?`, message: "New VMs stop getting it. VMs that already have it keep their copy.", confirm: "Delete secret", danger: true });
      if (!ok) return;
      await ctx.api.deleteSecret(secret.name);
      await refresh();
    };
    return h(
      "div",
      {},
      table(
        "Team secrets",
        [
          { label: "Name", cell: (x) => h("code", {}, x.name) },
          { label: "Updated", cell: (x) => fmtTime(x.updated) },
          { label: "Actions", cell: (x) => button("Delete", (e) => run(() => remove(x), e.currentTarget), { ...danger, "aria-label": `Delete secret ${x.name}` }) },
        ],
        secrets,
        "No secrets yet.",
      ),
      h(
        "form",
        { class: "form", onsubmit: (e) => { e.preventDefault(); run(save, submitOf(e)); } },
        h("h3", {}, "Add or rotate"),
        field("Name", name),
        field("Value", value, "Write-only: the API never returns a value, here or anywhere else."),
        h("div", { class: "row" }, h("button", { type: "submit", class: "primary" }, "Save secret")),
      ),
    );
  };
  return [h("h1", { tabindex: "-1" }, "Secrets"), section("Team secrets", load)];
}

export function keysScreen(ctx) {
  const load = async (refresh) => {
    const keys = await ctx.api.listKeys();
    const name = h("input", { type: "text", required: true, autocomplete: "off" });
    const scopeBoxes = SCOPES.map((scope) => ({ scope, el: h("input", { type: "checkbox", checked: ctx.me.scopes?.includes(scope), disabled: !ctx.me.scopes?.includes(scope) }) }));
    const create = async () => {
      const key = await ctx.api.createKey({ name: name.value.trim(), scopes: scopeBoxes.filter((b) => b.el.checked).map((b) => b.scope) });
      await showValueDialog({ title: `Key ${key.name || key.id}`, message: "This token is shown once. Store it now.", label: "API key", value: key.token });
      await refresh();
    };
    const remove = async (key) => {
      const current = key.id === ctx.me.key_id;
      const ok = await askDialog({
        title: `Revoke key ${key.name || key.id}?`,
        message: current ? "This is the key you are signed in with, so revoking it signs you out." : "Anything using this key stops working at once.",
        confirm: "Revoke key",
        danger: true,
      });
      if (!ok) return;
      await ctx.api.deleteKey(key.id);
      if (current) ctx.signOut();
      else await refresh();
    };
    return h(
      "div",
      {},
      table(
        "API keys",
        [
          { label: "Name", cell: (k) => [k.name || "—", k.id === ctx.me.key_id ? h("span", { class: "tag" }, "this session") : null] },
          { label: "ID", cell: (k) => h("code", {}, k.id) },
          { label: "Scopes", cell: (k) => (k.scopes?.length ? k.scopes.join(" ") : "all of the role's scopes") },
          { label: "Created", cell: (k) => fmtTime(k.created) },
          { label: "Actions", cell: (k) => button("Revoke", (e) => run(() => remove(k), e.currentTarget), { ...danger, "aria-label": `Revoke key ${k.name || k.id}` }) },
        ],
        keys,
        "No keys.",
      ),
      h(
        "form",
        { class: "form", onsubmit: (e) => { e.preventDefault(); run(create, submitOf(e)); } },
        h("h3", {}, "New key"),
        field("Name", name),
        h("fieldset", { class: "field" }, h("legend", {}, "Scopes"), scopeBoxes.map((b) => h("label", { class: "check" }, b.el, b.scope)), h("small", { class: "hint" }, "A key cannot have more scopes than the key that creates it.")),
        h("div", { class: "row" }, h("button", { type: "submit", class: "primary" }, "Create key")),
      ),
    );
  };
  return [h("h1", { tabindex: "-1" }, "API keys"), section("API keys", load)];
}

function deliveryLog(ctx, hook, holder) {
  return async () => {
    const deliveries = await ctx.api.listDeliveries(hook.id);
    holder.replaceChildren(
      h("h3", {}, `Deliveries for ${hook.url}`),
      table(
        "Webhook deliveries",
        [
          { label: "When", cell: (d) => fmtTime(d.at) },
          { label: "Event", cell: (d) => d.event },
          { label: "Attempt", cell: (d) => d.attempt },
          { label: "Status", cell: (d) => d.status || "—" },
          { label: "Result", cell: (d) => badge(d.ok ? "delivered" : "failed") },
          { label: "Error", cell: (d) => d.error || "—" },
        ],
        deliveries,
        "No deliveries yet.",
      ),
    );
  };
}

export function webhooksScreen(ctx) {
  const load = async (refresh) => {
    const hooks = await ctx.api.listWebhooks();
    const log = h("div");
    const url = h("input", { type: "url", required: true, autocomplete: "off", placeholder: "https://example.com/hook", pattern: "https://.*", title: "An https URL" });
    const events = h("input", { type: "text", autocomplete: "off", spellcheck: "false", placeholder: "vm.* vm.stopped" });
    const create = async () => {
      const hook = await ctx.api.createWebhook({ url: url.value, events: parseList(events.value) });
      await showValueDialog({ title: "Webhook created", message: "This signing secret is shown once. Store it now.", label: "Signing secret", value: hook.secret });
      await refresh();
    };
    const remove = async (hook) => {
      const ok = await askDialog({ title: "Delete this webhook?", message: hook.url, confirm: "Delete webhook", danger: true });
      if (!ok) return;
      await ctx.api.deleteWebhook(hook.id);
      await refresh();
    };
    const test = async (hook) => {
      await ctx.api.testWebhook(hook.id);
      toast("Test event queued");
      await deliveryLog(ctx, hook, log)();
    };
    return h(
      "div",
      {},
      table(
        "Webhooks",
        [
          { label: "URL", cell: (x) => h("code", {}, x.url) },
          { label: "Events", cell: (x) => (x.events?.length ? x.events.join(" ") : "all") },
          { label: "Created", cell: (x) => fmtTime(x.created) },
          {
            label: "Actions",
            cell: (x) => h(
              "div",
              { class: "row" },
              button("Deliveries", (e) => run(deliveryLog(ctx, x, log), e.currentTarget)),
              button("Send test", (e) => run(() => test(x), e.currentTarget)),
              button("Delete", (e) => run(() => remove(x), e.currentTarget), danger),
            ),
          },
        ],
        hooks,
        "No webhooks yet.",
      ),
      log,
      h(
        "form",
        { class: "form", onsubmit: (e) => { e.preventDefault(); run(create, submitOf(e)); } },
        h("h3", {}, "New webhook"),
        field("URL", url, "Must be https and reach a public address."),
        field("Events", events, "Separated by spaces or commas. Blank means all events."),
        h("div", { class: "row" }, h("button", { type: "submit", class: "primary" }, "Create webhook")),
      ),
    );
  };
  return [h("h1", { tabindex: "-1" }, "Webhooks"), section("Webhooks", load)];
}

export function teamScreen(ctx) {
  const teamId = ctx.me.team?.id;
  const load = async (refresh) => {
    const team = await ctx.api.getTeam(teamId);
    const name = h("input", { type: "text", required: true, autocomplete: "off" });
    const role = h("select", {}, ["member", "admin", "owner"].map((r) => h("option", { value: r }, r)));
    const add = async () => {
      const added = await ctx.api.addMember(teamId, { name: name.value.trim(), role: role.value });
      await showValueDialog({ title: `Added ${name.value.trim()}`, message: "This is their first API key. It is shown once. Pass it on securely.", label: "API key", value: added.key?.token ?? "" });
      await refresh();
    };
    const remove = async (member) => {
      const ok = await askDialog({
        title: `Remove ${member.user_id}?`,
        message: "This revokes their API keys. ssh keys they registered stay in VMs, so re-key or delete those VMs to cut off access.",
        confirm: "Remove member",
        danger: true,
      });
      if (!ok) return;
      await ctx.api.removeMember(teamId, member.user_id);
      await refresh();
    };
    return h(
      "div",
      {},
      table(
        "Team members",
        [
          { label: "User", cell: (m) => [h("code", {}, m.user_id), m.user_id === ctx.me.user?.id ? h("span", { class: "tag" }, "you") : null] },
          { label: "Role", cell: (m) => m.role },
          { label: "Actions", cell: (m) => button("Remove", (e) => run(() => remove(m), e.currentTarget), { ...danger, "aria-label": `Remove ${m.user_id}` }) },
        ],
        team.members ?? [],
        "No members.",
      ),
      h(
        "form",
        { class: "form", onsubmit: (e) => { e.preventDefault(); run(add, submitOf(e)); } },
        h("h3", {}, "Add member"),
        h("div", { class: "grid2" }, field("Name", name), field("Role", role)),
        h("div", { class: "row" }, h("button", { type: "submit", class: "primary" }, "Add member")),
      ),
    );
  };
  return [h("h1", { tabindex: "-1" }, `Team ${ctx.me.team?.name ?? ""}`), section("Members", load)];
}

export function environmentsScreen(ctx) {
  const load = async (refresh) => {
    const environments = await ctx.api.listEnvironments();
    const name = h("input", { type: "text", required: true, autocomplete: "off" });
    const image = h("input", { type: "text", autocomplete: "off" });
    const size = h("input", { type: "text", autocomplete: "off" });
    const setup = h("textarea", { rows: "4", spellcheck: "false" });
    const env = h("textarea", { rows: "3", spellcheck: "false" });
    const create = async () => {
      const spec = { name: name.value.trim(), image: image.value.trim() || undefined, size: size.value.trim() || undefined, setup_script: setup.value || undefined, env: parseKV(env.value) };
      await ctx.api.createEnvironment(spec);
      await refresh();
    };
    const build = async (x) => {
      await ctx.api.buildEnvironment(x.id);
      toast(`Building ${x.name || x.id}`);
      await refresh();
    };
    const remove = async (x) => {
      const ok = await askDialog({ title: `Delete environment ${x.name || x.id}?`, message: "This cannot be undone.", confirm: "Delete environment", danger: true });
      if (!ok) return;
      await ctx.api.deleteEnvironment(x.id);
      await refresh();
    };
    return h(
      "div",
      {},
      table(
        "Environments",
        [
          { label: "Name", cell: (x) => [x.name || x.id, x.is_default ? h("span", { class: "tag" }, "default") : null] },
          { label: "Image", cell: (x) => x.image || "—" },
          { label: "Size", cell: (x) => x.size || "—" },
          { label: "Variables", cell: (x) => Object.keys(x.env ?? {}).join(" ") || "—" },
          {
            label: "Actions",
            cell: (x) => h(
              "div",
              { class: "row" },
              button("Build", (e) => run(() => build(x), e.currentTarget)),
              button("Delete", (e) => run(() => remove(x), e.currentTarget), danger),
            ),
          },
        ],
        environments,
        "No environments yet.",
      ),
      h(
        "form",
        { class: "form", onsubmit: (e) => { e.preventDefault(); run(create, submitOf(e)); } },
        h("h3", {}, "New environment"),
        field("Name", name),
        h("div", { class: "grid2" }, field("Image", image, "Optional"), field("Size", size, "Optional")),
        field("Setup script", setup, "Optional. Runs when the environment is built."),
        field("Variables", env, "One KEY=value per line. Use team secrets for anything sensitive."),
        h("div", { class: "row" }, h("button", { type: "submit", class: "primary" }, "Create environment")),
      ),
    );
  };
  return [h("h1", { tabindex: "-1" }, "Environments"), section("Environments", load)];
}
