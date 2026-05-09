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

const RECYCLE_LANE_HEIGHT = 18;
const RECYCLE_BAND_TOP_PAD = 8;
const RECYCLE_BAND_BOTTOM_PAD = 8;
const RECYCLE_TARGET_OVERSHOOT = 14;

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
    const { s, t } = r;
    const laneY = RECYCLE_BAND_TOP_PAD + (lane + 0.5) * RECYCLE_LANE_HEIGHT;
    const sX = s.cx;
    const sY = s.bottom;
    const cornerX = t.left - RECYCLE_TARGET_OVERSHOOT;
    const tX = t.left;
    const tY = t.cy;
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
      cornerX,
      tX,
      tY,
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
    if (group.length === 1) {
      const r = group[0];
      const d = [
        `M ${r.sX} ${r.sY}`,
        `L ${r.sX} ${r.laneY}`,
        `L ${r.cornerX} ${r.laneY}`,
        `L ${r.cornerX} ${r.tY}`,
        `L ${r.tX} ${r.tY}`,
      ].join(" ");
      paths.push({
        arrow: r.arrow,
        d,
        cls: r.cls,
        title: r.title,
        markerEnd: true,
      });
      continue;
    }

    const busX = Math.min(...group.map((r) => r.cornerX));
    const target = group[0];
    for (const r of group) {
      const d = [
        `M ${r.sX} ${r.sY}`,
        `L ${r.sX} ${r.laneY}`,
        `L ${busX} ${r.laneY}`,
      ].join(" ");
      paths.push({
        arrow: r.arrow,
        d,
        cls: r.cls,
        title: r.title,
        markerEnd: false,
      });
    }

    const minY = Math.min(target.tY, ...group.map((r) => r.laneY));
    const maxY = Math.max(target.tY, ...group.map((r) => r.laneY));
    const anyActive = group.some((r) => r.arrow.active);
    const allInactive = group.every((r) => r.inactive);
    const bundleCls = [
      "dag-recycle-path",
      anyActive ? "fired" : "registered",
      allInactive ? "inactive" : "active",
    ].join(" ");
    const sources = group.map((r) => r.arrow.source).join(", ");
    const d = [
      `M ${busX} ${maxY}`,
      `L ${busX} ${minY}`,
      `L ${target.tX} ${target.tY}`,
    ].join(" ");
    paths.push({
      arrow: target.arrow,
      d,
      cls: bundleCls,
      title: `${sources} ↻ ${target.arrow.target}: bundled phase entry`,
      markerEnd: true,
    });
  }

  const bandHeight = renderable.length === 0
    ? 0
    : RECYCLE_BAND_TOP_PAD + RECYCLE_BAND_BOTTOM_PAD + renderable.length * RECYCLE_LANE_HEIGHT;
  return { paths, bandHeight };
}
