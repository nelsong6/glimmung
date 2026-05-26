import { describe, expect, it } from "vitest";

import {
  runTopologyToPhaseGraphModel,
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
          jobs: [
            { id: "plan", name: "plan" },
            { id: "implement", name: "implement", image: "runner:latest" },
          ],
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
          jobs: [
            { id: "plan", name: "plan", image: undefined },
            { id: "implement", name: "implement", image: "runner:latest" },
          ],
        },
        {
          name: "verification",
          kind: "k8s_job",
          verify: undefined,
          always: true,
          evidence_verification_gate: true,
          depends_on: ["implementation"],
          jobs: [],
        },
      ],
      prEnabled: true,
      entryArrows: [{
        target: "implementation",
        label: "manual trigger",
        active: false,
        kind: "manual_trigger",
      }],
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
      entryArrows: [],
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

  it("keeps explicit recycle targets that return to the entry phase", () => {
    const workflow: WorkflowGraphSource = {
      name: "ambience",
      phases: [
        { name: "env-prep", kind: "k8s_job" },
        { name: "llm-work", kind: "k8s_job", depends_on: ["env-prep"] },
        {
          name: "evidence-gate",
          kind: "k8s_job",
          depends_on: ["llm-work"],
          recycle_policy: {
            max_attempts: 3,
            on: ["verify_fail"],
            lands_at: "env-prep",
          },
        },
      ],
      pr: {
        enabled: true,
        recycle_policy: {
          max_attempts: 3,
          on: ["changes_requested"],
          lands_at: "env-prep",
        },
      },
    };

    const model = workflowToPhaseGraphModel(workflow);
    expect(model.entryArrows).toEqual([{
      target: "env-prep",
      label: "manual trigger",
      active: false,
      kind: "manual_trigger",
    }]);
    expect(model.recycleArrows).toEqual([
      {
        source: "evidence-gate",
        target: "env-prep",
        trigger: "verify_fail",
        max_attempts: 3,
        active: false,
        kind: "phase_recycle",
      },
      {
        source: "touchpoint",
        target: "env-prep",
        trigger: "changes_requested",
        max_attempts: 3,
        active: false,
        kind: "touchpoint_recycle",
      },
    ]);
  });
});

describe("runTopologyToPhaseGraphModel", () => {
  it("uses run projection topology as the execution graph shape", () => {
    expect(runTopologyToPhaseGraphModel({
      phases: [
        {
          name: "env-prep",
          kind: "k8s_job",
          verify: false,
          always: false,
          depends_on: [],
          jobs: [{ id: "prepare", name: "Prepare env" }],
        },
        {
          name: "agent-execute",
          kind: "k8s_job",
          verify: true,
          always: false,
          depends_on: ["env-prep"],
          jobs: [{ id: "agent", name: null, image: "agent:latest" }],
        },
      ],
      default_entry: { target: "env-prep", active: true, kind: "default" },
      terminal: { kind: "touchpoint", enabled: true },
      recycle_arrows: [{
        source: "touchpoint",
        target: "env-prep",
        trigger: "changes_requested",
        max_attempts: 3,
        active: false,
        kind: "touchpoint_recycle",
      }],
    })).toEqual({
      phases: [
        {
          name: "env-prep",
          kind: "k8s_job",
          verify: false,
          always: false,
          depends_on: [],
          jobs: [{ id: "prepare", name: "Prepare env", image: undefined }],
        },
        {
          name: "agent-execute",
          kind: "k8s_job",
          verify: true,
          always: false,
          depends_on: ["env-prep"],
          jobs: [{ id: "agent", name: "agent", image: "agent:latest" }],
        },
      ],
      prEnabled: true,
      entryArrows: [{
        target: "env-prep",
        label: "manual trigger",
        active: true,
        kind: "default",
      }],
      recycleArrows: [{
        source: "touchpoint",
        target: "env-prep",
        trigger: "changes_requested",
        max_attempts: 3,
        active: false,
        kind: "touchpoint_recycle",
      }],
    });
  });
});
