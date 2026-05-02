# SKILL: Designing for Glimmung

> Use this when designing any Glimmung surface — admin pages, status dashboards, host detail views, run detail pages, or any new HTML mockup that should feel like it belongs in the same product.

## Voice & posture

Glimmung is a **personal infrastructure dashboard** — not enterprise SaaS. The voice is **terse, lowercase, mechanical, and a little wry**. Status pills say "free / busy / drained", not "Available / In Use / Maintenance". Buttons say "dispatch", "cancel", "register host" — never "Submit" or "Click here".

The product is named after a Philip K. Dick character; an epigraph from the novel sits in the header. **Keep that literary register**: the UI is a control panel for "summoned beings" doing work. Don't sand it down with corporate copy.

## What's already in the design system

Pull these from the project root before designing anything:

- `colors_and_type.css` — all CSS variables. Use them; never hand-pick hex.
- `assets/fonts/` — IBM Plex Sans (single family used everywhere). Real mono is reserved for `<code>` / `<pre>` only.
- `assets/favicon.svg`, `assets/glimmung-mark.svg` — wordmark glyph.
- `ui_kits/dashboard/` — the canonical dashboard recreation. Lift its `Pill`, `CountPill`, `MonoCell`, `Empty`, `Button`, `Field`, `relTime` primitives instead of re-inventing.

## Layout rules

| Rule | Detail |
|---|---|
| Sidebar | 220px, projects + workflows tree, count pills right-aligned, 2px left-border on selected. |
| Content padding | 24px top/bottom, 32px sides. |
| Section heads | `<h2>` uppercase 0.05em tracking, color `--fg-muted`. Append a one-line `.filter-hint` when scoped. |
| Tables | full-width, `border-collapse`, `tabular-nums`, 8/12 cell padding, single bottom border. IBM Plex Sans throughout; tabular-nums carries numeric alignment. |
| Empty | dashed border, italic, "No X yet." — never a hero illustration. |
| Pills | `free` (green) / `busy` (amber) / `drain` (red) / `info` (blue). Never invent a fifth without a real new state. **Pills are "console tags"** — chamfered top-left corner (6px), 2px state-color rail on the leading edge, hairline tinted edge, faint state-tinted body, mono lowercase. Never `border-radius: 999px`. |
| Buttons | **"Console plate"** — chamfered top-left corner (10px), 2px accent rail on the leading edge, hairline edge drawn via `::before` (clip-path eats the border), mono lowercase label, optional sigil. No box-shadows, no gradients. See `preview/components-buttons.html` for the canonical `.gb` styles. |
| Tabs | Mono lowercase labels, 2px bottom-rail accent on selected (same rail vocabulary as buttons / pills / sidebar / table eligible-row). |
| Count chips | `.count-pill` — same chamfer as pills but neutral (no rail by default). Selected variants get a state-color rail (success on project rows, info on workflow rows). |
| Dots | Round (`border-radius: 999px`) is reserved for *dots* only — the live-dot, tab-dot. A dot is a dot, not a pill. |

## Type rules

- **Single family**: IBM Plex Sans for the entire UI — body, IDs, hostnames, JSON, pills, buttons, KPIs, everything. Use `font-variant-numeric: tabular-nums` for numeric alignment in tables, counts, and KPIs.
- **Real mono only inside `<code>` / `<pre>`** for literal data and code blocks (token: `--font-code`). The legacy `--font-mono` token still exists but aliases to `--font-sans`, so existing rules render in sans without rewriting.
- **No emoji**, no icons inside buttons, no decorative gradients.
- **Sentence-case copy** at minimum; lowercase for action verbs (`dispatch`, `cancel`, `refresh`). Title Case is wrong.
- **Numbers always tabular** in tables and KPI strips.

## Color rules

- **Background is `--bg` (#0a0a0a) only.** Surfaces are `--surface` (#171717) and `--surface-hover` (#262626). Don't introduce a third tier.
- Accent green (`--state-success-fg`) is the primary action color — used for live indicator, primary buttons, links, focus rings, selected tabs.
- Amber means "in flight, no action needed". Red means "failed / drained / destructive confirm". Blue is informational only.

## Confirmation pattern

Destructive actions use **two-step inline confirm**, not modal:

```
[cancel]   →  [cancel?] / [keep]
```

Click 1 swaps the link for two siblings; click 2 commits. Esc / clicking "keep" reverts. Never open a modal for a single-row destructive op.

## Density & KPI strips

Above any table with > 3 row-state types, add a horizontal KPI strip showing counts (`hosts · free · busy · drained · pending · active`). Tabular-nums values, uppercase keys. This is the one redesign affordance the kit adds beyond the live product — use it everywhere it applies.

## When you must invent

If you need a new component that isn't in the kit:
1. Draw it in the same vocabulary — `--surface`, single 1px border, `--radius-sm` (4px), no shadows. **For chips/pills/buttons**, use the chamfered-top-left + leading-rail geometry (the "console plate" / "console tag" language). Never `border-radius: 999px` except on actual dots.
2. Match the existing density (8/12 padding, 14px base font).
3. Prefer adding a row to a table over a bespoke card.
4. Real focus states (`outline: none; border-color: var(--state-success-fg)`).
5. **Selection is communicated via a 2px accent rail** (left-rail on rows, bottom-rail on tabs, leading-rail on chips). This is the universal selection vocabulary — don't introduce a fill-only or outline-only selected state.

## Checklist before shipping a new Glimmung mock

- [ ] Imports `colors_and_type.css` — no hand-rolled hex
- [ ] IBM Plex Sans loaded; single family across the UI; tabular-nums on numeric chrome
- [ ] Lowercase action verbs, no emoji, no icons-in-buttons
- [ ] Pills only from {free, busy, drain, info}, with chamfer + leading-rail (no `border-radius: 999px`)
- [ ] Buttons use `.gb` console-plate styles (chamfer + leading-rail), or match that geometry
- [ ] Empty states are dashed-border italic copy
- [ ] Destructive actions are two-step inline, not modal
- [ ] Tabular nums on every numeric column
- [ ] If > 3 row states, add a KPI strip
- [ ] Tabs implemented as `<button>` (or anchors with explicit `text-decoration: none` on `:hover`/`:focus-visible`) — Chrome/Edge underline `<NavLink>` text on hover by default; the global reset in `colors_and_type.css` (`button { text-decoration: none }`) covers buttons but not anchor-based nav.

## Known browser quirks

- **Chrome/Edge underline NavLink tabs on hover.** The live product uses react-router `<NavLink>`, which renders an `<a>`. Default `a:hover { text-decoration: underline }` cascades through. Either swap to `<button>` (preferred — tabs aren't navigation in the URL sense for this UI) or add `.tab:hover, .tab:focus-visible { text-decoration: none }` explicitly on the link. Safari hides this; Chromium-based browsers don't.
