import type { EntryArrow, PhaseGraphPhase, RecycleArrow } from "./PhaseGraph";

type RecyclePolicy = {
  max_attempts: number;
  on: string[];
  lands_at: string;
};

export type WorkflowGraphPhase = PhaseGraphPhase & {
  recycle_policy?: RecyclePolicy | null;
};

export type WorkflowGraphSource = {
  name: string;
  phases: WorkflowGraphPhase[];
  pr: {
    enabled: boolean;
    recycle_policy?: RecyclePolicy | null;
  };
  workflow_filename?: string | null;
  workflow_ref?: string | null;
  default_requirements?: Record<string, unknown>;
};

export type WorkflowGraphModel = {
  phases: PhaseGraphPhase[];
  prEnabled: boolean;
  entryArrows: EntryArrow[];
  recycleArrows: RecycleArrow[];
};

export type RunProjectionTopologySource = {
  phases: PhaseGraphPhase[];
  default_entry?: { target: string; active: boolean; kind: string } | null;
  recycle_arrows: RecycleArrow[];
  terminal: { kind?: string; enabled: boolean };
};

function phaseRecycleArrow(phase: WorkflowGraphPhase, active: boolean): RecycleArrow | null {
  if (!phase.recycle_policy) return null;
  return {
    source: phase.name,
    target: phase.recycle_policy.lands_at === "self" ? phase.name : phase.recycle_policy.lands_at,
    trigger: phase.recycle_policy.on.join(" / "),
    max_attempts: phase.recycle_policy.max_attempts,
    active,
    kind: "phase_recycle",
  };
}

function touchpointRecycleArrow(policy: RecyclePolicy | null | undefined, active: boolean): RecycleArrow | null {
  if (!policy) return null;
  return {
    source: "touchpoint",
    target: policy.lands_at,
    trigger: policy.on.join(" / "),
    max_attempts: policy.max_attempts,
    active,
    kind: "touchpoint_recycle",
  };
}

function manualTriggerEntryArrow(phases: PhaseGraphPhase[]): EntryArrow[] {
  const firstPhase = phases.find((phase) => phase.name !== "");
  if (!firstPhase) return [];
  return [{
    target: firstPhase.name,
    label: "manual trigger",
    active: false,
    kind: "manual_trigger",
  }];
}

function topologyEntryArrow(
  entry: RunProjectionTopologySource["default_entry"] | null | undefined,
): EntryArrow[] {
  if (!entry?.target) return [];
  return [{
    target: entry.target,
    label: entryLabel(entry.kind),
    active: entry.active,
    kind: entry.kind,
  }];
}

function entryLabel(kind: string): string {
  if (kind === "" || kind === "default" || kind === "manual_trigger") return "manual trigger";
  return kind.replace(/_/g, " ");
}

export function workflowToPhaseGraphModel(
  workflow: WorkflowGraphSource,
  options: {
    recycleActive?: boolean;
  } = {},
): WorkflowGraphModel {
  const active = options.recycleActive ?? false;
  const phases = workflow.phases.map((phase) => ({
    name: phase.name,
    kind: phase.kind,
    verify: phase.verify,
    always: phase.always,
    evidence_verification_gate: phase.evidence_verification_gate,
    depends_on: phase.depends_on ?? [],
    jobs: (phase.jobs ?? []).map((job) => ({
      id: job.id,
      name: job.name,
      image: job.image,
    })),
  }));
  return {
    phases,
    prEnabled: workflow.pr.enabled,
    entryArrows: manualTriggerEntryArrow(phases),
    recycleArrows: [
      ...workflow.phases.flatMap((phase) => {
        const arrow = phaseRecycleArrow(phase, active);
        return arrow ? [arrow] : [];
      }),
      ...(() => {
        const arrow = touchpointRecycleArrow(workflow.pr.recycle_policy, active);
        return arrow ? [arrow] : [];
      })(),
    ],
  };
}

export function runTopologyToPhaseGraphModel(topology: RunProjectionTopologySource): WorkflowGraphModel {
  const phases = topology.phases.map((phase) => ({
    ...phase,
    depends_on: phase.depends_on ?? [],
    jobs: (phase.jobs ?? []).map((job) => ({
      id: job.id,
      name: job.name ?? job.id,
      image: job.image,
    })),
  }));
  return {
    phases,
    prEnabled: topology.terminal.enabled,
    entryArrows: topologyEntryArrow(topology.default_entry),
    recycleArrows: topology.recycle_arrows,
  };
}
