# Glimmung Dashboard — UI Kit

A pixel-faithful + lightly-redesigned recreation of the Glimmung dashboard, with the visual language of `frontend/src/index.css` preserved and small frontend-elegance improvements layered on top:

- **Tighter information hierarchy** — header consolidated, section heads paired with a one-line summary, KPI strip above the hosts table.
- **Better mono/sans rhythm** — IDs, hosts, capability JSON in mono; everything else in sans.
- **Real focus states** — green outline on tabs and inputs (the live UI has none on tabs).
- **Density preserved** — same 14px base, same paddings, same pill semantics.

## Files

- `index.html` — runnable dashboard (capacity / issues / prs tabs, sidebar, admin reveal).
- `Dashboard.jsx` — top-level layout, header, tabs, sidebar.
- `CapacityView.jsx` — hosts + pending + active tables, KPI strip.
- `IssuesView.jsx` — issues table with dispatch action.
- `PrsView.jsx` — PRs table.
- `AdminPanel.jsx` — register host / project / workflow forms.
- `primitives.jsx` — `Pill`, `CountPill`, `EyebrowKey`, `MonoCell`, `Empty`, `LiveDot`, `Button`, `Input`.
- `data.js` — fixture data (hosts, leases, issues, prs).

## Notes on what changed vs the live product

| Change | Why |
|---|---|
| Header packs wordmark + connection + epigraph + nav into one row at consistent baseline | The original mixed `align-items: baseline` with auth controls that have their own height — alignment drifts. |
| KPI strip ("3 hosts · 1 free · 2 busy · 1 pending") above hosts table | Density boost — the same info was visible only by scanning rows. |
| Tab active state uses both underline AND foreground (was foreground-only on hover) | Made non-hover selection more obvious. |
| Two-step cancel kept as `cancel` → `cancel? / keep` | This pattern is good — preserved exactly. |
| Sidebar count pill uses tabular-nums everywhere (not just rows) | Alignment fix. |

Nothing was removed.
