// AI proxy credentials, proxy toggle and lifetime token usage.

import { aiUsageTotals, fmtTime } from "./format.js";
import { askDialog, badge, button, field, h, run, section, table, toast } from "./ui.js";

export const AI_PROVIDERS = ["anthropic", "openai", "openrouter", "deepseek", "mistral", "moonshot", "xai"];

const submitOf = (e) => e.currentTarget.querySelector("button[type=submit]");
const count = (n) => Number(n ?? 0).toLocaleString();

function proxySection(ctx) {
  return section("AI proxy", async (refresh) => {
    const state = await ctx.api.getAIProxy();
    const toggle = async () => {
      const next = await ctx.api.setAIProxy(!state.enabled);
      toast(next.enabled ? "AI proxy enabled" : "AI proxy disabled");
      await refresh();
    };
    return h(
      "div",
      { class: "form" },
      h(
        "div",
        { class: "row spread" },
        h("div", { class: "row" }, badge(state.enabled ? "running" : "stopped"), h("span", {}, state.enabled ? "Enabled" : "Disabled")),
        button(state.enabled ? "Disable proxy" : "Enable proxy", (e) => run(toggle, e.currentTarget), { disabled: !state.available }),
      ),
      !state.available ? h("p", { class: "muted" }, "This server is not configured with an AI proxy upstream (-ai-proxy-url).") : null,
    );
  });
}

function credentialsSection(ctx) {
  return section("AI credentials", async (refresh) => {
    const creds = await ctx.api.listAICredentials();
    const provider = h("select", {}, AI_PROVIDERS.map((p) => h("option", { value: p }, p)));
    const value = h("input", { type: "password", required: true, autocomplete: "new-password" });
    const save = async () => {
      await ctx.api.putAICredential(provider.value, value.value);
      value.value = "";
      toast(`Saved ${provider.value} key`);
      await refresh();
    };
    const remove = async (cred) => {
      const ok = await askDialog({
        title: `Delete ${cred.provider} key?`,
        message: "The host removes this provider key and VMs stop reaching it through the AI proxy.",
        confirm: "Delete key",
        danger: true,
      });
      if (!ok) return;
      await ctx.api.deleteAICredential(cred.provider);
      toast(`Deleted ${cred.provider} key`);
      await refresh();
    };
    return h(
      "div",
      {},
      table(
        "AI provider credentials",
        [
          { label: "Provider", cell: (c) => h("code", {}, c.provider) },
          { label: "Key", cell: (c) => h("code", {}, c.last4 ? `••••${c.last4}` : "—") },
          { label: "Updated", cell: (c) => fmtTime(c.updated) },
          {
            label: "Actions",
            cell: (c) => button("Delete", (e) => run(() => remove(c), e.currentTarget), { class: "danger-text", "aria-label": `Delete ${c.provider} key` }),
          },
        ],
        creds,
        "No AI provider keys configured yet.",
      ),
      h(
        "form",
        { class: "form", onsubmit: (e) => { e.preventDefault(); run(save, submitOf(e)); } },
        h("h3", {}, "Add or update provider key"),
        h("div", { class: "grid2" }, field("Provider", provider), field("API key", value, "Stays on the host; only the last 4 characters are ever returned.")),
        h("div", { class: "row" }, h("button", { type: "submit", class: "primary" }, "Save provider key")),
      ),
    );
  });
}

function aiUsageSection(ctx) {
  return section("AI usage", async (refresh) => {
    const [report, vms] = await Promise.all([ctx.api.aiUsage(), ctx.api.listVMs().catch(() => [])]);
    const names = Object.fromEntries(vms.map((vm) => [vm.id, vm.name]));
    const { rows, totals } = aiUsageTotals(report);
    const stat = (label, val) => h("div", { class: "stat" }, h("div", { class: "stat-value" }, val), h("div", { class: "muted" }, label));
    return h(
      "div",
      {},
      h(
        "div",
        { class: "stats" },
        stat("Requests", count(totals.requests)),
        stat("Input tokens", count(totals.input_tokens)),
        stat("Output tokens", count(totals.output_tokens)),
      ),
      h("div", { class: "row" }, button("Refresh", refresh)),
      table(
        "AI proxy usage",
        [
          {
            label: "VM",
            cell: (r) => (r.vm_id ? h("a", { href: `#/vms/${encodeURIComponent(r.vm_id)}` }, names[r.vm_id] || r.vm_id) : "—"),
          },
          { label: "Provider", cell: (r) => r.provider || "—" },
          { label: "Model", cell: (r) => h("code", {}, r.model || "—") },
          { label: "Requests", cell: (r) => count(r.requests) },
          { label: "Input tokens", cell: (r) => count(r.input_tokens) },
          { label: "Output tokens", cell: (r) => count(r.output_tokens) },
          { label: "Last used", cell: (r) => fmtTime(r.updated) },
        ],
        rows,
        "No AI proxy usage yet.",
      ),
    );
  });
}

export function aiScreen(ctx) {
  return [
    h("h1", { tabindex: "-1" }, "AI"),
    proxySection(ctx),
    credentialsSection(ctx),
    aiUsageSection(ctx),
  ];
}
