import { P } from "./Typography";
import { cn } from "../utils";

type KeyValueProps = {
  label: string;
  value: React.ReactNode;
  className?: string;
  labelClassName?: string;
  valueClassName?: string;
};

export const KeyValue = ({
  label,
  value,
  className,
  labelClassName,
  valueClassName,
}: KeyValueProps) => (
  <P className={cn("flex gap-2", className)}>
    <span className={cn("font-medium shrink-0", labelClassName)}>{label}</span>
    <span className={cn("truncate", valueClassName)}>{value}</span>
  </P>
);
