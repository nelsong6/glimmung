/* global React, Pill, MonoCell, Empty */
function PrsView({ data, projectFilter }) {
  const rows = projectFilter ? data.prs.filter(p => p.project === projectFilter) : data.prs;
  return (
    <>
      <h2>Open PRs ({rows.length})
        {projectFilter && <span className="filter-hint"> — filtered to {projectFilter}</span>}
        <button className="inline-action">refresh</button>
      </h2>
      {rows.length === 0 ? <Empty>No open PRs yet.</Empty> : (
        <table>
          <thead><tr><th>Project</th><th>PR</th><th>Title</th><th>Issue</th><th>Run state</th><th>Attempts</th><th>Cost</th><th>Triage</th></tr></thead>
          <tbody>
            {rows.map(r => (
              <tr key={r.id} className={`clickable ${r.pr_lock_held ? "eligible" : ""}`}>
                <td>{r.project}</td>
                <MonoCell>{r.repo}#{r.pr_number}</MonoCell>
                <td>{r.title}</td>
                <MonoCell dim>{r.issue_number !== null ? `#${r.issue_number}` : "—"}</MonoCell>
                <td>
                  {r.run_state ? <Pill kind={runPill(r.run_state)}>{r.run_state}</Pill> :
                    <span style={{ color: "var(--fg-dimmer)" }}>manual</span>}
                </td>
                <MonoCell dim>{r.run_attempts || "—"}</MonoCell>
                <MonoCell dim>{r.run_cumulative_cost_usd ? `$${r.run_cumulative_cost_usd.toFixed(2)}` : "—"}</MonoCell>
                <td>{r.pr_lock_held ? <Pill kind="busy">in flight</Pill> : <span style={{ color: "var(--fg-dimmer)" }}>—</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function runPill(s) {
  if (s === "passed") return "free";
  if (s === "in_progress") return "busy";
  if (s === "aborted") return "drain";
  return "free";
}

window.PrsView = PrsView;
