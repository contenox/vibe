import { cn } from "../utils";
import { Panel } from "./Panel";
import { H2, P } from "./Typography";

type SectionVariant = "surface" | "bordered" | "body";

interface SectionProps extends React.HTMLAttributes<HTMLDivElement> {
  title?: string;
  className?: string;
  children: React.ReactNode;
  description?: string;
  variant?: SectionVariant;
}

export function Section({
  title,
  description,
  className,
  children,
  variant = "bordered",
  ...props
}: SectionProps) {
  return (
    <Panel variant={variant} className={cn(className)} {...props}>
      {title && <H2>{title}</H2>}
      {description && <P>{description}</P>}
      <section>{children}</section>
    </Panel>
  );
}
