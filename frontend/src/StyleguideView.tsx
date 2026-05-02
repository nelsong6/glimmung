/**
 * Styleguide route (#85) — visual catalog of every component glimmung's
 * frontend ships, in every variant. Lives at /_styleguide. The platform
 * contract is documented in docs/styleguide-contract.md; the
 * agent-run.yml validation step curls this route and fails the run if
 * it 404s.
 *
 * Hand-rolled JSX continuing design-system/ui_kits/dashboard/. No
 * interactivity in v1 — visual catalog only.
 */
import { useState } from "react";
import "./index.css";

export function StyleguideView() {
  return (
    <div className="layout" style={{ display: "block" }}>
      <main className="content" style={{ marginLeft: 0 }}>
        <header>
          <div className="header-left">
            <div className="header-title">
              <h1>glimmung</h1>
              <span className="connection live">styleguide</span>
            </div>
            <div className="epigraph">
              Visual catalog of every component this frontend ships. See
              {' '}<code>docs/styleguide-contract.md</code> and{' '}
              <code>design-system/SKILL.md</code> for the rules these follow.
            </div>
          </div>
        </header>

        <Section title="pills" caption="Status tags. Only four states: free, busy, drain, info. Chamfered top-left, leading rail, mono lowercase. Never border-radius: 999px.">
          <Row>
            <span className="pill free">free</span>
            <span className="pill busy">busy</span>
            <span className="pill drain">drain</span>
            <span className="pill info">info</span>
          </Row>
        </Section>

        <Section title="count pills" caption="Neutral chamfered chip used in the sidebar. State-color rail variants when selected.">
          <Row>
            <span className="count-pill">3</span>
            <span className="count-pill">12</span>
            <span className="count-pill selected">7</span>
          </Row>
        </Section>

        <Section title="buttons" caption="Console-plate geometry: chamfered top-left (10px), 2px leading rail, hairline edge via ::before. Lowercase mono labels.">
          <Row>
            <button type="button" className="gb primary"><span className="sigil">›</span><span className="label">dispatch</span></button>
            <button type="button" className="gb"><span className="label">cancel</span></button>
            <button type="button" className="gb danger"><span className="label">drain</span></button>
            <button type="button" className="gb quiet"><span className="label">refresh</span></button>
            <button type="button" className="gb active"><span className="label">active</span></button>
            <button type="button" className="gb" disabled><span className="label">disabled</span></button>
          </Row>
          <Row>
            <button type="button" className="gb sm"><span className="sigil">∷</span><span className="label">admin</span></button>
            <button type="button" className="gb sm quiet"><span className="label">sign out</span></button>
          </Row>
          <Row>
            <button type="button" className="gb ghost">ghost</button>
            <button type="button" className="link">edit</button>
            <button type="button" className="link danger-text">cancel?</button>
          </Row>
        </Section>

        <Section title="two-step inline confirm" caption="Destructive ops never use modals. First click swaps the link for [cancel?] / [keep]; second click commits. Esc / keep reverts.">
          <ConfirmDemo />
        </Section>

        <Section title="tabs" caption="Mono lowercase labels, 2px bottom-rail accent on selected. Same rail vocabulary as buttons / pills / sidebar.">
          <div className="tabs" role="tablist">
            <button type="button" role="tab" className="tab selected">description</button>
            <button type="button" role="tab" className="tab">in progress<span className="tab-dot" aria-label="active" /></button>
            <button type="button" role="tab" className="tab">lineage</button>
          </div>
        </Section>

        <Section title="KPI strip" caption="Above any table with > 3 row-state types. Tabular-nums values, uppercase keys.">
          <div className="kpi-strip">
            <div className="kpi"><span className="k">hosts</span><span className="v">8</span></div>
            <div className="kpi"><span className="k">free</span><span className="v green">5</span></div>
            <div className="kpi"><span className="k">busy</span><span className="v amber">2</span></div>
            <div className="kpi"><span className="k">drained</span><span className="v red">1</span></div>
            <div className="kpi"><span className="k">pending</span><span className="v">3</span></div>
            <div className="kpi"><span className="k">active</span><span className="v">2</span></div>
          </div>
        </Section>

        <Section title="empty" caption="Dashed border, italic copy. Never a hero illustration.">
          <div className="empty">No leases waiting.</div>
        </Section>

        <Section title="filter hint" caption="One-line scope clue appended to scoped section heads.">
          <h2 style={{ marginTop: 0 }}>
            Pending queue (3)
            <span className="filter-hint"> — filtered to glimmung.agent-run</span>
          </h2>
        </Section>

        <Section title="project info" caption="Key/value rows used on detail pages. Mono values for IDs and refs.">
          <div className="project-info">
            <div className="row"><span className="key">project</span><span className="val mono">glimmung</span></div>
            <div className="row"><span className="key">github</span><span className="val mono">nelsong6/glimmung</span></div>
            <div className="row"><span className="key">workflow</span><span className="val mono">agent-run</span></div>
            <div className="row"><span className="key">requires</span><span className="val mono">{`{"role":"slot"}`}</span></div>
          </div>
        </Section>

        <Section title="table" caption="Full-width, single bottom border, tabular-nums, 8/12 cell padding. Selected rows get a 2px accent rail (here: eligible).">
          <table>
            <thead>
              <tr>
                <th>Name</th><th>State</th><th>Lease</th><th>Last heartbeat</th>
              </tr>
            </thead>
            <tbody>
              <tr className="eligible">
                <td className="mono">slot-1</td>
                <td><span className="pill free">free</span></td>
                <td className="mono dim">—</td>
                <td className="mono dim">3s ago</td>
              </tr>
              <tr>
                <td className="mono">slot-2</td>
                <td><span className="pill busy">busy</span></td>
                <td className="mono dim">01HXAZ…</td>
                <td className="mono dim">1s ago</td>
              </tr>
              <tr>
                <td className="mono">slot-3</td>
                <td><span className="pill drain">drained</span></td>
                <td className="mono dim">—</td>
                <td className="mono dim">12m ago</td>
              </tr>
            </tbody>
          </table>
        </Section>

        <Section title="attempt card" caption="Per-attempt summary. Status pill + phase + elapsed; key/value body for cost, gh run link, decision; verification reasons as a muted list.">
          <div className="attempt-list">
            <div className="attempt-card">
              <div className="attempt-card-head">
                <strong>attempt 0</strong>
                <span className="pill free">pass</span>
                <span className="dim mono">agent</span>
                <span className="dim mono">ran 4m 12s</span>
              </div>
              <div className="attempt-card-body">
                <div><span className="key">dispatched</span> <span className="mono">5/2/2026, 14:01</span></div>
                <div><span className="key">completed</span> <span className="mono">5/2/2026, 14:05</span></div>
                <div><span className="key">workflow</span> <span className="mono">agent-run.yml</span></div>
                <div><span className="key">cost</span> <span className="mono">$0.0421</span></div>
                <div><span className="key">decision</span> <span className="mono">ADVANCE</span></div>
              </div>
            </div>
            <div className="attempt-card running">
              <div className="attempt-card-head">
                <strong>attempt 1</strong>
                <span className="pill busy">running</span>
                <span className="dim mono">verify</span>
                <span className="dim mono">42s elapsed</span>
              </div>
              <div className="attempt-card-body">
                <div><span className="key">dispatched</span> <span className="mono">5/2/2026, 14:06</span></div>
                <div><span className="key">workflow</span> <span className="mono">verify.yml</span></div>
              </div>
            </div>
            <div className="attempt-card">
              <div className="attempt-card-head">
                <strong>attempt 2</strong>
                <span className="pill drain">fail</span>
                <span className="dim mono">verify</span>
                <span className="dim mono">ran 1m 8s</span>
              </div>
              <div className="attempt-card-body">
                <div><span className="key">cost</span> <span className="mono">$0.0083</span></div>
                <div><span className="key">decision</span> <span className="mono">RETRY</span></div>
              </div>
              <ul className="attempt-card-reasons">
                <li className="mono dim">curl /_styleguide returned 404</li>
                <li className="mono dim">test_styleguide_contract.py::test_route failed</li>
              </ul>
            </div>
          </div>
        </Section>

        <Section title="run panel" caption="Wraps a run's header + meta + attempt list. Live-dot when the run is in flight.">
          <div className="run-panel">
            <div className="run-panel-header">
              <div>
                <span className="pill busy">in_progress</span>
                <span className="mono dim" style={{ marginLeft: "0.5rem" }}>run 01HXAZ…</span>
                <span className="live-dot" aria-label="live" />
              </div>
              <span className="dim mono">started 5/2/2026, 14:01</span>
            </div>
            <div className="run-panel-meta">
              <div><span className="key">workflow</span> <span className="mono">agent-run</span></div>
              <div><span className="key">trigger</span> <span className="mono">issues.opened</span></div>
              <div><span className="key">attempts</span> <span className="mono">2</span></div>
              <div><span className="key">cost</span> <span className="mono">$0.0504</span></div>
            </div>
          </div>
        </Section>

        <Section title="connection chip" caption="Header-right indicator. Tracks the SSE state.">
          <Row>
            <span className="connection live">live</span>
            <span className="connection stale">stale</span>
            <span className="connection dead">dead</span>
          </Row>
        </Section>

        <Section title="sidebar rows" caption="Project + workflow tree. 2px left-border on selected, count chips right-aligned.">
          <aside className="sidebar" style={{ position: "static", height: "auto", maxWidth: 220 }}>
            <div className="sidebar-title">Projects</div>
            <button type="button" className="project-row"><span className="name">All</span><span className="count">12</span></button>
            <div className="project-group">
              <button type="button" className="project-row selected"><span className="name">glimmung</span><span className="count">7</span></button>
              <button type="button" className="workflow-row selected"><span className="name">agent-run</span><span className="count">3</span></button>
              <button type="button" className="workflow-row"><span className="name">verify</span><span className="count">1</span></button>
            </div>
            <div className="project-group">
              <button type="button" className="project-row"><span className="name">ambience</span><span className="count">3</span></button>
            </div>
          </aside>
        </Section>

        <Section title="form fields" caption="Admin / edit forms. Single 1px border, focus ring uses --state-success-fg.">
          <form className="admin-form" onSubmit={(e) => e.preventDefault()}>
            <label>
              <span>Title</span>
              <input defaultValue="agent run" />
            </label>
            <label>
              <span>Body</span>
              <textarea rows={3} defaultValue="context body…" />
            </label>
            <label>
              <span>Labels (comma-separated)</span>
              <input className="mono" defaultValue="ui, frontend" />
            </label>
            <label>
              <span>State</span>
              <select defaultValue="open">
                <option value="open">open</option>
                <option value="closed">closed</option>
              </select>
            </label>
            <label className="checkbox">
              <input type="checkbox" defaultChecked />
              <span>verify enabled</span>
            </label>
            <Row>
              <button type="submit" className="gb primary"><span className="label">save</span></button>
              <button type="button" className="link">cancel</button>
            </Row>
          </form>
        </Section>

        <Section title="user identity cluster" caption="Header-right widget when signed in. user-dot + handle, with admin toggle and sign-out.">
          <div className="user-cluster">
            <button type="button" className="gb sm"><span className="sigil">∷</span><span className="label">admin</span></button>
            <span className="user-id">
              <span className="user-dot" />
              <span className="user-handle">curator@example.com</span>
            </span>
            <button type="button" className="gb sm quiet"><span className="label">sign out</span></button>
          </div>
        </Section>

        <Section title="typography" caption="IBM Plex Sans across the UI. Real mono only inside <code> / <pre>.">
          <h1>h1 — page title</h1>
          <h2>h2 — section head</h2>
          <p>Body copy in IBM Plex Sans. Lowercase action verbs (dispatch, cancel, refresh). Sentence-case at minimum; never Title Case. <span className="dim">dim copy uses --fg-muted.</span></p>
          <p><code>code: monospace, used for IDs, paths, refs.</code></p>
          <pre>{`pre block:
{
  "workflow": "agent-run",
  "cost_usd": 0.0421
}`}</pre>
        </Section>
      </main>
    </div>
  );
}

function Section({
  title,
  caption,
  children,
}: {
  title: string;
  caption?: string;
  children: React.ReactNode;
}) {
  return (
    <section style={{ marginTop: "1.75rem" }}>
      <h2>{title}</h2>
      {caption && <p className="dim" style={{ marginTop: "-0.25rem" }}>{caption}</p>}
      {children}
    </section>
  );
}

function Row({ children }: { children: React.ReactNode }) {
  return (
    <div style={{ display: "flex", gap: "0.5rem", flexWrap: "wrap", alignItems: "center", margin: "0.5rem 0" }}>
      {children}
    </div>
  );
}

function ConfirmDemo() {
  const [armed, setArmed] = useState(false);
  return (
    <Row>
      {armed ? (
        <span className="confirm">
          <button type="button" className="link danger-text" onClick={() => setArmed(false)}>cancel?</button>
          <span className="sep">/</span>
          <button type="button" className="link" onClick={() => setArmed(false)}>keep</button>
        </span>
      ) : (
        <button type="button" className="link" onClick={() => setArmed(true)}>cancel</button>
      )}
    </Row>
  );
}
