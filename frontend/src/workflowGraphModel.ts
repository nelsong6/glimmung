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

export function workflowToPhaseGraphModel(
  workflow: WorkflowGraphSource,
  options: {
    fallbackPhaseName?: string;
    recycleActive?: boolean;
  } = {},
): WorkflowGraphModel {
  const fallbackPhase: WorkflowGraphPhase = {
    name: options.fallbackPhaseName ?? workflow.name,
    kind: "gha_dispatch",
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
      ...phases.flatMap((phase) => phase.recycle_policy
        ? phase.recycle_policy.on.map((trigger) => ({
            source: phase.name,
            target: phase.recycle_policy!.lands_at,
            trigger,
            max_attempts: phase.recycle_policy!.max_attempts,
            active,
            kind: "phase_recycle" as const,
          }))
        : []
      ),
      ...(workflow.pr.recycle_policy
        ? workflow.pr.recycle_policy.on.map((trigger) => ({
            source: "report",
            target: workflow.pr.recycle_policy!.lands_at,
            trigger,
            max_attempts: workflow.pr.recycle_policy!.max_attempts,
            active,
            kind: "report_recycle" as const,
          }))
        : []
      ),
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
