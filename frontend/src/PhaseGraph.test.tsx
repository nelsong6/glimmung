import { readFileSync } from "node:fs";
import { join } from "node:path";
import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { PhaseGraph } from "./PhaseGraph";

describe("PhaseGraph", () => {
  it("reserves routing space for recycle arrows that return to the first phase", async () => {
    const { container } = render(
      <PhaseGraph
        ariaLabel="workflow graph"
        phases={[
          { name: "env-prep", kind: "k8s_job", jobs: [{ id: "env-prep" }] },
          { name: "llm-work", kind: "k8s_job", depends_on: ["env-prep"], jobs: [{ id: "llm-work" }] },
          { name: "llm-verify", kind: "k8s_job", depends_on: ["llm-work"], jobs: [{ id: "llm-verify" }] },
          { name: "evidence-gate", kind: "k8s_job", depends_on: ["llm-verify"], jobs: [{ id: "evidence-gate" }] },
          { name: "env-destroy", kind: "k8s_job", always: true, depends_on: ["evidence-gate"], jobs: [{ id: "env-destroy" }] },
          { name: "touchpoint", kind: "k8s_job", always: true, depends_on: ["env-destroy"], jobs: [{ id: "pr-touchpoint", primitive: "pr_touchpoint" }] },
        ]}
        entryArrows={[{
          target: "env-prep",
          active: false,
          kind: "default",
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
    expect(container.querySelector('[data-id="touchpoint"]')).not.toBeInTheDocument();
    expect(container.querySelector('[data-id="phase:5"]')).toBeInTheDocument();
    const entryPath = await edgePathD(container, "rf__edge-entry:env-prep:0");
    const evidenceRecyclePath = await edgePathD(container, "rf__edge-recycle:evidence-gate:env-prep:0");
    const touchpointRecyclePath = await edgePathD(container, "rf__edge-recycle:touchpoint:env-prep:1");
    expect(entryPath).not.toContain(" C ");
    expect(entryPath).toContain(" Q ");
    expect(pathStart(entryPath).y).toBeGreaterThan(pathEnd(entryPath).y);
    expect(entryBendX(entryPath)).toBeCloseTo(outerVerticalX(touchpointRecyclePath) - 16);
    expect(pathsIntersect(evidenceRecyclePath, touchpointRecyclePath)).toBe(false);
    expect(lastSegment(touchpointRecyclePath)).toMatchObject({ from: { y: pathEnd(touchpointRecyclePath).y } });
    expect(pathEnd(touchpointRecyclePath).y).toBeLessThan(pathEnd(evidenceRecyclePath).y);
    expect(container.querySelector(".dag-rf-surface")).toHaveStyle({
      width: "1768px",
      height: "216px",
    });
  });

  it("keeps highlighted SVG edge paths unfilled", () => {
    const indexCss = readFileSync(join(process.cwd(), "src/index.css"), "utf8");
    expect(indexCss).toMatch(/\.dag-rf \.react-flow__edge-path\s*\{[^}]*fill:\s*none;/s);

    const highlightedPathRule = indexCss.match(
      /\.dag-rf \.react-flow__edge\.dag-rf-edge\.entry \.react-flow__edge-path,\s*\.dag-rf \.react-flow__edge\.dag-rf-edge\.fired \.react-flow__edge-path\s*\{([^}]*)\}/s,
    );
    expect(highlightedPathRule?.[1]).toContain("fill: none");
    expect(highlightedPathRule?.[1]).not.toContain("fill: var(--state-success-fg)");
  });
});

type Point = { x: number; y: number };
type Segment = { from: Point; to: Point };

async function edgePathD(container: HTMLElement, testId: string): Promise<string> {
  return waitFor(() => {
    const edge = Array.from(container.querySelectorAll<SVGGElement>("[data-testid]"))
      .find((el) => el.getAttribute("data-testid") === testId);
    const d = edge?.querySelector<SVGPathElement>(".react-flow__edge-path")?.getAttribute("d");
    expect(d).toBeTruthy();
    return d ?? "";
  });
}

function pathsIntersect(a: string, b: string): boolean {
  const aSegments = lineSegments(a);
  const bSegments = lineSegments(b);
  return aSegments.some((segmentA) => bSegments.some((segmentB) => segmentsIntersect(segmentA, segmentB)));
}

function lineSegments(path: string): Segment[] {
  const segments: Segment[] = [];
  let current: Point | null = null;
  const matcher = /([MLQC])\s*([^MLQC]+)/g;
  for (const match of path.matchAll(matcher)) {
    const nums = Array.from(match[2].matchAll(/-?\d+(?:\.\d+)?/g), (num) => Number(num[0]));
    if (match[1] === "M") {
      current = { x: nums[0], y: nums[1] };
      continue;
    }
    if (match[1] === "L" && current) {
      const next = { x: nums[0], y: nums[1] };
      segments.push({ from: current, to: next });
      current = next;
      continue;
    }
    if (match[1] === "Q" && current) {
      current = appendQuadraticSegments(segments, current, { x: nums[0], y: nums[1] }, { x: nums[2], y: nums[3] });
      continue;
    }
    if (match[1] === "C" && current) {
      current = appendCubicSegments(
        segments,
        current,
        { x: nums[0], y: nums[1] },
        { x: nums[2], y: nums[3] },
        { x: nums[4], y: nums[5] },
      );
    }
  }
  return segments;
}

function appendQuadraticSegments(segments: Segment[], start: Point, control: Point, end: Point): Point {
  let previous = start;
  for (let step = 1; step <= 8; step += 1) {
    const t = step / 8;
    const next = {
      x: ((1 - t) ** 2) * start.x + 2 * (1 - t) * t * control.x + (t ** 2) * end.x,
      y: ((1 - t) ** 2) * start.y + 2 * (1 - t) * t * control.y + (t ** 2) * end.y,
    };
    segments.push({ from: previous, to: next });
    previous = next;
  }
  return end;
}

function appendCubicSegments(segments: Segment[], start: Point, controlA: Point, controlB: Point, end: Point): Point {
  let previous = start;
  for (let step = 1; step <= 12; step += 1) {
    const t = step / 12;
    const next = {
      x: ((1 - t) ** 3) * start.x
        + 3 * ((1 - t) ** 2) * t * controlA.x
        + 3 * (1 - t) * (t ** 2) * controlB.x
        + (t ** 3) * end.x,
      y: ((1 - t) ** 3) * start.y
        + 3 * ((1 - t) ** 2) * t * controlA.y
        + 3 * (1 - t) * (t ** 2) * controlB.y
        + (t ** 3) * end.y,
    };
    segments.push({ from: previous, to: next });
    previous = next;
  }
  return end;
}

function lastSegment(path: string): Segment {
  const segments = lineSegments(path);
  expect(segments.length).toBeGreaterThan(0);
  return segments[segments.length - 1];
}

function pathEnd(path: string): Point {
  const nums = pathNumbers(path);
  return { x: nums[nums.length - 2], y: nums[nums.length - 1] };
}

function segmentsIntersect(a: Segment, b: Segment): boolean {
  const d1 = direction(a.from, a.to, b.from);
  const d2 = direction(a.from, a.to, b.to);
  const d3 = direction(b.from, b.to, a.from);
  const d4 = direction(b.from, b.to, a.to);
  if (((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) && ((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0))) return true;
  if (same(d1, 0) && pointOnSegment(b.from, a)) return true;
  if (same(d2, 0) && pointOnSegment(b.to, a)) return true;
  if (same(d3, 0) && pointOnSegment(a.from, b)) return true;
  if (same(d4, 0) && pointOnSegment(a.to, b)) return true;
  return false;
}

function pathStart(path: string): Point {
  const nums = pathNumbers(path);
  return { x: nums[0], y: nums[1] };
}

function entryBendX(path: string): number {
  const nums = pathNumbers(path);
  return nums[4];
}

function outerVerticalX(path: string): number {
  const nums = pathNumbers(path);
  return nums[10];
}

function pathNumbers(path: string): number[] {
  return Array.from(path.matchAll(/-?\d+(?:\.\d+)?/g), (num) => Number(num[0]));
}

function direction(a: Point, b: Point, c: Point): number {
  return (c.x - a.x) * (b.y - a.y) - (c.y - a.y) * (b.x - a.x);
}

function pointOnSegment(point: Point, segment: Segment): boolean {
  return point.x >= Math.min(segment.from.x, segment.to.x) - 0.001
    && point.x <= Math.max(segment.from.x, segment.to.x) + 0.001
    && point.y >= Math.min(segment.from.y, segment.to.y) - 0.001
    && point.y <= Math.max(segment.from.y, segment.to.y) + 0.001;
}

function same(a: number, b: number): boolean {
  return Math.abs(a - b) < 0.001;
}
