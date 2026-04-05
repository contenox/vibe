import { User2Icon } from "lucide-react";
import { ReactNode } from "react";
import { cn } from "../utils";
import { P, Span } from "./Typography";
import { Button } from "./Button";
import { Dropdown } from "./Dropdown";
import { Section } from "./Section";

type UserMenuProps = {
  isOpen?: boolean;
  friendlyName?: string;
  mail?: string;
  onToggle?: (isOpen: boolean) => void;
  logout?: () => void;
  className?: string;
  children?: ReactNode;
};

export function UserMenu({
  isOpen,
  friendlyName,
  mail,
  logout,
  onToggle,
  className,
  children,
}: UserMenuProps) {
  return (
    <Dropdown
      isOpen={isOpen}
      onToggle={onToggle}
      trigger={
        <Button variant="ghost" size="icon" aria-label="User Menu">
          <User2Icon className="h-6 w-6" />
        </Button>
      }
      className={cn("relative", className)}
      contentClassName="absolute right-0 mt-2 w-48 rounded-lg shadow-lg z-50"
    >
      <Section>
        {(friendlyName || mail) && (
          <Span>
            {friendlyName && <P>{friendlyName}</P>}
            {mail && <P>{mail}</P>}
          </Span>
        )}
        <Span>{logout && <Button onClick={logout}>logout</Button>}</Span>
        <Span>{children && children}</Span>
      </Section>
    </Dropdown>
  );
}
