import React, { useRef } from "react";
import { cn } from "../utils";
import { useDragDropContext } from "./DragDropContext";

interface DroppableProps {
  droppableId: string;
  children: React.ReactNode;
  className?: string;
  isDropDisabled?: boolean;
}

export function Droppable({
  droppableId,
  children,
  className,
  isDropDisabled = false,
}: DroppableProps) {
  const { dragState, updateDrag, endDrag } = useDragDropContext();
  const elementRef = useRef<HTMLDivElement>(null);

  const isDraggingOver =
    !isDropDisabled &&
    dragState.destinationDroppableId === droppableId &&
    dragState.draggedId !== null;

  const handleDragEnter = (e: React.DragEvent) => {
    e.preventDefault();
    if (!isDropDisabled) {
      updateDrag(droppableId);
    }
  };

  const handleDragOver = (e: React.DragEvent) => {
    e.preventDefault();
    if (!isDropDisabled) {
      updateDrag(droppableId);
    }
  };

  const handleDragLeave = (e: React.DragEvent) => {
    e.preventDefault();
    if (
      !isDropDisabled &&
      !elementRef.current?.contains(e.relatedTarget as Node)
    ) {
      updateDrag(null);
    }
  };

  const handleDrop = (e: React.DragEvent) => {
    e.preventDefault();
    if (!isDropDisabled) {
      endDrag();
    }
  };

  return (
    <div
      ref={elementRef}
      className={cn(
        "transition-all duration-200 ease-in-out",
        isDraggingOver &&
          "ring-2 ring-accent-500 bg-accent-50 dark:bg-dark-accent-50 rounded-lg",
        className,
      )}
      onDragEnter={handleDragEnter}
      onDragOver={handleDragOver}
      onDragLeave={handleDragLeave}
      onDrop={handleDrop}
    >
      {children}
    </div>
  );
}
