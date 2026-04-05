import * as dagre from "@dagrejs/dagre";

export type LayoutDirection = "horizontal" | "vertical";
export type NodePosition = {
  id: string;
  x: number;
  y: number;
  width: number;
  height: number;
};
export type Edge = {
  from: string;
  to: string;
  label: string;
  isError?: boolean;
  fromType: string;
};
export type AddButtonPosition = {
  x: number;
  y: number;
  fromNodeId: string;
  toNodeId?: string;
};

const NODE_WIDTH = 250;
const NODE_HEIGHT = 120;
const HORIZONTAL_SPACING = 85;
const VERTICAL_SPACING = 100;

const ADD_BTN_RADIUS = 12;
const MIN_BTN_SEPARATION = ADD_BTN_RADIUS * 2 + 4;
const NUDGE = 26;

export const getConnectorPath = (
  source: NodePosition,
  target: NodePosition,
  direction: LayoutDirection,
): string => {
  if (direction === "vertical") {
    const startX = source.x + source.width / 2;
    const startY = source.y + source.height;
    const endX = target.x + target.width / 2;
    const endY = target.y;
    const midY = startY + (endY - startY) / 2;
    return `M${startX},${startY} C${startX},${midY} ${endX},${midY} ${endX},${endY}`;
  } else {
    // horizontal
    const startX = source.x + source.width;
    const startY = source.y + source.height / 2;
    const endX = target.x;
    const endY = target.y + target.height / 2;
    const midX = startX + (endX - startX) / 2;
    return `M${startX},${startY} C${midX},${startY} ${midX},${endY} ${endX},${endY}`;
  }
};

export const calculateLayout = (
  nodes: Array<{ id: string }>,
  edges: Edge[],
  direction: LayoutDirection,
): {
  nodePositions: Record<string, NodePosition>;
  edges: Edge[];
  addButtons: AddButtonPosition[];
} => {
  if (nodes.length === 0) {
    return { nodePositions: {}, edges: [], addButtons: [] };
  }

  const graph = new dagre.graphlib.Graph();
  graph.setGraph({
    rankdir: direction === "horizontal" ? "LR" : "TB",
    nodesep: HORIZONTAL_SPACING,
    ranksep: VERTICAL_SPACING,
    marginx: 25,
    marginy: 25,
  });
  graph.setDefaultEdgeLabel(() => ({}));

  nodes.forEach((node) => {
    graph.setNode(node.id, {
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
      label: node.id,
    });
  });

  edges.forEach((edge) => {
    graph.setEdge(edge.from, edge.to);
  });

  dagre.layout(graph);

  const nodePositions: Record<string, NodePosition> = {};
  graph.nodes().forEach((id) => {
    const node = graph.node(id);
    if (node) {
      nodePositions[id] = {
        id,
        x: node.x - node.width / 2,
        y: node.y - node.height / 2,
        width: node.width,
        height: node.height,
      };
    }
  });

  const addButtons: AddButtonPosition[] = [];

  const isTooClose = (x: number, y: number) =>
    addButtons.some((b) => {
      const dx = b.x - x;
      const dy = b.y - y;
      return Math.hypot(dx, dy) < MIN_BTN_SEPARATION;
    });

  const resolveCollision = (x: number, y: number) => {
    if (!isTooClose(x, y)) return { x, y };

    const candidates =
      direction === "vertical"
        ? [
            { x: x + NUDGE, y },
            { x: x - NUDGE, y },
            { x: x + NUDGE * 2, y },
            { x: x - NUDGE * 2, y },
          ]
        : [
            { x, y: y + NUDGE },
            { x, y: y - NUDGE },
            { x, y: y + NUDGE * 2 },
            { x, y: y - NUDGE * 2 },
          ];

    for (const c of candidates) {
      if (!isTooClose(c.x, c.y)) return c;
    }
    return { x, y };
  };

  edges.forEach((edge: Edge) => {
    const from = nodePositions[edge.from];
    const to = nodePositions[edge.to];
    if (!from || !to) return;

    let x: number;
    let y: number;

    if (direction === "vertical") {
      x = (from.x + from.width / 2 + to.x + to.width / 2) / 2;
      y = from.y + from.height + (to.y - (from.y + from.height)) / 2;
    } else {
      x = from.x + from.width + (to.x - (from.x + from.width)) / 2;
      y = (from.y + from.height / 2 + to.y + to.height / 2) / 2;
    }

    const pos = resolveCollision(x, y);
    addButtons.push({
      x: pos.x,
      y: pos.y,
      fromNodeId: edge.from,
      toNodeId: edge.to,
    });
  });

  nodes.forEach((node) => {
    const nodePos = nodePositions[node.id];
    if (!nodePos) return;

    let x: number;
    let y: number;

    if (direction === "vertical") {
      x = nodePos.x + nodePos.width / 2;
      y = nodePos.y + nodePos.height + 40;
    } else {
      x = nodePos.x + nodePos.width + 40;
      y = nodePos.y + nodePos.height / 2;
    }

    const pos = resolveCollision(x, y);
    addButtons.push({
      x: pos.x,
      y: pos.y,
      fromNodeId: node.id,
    });
  });

  // Return the edges along with node positions and add buttons
  return { nodePositions, edges, addButtons };
};
