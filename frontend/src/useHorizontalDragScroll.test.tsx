import { cleanup, fireEvent, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { useHorizontalDragScroll } from "./useHorizontalDragScroll";

afterEach(() => cleanup());

function Harness({ onPick }: { onPick: () => void }) {
  const { ref, onClickCapture } = useHorizontalDragScroll<HTMLDivElement>();
  return (
    <div ref={ref} data-testid="surface" onClickCapture={onClickCapture}>
      <button type="button" onClick={onPick}>
        node
      </button>
    </div>
  );
}

describe("useHorizontalDragScroll", () => {
  it("pans scrollLeft horizontally on left-button drag", () => {
    const onPick = vi.fn();
    const { getByTestId } = render(<Harness onPick={onPick} />);
    const surface = getByTestId("surface");
    surface.scrollLeft = 0;

    fireEvent.mouseDown(surface, { button: 0, clientX: 200 });
    fireEvent.mouseMove(window, { clientX: 140 });

    // dragged left by 60px -> content moves right -> scrollLeft increases.
    expect(surface.scrollLeft).toBe(60);
    expect(surface.classList.contains("dragging")).toBe(true);

    fireEvent.mouseUp(window);
    expect(surface.classList.contains("dragging")).toBe(false);
  });

  it("swallows the trailing click so a drag does not select a node", () => {
    const onPick = vi.fn();
    const { getByTestId, getByText } = render(<Harness onPick={onPick} />);
    const surface = getByTestId("surface");

    fireEvent.mouseDown(surface, { button: 0, clientX: 100 });
    fireEvent.mouseMove(window, { clientX: 40 });
    fireEvent.mouseUp(window);

    fireEvent.click(getByText("node"));
    expect(onPick).not.toHaveBeenCalled();
  });

  it("lets a plain click through when there was no drag", () => {
    const onPick = vi.fn();
    const { getByText } = render(<Harness onPick={onPick} />);

    fireEvent.click(getByText("node"));
    expect(onPick).toHaveBeenCalledTimes(1);
  });

  it("ignores non-left buttons", () => {
    const onPick = vi.fn();
    const { getByTestId } = render(<Harness onPick={onPick} />);
    const surface = getByTestId("surface");
    surface.scrollLeft = 10;

    fireEvent.mouseDown(surface, { button: 2, clientX: 200 });
    fireEvent.mouseMove(window, { clientX: 100 });
    expect(surface.scrollLeft).toBe(10);
  });
});
