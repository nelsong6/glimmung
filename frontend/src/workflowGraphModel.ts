import type { PhaseGraphPhase, RecycleArrow } from "./PhaseGraph";

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

export function workflowToPhaseGraphModel(
  workflow: WorkflowGraphSource,
  options: {
    recycleActive?: boolean;
  } = {},
): WorkflowGraphModel {
  const active = options.recycleActive ?? false;
  return {
    phases: workflow.phases.map((phase) => ({
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
    })),
    prEnabled: workflow.pr.enabled,
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
  return {
    phases: topology.phases.map((phase) => ({
      ...phase,
      depends_on: phase.depends_on ?? [],
      jobs: (phase.jobs ?? []).map((job) => ({
        id: job.id,
        name: job.name ?? job.id,
        image: job.image,
      })),
    })),
    prEnabled: topology.terminal.enabled,
    recycleArrows: topology.recycle_arrows,
  };
}

export function phaseGraphModelFromNames(
  phaseNames: string[],
  options: {
    prEnabled?: boolean;
    recycleArrows?: RecycleArrow[];
  } = {},
): WorkflowGraphModel {
  return {
    phases: phaseNames.map((name, index) => ({
      name,
      kind: "phase",
      depends_on: index === 0 ? [] : [phaseNames[index - 1]],
    })),
    prEnabled: options.prEnabled ?? true,
    recycleArrows: options.recycleArrows ?? [],
  };
}
