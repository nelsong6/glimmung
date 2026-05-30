/**
 * Left-click-drag horizontal panning for an overflow-x scroll container.
 *
 * The run-detail DAG (`ProjectionPipelineDag`) lays phases out left-to-right
 * and routinely overflows the viewport. Wheel/scrollbar scrolling works, but
 * grabbing the surface and dragging it is the expected way to move a wide
 * graph around. This hook wires that gesture onto any element that scrolls
 * horizontally.
 *
 * Returns a `ref` to attach to the scrollable element and an `onClickCapture`
 * handler that swallows the click synthesized at the end of a drag, so a pan
 * gesture never doubles as a node selection. Horizontal only by design — the
 * vertical axis is intentionally left alone.
 */
import { useCallback, useEffect, useRef } from "react";
import type React from "react";

const DRAG_THRESHOLD_PX = 4;

export function useHorizontalDragScroll<T extends HTMLElement>() {
  const ref = useRef<T | null>(null);
  // True once a press has moved past the drag threshold. Read by the click
  // capture handler to cancel the trailing click, reset on the next press.
  const movedRef = useRef(false);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;

    let dragging = false;
    let startX = 0;
    let startScrollLeft = 0;

    const onMouseMove = (e: MouseEvent) => {
      if (!dragging) return;
      const dx = e.clientX - startX;
      if (!movedRef.current && Math.abs(dx) > DRAG_THRESHOLD_PX) {
        movedRef.current = true;
        el.classList.add("dragging");
      }
      if (movedRef.current) {
        el.scrollLeft = startScrollLeft - dx;
        // Suppress text selection / native drag while panning.
        e.preventDefault();
      }
    };

    const endDrag = () => {
      if (!dragging) return;
      dragging = false;
      el.classList.remove("dragging");
      window.removeEventListener("mousemove", onMouseMove);
      window.removeEventListener("mouseup", endDrag);
    };

    const onMouseDown = (e: MouseEvent) => {
      // Left button only; ignore middle/right and modifier-driven gestures.
      if (e.button !== 0) return;
      dragging = true;
      movedRef.current = false;
      startX = e.clientX;
      startScrollLeft = el.scrollLeft;
      window.addEventListener("mousemove", onMouseMove);
      window.addEventListener("mouseup", endDrag);
    };

    el.addEventListener("mousedown", onMouseDown);
    return () => {
      el.removeEventListener("mousedown", onMouseDown);
      window.removeEventListener("mousemove", onMouseMove);
      window.removeEventListener("mouseup", endDrag);
    };
  }, []);

  const onClickCapture = useCallback((e: React.MouseEvent) => {
    if (movedRef.current) {
      e.preventDefault();
      e.stopPropagation();
      movedRef.current = false;
    }
  }, []);

  return { ref, onClickCapture };
}
