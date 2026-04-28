import { useState } from "react";
import { authedFetch } from "./auth";

type Props = {
  onSuccess: () => void;
};

export function AdminPanel({ onSuccess }: Props) {
  const [tab, setTab] = useState<"project" | "host">("project");
  return (
    <div className="admin-panel">
      <div className="admin-tabs">
        <button
          type="button"
          className={`tab ${tab === "project" ? "selected" : ""}`}
          onClick={() => setTab("project")}
        >
          Register project
        </button>
        <button
          type="button"
          className={`tab ${tab === "host" ? "selected" : ""}`}
          onClick={() => setTab("host")}
        >
          Register host
        </button>
      </div>
      {tab === "project" ? (
        <ProjectForm onSuccess={onSuccess} />
      ) : (
        <HostForm onSuccess={onSuccess} />
      )}
    </div>
  );
}

function ProjectForm({ onSuccess }: { onSuccess: () => void }) {
  const [name, setName] = useState("");
  const [githubRepo, setGithubRepo] = useState("");
  const [workflowFilename, setWorkflowFilename] = useState("issue-agent.yaml");
  const [workflowRef, setWorkflowRef] = useState("main");
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
      const r = await authedFetch("/v1/projects", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name,
          github_repo: githubRepo,
          workflow_filename: workflowFilename,
          workflow_ref: workflowRef,
          trigger_label: triggerLabel,
          default_requirements: reqs,
        }),
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
        <input value={githubRepo} onChange={(e) => setGithubRepo(e.target.value)} placeholder="nelsong6/spirelens" required />
      </label>
      <label>
        <span>Workflow filename</span>
        <input value={workflowFilename} onChange={(e) => setWorkflowFilename(e.target.value)} required />
      </label>
      <label>
        <span>Workflow ref</span>
        <input value={workflowRef} onChange={(e) => setWorkflowRef(e.target.value)} />
      </label>
      <label>
        <span>Trigger label</span>
        <input value={triggerLabel} onChange={(e) => setTriggerLabel(e.target.value)} />
      </label>
      <label>
        <span>Requirements (JSON)</span>
        <input value={requirements} onChange={(e) => setRequirements(e.target.value)} className="mono" />
      </label>
      {error && <div className="error">{error}</div>}
      <button type="submit" disabled={busy}>{busy ? "Submitting…" : "Register"}</button>
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
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="win-a" required />
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
      <button type="submit" disabled={busy}>{busy ? "Submitting…" : "Register"}</button>
    </form>
  );
}
