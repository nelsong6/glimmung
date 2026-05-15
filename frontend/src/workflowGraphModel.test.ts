import { describe, expect, it } from "vitest";

import {
  phaseGraphModelFromNames,
  workflowToPhaseGraphModel,
  type WorkflowGraphSource,
} from "./workflowGraphModel";

describe("workflowToPhaseGraphModel", () => {
  it("maps workflow phases without leaking recycle policy fields into graph phases", () => {
    const workflow: WorkflowGraphSource = {
      name: "issue-agent",
      phases: [
        {
          name: "implementation",
          kind: "k8s_job",
          verify: true,
          depends_on: [],
          recycle_policy: {
            max_attempts: 2,
            on: ["failed", "needs_review"],
            lands_at: "self",
          },
        },
        {
          name: "verification",
          kind: "k8s_job",
          always: true,
          evidence_verification_gate: true,
          depends_on: ["implementation"],
        },
      ],
      pr: { enabled: true },
    };

    expect(workflowToPhaseGraphModel(workflow, { recycleActive: true })).toEqual({
      phases: [
        {
          name: "implementation",
          kind: "k8s_job",
          verify: true,
          always: undefined,
          evidence_verification_gate: undefined,
          depends_on: [],
        },
        {
          name: "verification",
          kind: "k8s_job",
          verify: undefined,
          always: true,
          evidence_verification_gate: true,
          depends_on: ["implementation"],
        },
      ],
      prEnabled: true,
      recycleArrows: [
        {
          source: "implementation",
          target: "implementation",
          trigger: "failed / needs_review",
          max_attempts: 2,
          active: true,
          kind: "phase_recycle",
        },
      ],
    });
  });

  it("preserves an empty phase list when a workflow definition has no phases", () => {
    const workflow: WorkflowGraphSource = {
      name: "native-workflow",
      phases: [],
      pr: {
        enabled: false,
        recycle_policy: {
          max_attempts: 3,
          on: ["changes_requested"],
          lands_at: "implementation",
        },
      },
    };

    expect(workflowToPhaseGraphModel(workflow)).toEqual({
      phases: [],
      prEnabled: false,
      recycleArrows: [
        {
          source: "touchpoint",
          target: "implementation",
          trigger: "changes_requested",
          max_attempts: 3,
          active: false,
          kind: "touchpoint_recycle",
        },
      ],
    });
  });
});

describe("phaseGraphModelFromNames", () => {
  it("links provided phase names in order", () => {
    expect(phaseGraphModelFromNames(["plan", "implement", "verify"], { prEnabled: false })).toEqual({
      phases: [
        { name: "plan", kind: "phase", depends_on: [] },
        { name: "implement", kind: "phase", depends_on: ["plan"] },
        { name: "verify", kind: "phase", depends_on: ["implement"] },
      ],
      prEnabled: false,
      recycleArrows: [],
    });
  });

  it("preserves an empty phase list", () => {
    expect(phaseGraphModelFromNames([])).toEqual({
      phases: [],
      prEnabled: true,
      recycleArrows: [],
    });
  });
});

