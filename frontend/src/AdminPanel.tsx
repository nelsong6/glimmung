import { useState } from "react";
import { authedFetch } from "./auth";

type Project = {
  name: string;
  github_repo: string;
};

type Props = {
  projects: Project[];
  onSuccess: () => void;
};

type Tab = "project" | "workflow" | "host";

export function AdminPanel({ projects, onSuccess }: Props) {
  const [tab, setTab] = useState<Tab>("project");
  return (
    <div className="admin-panel">
      <div className="admin-tabs">
        {(["project", "workflow", "host"] as Tab[]).map((t) => (
          <button
            type="button"
            key={t}
            className={`tab ${tab === t ? "selected" : ""}`}
            onClick={() => setTab(t)}
          >
            Register {t}
          </button>
        ))}
      </div>
      {tab === "project" && <ProjectForm onSuccess={onSuccess} />}
      {tab === "workflow" && <WorkflowForm projects={projects} onSuccess={onSuccess} />}
      {tab === "host" && <HostForm onSuccess={onSuccess} />}
    </div>
  );
}

function ProjectForm({ onSuccess }: { onSuccess: () => void }) {
  const [name, setName] = useState("");
  const [githubRepo, setGithubRepo] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const r = await authedFetch("/v1/projects", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name, github_repo: githubRepo }),
      });
      if (!r.ok) {
        setError(`${r.status}: ${await r.text()}`);
        return;
      }
      setName("");
      setGithubRepo("");
      onSuccess();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="admin-form">
      <label>
        <span>Name</span>
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="spirelens" required />
      </label>
      <label>
        <span>GitHub repo</span>
        <input
          value={githubRepo}
          onChange={(e) => setGithubRepo(e.target.value)}
          placeholder="nelsong6/spirelens"
          required
        />
      </label>
      {error && <div className="error">{error}</div>}
      <button type="submit" disabled={busy}>
        {busy ? "Submitting…" : "Register"}
      </button>
    </form>
  );
}

function WorkflowForm({ projects, onSuccess }: { projects: Project[]; onSuccess: () => void }) {
  const [project, setProject] = useState(projects[0]?.name ?? "");
  const [name, setName] = useState("issue-agent");
  const [filename, setFilename] = useState("issue-agent.yaml");
  const [ref, setRef] = useState("main");
  const [triggerLabel, setTriggerLabel] = useState("issue-agent");
  const [requirements, setRequirements] = useState('{"apps":["sts2"]}');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const reqs = JSON.parse(requirements || "{}");
      const r = await authedFetch("/v1/workflows", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          project,
          name,
          workflow_filename: filename,
          workflow_ref: ref,
          trigger_label: triggerLabel,
          default_requirements: reqs,
        }),
      });
      if (!r.ok) {
        setError(`${r.status}: ${await r.text()}`);
        return;
      }
      setName("");
      onSuccess();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  if (projects.length === 0) {
    return <div className="admin-form"><div className="error">Register a project first.</div></div>;
  }

  return (
    <form onSubmit={submit} className="admin-form">
      <label>
        <span>Project</span>
        <select value={project} onChange={(e) => setProject(e.target.value)} required>
          {projects.map((p) => (
            <option key={p.name} value={p.name}>
              {p.name}
            </option>
          ))}
        </select>
      </label>
      <label>
        <span>Workflow name</span>
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="issue-agent" required />
      </label>
      <label>
        <span>Workflow filename</span>
        <input value={filename} onChange={(e) => setFilename(e.target.value)} required />
      </label>
      <label>
        <span>Ref</span>
        <input value={ref} onChange={(e) => setRef(e.target.value)} />
      </label>
      <label>
        <span>Trigger label</span>
        <input value={triggerLabel} onChange={(e) => setTriggerLabel(e.target.value)} />
      </label>
      <label>
        <span>Requirements (JSON)</span>
        <input
          value={requirements}
          onChange={(e) => setRequirements(e.target.value)}
          className="mono"
        />
      </label>
      {error && <div className="error">{error}</div>}
      <button type="submit" disabled={busy}>
        {busy ? "Submitting…" : "Register"}
      </button>
    </form>
  );
}

function HostForm({ onSuccess }: { onSuccess: () => void }) {
  const [name, setName] = useState("");
  const [capabilities, setCapabilities] = useState('{"os":"windows","apps":["sts2"]}');
  const [drained, setDrained] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const caps = JSON.parse(capabilities || "{}");
      const r = await authedFetch("/v1/hosts", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name, capabilities: caps, drained }),
      });
      if (!r.ok) {
        setError(`${r.status}: ${await r.text()}`);
        return;
      }
      setName("");
      onSuccess();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="admin-form">
      <label>
        <span>Name</span>
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="nelsonpc" required />
      </label>
      <label>
        <span>Capabilities (JSON)</span>
        <input value={capabilities} onChange={(e) => setCapabilities(e.target.value)} className="mono" />
      </label>
      <label className="checkbox">
        <input type="checkbox" checked={drained} onChange={(e) => setDrained(e.target.checked)} />
        <span>Drained</span>
      </label>
      {error && <div className="error">{error}</div>}
      <button type="submit" disabled={busy}>
        {busy ? "Submitting…" : "Register"}
      </button>
    </form>
  );
}
