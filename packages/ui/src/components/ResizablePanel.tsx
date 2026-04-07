import { forwardRef, useCallback, useEffect, useRef, useState } from "react";
import { cn } from "../utils";

type Orientation = "horizontal" | "vertical";

interface ResizablePanelGroupProps extends React.HTMLAttributes<HTMLDivElement> {
  orientation?: Orientation;
}

/**
 * Container that lays out two children with a draggable divider between them.
 * Wrap exactly two `ResizablePanel` children and one `ResizablePanelHandle`.
 */
export const ResizablePanelGroup = forwardRef<
  HTMLDivElement,
  ResizablePanelGroupProps
>(function ResizablePanelGroup(
  { className, orientation = "horizontal", ...props },
  ref,
) {
  return (
    <div
      ref={ref}
      data-orientation={orientation}
      className={cn(
        "flex min-h-0 min-w-0",
        orientation === "horizontal" ? "flex-row" : "flex-col",
        className,
      )}
      {...props}
    />
  );
});

/* ------------------------------------------------------------------ */

interface ResizablePanelProps extends React.HTMLAttributes<HTMLDivElement> {
  /** Default size as a CSS value (e.g. "50%", "300px"). */
  defaultSize?: string;
  /** Minimum size in px. */
  minSize?: number;
  /** Maximum size in px. */
  maxSize?: number;
}

export const ResizablePanel = forwardRef<HTMLDivElement, ResizablePanelProps>(
  function ResizablePanel(
    { className, defaultSize, minSize, maxSize, style, ...props },
    ref,
  ) {
    return (
      <div
        ref={ref}
        className={cn("min-h-0 min-w-0 overflow-auto", className)}
        style={{
          flexBasis: defaultSize,
          flexGrow: defaultSize ? 0 : 1,
          flexShrink: defaultSize ? 0 : 1,
          minWidth: minSize,
          maxWidth: maxSize,
          ...style,
        }}
        {...props}
      />
    );
  },
);

/* ------------------------------------------------------------------ */

interface ResizablePanelHandleProps
  extends React.HTMLAttributes<HTMLDivElement> {
  orientation?: Orientation;
  onResize?: (delta: number) => void;
  /** Called when the drag ends (pointer up). Useful for persisting layout. */
  onResizeEnd?: () => void;
}

/**
 * Draggable divider between two `ResizablePanel`s.
 * Resizes the **previous** sibling by the pointer‐move delta.
 */
export const ResizablePanelHandle = forwardRef<
  HTMLDivElement,
  ResizablePanelHandleProps
>(function ResizablePanelHandle(
  { className, orientation = "horizontal", onResize, onResizeEnd, ...props },
  ref,
) {
  const innerRef = useRef<HTMLDivElement | null>(null);
  const [dragging, setDragging] = useState(false);
  const lastPos = useRef(0);

  const assignRef = useCallback(
    (el: HTMLDivElement | null) => {
      innerRef.current = el;
      if (typeof ref === "function") ref(el);
      else if (ref) (ref as React.MutableRefObject<HTMLDivElement | null>).current = el;
    },
    [ref],
  );

  const handlePointerDown = useCallback(
    (e: React.PointerEvent) => {
      e.preventDefault();
      setDragging(true);
      lastPos.current =
        orientation === "horizontal" ? e.clientX : e.clientY;
      (e.target as HTMLElement).setPointerCapture(e.pointerId);
    },
    [orientation],
  );

  const handlePointerMove = useCallback(
    (e: React.PointerEvent) => {
      if (!dragging) return;
      const current =
        orientation === "horizontal" ? e.clientX : e.clientY;
      const delta = current - lastPos.current;
      lastPos.current = current;

      const prev = innerRef.current?.previousElementSibling as HTMLElement | null;
      if (prev && delta !== 0) {
        const currentSize =
          orientation === "horizontal"
            ? prev.getBoundingClientRect().width
            : prev.getBoundingClientRect().height;
        const newSize = currentSize + delta;
        prev.style.flexBasis = `${newSize}px`;
        prev.style.flexGrow = "0";
        prev.style.flexShrink = "0";
        onResize?.(delta);
      }
    },
    [dragging, orientation, onResize],
  );

  const handlePointerUp = useCallback(() => {
    setDragging(false);
    onResizeEnd?.();
  }, [onResizeEnd]);

  useEffect(() => {
    if (!dragging) return;
    const up = () => setDragging(false);
    window.addEventListener("pointerup", up);
    return () => window.removeEventListener("pointerup", up);
  }, [dragging]);

  const isHorizontal = orientation === "horizontal";

  return (
    <div
      ref={assignRef}
      role="separator"
      aria-orientation={orientation}
      tabIndex={0}
      className={cn(
        "flex shrink-0 items-center justify-center",
        "bg-surface-200 dark:bg-dark-surface-400",
        "hover:bg-surface-300 dark:hover:bg-dark-surface-500",
        "active:bg-surface-400 dark:active:bg-dark-surface-600",
        "transition-colors",
        isHorizontal
          ? "w-1.5 cursor-col-resize"
          : "h-1.5 cursor-row-resize",
        dragging &&
          (isHorizontal
            ? "bg-surface-400 dark:bg-dark-surface-600"
            : "bg-surface-400 dark:bg-dark-surface-600"),
        className,
      )}
      onPointerDown={handlePointerDown}
      onPointerMove={handlePointerMove}
      onPointerUp={handlePointerUp}
      {...props}
    >
      <div
        className={cn(
          "rounded-full bg-surface-400 dark:bg-dark-surface-600",
          isHorizontal ? "h-6 w-0.5" : "h-0.5 w-6",
        )}
      />
    </div>
  );
});
