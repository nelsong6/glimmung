import { describe, expect, it } from "vitest";

import {
  runTopologyToPhaseGraphModel,
  workflowToPhaseGraphModel,
  type WorkflowGraphSource,
} from "./workflowGraphModel";

describe("workflowToPhaseGraphModel", () => {
  it("maps registered phases without leaking recycle policy fields into graph phases", () => {
    const workflow: WorkflowGraphSource = {
      name: "default",
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
          name: "touchpoint",
          kind: "k8s_job",
          run_on: "success",
          purpose: "review_touchpoint",
          depends_on: ["implementation"],
          jobs: [{ id: "pr-touchpoint", name: "PR touchpoint", primitive: "pr_touchpoint" }],
        },
      ],
      pr: { recycle_policy: null },
    };

    expect(workflowToPhaseGraphModel(workflow, { recycleActive: true })).toEqual({
      phases: [
        {
          name: "implementation",
          kind: "k8s_job",
          verify: true,
          run_on: undefined,
          purpose: undefined,
          evidence_verification_gate: undefined,
          depends_on: [],
          jobs: [
            { id: "plan", name: "plan", image: undefined, primitive: undefined },
            { id: "implement", name: "implement", image: "runner:latest", primitive: undefined },
          ],
        },
        {
          name: "touchpoint",
          kind: "k8s_job",
          verify: undefined,
          run_on: "success",
          purpose: "review_touchpoint",
          evidence_verification_gate: undefined,
          depends_on: ["implementation"],
          jobs: [{ id: "pr-touchpoint", name: "PR touchpoint", image: undefined, primitive: "pr_touchpoint" }],
        },
      ],
      entryArrows: [{
        target: "implementation",
        active: false,
        kind: "default",
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

  it("sources pr recycle arrows from the registered pr_touchpoint phase", () => {
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
        {
          name: "review-surface",
          kind: "k8s_job",
          run_on: "success",
          purpose: "review_touchpoint",
          depends_on: ["evidence-gate"],
          jobs: [{ id: "pr-touchpoint", primitive: "pr_touchpoint" }],
        },
      ],
      pr: {
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
      active: false,
      kind: "default",
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
        source: "review-surface",
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
          run_on: "success",
          purpose: "work",
          depends_on: [],
          jobs: [{ id: "prepare", name: "Prepare env" }],
        },
        {
          name: "touchpoint",
          kind: "k8s_job",
          verify: false,
          run_on: "success",
          purpose: "review_touchpoint",
          depends_on: ["env-prep"],
          jobs: [{ id: "pr-touchpoint", name: "PR touchpoint" }],
        },
      ],
      default_entry: { target: "env-prep", active: true, kind: "default" },
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
          run_on: "success",
          purpose: "work",
          depends_on: [],
          jobs: [{ id: "prepare", name: "Prepare env", image: undefined }],
        },
        {
          name: "touchpoint",
          kind: "k8s_job",
          verify: false,
          run_on: "success",
          purpose: "review_touchpoint",
          depends_on: ["env-prep"],
          jobs: [{ id: "pr-touchpoint", name: "PR touchpoint", image: undefined }],
        },
      ],
      entryArrows: [{
        target: "env-prep",
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
