// components/Label.tsx
import { cn } from "../utils";

type LabelProps = React.LabelHTMLAttributes<HTMLLabelElement>;

export function Label({ className, ...props }: LabelProps) {
  return (
    <label
      className={cn(
        "text-text dark:text-dark-text text-sm font-medium",
        className,
      )}
      {...props}
    />
  );
}
