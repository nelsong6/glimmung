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

export type PhaseGraphPhase = {
  name: string;
  kind: string;
  verify?: boolean;
  always?: boolean;
  evidence_verification_gate?: boolean;
  depends_on?: string[];
  jobs?: PhaseGraphJob[];
};

export type PhaseGraphJob = {
  id: string;
  name?: string | null;
  image?: string;
  primitive?: string;
};

export type RecycleArrow = {
  source: string;
  target: string;
  trigger: string;
  max_attempts: number;
  active: boolean;
  kind: "phase_recycle" | "touchpoint_recycle";
};

export type EntryArrow = {
  target: string;
  active: boolean;
  kind: string;
};

export type PhaseGraphProps = {
  phases: PhaseGraphPhase[];
  renderPhase?: (phase: PhaseGraphPhase) => ReactNode;
  phaseRef?: (phase: PhaseGraphPhase, el: HTMLDivElement | null) => void;
  dagClassName?: string;
  ariaLabel?: string;
  selectedPhaseName?: string | null;
  onSelectPhase?: (phase: PhaseGraphPhase) => void;
  entryPhaseName?: string | null;
  entryArrows?: EntryArrow[];
  recycleArrows?: RecycleArrow[];
};

type PhaseNodeData = {
  col: PhaseGraphPhase[];
  index: number;
  title: string;
  renderPhase: (phase: PhaseGraphPhase) => ReactNode;
  phaseRef?: (phase: PhaseGraphPhase, el: HTMLDivElement | null) => void;
  selectedPhaseName?: string | null;
  onSelectPhase?: (phase: PhaseGraphPhase) => void;
  recycleTargets: number;
};

type EntrySourceNodeData = Record<string, never>;

type RecycleEdgeData = {
  laneIndex: number;
  laneBaseY: number;
};

type EntryEdgeData = {
  recycleTargets?: number;
};

type AdvanceGraphEdge = Edge & {
  type: "advance";
};

type GraphEdge = Edge<RecycleEdgeData | EntryEdgeData>;

type RecycleGraphEdge = Edge<RecycleEdgeData> & {
  type: "recycle";
};

type EntryGraphEdge = Edge<EntryEdgeData> & {
  type: "entry";
};

type GraphNodeHandle = NonNullable<Node["handles"]>[number];

const PHASE_WIDTH = 172;
const PHASE_X_GAP = 240;
const PHASE_Y = 12;
const JOB_HEIGHT = 70;
const PHASE_BASE_HEIGHT = 44;
const HANDLE_SIZE = 1;
const ADVANCE_HANDLE_WITH_RECYCLE_PERCENT = 34;
const RECYCLE_HANDLE_START_PERCENT = 54;
const RECYCLE_HANDLE_END_PERCENT = 86;
const RECYCLE_LANE_TOP_OFFSET = 24;
const RECYCLE_LANE_GAP = 24;
const RECYCLE_LANE_BOTTOM_PADDING = 42;
const RECYCLE_APPROACH_OFFSET = 30;
const RECYCLE_APPROACH_STAGGER = 16;
const RECYCLE_FIRST_COLUMN_GUTTER = 72;
const ENTRY_LEFT_GUTTER = 156;

function estimatedPhaseHeight(col: PhaseGraphPhase[]): number {
  const phase = col[0];
  const jobCount = Math.max(1, phase?.jobs?.length ?? 0);
  return PHASE_BASE_HEIGHT + jobCount * JOB_HEIGHT;
}

function columnsFor(phases: PhaseGraphPhase[]): PhaseGraphPhase[][] {
  return phases.map((phase) => [phase]);
}

function defaultPhaseNode(phase: PhaseGraphPhase): ReactNode {
  const meta = phase.evidence_verification_gate
    ? "verify-gate"
    : phase.always
      ? "always"
      : phase.verify
        ? "verify"
        : phase.kind;
  const jobs = phase.jobs && phase.jobs.length > 0
    ? phase.jobs
    : [{ id: phase.name, name: phase.name }];
  return (
    <>
      {jobs.map((job) => (
        <div className="dag-node dag-node-phase dag-node-definition" key={job.id}>
          <div className="dag-job-head">
            <span className="dag-job-title">{job.name || job.id}</span>
            <span className="dag-job-kicker">job</span>
          </div>
          <div className="dag-node-meta dim mono">{job.id === phase.name ? meta : job.id}</div>
        </div>
      ))}
    </>
  );
}

function advanceHandlePercent(recycleTargets: number): number {
  return recycleTargets > 0 ? ADVANCE_HANDLE_WITH_RECYCLE_PERCENT : 50;
}

function recycleHandleTopPercent(index: number, count: number): number {
  if (count <= 1) return 60;
  const usableRange = RECYCLE_HANDLE_END_PERCENT - RECYCLE_HANDLE_START_PERCENT;
  const step = Math.min(16, usableRange / Math.max(1, count - 1));
  return RECYCLE_HANDLE_START_PERCENT + index * step;
}

function centeredHandleOffset(size: number, percent: number): number {
  return size * (percent / 100) - HANDLE_SIZE / 2;
}

function graphNodeHandle(
  id: string,
  type: "source" | "target",
  position: Position,
  x: number,
  y: number,
): GraphNodeHandle {
  return { id, type, position, x, y, width: HANDLE_SIZE, height: HANDLE_SIZE };
}

function phaseHandles(height: number, recycleTargets: number): GraphNodeHandle[] {
  return [
    graphNodeHandle(
      "advance-in",
      "target",
      Position.Left,
      0,
      centeredHandleOffset(height, advanceHandlePercent(recycleTargets)),
    ),
    graphNodeHandle(
      "advance-out",
      "source",
      Position.Right,
      PHASE_WIDTH - HANDLE_SIZE,
      centeredHandleOffset(height, 50),
    ),
    graphNodeHandle(
      "recycle-out",
      "source",
      Position.Bottom,
      centeredHandleOffset(PHASE_WIDTH, 50),
      height - HANDLE_SIZE,
    ),
    ...Array.from({ length: Math.max(1, recycleTargets) }).map((_, idx) => graphNodeHandle(
      `recycle-in-${idx}`,
      "target",
      Position.Left,
      0,
      centeredHandleOffset(height, recycleHandleTopPercent(idx, recycleTargets)),
    )),
  ];
}

function PhaseFlowNode({ data }: NodeProps<Node<PhaseNodeData>>) {
  const hasParallelJobs = data.col.some((phase) => (phase.jobs?.length ?? 0) > 1);
  const selected = data.col.some((phase) => phase.name === data.selectedPhaseName);
  const primaryPhase = data.col[0] ?? null;
  const phaseHead = (
    <>
      <span className="dag-phase-title">{data.title}</span>
      <span className="dag-phase-kicker">phase</span>
    </>
  );
  return (
    <div
      className={`dag-phase dag-phase-column${hasParallelJobs ? " dag-phase-parallel" : ""}${selected ? " selected" : ""}`}
      ref={(el) => {
        for (const phase of data.col) data.phaseRef?.(phase, el);
      }}
    >
      <Handle
        id="advance-in"
        type="target"
        position={Position.Left}
        className="dag-rf-handle"
        style={{ top: `${advanceHandlePercent(data.recycleTargets)}%` }}
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
          style={{ top: `${recycleHandleTopPercent(idx, data.recycleTargets)}%` }}
        />
      ))}
      {data.onSelectPhase && primaryPhase ? (
        <button
          type="button"
          className={`dag-phase-head dag-phase-head-button${selected ? " selected" : ""}`}
          onClick={() => data.onSelectPhase?.(primaryPhase)}
          aria-pressed={selected}
        >
          {phaseHead}
        </button>
      ) : (
        <div className="dag-phase-head">{phaseHead}</div>
      )}
      <div className="dag-phase-body">
        {data.col.map((phase) => (
          <Fragment key={phase.name}>{data.renderPhase(phase)}</Fragment>
        ))}
      </div>
    </div>
  );
}

function EntrySourceFlowNode() {
  return (
    <div className="dag-rf-entry-source">
      <Handle id="entry-out" type="source" position={Position.Right} className="dag-rf-handle" />
    </div>
  );
}

const nodeTypes = {
  entrySource: EntrySourceFlowNode,
  phase: PhaseFlowNode,
};

function EntryFlowEdge({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  markerEnd,
  style,
  data,
  interactionWidth,
}: EdgeProps<EntryGraphEdge>) {
  const recycleTargets = data?.recycleTargets ?? 0;
  const path = entryPath(sourceX, sourceY, targetX, targetY, recycleTargets);

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

function entryPath(sourceX: number, sourceY: number, targetX: number, targetY: number, recycleTargets: number): string {
  const verticalDelta = Math.abs(sourceY - targetY);
  if (verticalDelta < 1) return `M ${sourceX},${sourceY} L ${targetX},${targetY}`;

  const outerLaneIndex = Math.max(0, recycleTargets - 1);
  const outerApproachX = targetX - RECYCLE_APPROACH_OFFSET - outerLaneIndex * RECYCLE_APPROACH_STAGGER;
  const bendX = Math.max(sourceX, outerApproachX - RECYCLE_APPROACH_STAGGER);
  const radius = Math.min(10, verticalDelta / 2, Math.abs(targetX - bendX) / 2);
  const verticalDirection = targetY > sourceY ? 1 : -1;

  return [
    `M ${sourceX},${sourceY}`,
    `L ${bendX - radius},${sourceY}`,
    `Q ${bendX},${sourceY} ${bendX},${sourceY + verticalDirection * radius}`,
    `L ${bendX},${targetY - verticalDirection * radius}`,
    `Q ${bendX},${targetY} ${bendX + radius},${targetY}`,
    `L ${targetX},${targetY}`,
  ].join(" ");
}

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
  const laneY = laneBaseY + laneIndex * RECYCLE_LANE_GAP;
  const approachX = targetX - RECYCLE_APPROACH_OFFSET - laneIndex * RECYCLE_APPROACH_STAGGER;
  const finalRadius = Math.min(radius, Math.abs(laneY - targetY) / 2, Math.abs(targetX - approachX) / 2);
  const path = [
    `M ${sourceX},${sourceY}`,
    `L ${sourceX},${laneY - radius}`,
    `Q ${sourceX},${laneY} ${sourceX - radius},${laneY}`,
    `L ${approachX + radius},${laneY}`,
    `Q ${approachX},${laneY} ${approachX},${laneY - radius}`,
    `L ${approachX},${targetY + finalRadius}`,
    `Q ${approachX},${targetY} ${approachX + finalRadius},${targetY}`,
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
  entry: EntryFlowEdge,
  recycle: RecycleFlowEdge,
};

export function PhaseGraph({
  phases,
  renderPhase = defaultPhaseNode,
  phaseRef,
  dagClassName,
  ariaLabel,
  selectedPhaseName = null,
  onSelectPhase,
  entryPhaseName = null,
  entryArrows = [],
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
  const visibleRecycleArrows = useMemo(
    () => recycleArrows.filter((arrow) => phaseToColumn.has(arrow.source) && phaseToColumn.has(arrow.target)),
    [phaseToColumn, recycleArrows],
  );
  const recycleTargetCounts = useMemo(() => {
    const counts = new Map<string, number>();
    for (const arrow of visibleRecycleArrows) {
      counts.set(arrow.target, (counts.get(arrow.target) ?? 0) + 1);
    }
    return counts;
  }, [visibleRecycleArrows]);
  const maxRecycleLanes = useMemo(
    () => Math.max(0, ...Array.from(recycleTargetCounts.values())),
    [recycleTargetCounts],
  );
  const firstColumnRecycleLanes = useMemo(() => {
    const firstColumn = columns[0] ?? [];
    return Math.max(0, ...firstColumn.map((phase) => recycleTargetCounts.get(phase.name) ?? 0));
  }, [columns, recycleTargetCounts]);
  const recycleLeftGutter = firstColumnRecycleLanes > 0
    ? RECYCLE_FIRST_COLUMN_GUTTER + (firstColumnRecycleLanes - 1) * RECYCLE_APPROACH_STAGGER
    : 0;
  const visibleEntryArrows = useMemo(
    () => entryArrows.filter((arrow) => phaseToColumn.has(arrow.target)),
    [entryArrows, phaseToColumn],
  );
  const entryLeftGutter = visibleEntryArrows.length > 0 ? ENTRY_LEFT_GUTTER : 0;
  const leftGutter = Math.max(recycleLeftGutter, entryLeftGutter);

  const nodes = useMemo<Node[]>(() => {
    const phaseHeight = (idx: number, col: PhaseGraphPhase[]) => nodeHeights[`phase:${idx}`] ?? estimatedPhaseHeight(col);
    const measuredPhaseHeights = columns.map((col, idx) => phaseHeight(idx, col));
    const maxPhaseHeight = Math.max(...measuredPhaseHeights, estimatedPhaseHeight([]));
    const entryNodes: Node<EntrySourceNodeData>[] = visibleEntryArrows.map((arrow, idx) => {
      const targetCol = phaseToColumn.get(arrow.target) ?? 0;
      const col = columns[targetCol] ?? [];
      const targetHeight = phaseHeight(targetCol, col);
      const sourceY = PHASE_Y
        + (maxPhaseHeight - targetHeight) / 2
        + targetHeight / 2;
      return {
        id: `entry-source:${idx}`,
        type: "entrySource",
        position: {
          x: 0,
          y: sourceY - 0.5,
        },
        initialWidth: 1,
        initialHeight: 1,
        handles: [graphNodeHandle("entry-out", "source", Position.Right, 0, 0)],
        style: { width: 1, height: 1, pointerEvents: "none" },
        draggable: false,
        selectable: false,
        data: {},
      };
    });
    const phaseNodes: Node<PhaseNodeData>[] = columns.map((col, idx) => {
      const height = phaseHeight(idx, col);
      const recycleTargets = Math.max(...col.map((phase) => recycleTargetCounts.get(phase.name) ?? 0), 0);
      return {
        id: `phase:${idx}`,
        type: "phase",
        position: {
          x: leftGutter + idx * PHASE_X_GAP,
          y: PHASE_Y + (maxPhaseHeight - height) / 2,
        },
        initialWidth: PHASE_WIDTH,
        initialHeight: height,
        handles: phaseHandles(height, recycleTargets),
        style: { pointerEvents: "all" },
        draggable: false,
        selectable: false,
        data: {
          col,
          index: idx,
          title: col.length > 1 ? `phase ${idx + 1}` : (col[0]?.name ?? `phase ${idx + 1}`),
          renderPhase,
          phaseRef,
          selectedPhaseName,
          onSelectPhase,
          recycleTargets,
        },
      };
    });
    return [...entryNodes, ...phaseNodes];
  }, [columns, leftGutter, nodeHeights, onSelectPhase, phaseRef, phaseToColumn, recycleTargetCounts, renderPhase, selectedPhaseName, visibleEntryArrows]);

  const edges = useMemo<GraphEdge[]>(() => {
    const out: GraphEdge[] = [];
    const maxPhaseHeight = Math.max(
      ...columns.map((col, idx) => nodeHeights[`phase:${idx}`] ?? estimatedPhaseHeight(col)),
      estimatedPhaseHeight([]),
    );
    const recycleLaneBaseY = PHASE_Y + maxPhaseHeight + RECYCLE_LANE_TOP_OFFSET;
    visibleEntryArrows.forEach((arrow, idx) => {
      const targetCol = phaseToColumn.get(arrow.target);
      if (targetCol == null) return;
      const col = columns[targetCol] ?? [];
      const recycleTargets = Math.max(...col.map((phase) => recycleTargetCounts.get(phase.name) ?? 0), 0);
      out.push({
        id: `entry:${arrow.target}:${idx}`,
        source: `entry-source:${idx}`,
        sourceHandle: "entry-out",
        target: `phase:${targetCol}`,
        targetHandle: "advance-in",
        type: "entry",
        markerEnd: { type: MarkerType.ArrowClosed },
        className: "dag-rf-edge dag-rf-entry-edge",
        data: { recycleTargets },
      });
    });
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
    const orderedByTarget = new Map<string, RecycleArrow[]>();
    for (const arrow of visibleRecycleArrows) {
      const list = orderedByTarget.get(arrow.target) ?? [];
      list.push(arrow);
      orderedByTarget.set(arrow.target, list);
    }
    orderedByTarget.forEach((arrows, target) => {
      const targetCol = phaseToColumn.get(target);
      if (targetCol == null) return;
      const orderedArrows = arrows
        .slice()
        .sort((a, b) => sourceOrder(a, phaseToColumn) - sourceOrder(b, phaseToColumn));
      orderedArrows.forEach((arrow, idx) => {
        const sourceCol = phaseToColumn.get(arrow.source);
        if (sourceCol == null) return;
        const targetSlotIndex = Math.max(0, orderedArrows.length - 1 - idx);
        out.push({
          id: `recycle:${arrow.source}:${arrow.target}:${idx}`,
          source: `phase:${sourceCol}`,
          sourceHandle: "recycle-out",
          target: `phase:${targetCol}`,
          targetHandle: `recycle-in-${targetSlotIndex}`,
          type: "recycle",
          markerEnd: { type: MarkerType.ArrowClosed },
          className: `dag-rf-edge dag-rf-recycle${arrow.active ? " fired" : ""}`,
          data: { laneIndex: idx, laneBaseY: recycleLaneBaseY },
          label: "",
        });
      });
    });
    return out;
  }, [columns, entryPhaseName, nodeHeights, phaseToColumn, visibleEntryArrows, visibleRecycleArrows]);

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

  const maxPhaseHeight = Math.max(
    ...columns.map((col, idx) => nodeHeights[`phase:${idx}`] ?? estimatedPhaseHeight(col)),
    estimatedPhaseHeight([]),
  );
  const recycleBottomRoom = maxRecycleLanes > 0
    ? RECYCLE_LANE_TOP_OFFSET + (maxRecycleLanes - 1) * RECYCLE_LANE_GAP + RECYCLE_LANE_BOTTOM_PADDING
    : 64;
  const graphHeight = PHASE_Y + maxPhaseHeight + Math.max(64, recycleBottomRoom);
  const graphWidth = leftGutter + columns.length * PHASE_X_GAP + PHASE_WIDTH;

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

function sourceOrder(arrow: RecycleArrow, phaseToColumn: Map<string, number>): number {
  return phaseToColumn.get(arrow.source) ?? 0;
}
