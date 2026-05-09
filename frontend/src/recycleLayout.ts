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

const RECYCLE_LANE_HEIGHT = 28;
const RECYCLE_BAND_TOP_PAD = 8;
const RECYCLE_BAND_BOTTOM_PAD = 8;
const RECYCLE_TARGET_OVERSHOOT = 28;
const RECYCLE_ENTRY_ARC_RADIUS = 44;
const RECYCLE_ENTRY_ARC_SWEEP = 12;
const RECYCLE_TARGET_PORT_GAP = 16;
const RECYCLE_APPROACH_GAP = 12;
const RECYCLE_CORNER_RADIUS = 6;

type Point = { x: number; y: number };

function roundedOrthogonalPath(points: Point[], radius = RECYCLE_CORNER_RADIUS): string {
  if (points.length === 0) return "";
  if (points.length === 1) return `M ${points[0].x} ${points[0].y}`;
  const parts = [`M ${points[0].x} ${points[0].y}`];
  for (let i = 1; i < points.length - 1; i += 1) {
    const prev = points[i - 1];
    const curr = points[i];
    const next = points[i + 1];
    const inLen = Math.hypot(curr.x - prev.x, curr.y - prev.y);
    const outLen = Math.hypot(next.x - curr.x, next.y - curr.y);
    const r = Math.min(radius, inLen / 2, outLen / 2);
    if (r <= 0) {
      parts.push(`L ${curr.x} ${curr.y}`);
      continue;
    }
    const before = {
      x: curr.x + ((prev.x - curr.x) / inLen) * r,
      y: curr.y + ((prev.y - curr.y) / inLen) * r,
    };
    const after = {
      x: curr.x + ((next.x - curr.x) / outLen) * r,
      y: curr.y + ((next.y - curr.y) / outLen) * r,
    };
    parts.push(`L ${before.x} ${before.y}`);
    parts.push(`Q ${curr.x} ${curr.y} ${after.x} ${after.y}`);
  }
  const last = points[points.length - 1];
  parts.push(`L ${last.x} ${last.y}`);
  return parts.join(" ");
}

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
      const maxOffset = Math.min(RECYCLE_ENTRY_ARC_RADIUS - 6, Math.max(0, r.t.height / 2 - 12));
      const offset = Math.min(maxOffset, (targetPortIndex + 1) * RECYCLE_TARGET_PORT_GAP);
      const arcCenterX = r.t.left - RECYCLE_ENTRY_ARC_RADIUS;
      const pointOnEntryArc = (arcOffset: number) => ({
        x: arcCenterX + Math.sqrt(Math.max(0, RECYCLE_ENTRY_ARC_RADIUS ** 2 - arcOffset ** 2)),
        y: r.t.cy + arcOffset,
      });
      const end = pointOnEntryArc(offset);
      const arcStart = pointOnEntryArc(Math.min(maxOffset, offset + RECYCLE_ENTRY_ARC_SWEEP));
      const cornerX = r.t.left - RECYCLE_TARGET_OVERSHOOT - targetPortIndex * RECYCLE_APPROACH_GAP;
      const approach = roundedOrthogonalPath([
        { x: r.sX, y: r.sY },
        { x: r.sX, y: r.laneY },
        { x: cornerX, y: r.laneY },
        { x: cornerX, y: arcStart.y },
        arcStart,
      ]);
      const d = `${approach} A ${RECYCLE_ENTRY_ARC_RADIUS} ${RECYCLE_ENTRY_ARC_RADIUS} 0 0 0 ${end.x} ${end.y}`;
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
