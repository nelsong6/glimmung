/* global React, Pill, MonoCell, Empty */
function IssuesView({ data, projectFilter }) {
  const rows = projectFilter ? data.issues.filter(i => i.project === projectFilter) : data.issues;
  return (
    <>
      <h2>Open issues ({rows.length})
        {projectFilter && <span className="filter-hint"> — filtered to {projectFilter}</span>}
        <button className="inline-action">refresh</button>
      </h2>
      {rows.length === 0 ? <Empty>No open issues.</Empty> : (
        <table>
          <thead><tr><th>Project</th><th>Title</th><th>Labels</th><th>Last run</th><th>Action</th></tr></thead>
          <tbody>
            {rows.map(r => (
              <tr key={r.id}>
                <td>{r.project}</td>
                <td><button className="gb ghost" style={{ textAlign: "left" }}>{r.title}</button></td>
                <MonoCell dim>{r.labels.length ? r.labels.join(", ") : "—"}</MonoCell>
                <td>
                  {r.last_run_state ? (
                    <span style={{ display: "inline-flex", gap: 6, alignItems: "center" }}>
                      <Pill kind={runPill(r.last_run_state)}>{r.last_run_state}</Pill>
                      {r.issue_lock_held && <Pill kind="busy">in flight</Pill>}
                    </span>
                  ) : <span style={{ color: "var(--fg-dimmer)" }}>—</span>}
                </td>
                <td>
                  <button className="gb ghost" disabled={r.issue_lock_held}>
                    {r.issue_lock_held ? "in flight" : "dispatch"}
                  </button>
                </td>
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

window.IssuesView = IssuesView;
