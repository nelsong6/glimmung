// Shared phase graph renderer. Both workflow-definition and run-detail
// views use this component so phase/job structure and edge semantics
// stay aligned.

import { Fragment, ReactNode, useLayoutEffect, useMemo, useRef, useState } from "react";
import {
  BaseEdge,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  type Edge,
  type EdgeProps,
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

type RecycleEdgeData = {
  laneIndex: number;
  laneBaseY: number;
};

type AdvanceGraphEdge = Edge & {
  type: "advance";
};

type GraphEdge = Edge<RecycleEdgeData>;

type RecycleGraphEdge = Edge<RecycleEdgeData> & {
  type: "recycle";
};

const PHASE_WIDTH = 172;
const PHASE_X_GAP = 240;
const PHASE_Y = 12;
const JOB_HEIGHT = 70;
const PHASE_BASE_HEIGHT = 44;
const TOUCHPOINT_FALLBACK_HEIGHT = 55;
const ENTRY_OFFSET_PERCENT = 13;

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
  if (count <= 1) return "50%";
  return `${50 + index * ENTRY_OFFSET_PERCENT}%`;
}

function PhaseFlowNode({ data }: NodeProps<Node<PhaseNodeData>>) {
  return (
    <div
      className={`dag-phase dag-phase-column${data.col.length > 1 ? " dag-phase-parallel" : ""}`}
      ref={(el) => {
        for (const phase of data.col) data.phaseRef?.(phase, el);
      }}
    >
      <Handle
        id="advance-in"
        type="target"
        position={Position.Left}
        className="dag-rf-handle"
        style={data.recycleTargets > 0 ? { top: `${50 - ENTRY_OFFSET_PERCENT}%` } : undefined}
      />
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

function RecycleFlowEdge({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  markerEnd,
  style,
  data,
  interactionWidth,
}: EdgeProps<RecycleGraphEdge>) {
  const laneIndex = data?.laneIndex ?? 0;
  const laneBaseY = data?.laneBaseY ?? Math.max(sourceY, targetY) + 30;
  const radius = 10;
  const laneY = laneBaseY + laneIndex * 24;
  const approachX = targetX - 30 - laneIndex * 16;
  const path = [
    `M ${sourceX},${sourceY}`,
    `L ${sourceX},${laneY - radius}`,
    `Q ${sourceX},${laneY} ${sourceX - radius},${laneY}`,
    `L ${approachX + radius},${laneY}`,
    `Q ${approachX},${laneY} ${approachX},${laneY - radius}`,
    `L ${approachX},${targetY + radius}`,
    `Q ${approachX},${targetY} ${approachX + radius},${targetY}`,
    `L ${targetX},${targetY}`,
  ].join(" ");

  return (
    <BaseEdge
      id={id}
      path={path}
      markerEnd={markerEnd}
      style={style}
      interactionWidth={interactionWidth}
    />
  );
}

function AdvanceFlowEdge({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  markerEnd,
  style,
  interactionWidth,
}: EdgeProps<AdvanceGraphEdge>) {
  const verticalDelta = Math.abs(sourceY - targetY);
  const path = verticalDelta < 1
    ? `M ${sourceX},${sourceY} L ${targetX},${targetY}`
    : `M ${sourceX},${sourceY} C ${sourceX + 44},${sourceY} ${targetX - 44},${targetY} ${targetX},${targetY}`;

  return (
    <BaseEdge
      id={id}
      path={path}
      markerEnd={markerEnd}
      style={style}
      interactionWidth={interactionWidth}
    />
  );
}

const edgeTypes = {
  advance: AdvanceFlowEdge,
  recycle: RecycleFlowEdge,
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
  const graphRef = useRef<HTMLDivElement | null>(null);
  const [nodeHeights, setNodeHeights] = useState<Record<string, number>>({});
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
    const phaseHeight = (idx: number, col: PhaseGraphPhase[]) => nodeHeights[`phase:${idx}`] ?? estimatedPhaseHeight(col);
    const measuredPhaseHeights = columns.map((col, idx) => phaseHeight(idx, col));
    const maxPhaseHeight = Math.max(...measuredPhaseHeights, estimatedPhaseHeight([]));
    const phaseNodes: Node<PhaseNodeData>[] = columns.map((col, idx) => ({
      id: `phase:${idx}`,
      type: "phase",
      position: {
        x: idx * PHASE_X_GAP,
        y: PHASE_Y + (maxPhaseHeight - phaseHeight(idx, col)) / 2,
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
          y: PHASE_Y + (maxPhaseHeight - (nodeHeights.touchpoint ?? TOUCHPOINT_FALLBACK_HEIGHT)) / 2,
        },
        draggable: false,
        selectable: false,
        data: { renderTouchpoint },
      } satisfies Node<TouchpointNodeData>,
    ];
  }, [columns, nodeHeights, phaseRef, prEnabled, recycleTargetCounts, renderPhase, renderTouchpoint]);

  const edges = useMemo<GraphEdge[]>(() => {
    const out: GraphEdge[] = [];
    const maxPhaseHeight = Math.max(
      ...columns.map((col, idx) => nodeHeights[`phase:${idx}`] ?? estimatedPhaseHeight(col)),
      estimatedPhaseHeight([]),
    );
    const recycleLaneBaseY = PHASE_Y + maxPhaseHeight + 8;
    for (let idx = 0; idx < columns.length - 1; idx += 1) {
      const entry = columns[idx + 1].some((phase) => entryPhaseName != null && phase.name === entryPhaseName);
      out.push({
        id: `advance:${idx}:${idx + 1}`,
        source: `phase:${idx}`,
        sourceHandle: "advance-out",
        target: `phase:${idx + 1}`,
        targetHandle: "advance-in",
        type: "advance",
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
        type: "advance",
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
      const orderedArrows = arrows
        .slice()
        .sort((a, b) => sourceOrder(a, phaseToColumn, columns.length) - sourceOrder(b, phaseToColumn, columns.length));
      const reportArrowIndex = orderedArrows.findIndex((arrow) => arrow.kind === "report_recycle" || arrow.source === "report");
      orderedArrows.forEach((arrow, idx) => {
          const targetHandleIndex = reportArrowIndex >= 0
            ? idx === reportArrowIndex
              ? 0
              : idx < reportArrowIndex
                ? idx + 1
                : idx
            : idx;
          const source = arrow.kind === "report_recycle" || arrow.source === "report"
            ? "touchpoint"
            : `phase:${phaseToColumn.get(arrow.source) ?? 0}`;
          out.push({
            id: `recycle:${arrow.source}:${arrow.target}:${idx}`,
            source,
            sourceHandle: "recycle-out",
            target: `phase:${targetCol}`,
            targetHandle: `recycle-in-${targetHandleIndex}`,
            type: "recycle",
            markerEnd: { type: MarkerType.ArrowClosed },
            className: `dag-rf-edge dag-rf-recycle${arrow.active ? " fired" : ""}${arrow.max_attempts <= 0 ? " policy-disabled" : ""}`,
            data: { laneIndex: idx, laneBaseY: recycleLaneBaseY },
            label: "",
          });
        });
    });
    return out;
  }, [columns, entryPhaseName, nodeHeights, phaseToColumn, prEnabled, recycleArrows]);

  useLayoutEffect(() => {
    const root = graphRef.current;
    if (!root) return;

    let raf = 0;
    const measure = () => {
      const next: Record<string, number> = {};
      for (let idx = 0; idx < columns.length; idx += 1) {
        const el = root.querySelector<HTMLElement>(`.react-flow__node[data-id="phase:${idx}"]`);
        if (el) next[`phase:${idx}`] = el.getBoundingClientRect().height;
      }
      const touchpoint = root.querySelector<HTMLElement>('.react-flow__node[data-id="touchpoint"]');
      if (touchpoint) next.touchpoint = touchpoint.getBoundingClientRect().height;

      setNodeHeights((current) => {
        const keys = new Set([...Object.keys(current), ...Object.keys(next)]);
        for (const key of keys) {
          if (Math.abs((current[key] ?? 0) - (next[key] ?? 0)) > 0.5) return next;
        }
        return current;
      });
    };

    raf = window.requestAnimationFrame(measure);
    const observer = new ResizeObserver(() => {
      window.cancelAnimationFrame(raf);
      raf = window.requestAnimationFrame(measure);
    });
    observer.observe(root);
    return () => {
      window.cancelAnimationFrame(raf);
      observer.disconnect();
    };
  }, [columns.length]);

  const graphHeight =
    Math.max(
      ...columns.map((col, idx) => nodeHeights[`phase:${idx}`] ?? estimatedPhaseHeight(col)),
      estimatedPhaseHeight([]),
    ) + 76;
  const graphWidth = (columns.length + (prEnabled ? 1 : 0)) * PHASE_X_GAP + PHASE_WIDTH;

  return (
    <div className={`dag dag-rf${dagClassName ? " " + dagClassName : ""}`} aria-label={ariaLabel}>
      <div ref={graphRef} className="dag-rf-surface" style={{ width: graphWidth, height: graphHeight }}>
        <ReactFlow
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          edgeTypes={edgeTypes}
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
