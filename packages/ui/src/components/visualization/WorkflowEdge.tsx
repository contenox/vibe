import React, { useMemo } from "react";
import { cn } from "../../utils";
import { getConnectorPath } from "./utils";
import type { LayoutDirection, NodePosition } from "./utils";

export interface WorkflowEdgeProps {
  source: NodePosition;
  target: NodePosition;
  label?: string;
  direction: LayoutDirection;
  isHighlighted?: boolean;
  isError?: boolean;
  className?: string;

  // Used so the label chip avoids covering the (+) insert buttons
  addButtonPositions?: Array<{ x: number; y: number }>;

  // Compose info (for color + suffix)
  hasCompose?: boolean;
  composeStrategy?: string | undefined;

  // Clicking the ONLY label chip should open the compose sidebar
  onComposeClick?: () => void;
}

const ONE_LINE_HEIGHT = 22;
const TWO_LINE_HEIGHT = 34; // when a strategy line is shown
const LABEL_PAD_X = 10;
const LABEL_MIN_W = 44;
const LABEL_MAX_W = 200;
const BUTTON_RADIUS = 12;

// Slightly larger margins so we never get too close to buttons/nodes
const COLLISION_MARGIN = 10;
const NODE_PADDING = 8;

// very light width estimate; good enough for chip sizing
const measureTextWidth = (text: string, approxCharPx = 7) =>
  Math.max(
    LABEL_MIN_W,
    Math.min(LABEL_MAX_W, text.length * approxCharPx + LABEL_PAD_X * 2),
  );

const circleIntersectsRect = (
  cx: number,
  cy: number,
  r: number,
  rect: { left: number; right: number; top: number; bottom: number },
  pad = 0,
) => {
  const pr = r + pad;
  const clampedX = Math.max(rect.left, Math.min(cx, rect.right));
  const clampedY = Math.max(rect.top, Math.min(cy, rect.bottom));
  const dx = cx - clampedX;
  const dy = cy - clampedY;
  return dx * dx + dy * dy <= pr * pr;
};

const rectsOverlap = (
  a: { left: number; right: number; top: number; bottom: number },
  b: { left: number; right: number; top: number; bottom: number },
  pad = 0,
) => {
  return !(
    a.right < b.left - pad ||
    a.left > b.right + pad ||
    a.bottom < b.top - pad ||
    a.top > b.bottom + pad
  );
};

// Color + short label per compose strategy
const STRATEGY_STYLE: Record<
  string,
  { fill: string; stroke: string; short: string }
> = {
  override: { fill: "#10b981", stroke: "#064e3b", short: "OVR" }, // emerald
  merge_chat_histories: { fill: "#0ea5e9", stroke: "#075985", short: "MERGE" }, // sky
  append_string_to_chat_history: {
    fill: "#a78bfa",
    stroke: "#5b21b6",
    short: "APPEND",
  }, // violet
  default: { fill: "#64748b", stroke: "#334155", short: "DEFAULT" }, // slate
};

const getStyleForStrategy = (s?: string) =>
  STRATEGY_STYLE[(s || "default").toLowerCase()] ?? STRATEGY_STYLE.default;

export const WorkflowEdge: React.FC<WorkflowEdgeProps> = ({
  source,
  target,
  label,
  direction,
  isHighlighted = false,
  isError = false,
  className,
  addButtonPositions = [],
  hasCompose = false,
  composeStrategy,
  onComposeClick,
}) => {
  if (!source || !target) return null;

  const getEdgeStrokeClass = (): string => {
    if (isError) return "stroke-error-500 dark:stroke-dark-error-500";
    if (isHighlighted) return "stroke-accent-500 dark:stroke-dark-accent-400";
    return "stroke-primary-500 dark:stroke-dark-primary-500";
  };

  const path = getConnectorPath(source, target, direction);
  const strokeClass = getEdgeStrokeClass();
  const fillClass = isError
    ? "fill-error-500 dark:fill-dark-error-500"
    : isHighlighted
      ? "fill-accent-500 dark:fill-dark-accent-400"
      : "fill-primary-500 dark:fill-dark-primary-500";

  const strokeWidth = isHighlighted ? 2.5 : 1.5;

  // Edge midpoint between node centers
  const centerX =
    (source.x + source.width / 2 + target.x + target.width / 2) / 2;
  const centerY =
    (source.y + source.height / 2 + target.y + target.height / 2) / 2;

  // Directional vector from source-center to target-center
  const sx = source.x + source.width / 2;
  const sy = source.y + source.height / 2;
  const tx = target.x + target.width / 2;
  const ty = target.y + target.height / 2;
  const dx = tx - sx;
  const dy = ty - sy;
  const horizontalDominant = Math.abs(dx) >= Math.abs(dy);

  // Node rects to avoid placing the label over them
  const srcRect = useMemo(
    () => ({
      left: source.x,
      right: source.x + source.width,
      top: source.y,
      bottom: source.y + source.height,
    }),
    [source.x, source.y, source.width, source.height],
  );

  const tgtRect = useMemo(
    () => ({
      left: target.x,
      right: target.x + target.width,
      top: target.y,
      bottom: target.y + target.height,
    }),
    [target.x, target.y, target.width, target.height],
  );

  // Determine chip style from compose strategy
  const {
    fill: chipFill,
    stroke: chipStroke,
    short: shortStrat,
  } = getStyleForStrategy(composeStrategy);

  // Two-line content when composed
  const line1 = (label || "default").trim();
  const line2 = hasCompose ? shortStrat : "";

  // Chip dimensions
  const chipWidth = useMemo(
    () =>
      Math.max(
        measureTextWidth(line1),
        hasCompose ? measureTextWidth(line2, 6.5) : 0,
      ),
    [line1, line2, hasCompose],
  );
  const chipHeight = hasCompose ? TWO_LINE_HEIGHT : ONE_LINE_HEIGHT;

  const halfW = chipWidth / 2;
  const halfH = chipHeight / 2;

  // Preferred offsets from the midpoint depend on chipHeight now
  const normalOffset = BUTTON_RADIUS + halfH + 10;
  const alongOffset = BUTTON_RADIUS + halfW + 10;
  const diagOffsetX = halfW + BUTTON_RADIUS + 12;
  const diagOffsetY = halfH + BUTTON_RADIUS + 12;

  // Subtle placement bias (deterministic)
  const candidates = useMemo(() => {
    if (horizontalDominant) {
      const primary = [
        { x: centerX, y: centerY - normalOffset },
        { x: centerX, y: centerY + normalOffset },
      ];
      const diagonals = [
        { x: centerX - diagOffsetX, y: centerY - diagOffsetY }, // TL
        { x: centerX + diagOffsetX, y: centerY - diagOffsetY }, // TR
        { x: centerX - diagOffsetX, y: centerY + diagOffsetY }, // BL
        { x: centerX + diagOffsetX, y: centerY + diagOffsetY }, // BR
      ];
      const lateral =
        dx >= 0
          ? [
              { x: centerX - alongOffset, y: centerY },
              { x: centerX + alongOffset, y: centerY },
            ]
          : [
              { x: centerX + alongOffset, y: centerY },
              { x: centerX - alongOffset, y: centerY },
            ];
      return [...primary, ...diagonals, ...lateral];
    } else {
      const lateral =
        dx >= 0
          ? [
              { x: centerX - alongOffset, y: centerY },
              { x: centerX + alongOffset, y: centerY },
            ]
          : [
              { x: centerX + alongOffset, y: centerY },
              { x: centerX - alongOffset, y: centerY },
            ];

      const diagonals =
        dy >= 0
          ? [
              { x: centerX - diagOffsetX, y: centerY - diagOffsetY },
              { x: centerX + diagOffsetX, y: centerY - diagOffsetY },
              { x: centerX - diagOffsetX, y: centerY + diagOffsetY },
              { x: centerX + diagOffsetX, y: centerY + diagOffsetY },
            ]
          : [
              { x: centerX - diagOffsetX, y: centerY + diagOffsetY },
              { x: centerX + diagOffsetX, y: centerY + diagOffsetY },
              { x: centerX - diagOffsetX, y: centerY - diagOffsetY },
              { x: centerX + diagOffsetX, y: centerY - diagOffsetY },
            ];

      const vertical = [
        { x: centerX, y: centerY - normalOffset },
        { x: centerX, y: centerY + normalOffset },
      ];
      return [...lateral, ...diagonals, ...vertical];
    }
  }, [
    horizontalDominant,
    dx,
    dy,
    centerX,
    centerY,
    normalOffset,
    alongOffset,
    diagOffsetX,
    diagOffsetY,
  ]);

  const candidateIsSafe = (cx: number, cy: number) => {
    const rect = {
      left: cx - halfW,
      right: cx + halfW,
      top: cy - halfH,
      bottom: cy + halfH,
    };

    // 1) avoid overlapping with any + button circle
    const hitsButton = addButtonPositions.some((btn) =>
      circleIntersectsRect(btn.x, btn.y, BUTTON_RADIUS, rect, COLLISION_MARGIN),
    );
    if (hitsButton) return false;

    // 2) avoid overlapping with source/target nodes (extra padding)
    const hitsNodes =
      rectsOverlap(rect, srcRect, NODE_PADDING) ||
      rectsOverlap(rect, tgtRect, NODE_PADDING);
    if (hitsNodes) return false;

    return true;
  };

  // Pick a stable candidate for the label chip
  const labelCenter = useMemo(() => {
    for (const c of candidates) {
      if (candidateIsSafe(c.x, c.y)) return c;
    }
    return candidates[0];
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    candidates,
    addButtonPositions,
    halfW,
    halfH,
    srcRect.left,
    srcRect.right,
    srcRect.top,
    srcRect.bottom,
    tgtRect.left,
    tgtRect.right,
    tgtRect.top,
    tgtRect.bottom,
  ]);

  const handleChipClick = (e: React.MouseEvent) => {
    e.stopPropagation();
    onComposeClick?.();
  };

  return (
    <g className={cn(className)}>
      <defs>
        <marker
          id="arrowhead"
          viewBox="0 0 10 10"
          refX="8"
          refY="5"
          markerWidth="6"
          markerHeight="6"
          orient="auto-start-reverse"
        >
          <path
            d="M 0 0 L 10 5 L 0 10 z"
            className={`${strokeClass} ${fillClass}`}
          />
        </marker>
      </defs>

      <path
        d={path}
        fill="none"
        className={cn("transition-all duration-300", strokeClass)}
        strokeWidth={strokeWidth}
        markerEnd="url(#arrowhead)"
      />

      {/* SINGLE CLICKABLE LABEL CHIP (two lines when composed) */}
      <g
        transform={`translate(${labelCenter.x}, ${labelCenter.y})`}
        className="cursor-pointer select-none"
        onClick={handleChipClick}
        pointerEvents="all"
        role="button"
        aria-label={`Transition: ${line1}${hasCompose ? `. Strategy ${composeStrategy ?? "default"}` : ""}`}
      >
        <rect
          x={-halfW}
          y={-halfH}
          width={chipWidth}
          height={chipHeight}
          rx="12"
          strokeWidth={1.25}
          fill={chipFill}
          stroke={chipStroke}
          className="shadow-sm"
        />

        {/* Text: one or two lines */}
        {hasCompose ? (
          <>
            <text
              x={0}
              y={-3}
              textAnchor="middle"
              dominantBaseline="central"
              fontSize="11"
              fontWeight={700}
              fill="white"
              pointerEvents="none"
            >
              {line1}
            </text>
            <text
              x={0}
              y={10}
              textAnchor="middle"
              dominantBaseline="central"
              fontSize="10"
              fontWeight={600}
              fill="white"
              opacity={0.95}
              pointerEvents="none"
            >
              {line2}
            </text>
          </>
        ) : (
          <text
            x={0}
            y={1}
            textAnchor="middle"
            dominantBaseline="middle"
            fontSize="11"
            fontWeight={700}
            fill="white"
            pointerEvents="none"
          >
            {line1}
          </text>
        )}

        <title>
          {hasCompose
            ? `Compose strategy: ${composeStrategy ?? "default"}`
            : "Click to add compose"}
        </title>
      </g>
    </g>
  );
};

export default WorkflowEdge;
