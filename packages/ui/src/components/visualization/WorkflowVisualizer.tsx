import React, {
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  PropsWithChildren,
} from "react";

type Bounds = { x: number; y: number; width: number; height: number };

function cn(...xs: Array<string | false | null | undefined>) {
  return xs.filter(Boolean).join(" ");
}

export interface WorkflowVisualizerProps extends PropsWithChildren {
  debug?: boolean;
  height?: number | string;
  contentBounds: Bounds;
  initialZoom?: number;
  className?: string;
  scrollOnOverflow?: boolean;
}

export const WorkflowVisualizer: React.FC<WorkflowVisualizerProps> = ({
  debug = false,
  height,
  contentBounds,
  initialZoom = 1,
  className,
  children,
  scrollOnOverflow = false,
}) => {
  const containerRef = useRef<HTMLDivElement>(null);
  const svgRef = useRef<SVGSVGElement>(null);

  const [containerPx, setContainerPx] = useState({ w: 0, h: 0 });
  const [zoom, setZoom] = useState(initialZoom);
  const [viewBox, setViewBox] = useState<Bounds>(() => ({
    x: 0,
    y: 0,
    width: 100,
    height: 100,
  }));

  const userAdjustedRef = useRef(false);

  useLayoutEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const measure = () => {
      const r = el.getBoundingClientRect();
      setContainerPx({
        w: Math.max(r.width | 0, 0),
        h: Math.max(r.height | 0, 0),
      });
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const PAD = 35;
  const fitBox: Bounds = {
    x: contentBounds.x - PAD,
    y: contentBounds.y - PAD,
    width: Math.max(1, contentBounds.width + PAD * 2),
    height: Math.max(1, contentBounds.height + PAD * 2),
  };

  useEffect(() => {
    if (scrollOnOverflow) return;
    if (userAdjustedRef.current) return;
    setViewBox(fitBox);
  }, [fitBox.x, fitBox.y, fitBox.width, fitBox.height, scrollOnOverflow]);

  const gridId = useMemo(
    () => `grid-${Math.random().toString(36).slice(2)}`,
    [],
  );

  const DebugViewBoxRect = debug ? (
    <rect
      x={viewBox.x}
      y={viewBox.y}
      width={viewBox.width}
      height={viewBox.height}
      fill="none"
      stroke="blue"
      strokeWidth={1}
      pointerEvents="none"
    />
  ) : null;

  const DebugContentRect = debug ? (
    <rect
      x={contentBounds.x}
      y={contentBounds.y}
      width={contentBounds.width}
      height={contentBounds.height}
      fill="none"
      stroke="orange"
      strokeDasharray="4 3"
      strokeWidth={1}
      pointerEvents="none"
    />
  ) : null;

  const containerRing = debug ? "ring-2 ring-fuchsia-500" : "";

  const zoomIn = () => {
    userAdjustedRef.current = true;
    setZoom((z) => Math.min(z * 1.2, 8));
  };
  const zoomOut = () => {
    userAdjustedRef.current = true;
    setZoom((z) => Math.max(z / 1.2, 0.05));
  };
  const resetZoom = () => {
    userAdjustedRef.current = false;
    setZoom(1);
    if (!scrollOnOverflow) setViewBox(fitBox);
  };

  const zoomForRender = scrollOnOverflow ? 1 : zoom;
  const sceneW = scrollOnOverflow
    ? Math.max(1, fitBox.width * zoom)
    : undefined;
  const sceneH = scrollOnOverflow
    ? Math.max(1, fitBox.height * zoom)
    : undefined;

  return (
    <div className="relative flex h-full min-h-0 flex-col">
      {/* Top bar */}
      <div className="z-10 flex items-center justify-between border-b border-surface-300 dark:border-dark-surface-300 py-2 px-3">
        <h3 className="flex items-center gap-2 text-lg font-semibold text-text dark:text-dark-text">
          <svg
            xmlns="http://www.w3.org/2000/svg"
            width="18"
            height="18"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
            className="lucide lucide-workflow h-5 w-5"
          >
            <rect width="8" height="8" x="3" y="3" rx="2" />
            <path d="M7 11v4a2 2 0 0 0 2 2h4" />
            <rect width="8" height="8" x="13" y="13" rx="2" />
          </svg>
          Workflow
        </h3>
        <div className="flex items-center gap-2">
          <button
            onClick={zoomOut}
            className="inline-flex items-center rounded-lg p-2.5 hover:bg-surface-200 dark:hover:bg-dark-surface-200"
            aria-label="Zoom out"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              width="18"
              height="18"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              className="lucide lucide-zoom-out h-4 w-4"
            >
              <circle cx="11" cy="11" r="8" />
              <line x1="21" x2="16.65" y1="21" y2="16.65" />
              <line x1="8" x2="14" y1="11" y2="11" />
            </svg>
          </button>
          <span className="w-12 text-center text-sm font-medium tabular-nums">
            {Math.round(zoom * 100)}%
          </span>
          <button
            onClick={zoomIn}
            className="inline-flex items-center rounded-lg p-2.5 hover:bg-surface-200 dark:hover:bg-dark-surface-200"
            aria-label="Zoom in"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              width="18"
              height="18"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              className="lucide lucide-zoom-in h-4 w-4"
            >
              <circle cx="11" cy="11" r="8" />
              <line x1="21" x2="16.65" y1="21" y2="16.65" />
              <line x1="11" x2="11" y1="8" y2="14" />
              <line x1="8" x2="14" y1="11" y2="11" />
            </svg>
          </button>
          <button
            onClick={resetZoom}
            className="inline-flex items-center rounded-lg p-2.5 hover:bg-surface-200 dark:hover:bg-dark-surface-200"
            aria-label="Reset view"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              width="18"
              height="18"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              className="lucide lucide-maximize2 h-4 w-4"
            >
              <polyline points="15 3 21 3 21 9" />
              <polyline points="9 21 3 21 3 15" />
              <line x1="21" x2="14" y1="3" y2="10" />
              <line x1="3" x2="10" y1="21" y2="14" />
            </svg>
          </button>
        </div>
      </div>

      {/* Canvas */}
      <div
        ref={containerRef}
        className={cn(
          "relative flex-1 w-full",
          scrollOnOverflow ? "overflow-auto" : "overflow-hidden",
          containerRing,
          className,
        )}
        style={height != null ? { height } : undefined}
      >
        {scrollOnOverflow ? (
          <div
            className="absolute"
            style={{
              width: fitBox.width * zoom,
              height: fitBox.height * zoom,
              left: 0,
              top: 0,
            }}
          >
            <svg
              ref={svgRef}
              className="w-full h-full"
              viewBox={`${fitBox.x} ${fitBox.y} ${fitBox.width} ${fitBox.height}`}
              preserveAspectRatio="xMidYMid meet"
            >
              <defs>
                <pattern
                  id={gridId}
                  width="20"
                  height="20"
                  patternUnits="userSpaceOnUse"
                >
                  <path
                    d="M 20 0 L 0 0 0 20"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="0.5"
                    className="text-surface-300 dark:text-dark-surface-600"
                  />
                </pattern>
              </defs>
              <rect
                x={fitBox.x - 1000}
                y={fitBox.y - 1000}
                width={fitBox.width + 2000}
                height={fitBox.height + 2000}
                fill={`url(#${gridId})`}
                className="text-surface-200 dark:text-dark-surface-700"
              />
              {DebugViewBoxRect}
              {DebugContentRect}
              <g transform="scale(1)">{children}</g>
            </svg>
          </div>
        ) : (
          <svg
            ref={svgRef}
            className="absolute inset-0"
            width="100%"
            height="100%"
            viewBox={`${viewBox.x} ${viewBox.y} ${viewBox.width} ${viewBox.height}`}
            preserveAspectRatio="xMidYMid meet"
          >
            <defs>
              <pattern
                id={gridId}
                width="20"
                height="20"
                patternUnits="userSpaceOnUse"
              >
                <path
                  d="M 20 0 L 0 0 0 20"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="0.5"
                  className="text-surface-300 dark:text-dark-surface-600"
                />
              </pattern>
            </defs>

            {/* Grid across current viewBox */}
            <rect
              x={viewBox.x - 1000}
              y={viewBox.y - 1000}
              width={viewBox.width + 2000}
              height={viewBox.height + 2000}
              fill={`url(#${gridId})`}
              className="text-surface-200 dark:text-dark-surface-700"
            />

            {DebugViewBoxRect}
            {DebugContentRect}

            {/* zoom via group (user controlled) */}
            <g transform={`scale(${zoomForRender})`}>{children}</g>
          </svg>
        )}

        {debug && (
          <div className="pointer-events-none absolute left-2 top-2 z-20 rounded bg-black/70 px-2 py-1 text-xs text-white">
            <div>
              container: {Math.round(containerPx.w)}×{Math.round(containerPx.h)}
              px
            </div>
            <div>
              viewBox: {Math.round(viewBox.x)},{Math.round(viewBox.y)} →{" "}
              {Math.round(viewBox.width)}×{Math.round(viewBox.height)}
            </div>
            <div>zoom: {Math.round(zoom * 100)}%</div>
            <div>mode: {scrollOnOverflow ? "scroll" : "fit"}</div>
          </div>
        )}
      </div>
    </div>
  );
};

export default WorkflowVisualizer;
