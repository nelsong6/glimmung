/**
 * Design portfolio route. This keeps the /_styleguide contract alive while
 * presenting the system as reviewable specimens instead of a long static page.
 */
import { useState } from "react";
import "./index.css";

type ReviewState = "unset" | "good" | "needs-work";

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
    id: "sidebar",
    title: "Sidebar",
    caption: "Project and workflow tree with selected rails",
    render: () => (
      <Specimen title="sidebar">
        <aside className="sidebar portfolio-sidebar">
          <div className="sidebar-title">Projects</div>
          <button type="button" className="project-row"><span className="name">All</span><span className="count">12</span></button>
          <div className="project-group">
            <button type="button" className="project-row selected"><span className="name">glimmung</span><span className="count">7</span></button>
            <button type="button" className="workflow-row selected"><span className="name">agent-run</span><span className="count">3</span></button>
            <button type="button" className="workflow-row"><span className="name">verify</span><span className="count">1</span></button>
          </div>
        </aside>
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
    title: "Tabs & header",
    caption: "Page header, connection chip, tabs, and user identity cluster",
    render: () => (
      <Specimen title="tabs & header">
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
          <button type="button" role="tab" className="tab selected">description</button>
          <button type="button" role="tab" className="tab">in progress<span className="tab-dot" aria-label="active" /></button>
          <button type="button" role="tab" className="tab">lineage</button>
        </div>
      </Specimen>
    ),
  },
];

const DESIGN_FILE_ITEMS: PortfolioItem[] = [
  {
    id: "capacity-view",
    title: "Capacity dashboard",
    caption: "Host availability, queue, workflow eligibility, and admin entry points",
    initialOpen: true,
    render: () => (
      <Specimen title="capacity dashboard">
        <div className="kpi-strip">
          <div className="kpi"><span className="k">hosts</span><span className="v">8</span></div>
          <div className="kpi"><span className="k">free</span><span className="v green">5</span></div>
          <div className="kpi"><span className="k">busy</span><span className="v amber">2</span></div>
          <div className="kpi"><span className="k">drained</span><span className="v red">1</span></div>
        </div>
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
            <div><span className="key">attempts</span> <span className="mono">2</span></div>
            <div><span className="key">cost</span> <span className="mono">$0.0504</span></div>
          </div>
        </div>
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
];

export function StyleguideView() {
  const [activeTab, setActiveTab] = useState<"system" | "files">("system");
  const items = activeTab === "system" ? DESIGN_SYSTEM_ITEMS : DESIGN_FILE_ITEMS;
  const [openIds, setOpenIds] = useState<Set<string>>(() => new Set(DESIGN_SYSTEM_ITEMS.filter((item) => item.initialOpen).map((item) => item.id)));
  const [reviews, setReviews] = useState<Record<string, ReviewState>>({});

  const switchTab = (tab: "system" | "files") => {
    setActiveTab(tab);
    const nextItems = tab === "system" ? DESIGN_SYSTEM_ITEMS : DESIGN_FILE_ITEMS;
    setOpenIds(new Set(nextItems.filter((item) => item.initialOpen).map((item) => item.id)));
  };

  const toggleOpen = (id: string) => {
    setOpenIds((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
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
                className={`review good ${review === "good" ? "selected" : ""}`}
                onClick={() => onReview(item.id, "good")}
              >
                ✓ Looks good
              </button>
              <button
                type="button"
                className={`review needs-work ${review === "needs-work" ? "selected" : ""}`}
                onClick={() => onReview(item.id, "needs-work")}
              >
                Needs work...
              </button>
            </>
          )}
          {!isOpen && <span className={`review-mark ${review}`}>{review === "needs-work" ? "!" : "✓"}</span>}
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
