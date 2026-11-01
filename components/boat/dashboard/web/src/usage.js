// Usage: headline numbers plus simple SVG bar charts. boatd reports totals for a
// period, not a time series, so the charts rank by size, VM and snapshot.

import { errorText } from "./api.js";
import { fmtBytes, sinceISO, usageView } from "./format.js";
import { field, h, s, table } from "./ui.js";

const RANGES = [7, 30, 90];
const ROW_HEIGHT = 40;
const BAR_HEIGHT = 8;
const LABEL_BASELINE = 16;
const BAR_Y = 24;

const number = (value) => value.toLocaleString(undefined, { maximumFractionDigits: 1 });

// Sized in percent with no viewBox, so bars stretch to the container and text keeps its real size at phone width.
function barChart(title, unit, rows) {
  if (!rows.length) return h("section", { class: "chart" }, h("h3", {}, title), h("p", { class: "muted" }, "No usage in this period."));
  const max = Math.max(...rows.map((r) => r.value), 0) || 1;
  const summary = rows.map((r) => `${r.label}: ${number(r.value)} ${unit}`).join(", ");
  const bars = rows.map((row, i) => {
    const y = i * ROW_HEIGHT;
    return s(
      "g",
      {},
      s("title", {}, `${row.label}: ${number(row.value)} ${unit}`),
      s("text", { x: "0", y: y + LABEL_BASELINE, class: "chart-label" }, row.label),
      s("text", { x: "100%", y: y + LABEL_BASELINE, "text-anchor": "end", class: "chart-value" }, `${number(row.value)} ${unit}`),
      s("rect", { x: "0", y: y + BAR_Y, width: "100%", height: BAR_HEIGHT, rx: "4", class: "chart-track" }),
      s("rect", { x: "0", y: y + BAR_Y, width: `${Math.max((row.value / max) * 100, 0.5)}%`, height: BAR_HEIGHT, rx: "4", class: "chart-bar" }),
    );
  });
  return h(
    "section",
    { class: "chart" },
    h("h3", {}, title),
    s("svg", { width: "100%", height: rows.length * ROW_HEIGHT, role: "img", "aria-label": `${title}. ${summary}` }, bars),
    h(
      "details",
      {},
      h("summary", {}, "Show as table"),
      table(title, [{ label: "Item", cell: (r) => r.label }, { label: `Value (${unit})`, cell: (r) => number(r.value) }], rows),
    ),
  );
}

export function usageScreen(ctx) {
  const holder = h("div", { "aria-live": "polite" }, h("p", { class: "muted" }, "Loading…"));
  const range = h("select", {}, RANGES.map((d) => h("option", { value: String(d) }, `Last ${d} days`)));
  range.value = "30";

  const load = async () => {
    try {
      // VM names are a nicety on the chart labels. Usage still renders with ids without them.
      const [report, vms] = await Promise.all([ctx.api.usage(sinceISO(Number(range.value))), ctx.api.listVMs().catch(() => [])]);
      const names = Object.fromEntries(vms.map((vm) => [vm.id, vm.name]));
      const view = usageView(report, names);
      const stat = (label, value) => h("div", { class: "stat" }, h("div", { class: "stat-value" }, value), h("div", { class: "muted" }, label));
      holder.replaceChildren(
        h("div", { class: "stats" }, stat("VM hours", number(view.vmHours)), stat("Snapshot days", number(view.snapshotDays)), stat("Storage committed", fmtBytes(view.storageBytes))),
        barChart("VM hours by size", "h", view.bySize),
        barChart("VM hours by VM", "h", view.byVM),
        barChart("Snapshot days by snapshot", "d", view.bySnapshot),
      );
    } catch (err) {
      holder.replaceChildren(h("p", { class: "error", role: "alert" }, errorText(err)));
    }
  };
  range.addEventListener("change", load);
  load();

  return [h("h1", { tabindex: "-1" }, "Usage"), h("div", { class: "card form" }, field("Period", range)), holder];
}
