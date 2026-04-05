import { Button } from "../Button";
import { Plus } from "lucide-react";
import React from "react";

interface AddNodeButtonProps {
  x: number;
  y: number;
  onClick: () => void;
  className?: string;
}

export const AddNodeButton: React.FC<AddNodeButtonProps> = ({
  x,
  y,
  onClick,
  className,
}) => {
  return (
    <g
      transform={`translate(${x - 12}, ${y - 12})`}
      className={`cursor-pointer ${className}`}
    >
      <circle
        cx="12"
        cy="12"
        r="12"
        className="fill-primary-500 dark:fill-dark-primary-500 hover:fill-primary-600 dark:hover:fill-dark-primary-600 transition-colors duration-200"
      />
      <foreignObject width="24" height="24" x="0" y="0">
        <div className="flex h-6 w-6 items-center justify-center">
          <Button
            size="icon"
            variant="ghost"
            className="h-6 w-6 text-text-inverted dark:text-dark-text-inverted hover:bg-primary-600 dark:hover:bg-dark-primary-600 hover:text-text-inverted dark:hover:text-dark-text-inverted"
            onClick={onClick}
          >
            <Plus className="h-3 w-3" />
          </Button>
        </div>
      </foreignObject>
    </g>
  );
};

export default AddNodeButton;
