// Shared phase graph renderer. Both workflow-definition and run-detail
// views use this component so phase/job structure and edge semantics
// stay aligned.

import { Fragment, ReactNode, useMemo } from "react";
import {
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  type Edge,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { RecycleArrow } from "./recycleLayout";

export type PhaseGraphPhase = {
  name: string;
  kind: string;
  verify?: boolean;
  always?: boolean;
  evidence_verification_gate?: boolean;
  depends_on?: string[];
};

export type PhaseGraphProps = {
  phases: PhaseGraphPhase[];
  prEnabled: boolean;
  renderPhase?: (phase: PhaseGraphPhase) => ReactNode;
  renderTouchpoint?: () => ReactNode;
  phaseRef?: (phase: PhaseGraphPhase, el: HTMLDivElement | null) => void;
  dagClassName?: string;
  ariaLabel?: string;
  entryPhaseName?: string | null;
  recycleArrows?: RecycleArrow[];
};

type PhaseNodeData = {
  col: PhaseGraphPhase[];
  index: number;
  title: string;
  renderPhase: (phase: PhaseGraphPhase) => ReactNode;
  phaseRef?: (phase: PhaseGraphPhase, el: HTMLDivElement | null) => void;
  recycleTargets: number;
};

type TouchpointNodeData = {
  renderTouchpoint: () => ReactNode;
};

type GraphEdge = Edge & {
  pathOptions?: { borderRadius?: number; offset?: number };
};

const PHASE_WIDTH = 172;
const PHASE_X_GAP = 240;
const PHASE_Y = 12;
const JOB_HEIGHT = 70;
const PHASE_BASE_HEIGHT = 44;

function estimatedPhaseHeight(col: PhaseGraphPhase[]): number {
  return PHASE_BASE_HEIGHT + Math.max(1, col.length) * JOB_HEIGHT;
}

function computeDepths(phases: PhaseGraphPhase[]): Map<string, number> {
  const byName = new Map<string, PhaseGraphPhase>();
  phases.forEach((p) => byName.set(p.name, p));
  const depths = new Map<string, number>();
  const visit = (name: string, visiting: Set<string>): number => {
    if (depths.has(name)) return depths.get(name)!;
    if (visiting.has(name)) return 0;
    visiting.add(name);
    const phase = byName.get(name);
    if (!phase) return 0;
    let d = 0;
    for (const dep of phase.depends_on ?? []) {
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

function columnsFor(phases: PhaseGraphPhase[]): PhaseGraphPhase[][] {
  const depths = computeDepths(phases);
  const byDepth = new Map<number, PhaseGraphPhase[]>();
  let maxDepth = 0;
  for (const phase of phases) {
    const d = depths.get(phase.name) ?? 0;
    maxDepth = Math.max(maxDepth, d);
    const list = byDepth.get(d) ?? [];
    list.push(phase);
    byDepth.set(d, list);
  }
  const columns: PhaseGraphPhase[][] = [];
  for (let d = 0; d <= maxDepth; d += 1) columns.push(byDepth.get(d) ?? []);
  return columns;
}

function defaultPhaseNode(phase: PhaseGraphPhase): ReactNode {
  const meta = phase.evidence_verification_gate
    ? "verify-gate"
    : phase.always
      ? "always"
      : phase.verify
        ? "verify"
        : phase.kind;
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

function handleTop(index: number, count: number): string {
  if (count <= 1) return "72%";
  return `${64 + index * 18}%`;
}

function PhaseFlowNode({ data }: NodeProps<Node<PhaseNodeData>>) {
  return (
    <div
      className={`dag-phase dag-phase-column${data.col.length > 1 ? " dag-phase-parallel" : ""}`}
      ref={(el) => {
        for (const phase of data.col) data.phaseRef?.(phase, el);
      }}
    >
      <Handle id="advance-in" type="target" position={Position.Left} className="dag-rf-handle" />
      <Handle id="advance-out" type="source" position={Position.Right} className="dag-rf-handle" />
      <Handle id="recycle-out" type="source" position={Position.Bottom} className="dag-rf-handle" />
      {Array.from({ length: Math.max(1, data.recycleTargets) }).map((_, idx) => (
        <Handle
          key={idx}
          id={`recycle-in-${idx}`}
          type="target"
          position={Position.Left}
          className="dag-rf-handle"
          style={{ top: handleTop(idx, data.recycleTargets) }}
        />
      ))}
      <div className="dag-phase-head">
        <span className="dag-phase-title">{data.title}</span>
        <span className="dag-phase-kicker">phase</span>
      </div>
      <div className="dag-phase-body">
        {data.col.map((phase) => (
          <Fragment key={phase.name}>{data.renderPhase(phase)}</Fragment>
        ))}
      </div>
    </div>
  );
}

function TouchpointFlowNode({ data }: NodeProps<Node<TouchpointNodeData>>) {
  return (
    <div className="dag-terminal">
      <Handle id="advance-in" type="target" position={Position.Left} className="dag-rf-handle" />
      <Handle id="recycle-out" type="source" position={Position.Bottom} className="dag-rf-handle" />
      {data.renderTouchpoint()}
    </div>
  );
}

const nodeTypes = {
  phase: PhaseFlowNode,
  touchpoint: TouchpointFlowNode,
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
  recycleArrows = [],
}: PhaseGraphProps) {
  const columns = useMemo(() => columnsFor(phases), [phases]);
  const phaseToColumn = useMemo(() => {
    const map = new Map<string, number>();
    columns.forEach((col, idx) => col.forEach((phase) => map.set(phase.name, idx)));
    return map;
  }, [columns]);
  const recycleTargetCounts = useMemo(() => {
    const counts = new Map<string, number>();
    for (const arrow of recycleArrows) {
      counts.set(arrow.target, (counts.get(arrow.target) ?? 0) + 1);
    }
    return counts;
  }, [recycleArrows]);

  const nodes = useMemo<Node[]>(() => {
    const maxPhaseHeight = Math.max(...columns.map(estimatedPhaseHeight), estimatedPhaseHeight([]));
    const phaseNodes: Node<PhaseNodeData>[] = columns.map((col, idx) => ({
      id: `phase:${idx}`,
      type: "phase",
      position: {
        x: idx * PHASE_X_GAP,
        y: PHASE_Y + (maxPhaseHeight - estimatedPhaseHeight(col)) / 2,
      },
      draggable: false,
      selectable: false,
      data: {
        col,
        index: idx,
        title: col.length > 1 ? `phase ${idx + 1}` : (col[0]?.name ?? `phase ${idx + 1}`),
        renderPhase,
        phaseRef,
        recycleTargets: Math.max(...col.map((phase) => recycleTargetCounts.get(phase.name) ?? 0), 0),
      },
    }));
    if (!prEnabled) return phaseNodes;
    return [
      ...phaseNodes,
      {
        id: "touchpoint",
        type: "touchpoint",
        position: {
          x: columns.length * PHASE_X_GAP,
          y: PHASE_Y + (maxPhaseHeight - 55) / 2,
        },
        draggable: false,
        selectable: false,
        data: { renderTouchpoint },
      } satisfies Node<TouchpointNodeData>,
    ];
  }, [columns, phaseRef, prEnabled, recycleTargetCounts, renderPhase, renderTouchpoint]);

  const edges = useMemo<GraphEdge[]>(() => {
    const out: GraphEdge[] = [];
    for (let idx = 0; idx < columns.length - 1; idx += 1) {
      const entry = columns[idx + 1].some((phase) => entryPhaseName != null && phase.name === entryPhaseName);
      out.push({
        id: `advance:${idx}:${idx + 1}`,
        source: `phase:${idx}`,
        sourceHandle: "advance-out",
        target: `phase:${idx + 1}`,
        targetHandle: "advance-in",
        type: "straight",
        markerEnd: { type: MarkerType.ArrowClosed },
        className: `dag-rf-edge${entry ? " entry" : ""}`,
      });
    }
    if (prEnabled && columns.length > 0) {
      out.push({
        id: "advance:touchpoint",
        source: `phase:${columns.length - 1}`,
        sourceHandle: "advance-out",
        target: "touchpoint",
        targetHandle: "advance-in",
        type: "straight",
        markerEnd: { type: MarkerType.ArrowClosed },
        className: "dag-rf-edge",
      });
    }

    const orderedByTarget = new Map<string, RecycleArrow[]>();
    for (const arrow of recycleArrows) {
      const list = orderedByTarget.get(arrow.target) ?? [];
      list.push(arrow);
      orderedByTarget.set(arrow.target, list);
    }
    orderedByTarget.forEach((arrows, target) => {
      const targetCol = phaseToColumn.get(target);
      if (targetCol == null) return;
      arrows
        .slice()
        .sort((a, b) => sourceOrder(a, phaseToColumn, columns.length) - sourceOrder(b, phaseToColumn, columns.length))
        .forEach((arrow, idx) => {
          const source = arrow.kind === "report_recycle" || arrow.source === "report"
            ? "touchpoint"
            : `phase:${phaseToColumn.get(arrow.source) ?? 0}`;
          out.push({
            id: `recycle:${arrow.source}:${arrow.target}:${idx}`,
            source,
            sourceHandle: "recycle-out",
            target: `phase:${targetCol}`,
            targetHandle: `recycle-in-${idx}`,
            type: "smoothstep",
            markerEnd: { type: MarkerType.ArrowClosed },
            className: `dag-rf-edge dag-rf-recycle${arrow.active ? " fired" : ""}${arrow.max_attempts <= 0 ? " policy-disabled" : ""}`,
            pathOptions: { borderRadius: 8, offset: 36 + idx * 16 },
            label: "",
          });
        });
    });
    return out;
  }, [columns, entryPhaseName, phaseToColumn, prEnabled, recycleArrows]);

  const graphHeight = Math.max(...columns.map(estimatedPhaseHeight), estimatedPhaseHeight([])) + 76;
  const graphWidth = (columns.length + (prEnabled ? 1 : 0)) * PHASE_X_GAP + PHASE_WIDTH;

  return (
    <div className={`dag dag-rf${dagClassName ? " " + dagClassName : ""}`} aria-label={ariaLabel}>
      <div className="dag-rf-surface" style={{ width: graphWidth, height: graphHeight }}>
        <ReactFlow
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          fitView={false}
          panOnDrag={false}
          zoomOnScroll={false}
          zoomOnPinch={false}
          zoomOnDoubleClick={false}
          preventScrolling={false}
          nodesDraggable={false}
          nodesConnectable={false}
          elementsSelectable={false}
          proOptions={{ hideAttribution: true }}
          style={{ width: "100%", height: "100%" }}
        />
      </div>
    </div>
  );
}

function sourceOrder(arrow: RecycleArrow, phaseToColumn: Map<string, number>, terminalIndex: number): number {
  if (arrow.kind === "report_recycle" || arrow.source === "report") return terminalIndex;
  return phaseToColumn.get(arrow.source) ?? 0;
}
