import { getSmoothStepPath, Position } from "@xyflow/react";

export type RecycleArrow = {
  source: string;
  target: string;
  trigger: string;
  max_attempts: number;
  active: boolean;
  kind: "phase_recycle" | "report_recycle";
};

export type RecyclePathLayout = {
  arrow: RecycleArrow;
  d: string;
  cls: string;
  title: string;
  markerEnd?: boolean;
};

const RECYCLE_LANE_HEIGHT = 34;
const RECYCLE_BAND_TOP_PAD = 8;
const RECYCLE_BAND_BOTTOM_PAD = 8;
const RECYCLE_TARGET_ENTRY_OFFSET = 42;
const RECYCLE_TARGET_PORT_GAP = 24;
const RECYCLE_CORNER_RADIUS = 6;

export function computeRecyclePaths(
  arrows: RecycleArrow[],
  phaseRects: Map<string, DOMRect>,
  tpRect: DOMRect | null,
  bandLeft: number,
  bandTop: number,
): { paths: RecyclePathLayout[]; bandHeight: number } {
  const local = (rect: DOMRect) => ({
    left: rect.left - bandLeft,
    right: rect.right - bandLeft,
    top: rect.top - bandTop,
    bottom: rect.bottom - bandTop,
    cx: (rect.left + rect.right) / 2 - bandLeft,
    cy: (rect.top + rect.bottom) / 2 - bandTop,
    height: rect.height,
  });
  const sourceRectFor = (arrow: RecycleArrow): DOMRect | null => {
    if (arrow.kind === "report_recycle" || arrow.source === "report") return tpRect;
    return phaseRects.get(arrow.source) ?? null;
  };
  const targetRectFor = (arrow: RecycleArrow): DOMRect | null => {
    return phaseRects.get(arrow.target) ?? null;
  };

  const renderable = arrows
    .map((arrow) => {
      const sRaw = sourceRectFor(arrow);
      const tRaw = targetRectFor(arrow);
      if (!sRaw || !tRaw) return null;
      return { arrow, s: local(sRaw), t: local(tRaw) };
    })
    .filter((x): x is { arrow: RecycleArrow; s: ReturnType<typeof local>; t: ReturnType<typeof local> } => x !== null);

  // Lane assignment: shorter horizontal spans get inner lanes (closer
  // to the row), so a self-loop sits tighter under its node than a
  // long cross-phase loop draped underneath it.
  renderable.sort((a, b) => {
    const aSpan = Math.abs(a.s.cx - a.t.left);
    const bSpan = Math.abs(b.s.cx - b.t.left);
    return aSpan - bSpan;
  });

  const routed = renderable.map((r, lane) => {
    const { s } = r;
    const laneY = RECYCLE_BAND_TOP_PAD + (lane + 0.5) * RECYCLE_LANE_HEIGHT;
    const sX = s.cx;
    const sY = s.bottom;
    const inactive = r.arrow.max_attempts <= 0;
    const cls = [
      "dag-recycle-path",
      r.arrow.active ? "fired" : "registered",
      inactive ? "inactive" : "active",
    ].join(" ");
    const trigger = r.arrow.trigger || "recycle";
    return {
      ...r,
      lane,
      laneY,
      sX,
      sY,
      inactive,
      cls,
      title: `${r.arrow.source} ↻ ${r.arrow.target}: ${trigger}; ${
        inactive ? "no retries (max_attempts: 0)" : `max ${r.arrow.max_attempts}`
      }`,
    };
  });

  const byTarget = new Map<string, typeof routed>();
  for (const r of routed) {
    const list = byTarget.get(r.arrow.target) ?? [];
    list.push(r);
    byTarget.set(r.arrow.target, list);
  }

  const paths: RecyclePathLayout[] = [];
  for (const group of byTarget.values()) {
    group.sort((a, b) => a.lane - b.lane);
    group.forEach((r, targetPortIndex) => {
      const maxOffset = Math.max(0, r.t.height / 2 - 12);
      const targetOffset = Math.min(
        maxOffset,
        RECYCLE_TARGET_ENTRY_OFFSET + targetPortIndex * RECYCLE_TARGET_PORT_GAP,
      );
      const [d] = getSmoothStepPath({
        sourceX: r.sX,
        sourceY: r.sY,
        sourcePosition: Position.Bottom,
        targetX: r.t.left,
        targetY: r.t.cy + targetOffset,
        targetPosition: Position.Left,
        borderRadius: RECYCLE_CORNER_RADIUS,
        offset: 34 + targetPortIndex * 18,
        centerY: r.laneY,
      });
      paths.push({
        arrow: r.arrow,
        d,
        cls: r.cls,
        title: r.title,
        markerEnd: true,
      });
    });
  }

  const bandHeight = renderable.length === 0
    ? 0
    : RECYCLE_BAND_TOP_PAD + RECYCLE_BAND_BOTTOM_PAD + renderable.length * RECYCLE_LANE_HEIGHT;
  return { paths, bandHeight };
}
