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

import { Fragment, ReactNode, useId, useLayoutEffect, useRef, useState } from "react";
import { getSmoothStepPath, Position } from "@xyflow/react";

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

type AdvancePath = {
  d: string;
  entry: boolean;
};

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
  const markerId = useId().replace(/:/g, "");
  const dagRef = useRef<HTMLDivElement | null>(null);
  const columnRefs = useRef<Array<HTMLDivElement | null>>([]);
  const terminalRef = useRef<HTMLDivElement | null>(null);
  const [advancePaths, setAdvancePaths] = useState<AdvancePath[]>([]);
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
  const columnKey = columns.map((col) => col.map((phase) => phase.name).join(",")).join("|");

  useLayoutEffect(() => {
    const recompute = () => {
      const dag = dagRef.current;
      if (!dag) return;
      const dagRect = dag.getBoundingClientRect();
      const rects = columnRefs.current
        .slice(0, columns.length)
        .map((el) => el?.getBoundingClientRect() ?? null);
      const paths: AdvancePath[] = [];
      for (let idx = 1; idx < rects.length; idx += 1) {
        const from = rects[idx - 1];
        const to = rects[idx];
        if (!from || !to) continue;
        const sx = from.right - dagRect.left;
        const sy = from.top + from.height / 2 - dagRect.top;
        const ex = to.left - dagRect.left;
        const ey = to.top + to.height / 2 - dagRect.top;
        const entry = columns[idx].some(
          (phase) => entryPhaseName != null && phase.name === entryPhaseName,
        );
        const [d] = getSmoothStepPath({
          sourceX: sx,
          sourceY: sy,
          sourcePosition: Position.Right,
          targetX: ex,
          targetY: ey,
          targetPosition: Position.Left,
          borderRadius: 8,
          offset: 18,
        });
        paths.push({ d, entry });
      }
      const last = rects[rects.length - 1];
      const terminal = terminalRef.current?.getBoundingClientRect() ?? null;
      if (prEnabled && last && terminal) {
        const sx = last.right - dagRect.left;
        const sy = last.top + last.height / 2 - dagRect.top;
        const ex = terminal.left - dagRect.left;
        const ey = terminal.top + terminal.height / 2 - dagRect.top;
        const [d] = getSmoothStepPath({
          sourceX: sx,
          sourceY: sy,
          sourcePosition: Position.Right,
          targetX: ex,
          targetY: ey,
          targetPosition: Position.Left,
          borderRadius: 8,
          offset: 18,
        });
        paths.push({ d, entry: false });
      }
      setAdvancePaths(paths);
    };
    recompute();
    const ro = new ResizeObserver(recompute);
    if (dagRef.current) ro.observe(dagRef.current);
    columnRefs.current.forEach((el) => {
      if (el) ro.observe(el);
    });
    if (terminalRef.current) ro.observe(terminalRef.current);
    return () => ro.disconnect();
  }, [columnKey, entryPhaseName, prEnabled]);

  return (
    <div
      ref={dagRef}
      className={`dag${dagClassName ? " " + dagClassName : ""}`}
      aria-label={ariaLabel}
    >
      {advancePaths.length > 0 && (
        <svg className="dag-advance-layer" aria-hidden="true">
          <defs>
            <marker
              id={`${markerId}-advance-head`}
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
          {advancePaths.map((path, idx) => (
            <path
              key={`${path.d}:${idx}`}
              d={path.d}
              className={`dag-advance-path${path.entry ? " entry" : ""}`}
              markerEnd={`url(#${markerId}-advance-head)`}
            />
          ))}
        </svg>
      )}
      {columns.map((col, idx) => {
        return (
          <Fragment key={idx}>
            {idx > 0 && (
              <div className="dag-edge-spacer" aria-hidden="true" />
            )}
            <div
              className={`dag-phase dag-phase-column${col.length > 1 ? " dag-phase-parallel" : ""}`}
              ref={(el) => {
                columnRefs.current[idx] = el;
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
          <div className="dag-edge-spacer" aria-hidden="true" />
          <div className="dag-terminal" ref={terminalRef}>
            {renderTouchpoint()}
          </div>
        </>
      )}
    </div>
  );
}
