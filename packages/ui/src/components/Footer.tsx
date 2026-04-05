import { ChevronDown } from "lucide-react";
import { Section } from "./Section";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { cn } from "../utils";
import { Button } from "./Button";
import { Dropdown } from "./Dropdown";
import { Panel } from "./Panel";
import { Span } from "./Typography";

export interface MenuItem {
  path: string;
  label: string;
  icon?: React.ReactNode;
}

interface FooterProps {
  items: MenuItem[];
  panel?: React.ReactNode;
  message?: string;
  className?: string;
}

export default function Footer({
  items = [],
  panel,
  message,
  className,
}: FooterProps) {
  const { t } = useTranslation();
  const year = new Date().getFullYear();
  const [isOpen, setIsOpen] = useState(false);

  return (
    <Panel variant="flat" className={cn("mt-0 mr-2 ml-2 pt-0", className)}>
      <div className="mx-auto flex items-end justify-between align-bottom">
        {/* Left Section */}
        <div className="flex items-end gap-1 overflow-hidden">
          {message && (
            <Span
              variant="muted"
              className="max-w-1/2 truncate overflow-hidden"
            >
              {message}
            </Span>
          )}
          <Span className="overflow-hidden" variant="muted">
            {t("common.copyright", { year })}
          </Span>
        </div>

        {/* Center Panel */}
        {panel && <div className="w-max">{panel}</div>}

        {/* Right Section - Dropdown */}
        <Dropdown
          isOpen={isOpen}
          onToggle={setIsOpen}
          trigger={
            <Button
              variant="ghost"
              size="sm"
              aria-label={t("common.menu")}
              className="gap-1"
            >
              <ChevronDown
                className={cn(
                  "h-4 w-4 transition-transform",
                  isOpen && "rotate-180",
                )}
              />
            </Button>
          }
          contentClassName="absolute right-0 bottom-full mb-2 min-w-[160px]"
        >
          <Section>
            <nav className="py-2">
              {items.map((item) => (
                <Link
                  key={item.path}
                  to={item.path}
                  className={cn(
                    "flex items-end gap-2 px-4 py-2 text-sm",
                    "hover:bg-primary-600 dark:hover:bg-dark-primary-600",
                    "transition-colors duration-200",
                  )}
                  onClick={() => setIsOpen(false)}
                >
                  {item.icon && (
                    <Span className="text-primary-500 dark:text-dark-primary-500 h-4 w-4">
                      {item.icon}
                    </Span>
                  )}
                  <Span variant="body">{item.label}</Span>
                </Link>
              ))}
            </nav>
          </Section>
        </Dropdown>
      </div>
    </Panel>
  );
}
