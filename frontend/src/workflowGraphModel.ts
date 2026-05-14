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

function reportRecycleArrow(policy: RecyclePolicy | null | undefined, active: boolean): RecycleArrow | null {
  if (!policy) return null;
  return {
    source: "report",
    target: policy.lands_at,
    trigger: policy.on.join(" / "),
    max_attempts: policy.max_attempts,
    active,
    kind: "report_recycle",
  };
}

export function workflowToPhaseGraphModel(
  workflow: WorkflowGraphSource,
  options: {
    fallbackPhaseName?: string;
    recycleActive?: boolean;
  } = {},
): WorkflowGraphModel {
  const fallbackPhase: WorkflowGraphPhase = {
    name: options.fallbackPhaseName ?? workflow.name,
    kind: "k8s_job",
    verify: false,
    recycle_policy: null,
    depends_on: [],
  };
  const phases: WorkflowGraphPhase[] = workflow.phases.length > 0
    ? workflow.phases
    : [fallbackPhase];

  const active = options.recycleActive ?? false;
  return {
    phases: phases.map((phase) => ({
      name: phase.name,
      kind: phase.kind,
      verify: phase.verify,
      always: phase.always,
      evidence_verification_gate: phase.evidence_verification_gate,
      depends_on: phase.depends_on ?? [],
    })),
    prEnabled: workflow.pr.enabled,
    recycleArrows: [
      ...phases.flatMap((phase) => {
        const arrow = phaseRecycleArrow(phase, active);
        return arrow ? [arrow] : [];
      }),
      ...(() => {
        const arrow = reportRecycleArrow(workflow.pr.recycle_policy, active);
        return arrow ? [arrow] : [];
      })(),
    ],
  };
}

export function fallbackPhaseGraphModel(
  phaseNames: string[],
  options: {
    currentPhase?: string | null;
    prEnabled?: boolean;
    recycleArrows?: RecycleArrow[];
  } = {},
): WorkflowGraphModel {
  const names = phaseNames.length > 0
    ? phaseNames
    : [options.currentPhase ?? "phase"];
  return {
    phases: names.map((name, index) => ({
      name,
      kind: "phase",
      depends_on: index === 0 ? [] : [names[index - 1]],
    })),
    prEnabled: options.prEnabled ?? true,
    recycleArrows: options.recycleArrows ?? [],
  };
}
