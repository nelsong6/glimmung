import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { PhaseGraph } from "./PhaseGraph";

describe("PhaseGraph", () => {
  it("reserves routing space for recycle arrows that return to the first phase", () => {
    const { container } = render(
      <PhaseGraph
        ariaLabel="workflow graph"
        phases={[
          { name: "env-prep", kind: "k8s_job", jobs: [{ id: "env-prep" }] },
          { name: "llm-work", kind: "k8s_job", depends_on: ["env-prep"], jobs: [{ id: "llm-work" }] },
          { name: "llm-verify", kind: "k8s_job", depends_on: ["llm-work"], jobs: [{ id: "llm-verify" }] },
          { name: "evidence-gate", kind: "k8s_job", depends_on: ["llm-verify"], jobs: [{ id: "evidence-gate" }] },
          { name: "env-destroy", kind: "k8s_job", always: true, depends_on: ["evidence-gate"], jobs: [{ id: "env-destroy" }] },
        ]}
        prEnabled
        entryArrows={[{
          target: "env-prep",
          label: "manual trigger",
          active: false,
          kind: "manual_trigger",
        }]}
        recycleArrows={[
          {
            source: "evidence-gate",
            target: "env-prep",
            trigger: "verify_fail",
            max_attempts: 3,
            active: true,
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
        ]}
      />,
    );

    expect(screen.getByLabelText("workflow graph")).toBeInTheDocument();
    expect(container.querySelector('[data-id="entry-source:0"]')).toBeInTheDocument();
    expect(container.querySelector(".dag-rf-surface")).toHaveStyle({
      width: "1768px",
      height: "216px",
    });
  });
});
