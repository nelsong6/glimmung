/**
 * Design portfolio route. This keeps the /_styleguide contract alive while
 * presenting the system as reviewable specimens instead of a long static page.
 */
import { useState } from "react";
import "./index.css";

type ReviewState = "unset" | "reviewed" | "needs-review";
type PortfolioTab = "system" | "files";

type PortfolioItem = {
  id: string;
  title: string;
  caption: string;
  initialOpen?: boolean;
  render: () => React.ReactNode;
};

const DESIGN_SYSTEM_ITEMS: PortfolioItem[] = [
  {
    id: "buttons",
    title: "Buttons",
    caption: "Console-plate actions, quiet controls, and inline links",
    render: () => (
      <Specimen title="buttons - console plate">
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
          <button type="button" className="gb ghost">ghost</button>
          <button type="button" className="link">edit</button>
          <button type="button" className="link danger-text">cancel?</button>
        </Row>
      </Specimen>
    ),
  },
  {
    id: "colors-borders",
    title: "Colors - Borders",
    caption: "Shared hairline, rail, and chamfer vocabulary",
    render: () => (
      <Specimen title="colors - borders">
        <div className="portfolio-swatch-grid">
          <Swatch label="border" value="var(--border)" />
          <Swatch label="border strong" value="var(--border-strong)" />
          <Swatch label="success rail" value="var(--state-success-fg)" />
          <Swatch label="busy rail" value="var(--state-busy-fg)" />
          <Swatch label="danger rail" value="var(--state-danger-fg)" />
          <Swatch label="info rail" value="var(--state-info-fg)" />
        </div>
      </Specimen>
    ),
  },
  {
    id: "colors-foreground",
    title: "Colors - Foreground",
    caption: "Primary, muted, dim, and monospace reading colors",
    render: () => (
      <Specimen title="colors - foreground">
        <div className="portfolio-type-stack">
          <p>Primary foreground text for dense operational surfaces.</p>
          <p className="dim">Dim body copy for metadata, hints, and secondary context.</p>
          <p className="mono">01KQSXNCNBWHTSFSC6DRHG87H2</p>
          <p><code>code: refs, paths, ids, and compact JSON</code></p>
        </div>
      </Specimen>
    ),
  },
  {
    id: "colors-semantic",
    title: "Colors - Semantic",
    caption: "Success, busy, danger, and info state colors",
    render: () => (
      <Specimen title="colors - semantic">
        <Row>
          <span className="pill free">free</span>
          <span className="pill busy">busy</span>
          <span className="pill drain">drained</span>
          <span className="pill info">issue-agent</span>
        </Row>
      </Specimen>
    ),
  },
  {
    id: "colors-surfaces",
    title: "Colors - Surfaces",
    caption: "Deep page, panel, hover, and inset surfaces",
    render: () => (
      <Specimen title="colors - surfaces">
        <div className="portfolio-surface-row">
          <div className="portfolio-surface bg">bg</div>
          <div className="portfolio-surface deep">deep</div>
          <div className="portfolio-surface panel">surface</div>
          <div className="portfolio-surface hover">hover</div>
        </div>
      </Specimen>
    ),
  },
  {
    id: "form-inputs",
    title: "Form inputs",
    caption: "Admin fields, selects, textareas, and toggles",
    render: () => (
      <Specimen title="form inputs">
        <form className="admin-form portfolio-form" onSubmit={(e) => e.preventDefault()}>
          <label>
            <span>Title</span>
            <input defaultValue="agent run" />
          </label>
          <label>
            <span>Body</span>
            <textarea rows={3} defaultValue="context body..." />
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
        </form>
      </Specimen>
    ),
  },
  {
    id: "lease-actions",
    title: "Lease Row Actions",
    caption: "Inline destructive confirmation without modal interruption",
    render: () => (
      <Specimen title="lease row actions">
        <table>
          <tbody>
            <tr>
              <td className="mono">slot-1</td>
              <td><span className="pill busy">busy</span></td>
              <td><ConfirmDemo /></td>
            </tr>
          </tbody>
        </table>
      </Specimen>
    ),
  },
  {
    id: "pills",
    title: "Pills",
    caption: "Status badges · count pill · live-dot pulse",
    initialOpen: true,
    render: () => (
      <Specimen title="pills - console tag">
        <div className="tag-matrix">
          <span className="matrix-key">host</span>
          <Row>
            <span className="pill free">free</span>
            <span className="pill busy">busy</span>
            <span className="pill drain">drained</span>
          </Row>
          <span className="matrix-key">stream</span>
          <Row>
            <span className="connection live">live</span>
            <span className="connection stale">stale</span>
            <span className="connection dead">dead</span>
          </Row>
          <span className="matrix-key">run state</span>
          <Row>
            <span className="pill free">passed</span>
            <span className="pill busy">in_progress</span>
            <span className="pill drain">aborted</span>
            <span className="pill info">issue-agent</span>
          </Row>
          <span className="matrix-key">atoms</span>
          <Row>
            <span className="live-dot" aria-label="live" />
            <span className="dim">live-dot · pulse 1.6s</span>
            <span className="count-pill">12</span>
            <span className="dim">count-pill · neutral, no rail</span>
          </Row>
        </div>
      </Specimen>
    ),
  },
  {
    id: "spacing-radius",
    title: "Spacing & radius",
    caption: "Dense rhythm, small radius, and chamfered fixed-format pieces",
    render: () => (
      <Specimen title="spacing & radius">
        <div className="portfolio-spacing-demo">
          <div className="empty">No leases waiting.</div>
          <div className="project-info">
            <div className="row"><span className="key">project</span><span className="val mono">glimmung</span></div>
            <div className="row"><span className="key">workflow</span><span className="val mono">agent-run</span></div>
          </div>
        </div>
      </Specimen>
    ),
  },
  {
    id: "table",
    title: "Table",
    caption: "Operational rows, tabular numbers, and selected row rails",
    render: () => (
      <Specimen title="table">
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
              <td className="mono dim">-</td>
              <td className="mono dim">3s ago</td>
            </tr>
            <tr>
              <td className="mono">slot-2</td>
              <td><span className="pill busy">busy</span></td>
              <td className="mono dim">01HXAZ...</td>
              <td className="mono dim">1s ago</td>
            </tr>
            <tr>
              <td className="mono">slot-3</td>
              <td><span className="pill drain">drained</span></td>
              <td className="mono dim">-</td>
              <td className="mono dim">12m ago</td>
            </tr>
          </tbody>
        </table>
      </Specimen>
    ),
  },
  {
    id: "tabs-header",
    title: "Header & local tabs",
    caption: "Page header, connection chip, issue-local tabs, and user identity cluster",
    render: () => (
      <Specimen title="header & local tabs">
        <header className="portfolio-inner-header">
          <div className="header-left">
            <div className="header-title">
              <h1>glimmung</h1>
              <span className="connection live">live</span>
            </div>
            <div className="epigraph">Operational dashboard for scarce agent capacity.</div>
          </div>
          <div className="user-cluster">
            <button type="button" className="gb sm"><span className="sigil">∷</span><span className="label">admin</span></button>
            <span className="user-id">
              <span className="user-dot" />
              <span className="user-handle">curator@example.com</span>
            </span>
          </div>
        </header>
        <div className="tabs" role="tablist">
          <button type="button" role="tab" className="tab selected">issue</button>
          <button type="button" role="tab" className="tab">run<span className="tab-dot" aria-label="active" /></button>
          <button type="button" role="tab" className="tab">runs</button>
          <button type="button" role="tab" className="tab">touchpoint</button>
        </div>
      </Specimen>
    ),
  },
];

const DESIGN_FILE_ITEMS: PortfolioItem[] = [
  {
    id: "capacity-view",
    title: "Front page - Capacity",
    caption: "The dashboard landing surface: project scope, host capacity, queue, and active work",
    initialOpen: true,
    render: () => (
      <Specimen title="front page - capacity">
        <div className="frontpage-frame">
          <div className="frontpage-main">
            <header className="frontpage-header">
              <div className="header-left">
                <div className="header-title">
                  <h1>glimmung</h1>
                  <span className="connection live">live</span>
                </div>
                <div className="epigraph">glimmung / agent-run capacity and work queue</div>
              </div>
              <div className="user-cluster">
                <button type="button" className="gb sm"><span className="sigil">∷</span><span className="label">admin</span></button>
                <span className="user-id"><span className="user-dot" /><span className="user-handle">curator@example.com</span></span>
              </div>
            </header>
            <div className="kpi-strip frontpage-kpis">
              <div className="kpi"><span className="k">hosts</span><span className="v">8</span></div>
              <div className="kpi"><span className="k">free</span><span className="v green">5</span></div>
              <div className="kpi"><span className="k">busy</span><span className="v amber">2</span></div>
              <div className="kpi"><span className="k">drained</span><span className="v red">1</span></div>
              <div className="kpi"><span className="k">pending</span><span className="v">3</span></div>
            </div>
            <div className="frontpage-grid">
              <section>
                <h2>Hosts</h2>
                <table>
                  <thead>
                    <tr><th>Name</th><th>State</th><th>Lease</th></tr>
                  </thead>
                  <tbody>
                    <tr className="eligible"><td className="mono">glimmung-slot-1</td><td><span className="pill free">free</span></td><td className="mono dim">-</td></tr>
                    <tr><td className="mono">glimmung-slot-2</td><td><span className="pill busy">busy</span></td><td className="mono dim">01KQWF...</td></tr>
                    <tr><td className="mono">glimmung-slot-3</td><td><span className="pill drain">drained</span></td><td className="mono dim">-</td></tr>
                  </tbody>
                </table>
              </section>
              <section>
                <h2>Pending queue</h2>
                <table>
                  <thead>
                    <tr><th>Lease</th><th>Issue</th><th>Requested</th></tr>
                  </thead>
                  <tbody>
                    <tr><td className="mono">01KQWX...</td><td>design portfolio bootstrap</td><td className="mono dim">14s ago</td></tr>
                    <tr><td className="mono">01KQWY...</td><td>ui package bridge</td><td className="mono dim">2m ago</td></tr>
                  </tbody>
                </table>
              </section>
            </div>
            <section>
              <h2>Active work</h2>
              <div className="run-panel">
                <div className="run-panel-header">
                  <div>
                    <span className="pill busy">in_progress</span>
                    <span className="mono dim" style={{ marginLeft: "0.5rem" }}>run 01HXAZ...</span>
                    <span className="live-dot" aria-label="live" />
                  </div>
                  <span className="dim mono">started 5/2/2026, 14:01</span>
                </div>
                <div className="run-panel-meta">
                  <div><span className="key">workflow</span> <span className="mono">agent-run</span></div>
                  <div><span className="key">trigger</span> <span className="mono">issues.opened</span></div>
                  <div><span className="key">cycles</span> <span className="mono">2</span></div>
                  <div><span className="key">cost</span> <span className="mono">$0.0504</span></div>
                </div>
              </div>
            </section>
          </div>
        </div>
      </Specimen>
    ),
  },
  {
    id: "project-workspace",
    title: "Project workspace",
    caption: "A project-specific page: repo context, workflows, current work, and scoped issues",
    render: () => (
      <Specimen title="project workspace">
        <div className="project-info">
          <div className="row"><span className="key">project</span><span className="val mono">glimmung</span></div>
          <div className="row"><span className="key">github</span><span className="val mono">nelsong6/glimmung</span></div>
          <div className="row"><span className="key">work</span><span className="val mono">1 active / 2 pending</span></div>
          <div className="row"><span className="key">hosts</span><span className="val mono">nelsonpc</span></div>
        </div>
        <h2>Workflows</h2>
        <table>
          <thead>
            <tr><th>Name</th><th>File</th><th>Trigger</th><th>Work</th></tr>
          </thead>
          <tbody>
            <tr><td>issue-agent</td><td className="mono dim">issue-agent.yaml@main</td><td className="mono dim">issue-agent</td><td className="mono dim">1 active / 1 pending</td></tr>
            <tr><td>portfolio-agent</td><td className="mono dim">design-portfolio.yaml@main</td><td className="mono dim">design-portfolio</td><td className="mono dim">0 active / 1 pending</td></tr>
          </tbody>
        </table>
        <h2>Issues</h2>
        <table>
          <thead>
            <tr><th>#</th><th>Title</th><th>Run</th><th>Touchpoint</th></tr>
          </thead>
          <tbody>
            <tr><td className="mono">206</td><td>Display native run graph and step-level execution</td><td><span className="pill busy">in_progress</span></td><td className="mono dim">PR #218</td></tr>
            <tr><td className="mono">217</td><td>Generate reusable design portfolio from an existing repo</td><td><span className="pill info">review_required</span></td><td className="mono dim">PR #216</td></tr>
          </tbody>
        </table>
      </Specimen>
    ),
  },
  {
    id: "attempt-cards",
    title: "Issue run details",
    caption: "Attempt cards, state transitions, cost, and verification reasons",
    render: () => (
      <Specimen title="issue run details">
        <div className="attempt-list">
          <div className="attempt-card">
            <div className="attempt-card-head">
              <strong>attempt 0</strong>
              <span className="pill free">pass</span>
              <span className="dim mono">agent</span>
              <span className="dim mono">ran 4m 12s</span>
            </div>
            <div className="attempt-card-body">
              <div><span className="key">workflow</span> <span className="mono">agent-run.yml</span></div>
              <div><span className="key">cost</span> <span className="mono">$0.0421</span></div>
              <div><span className="key">decision</span> <span className="mono">ADVANCE</span></div>
            </div>
          </div>
          <div className="attempt-card">
            <div className="attempt-card-head">
              <strong>attempt 1</strong>
              <span className="pill drain">fail</span>
              <span className="dim mono">verify</span>
              <span className="dim mono">ran 1m 8s</span>
            </div>
            <ul className="attempt-card-reasons">
              <li className="mono dim">curl /_styleguide returned 404</li>
              <li className="mono dim">test_styleguide_contract.py::test_route failed</li>
            </ul>
          </div>
        </div>
      </Specimen>
    ),
  },
  {
    id: "job-step-log",
    title: "Job step log inspector",
    caption: "Selected job steps on the left, terminal output for the selected step",
    render: () => (
      <Specimen title="job step log inspector">
        <div className="run-workbench">
          <nav className="run-breadcrumb" aria-label="breadcrumb">
            <span>Issues</span>
            <span>/</span>
            <span>glimmung</span>
            <span>/</span>
            <span>Add design portfolio</span>
            <span>/</span>
            <strong>Run 01KQ... / cycle 2</strong>
          </nav>
          <div className="run-graph-board">
            <div className="run-graph" aria-label="run graph">
              <div className="stage-column" style={{ gridColumn: "1" }}>
                <div className="stage-heading">
                  <strong>env-prep</strong>
                  <span>passed</span>
                </div>
                <button type="button" className="run-graph-node done">
                  <strong>prepare env</strong>
                  <span>3m 12s</span>
                </button>
              </div>
              <div className="run-graph-edge horizontal" style={{ gridColumn: "2", gridRow: "2" }} />
              <div className="stage-column active" style={{ gridColumn: "3" }}>
                <div className="stage-heading">
                  <strong>agent-execute</strong>
                  <span>running</span>
                </div>
                <button type="button" className="run-graph-node selected">
                  <strong>portfolio-build</strong>
                  <span>5 steps</span>
                </button>
                <button type="button" className="run-graph-node pending">
                  <strong>screenshot</strong>
                  <span>queued</span>
                </button>
              </div>
              <div className="run-graph-edge horizontal" style={{ gridColumn: "4", gridRow: "2" }} />
              <div className="stage-column" style={{ gridColumn: "5" }}>
                <div className="stage-heading">
                  <strong>touchpoint</strong>
                  <span>waiting</span>
                </div>
                <button type="button" className="run-graph-node pending">
                  <strong>collect evidence</strong>
                  <span>blocked</span>
                </button>
                <button type="button" className="run-graph-node recycle">
                  <strong>request changes</strong>
                  <span>recycle</span>
                </button>
              </div>
              <div className="run-graph-edge recycle" style={{ gridColumn: "4", gridRow: "3" }} />
            </div>
            <div className="graph-inspector" aria-label="selected node details">
              <div className="graph-inspector-head">
                <span className="pill busy">running</span>
                <strong>portfolio-build</strong>
              </div>
              <div className="project-info">
                <div className="row"><span className="key">stage</span><span className="val mono">agent-execute</span></div>
                <div className="row"><span className="key">kind</span><span className="val mono">native k8s job</span></div>
                <div className="row"><span className="key">steps</span><span className="val mono">5</span></div>
                <div className="row"><span className="key">elapsed</span><span className="val mono">1m 18s</span></div>
              </div>
              <p className="dim">
                Selecting a node pins this inspector. Hover can preview later,
                but click selection is the durable review/debugging path.
              </p>
            </div>
          </div>
          <div className="step-log-layout">
            <aside className="step-list" aria-label="job steps">
              <button type="button" className="step-row done">
                <span>✓</span>
                <strong>checkout</strong>
                <small>8s</small>
              </button>
              <button type="button" className="step-row done">
                <span>✓</span>
                <strong>install</strong>
                <small>44s</small>
              </button>
              <button type="button" className="step-row active">
                <span>▶</span>
                <strong>build portfolio</strong>
                <small>1m 18s</small>
              </button>
              <button type="button" className="step-row pending">
                <span>·</span>
                <strong>screenshot</strong>
                <small>queued</small>
              </button>
              <button type="button" className="step-row pending">
                <span>·</span>
                <strong>summarize</strong>
                <small>queued</small>
              </button>
            </aside>
            <pre className="step-terminal">{`$ npm run build

> glimmung-frontend@0.1.0 build
> tsc && vite build

vite v5.4.21 building for production...
✓ 181 modules transformed.
rendering chunks...
computing gzip size...
dist/index.html                  0.47 kB
dist/assets/index.css           35.34 kB
dist/assets/index.js           530.47 kB

✓ built in 3.90s`}</pre>
          </div>
        </div>
      </Specimen>
    ),
  },
];

export function StyleguideView() {
  const [activeTab, setActiveTab] = useState<PortfolioTab>("system");
  const items = activeTab === "system" ? DESIGN_SYSTEM_ITEMS : DESIGN_FILE_ITEMS;
  const [openIdsByTab, setOpenIdsByTab] = useState<Record<PortfolioTab, Set<string>>>(() => ({
    system: new Set(DESIGN_SYSTEM_ITEMS.filter((item) => item.initialOpen).map((item) => item.id)),
    files: new Set(DESIGN_FILE_ITEMS.filter((item) => item.initialOpen).map((item) => item.id)),
  }));
  const [reviews, setReviews] = useState<Record<string, ReviewState>>({});
  const openIds = openIdsByTab[activeTab];

  const switchTab = (tab: PortfolioTab) => {
    setActiveTab(tab);
  };

  const toggleOpen = (id: string) => {
    setOpenIdsByTab((current) => {
      const nextTabOpenIds = new Set(current[activeTab]);
      if (nextTabOpenIds.has(id)) nextTabOpenIds.delete(id);
      else nextTabOpenIds.add(id);
      return {
        ...current,
        [activeTab]: nextTabOpenIds,
      };
    });
  };

  const setReview = (id: string, review: ReviewState) => {
    setReviews((current) => ({ ...current, [id]: current[id] === review ? "unset" : review }));
  };

  return (
    <main className="portfolio-shell">
      <div className="portfolio-topbar">
        <div className="portfolio-tabs" role="tablist" aria-label="portfolio mode">
          <button
            type="button"
            className={activeTab === "system" ? "selected" : ""}
            role="tab"
            aria-selected={activeTab === "system"}
            onClick={() => switchTab("system")}
          >
            <span className="portfolio-tab-icon">⌁</span>
            Design System
          </button>
          <button
            type="button"
            className={activeTab === "files" ? "selected" : ""}
            role="tab"
            aria-selected={activeTab === "files"}
            onClick={() => switchTab("files")}
          >
            <span className="portfolio-tab-icon">□</span>
            Design Files
          </button>
        </div>
        <button type="button" className="portfolio-share">Share</button>
      </div>

      <div className="portfolio-list">
        {items.map((item) => (
          <PortfolioRow
            key={item.id}
            item={item}
            isOpen={openIds.has(item.id)}
            review={reviews[item.id] ?? "unset"}
            onToggle={() => toggleOpen(item.id)}
            onReview={setReview}
          />
        ))}
      </div>
    </main>
  );
}

function PortfolioRow({
  item,
  isOpen,
  review,
  onToggle,
  onReview,
}: {
  item: PortfolioItem;
  isOpen: boolean;
  review: ReviewState;
  onToggle: () => void;
  onReview: (id: string, review: ReviewState) => void;
}) {
  return (
    <section className={`portfolio-row ${isOpen ? "open" : ""}`}>
      <div className="portfolio-row-summary">
        <button type="button" className="portfolio-disclosure" aria-expanded={isOpen} onClick={onToggle}>
          <span aria-hidden="true">{isOpen ? "⌄" : "›"}</span>
          <span>
            <strong>{item.title}</strong>
            <small>{item.caption}</small>
          </span>
        </button>
        <div className="portfolio-row-actions" aria-label={`${item.title} review`}>
          {isOpen && (
            <>
              <button
                type="button"
                className={`review reviewed ${review === "reviewed" ? "selected" : ""}`}
                onClick={() => onReview(item.id, "reviewed")}
              >
                ✓ Reviewed
              </button>
              <button
                type="button"
                className={`review needs-review ${review === "needs-review" ? "selected" : ""}`}
                onClick={() => onReview(item.id, "needs-review")}
              >
                Needs review
              </button>
            </>
          )}
          {!isOpen && <span className={`review-mark ${review}`}>{review === "needs-review" ? "!" : "✓"}</span>}
        </div>
      </div>
      {isOpen && <div className="portfolio-row-body">{item.render()}</div>}
    </section>
  );
}

function Specimen({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="portfolio-specimen">
      <h2>{title}</h2>
      {children}
    </div>
  );
}

function Row({ children }: { children: React.ReactNode }) {
  return <div className="portfolio-specimen-row">{children}</div>;
}

function Swatch({ label, value }: { label: string; value: string }) {
  return (
    <div className="portfolio-swatch">
      <span style={{ background: value }} />
      <strong>{label}</strong>
      <code>{value}</code>
    </div>
  );
}

function ConfirmDemo() {
  const [armed, setArmed] = useState(false);
  return (
    <>
      {armed ? (
        <span className="confirm">
          <button type="button" className="link danger-text" onClick={() => setArmed(false)}>cancel?</button>
          <span className="sep">/</span>
          <button type="button" className="link" onClick={() => setArmed(false)}>keep</button>
        </span>
      ) : (
        <button type="button" className="link" onClick={() => setArmed(true)}>cancel</button>
      )}
    </>
  );
}
