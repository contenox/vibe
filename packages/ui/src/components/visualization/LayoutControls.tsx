import { Button } from "../Button";
import { LayoutGrid, LayoutList } from "lucide-react";
import React from "react";

export type LayoutDirection = "horizontal" | "vertical";

interface LayoutControlsProps {
  direction: LayoutDirection;
  onChangeDirection: (dir: LayoutDirection) => void;
}

export const LayoutControls: React.FC<LayoutControlsProps> = ({
  direction,
  onChangeDirection,
}) => {
  return (
    <div className="flex gap-1 rounded-md border border-surface-300 dark:border-dark-surface-300 bg-surface-50 dark:bg-dark-surface-50 p-1">
      <Button
        size="icon"
        variant={direction === "horizontal" ? "primary" : "secondary"}
        onClick={() => onChangeDirection("horizontal")}
        aria-label="Horizontal layout"
        className={`${
          direction === "horizontal"
            ? "bg-primary-500 dark:bg-dark-primary-500 text-white hover:bg-primary-600 dark:hover:bg-dark-primary-600"
            : "bg-surface-100 dark:bg-dark-surface-100 text-text dark:text-dark-text hover:bg-surface-200 dark:hover:bg-dark-surface-200"
        }`}
      >
        <LayoutGrid className="h-4 w-4" />
      </Button>
      <Button
        size="icon"
        variant={direction === "vertical" ? "primary" : "secondary"}
        onClick={() => onChangeDirection("vertical")}
        aria-label="Vertical layout"
        className={`${
          direction === "vertical"
            ? "bg-primary-500 dark:bg-dark-primary-500 text-white hover:bg-primary-600 dark:hover:bg-dark-primary-600"
            : "bg-surface-100 dark:bg-dark-surface-100 text-text dark:text-dark-text hover:bg-surface-200 dark:hover:bg-dark-surface-200"
        }`}
      >
        <LayoutList className="h-4 w-4" />
      </Button>
    </div>
  );
};

export default LayoutControls;
