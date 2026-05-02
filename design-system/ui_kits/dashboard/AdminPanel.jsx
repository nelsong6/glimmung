/* global React, Field, Button */
const { useState: useStateAP } = React;

function AdminPanel({ projects, onClose }) {
  const [tab, setTab] = useStateAP("project");
  return (
    <div className="admin-panel">
      <div className="admin-tabs tabs" style={{ margin: 0 }}>
        {["project", "workflow", "host"].map(t => (
          <button key={t} className={`tab ${tab === t ? "selected" : ""}`} onClick={() => setTab(t)}>register {t}</button>
        ))}
      </div>
      {tab === "project" && (
        <form className="admin-form" onSubmit={e => { e.preventDefault(); onClose && onClose(); }}>
          <Field label="name"><input type="text" placeholder="spirelens" /></Field>
          <Field label="github_repo"><input type="text" className="mono" placeholder="nelsong6/spirelens" /></Field>
          <Button type="submit">register project</Button>
        </form>
      )}
      {tab === "workflow" && (
        <form className="admin-form" onSubmit={e => { e.preventDefault(); onClose && onClose(); }}>
          <Field label="project">
            <select>{projects.map(p => <option key={p.name}>{p.name}</option>)}</select>
          </Field>
          <Field label="name"><input type="text" placeholder="issue-agent" /></Field>
          <Field label="workflow_filename"><input type="text" className="mono" placeholder="issue-agent.yml" /></Field>
          <Field label="trigger_label"><input type="text" className="mono" placeholder="agent-run" /></Field>
          <Field label="requirements (json)"><input type="text" className="mono" placeholder='{"runtime":"x11","gpu":true}' /></Field>
          <Button type="submit">register workflow</Button>
        </form>
      )}
      {tab === "host" && (
        <form className="admin-form" onSubmit={e => { e.preventDefault(); onClose && onClose(); }}>
          <Field label="name"><input type="text" className="mono" placeholder="x11-gpu-03" /></Field>
          <Field label="capabilities (json)"><input type="text" className="mono" placeholder='{"runtime":"x11","gpu":true}' /></Field>
          <Button type="submit">register host</Button>
        </form>
      )}
    </div>
  );
}

window.AdminPanel = AdminPanel;
