// Shared phase graph renderer. Both the workflow definition view
// (no run state) and the latest-run strip (live state pills + selection
// callbacks) use this so the SHAPE of the rendered graph can never
// drift between the two views — only highlighting / pills can. Stage
// 3 of the spirelens-style parallel-LLM-stages refactor.
//
// Layout: phases are grouped by topological depth (depth = 1 + max
// depth of any depends_on predecessor). Each depth level renders as a
// vertical stack; depth levels render left-to-right separated by →
// arrows. Sequential workflows render as a single phase per column,
// matching the historical row layout. DAG workflows with parallel
// branches stack the parallel phases vertically at the same column.

import { Fragment, ReactNode } from "react";

export type PhaseGraphPhase = {
  name: string;
  kind: string;
  verify?: boolean;
  always?: boolean;
  evidence_verification_gate?: boolean;
  depends_on?: string[];
};

export type PhaseGraphProps = {
  /** Phases in declared order (the same `phases` field on Workflow). */
  phases: PhaseGraphPhase[];
  /** Whether to render the touchpoint (PR primitive) box at the end. */
  prEnabled: boolean;
  /**
   * Optional render override for each phase node. When omitted, a
   * default "definition" node is rendered (label + `verify`/`kind`
   * meta). The run-mode caller passes a renderer that uses live
   * attempt-rollup state and wires per-phase button refs for layout.
   */
  renderPhase?: (phase: PhaseGraphPhase) => ReactNode;
  /**
   * Optional render override for the touchpoint node. When omitted, a
   * default "definition" rendering is used.
   */
  renderTouchpoint?: () => ReactNode;
  /**
   * Ref callback for the visible phase container. Recycle/entry arrows
   * should target this surface rather than a child job node.
   */
  phaseRef?: (phase: PhaseGraphPhase, el: HTMLDivElement | null) => void;
  /** Extra className on the wrapping `.dag` div (e.g. `dag-definition`). */
  dagClassName?: string;
  /** Aria label on the wrapping `.dag` div. */
  ariaLabel?: string;
  /**
   * Phase name the active run entered at. The arrow leading into that
   * phase gets the `entry` class so reviewers can see "the run entered
   * here" — distinguishing default-entry runs from
   * recycle-child / resume runs that entered mid-pipeline.
   */
  entryPhaseName?: string | null;
};

/**
 * Compute topological depth per phase. Phases with no `depends_on` are
 * depth 0 (entry). Each subsequent phase = 1 + max(depth of its deps).
 *
 * Phases that reference unknown / forward / cyclic deps fall back to
 * their list-index position, since the workflow validator at registration
 * time rejects those shapes anyway — this is just defensive.
 */
function computeDepths(phases: PhaseGraphPhase[]): Map<string, number> {
  const byName = new Map<string, PhaseGraphPhase>();
  phases.forEach((p) => byName.set(p.name, p));
  const depths = new Map<string, number>();
  const visit = (name: string, visiting: Set<string>): number => {
    if (depths.has(name)) return depths.get(name)!;
    if (visiting.has(name)) return 0; // cycle defensive
    visiting.add(name);
    const phase = byName.get(name);
    if (!phase) return 0;
    const deps = phase.depends_on ?? [];
    let d = 0;
    for (const dep of deps) {
      const depDepth = visit(dep, visiting);
      if (depDepth + 1 > d) d = depDepth + 1;
    }
    visiting.delete(name);
    depths.set(name, d);
    return d;
  };
  for (const p of phases) visit(p.name, new Set());
  return depths;
}

function defaultPhaseNode(phase: PhaseGraphPhase): ReactNode {
  const meta = phase.evidence_verification_gate
    ? "verify-gate"
    : phase.always
      ? "always"
      : phase.verify
        ? "verify"
        : phase.kind;
  // No "not run" pill in definition view — the view itself signals
  // "this is a template, not an instance". State pills belong on the
  // run-pipeline strip.
  return (
    <div className="dag-node dag-node-phase dag-node-definition">
      <div className="dag-job-head">
        <span className="dag-job-title">{phase.name}</span>
        <span className="dag-job-kicker">job</span>
      </div>
      <div className="dag-node-meta dim mono">{meta}</div>
    </div>
  );
}

function defaultTouchpointNode(): ReactNode {
  return (
    <div className="dag-node dag-node-definition dag-node-pr">
      <div className="dag-node-label">touchpoint</div>
      <div className="dag-node-meta dim mono">PR primitive</div>
    </div>
  );
}

function FlowArrow({ entry }: { entry: boolean }) {
  return (
    <svg
      className={`dag-edge${entry ? " entry" : ""}`}
      viewBox="0 0 44 16"
      width="44"
      height="16"
      aria-label={entry ? "the run entered here" : undefined}
      aria-hidden={entry ? undefined : "true"}
    >
      {entry && <title>the run entered here</title>}
      <defs>
        <marker
          id="dag-flow-head"
          viewBox="0 0 10 10"
          refX="9"
          refY="5"
          markerWidth="7"
          markerHeight="7"
          orient="auto-start-reverse"
        >
          <path d="M 0 0 L 10 5 L 0 10 z" fill="context-stroke" />
        </marker>
      </defs>
      <path d="M 1 8 L 40 8" markerEnd="url(#dag-flow-head)" />
    </svg>
  );
}

export function PhaseGraph({
  phases,
  prEnabled,
  renderPhase = defaultPhaseNode,
  renderTouchpoint = defaultTouchpointNode,
  phaseRef,
  dagClassName,
  ariaLabel,
  entryPhaseName = null,
}: PhaseGraphProps) {
  const depths = computeDepths(phases);
  // Group phases by depth, preserving declared order within each depth.
  const byDepth = new Map<number, PhaseGraphPhase[]>();
  let maxDepth = 0;
  for (const phase of phases) {
    const d = depths.get(phase.name) ?? 0;
    if (d > maxDepth) maxDepth = d;
    const list = byDepth.get(d) ?? [];
    list.push(phase);
    byDepth.set(d, list);
  }
  const columns: PhaseGraphPhase[][] = [];
  for (let d = 0; d <= maxDepth; d++) {
    columns.push(byDepth.get(d) ?? []);
  }

  return (
    <div className={`dag${dagClassName ? " " + dagClassName : ""}`} aria-label={ariaLabel}>
      {columns.map((col, idx) => {
        // Highlight the arrow leading into a column when any phase in
        // that column matches `entryPhaseName`. Parallel columns can
        // have multiple entries; today only one entry phase is
        // active per run, so at most one column lights up.
        const colHasEntry = col.some(
          (phase) => entryPhaseName != null && phase.name === entryPhaseName,
        );
        return (
          <Fragment key={idx}>
            {idx > 0 && (
              <FlowArrow entry={colHasEntry} />
            )}
            <div
              className={`dag-phase dag-phase-column${col.length > 1 ? " dag-phase-parallel" : ""}`}
              ref={(el) => {
                for (const phase of col) phaseRef?.(phase, el);
              }}
            >
              <div className="dag-phase-head">
                <span className="dag-phase-title">
                  {col.length > 1 ? `phase ${idx + 1}` : (col[0]?.name ?? `phase ${idx + 1}`)}
                </span>
                <span className="dag-phase-kicker">phase</span>
              </div>
              <div className="dag-phase-body">
                {col.map((phase) => (
                  <Fragment key={phase.name}>{renderPhase(phase)}</Fragment>
                ))}
              </div>
            </div>
          </Fragment>
        );
      })}
      {prEnabled && (
        <>
          <FlowArrow entry={false} />
          {renderTouchpoint()}
        </>
      )}
    </div>
  );
}
