import React, {
  createContext,
  useContext,
  useReducer,
  useCallback,
} from "react";
import { cn } from "../utils";

interface DragState {
  draggedId: string | null;
  sourceDroppableId: string | null;
  destinationDroppableId: string | null;
}

interface DragDropContextType {
  dragState: DragState;
  startDrag: (draggableId: string, sourceDroppableId: string) => void;
  updateDrag: (destinationDroppableId: string | null) => void;
  endDrag: () => void;
  isDragging: (draggableId: string) => boolean;
}

const DragDropContext = createContext<DragDropContextType | undefined>(
  undefined,
);

type DragAction =
  | {
      type: "START_DRAG";
      payload: { draggableId: string; sourceDroppableId: string };
    }
  | { type: "UPDATE_DRAG"; payload: { destinationDroppableId: string | null } }
  | { type: "END_DRAG" };

function dragReducer(state: DragState, action: DragAction): DragState {
  switch (action.type) {
    case "START_DRAG":
      return {
        draggedId: action.payload.draggableId,
        sourceDroppableId: action.payload.sourceDroppableId,
        destinationDroppableId: action.payload.sourceDroppableId,
      };
    case "UPDATE_DRAG":
      return {
        ...state,
        destinationDroppableId: action.payload.destinationDroppableId,
      };
    case "END_DRAG":
      return {
        draggedId: null,
        sourceDroppableId: null,
        destinationDroppableId: null,
      };
    default:
      return state;
  }
}

interface DragDropContextProps {
  children: React.ReactNode;
  onDragEnd: (result: {
    draggableId: string;
    sourceDroppableId: string;
    destinationDroppableId: string;
  }) => void;
}

export function DragDropContextProvider({
  children,
  onDragEnd,
}: DragDropContextProps) {
  const [dragState, dispatch] = useReducer(dragReducer, {
    draggedId: null,
    sourceDroppableId: null,
    destinationDroppableId: null,
  });

  const startDrag = useCallback(
    (draggableId: string, sourceDroppableId: string) => {
      dispatch({
        type: "START_DRAG",
        payload: { draggableId, sourceDroppableId },
      });
    },
    [],
  );

  const updateDrag = useCallback((destinationDroppableId: string | null) => {
    dispatch({ type: "UPDATE_DRAG", payload: { destinationDroppableId } });
  }, []);

  const endDrag = useCallback(() => {
    if (
      dragState.draggedId &&
      dragState.sourceDroppableId &&
      dragState.destinationDroppableId
    ) {
      onDragEnd({
        draggableId: dragState.draggedId,
        sourceDroppableId: dragState.sourceDroppableId,
        destinationDroppableId: dragState.destinationDroppableId,
      });
    }
    dispatch({ type: "END_DRAG" });
  }, [dragState, onDragEnd]);

  const isDragging = useCallback(
    (draggableId: string) => {
      return dragState.draggedId === draggableId;
    },
    [dragState.draggedId],
  );

  const value: DragDropContextType = {
    dragState,
    startDrag,
    updateDrag,
    endDrag,
    isDragging,
  };

  return (
    <DragDropContext.Provider value={value}>
      {children}
    </DragDropContext.Provider>
  );
}

export function useDragDropContext() {
  const context = useContext(DragDropContext);
  if (context === undefined) {
    throw new Error(
      "useDragDropContext must be used within a DragDropContextProvider",
    );
  }
  return context;
}
