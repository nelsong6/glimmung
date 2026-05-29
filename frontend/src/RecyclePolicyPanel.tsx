// Recycle-policy visibility + control for a workflow definition.
//
// Every workflow's verify loop is bounded by a recycle policy: a lane
// fires on a trigger (`verify_fail`, `pr_review_changes_requested`, ...),
// lands back at an earlier phase, and retries up to `max_attempts` times
// before the decision engine gives up. That attempt count is the guard
// rail operators most often want to read and scale ("give this loop one
// more try"), but it had no surface — it lived only inside the workflow
// registration. This panel surfaces every lane's count and, for admins,
// lets them scale it. Saving PATCHes /v1/workflows/{project}/{name},
// which mints a new immutable workflow schema and moves the logical
// pointer forward (in-flight runs keep the schema they started with).
//
// Shared by the issue workflow tab and the standalone workflow view, so
// the typings here are structural — anything shaped like a workflow with
// phases + a pr recycle policy can render through it.

import { useEffect, useMemo, useState } from "react";
import { authedFetch } from "./auth";

export type RecyclePolicyLike = {
  max_attempts: number;
  on: string[];
  lands_at: string;
};

export type RecyclePolicyPhase = {
  name: string;
  recycle_policy: RecyclePolicyLike | null;
};

export type RecyclePolicyWorkflow = {
  project: string;
  name: string;
  phases: RecyclePolicyPhase[];
  pr: { recycle_policy: RecyclePolicyLike | null };
};

// Sentinel target for the workflow-level PR reject lane. Matches the Go
// server's RecyclePatchTargetPR.
const PR_TARGET = "pr";
const MIN_ATTEMPTS = 1;
const MAX_ATTEMPTS = 20;

type Lane = {
  target: string; // phase name, or PR_TARGET
  label: string;
  trigger: string;
  landsAt: string;
  maxAttempts: number;
};

function lanesFromWorkflow(workflow: RecyclePolicyWorkflow): Lane[] {
  const lanes: Lane[] = [];
  for (const phase of workflow.phases) {
    const policy = phase.recycle_policy;
    if (!policy) continue;
    lanes.push({
      target: phase.name,
      label: phase.name,
      trigger: policy.on.join(" / ") || "—",
      landsAt: policy.lands_at,
      maxAttempts: policy.max_attempts,
    });
  }
  const prPolicy = workflow.pr.recycle_policy;
  if (prPolicy) {
    lanes.push({
      target: PR_TARGET,
      label: "pr reject",
      trigger: prPolicy.on.join(" / ") || "—",
      landsAt: prPolicy.lands_at,
      maxAttempts: prPolicy.max_attempts,
    });
  }
  return lanes;
}

function draftsFromLanes(lanes: Lane[]): Record<string, string> {
  return Object.fromEntries(lanes.map((lane) => [lane.target, String(lane.maxAttempts)]));
}

export function RecyclePolicyPanel({
  workflow,
  signedIn,
  isAdmin,
  onSaved,
}: {
  workflow: RecyclePolicyWorkflow;
  signedIn: boolean;
  isAdmin: boolean;
  onSaved?: () => void;
}) {
  const lanes = useMemo(() => lanesFromWorkflow(workflow), [workflow]);
  const [drafts, setDrafts] = useState<Record<string, string>>(() => draftsFromLanes(lanes));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Re-sync drafts to the latest workflow once a save settles or the
  // upstream definition changes underneath us (e.g. live SSE snapshot).
  useEffect(() => {
    if (!saving) setDrafts(draftsFromLanes(lanes));
  }, [lanes, saving]);

  const canEdit = signedIn && isAdmin;

  const changed = useMemo(
    () =>
      lanes.filter((lane) => {
        const draft = drafts[lane.target];
        if (draft === undefined) return false;
        const parsed = Number.parseInt(draft, 10);
        return Number.isFinite(parsed) && parsed !== lane.maxAttempts;
      }),
    [lanes, drafts],
  );

  const save = async () => {
    if (!canEdit || changed.length === 0) return;
    const patches: { target: string; max_attempts: number }[] = [];
    for (const lane of changed) {
      const parsed = Number.parseInt(drafts[lane.target] ?? "", 10);
      if (!Number.isFinite(parsed) || parsed < MIN_ATTEMPTS || parsed > MAX_ATTEMPTS) {
        setError(`max attempts must be between ${MIN_ATTEMPTS} and ${MAX_ATTEMPTS}.`);
        return;
      }
      patches.push({ target: lane.target, max_attempts: parsed });
    }
    setSaving(true);
    setError(null);
    try {
      const response = await authedFetch(
        `/v1/workflows/${encodeURIComponent(workflow.project)}/${encodeURIComponent(workflow.name)}`,
        {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ recycle_max_attempts: patches }),
        },
      );
      if (!response.ok) {
        const text = await response.text().catch(() => "");
        setError(`${response.status} ${text || response.statusText}`);
        return;
      }
      onSaved?.();
    } catch (err) {
      setError(String(err));
    } finally {
      setSaving(false);
    }
  };

  if (lanes.length === 0) {
    return (
      <section className="recycle-policy" aria-label="recycle policy">
        <div className="recycle-policy-head">
          <h3>recycle policy</h3>
        </div>
        <div className="dim mono">this workflow declares no recycle lanes.</div>
      </section>
    );
  }

  return (
    <section className="recycle-policy" aria-label="recycle policy">
      <div className="recycle-policy-head">
        <h3>recycle policy</h3>
        <span className="dim mono">max attempts before the verify loop gives up</span>
      </div>
      <table className="recycle-policy-table">
        <thead>
          <tr>
            <th>lane</th>
            <th>fires on</th>
            <th>lands at</th>
            <th>max attempts</th>
          </tr>
        </thead>
        <tbody>
          {lanes.map((lane) => {
            const draft = drafts[lane.target] ?? String(lane.maxAttempts);
            const inputId = `recycle-attempts-${lane.target}`;
            return (
              <tr key={lane.target}>
                <td className="mono">{lane.label}</td>
                <td className="mono dim">{lane.trigger}</td>
                <td className="mono dim">{lane.landsAt}</td>
                <td>
                  {canEdit ? (
                    <input
                      id={inputId}
                      aria-label={`${lane.label} max attempts`}
                      type="number"
                      min={MIN_ATTEMPTS}
                      max={MAX_ATTEMPTS}
                      value={draft}
                      disabled={saving}
                      onChange={(event) =>
                        setDrafts((current) => ({ ...current, [lane.target]: event.target.value }))
                      }
                    />
                  ) : (
                    <span className="mono">{lane.maxAttempts}</span>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {canEdit ? (
        <div className="recycle-policy-actions">
          <button
            type="button"
            className="gb"
            disabled={saving || changed.length === 0}
            onClick={() => void save()}
          >
            {saving ? "saving..." : "apply"}
          </button>
          <span className="dim mono">
            {changed.length === 0 ? "no changes" : `${changed.length} change(s) pending`}
          </span>
        </div>
      ) : (
        <div className="dim mono">admin sign-in required to scale</div>
      )}
      {error && <div className="danger-text mono">{error}</div>}
    </section>
  );
}
