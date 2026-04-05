// packages/ui/src/Tabs/Tabs.tsx
import React, { useRef } from "react";
import { TabTrigger } from "./TabTrigger";
import { cn } from "../utils";

export type Tab<T extends string = string> = {
  id: T;
  label: React.ReactNode;
  disabled?: boolean;
};

export interface TabsProps<T extends string = string> {
  tabs: readonly Tab<T>[];
  activeTab: T;
  onTabChange: (tabId: T) => void;
  className?: string;
}

/**
 * Modern, accessible Tabs component
 * - Keyboard navigation (← → Home End)
 * - Uses TabTrigger for styling
 * - Drop-in replacement for the old Tabs
 */
export function Tabs<T extends string = string>({
  tabs,
  activeTab,
  onTabChange,
  className,
}: TabsProps<T>) {
  const refs = useRef<Record<string, HTMLButtonElement | null>>({});

  const onKeyDown: React.KeyboardEventHandler<HTMLDivElement> = (e) => {
    const idx = tabs.findIndex((t) => t.id === activeTab);
    if (idx === -1) return;

    let nextIdx = idx;
    if (e.key === "ArrowRight") nextIdx = (idx + 1) % tabs.length;
    else if (e.key === "ArrowLeft")
      nextIdx = (idx - 1 + tabs.length) % tabs.length;
    else if (e.key === "Home") nextIdx = 0;
    else if (e.key === "End") nextIdx = tabs.length - 1;
    else return;

    e.preventDefault();
    const nextId = tabs[nextIdx].id;
    onTabChange(nextId);
    refs.current[String(nextId)]?.focus();
  };

  return (
    <div
      role="tablist"
      aria-orientation="horizontal"
      className={cn("flex gap-1", className)}
      onKeyDown={onKeyDown}
    >
      {tabs.map((tab) => {
        const isActive = tab.id === activeTab;
        return (
          <TabTrigger
            key={String(tab.id)}
            ref={(el) => {
              refs.current[tab.id] = el ?? null;
            }}
            active={isActive}
            disabled={tab.disabled}
            id={`tab-${String(tab.id)}`}
            aria-controls={`panel-${String(tab.id)}`}
            tabIndex={isActive ? 0 : -1}
            onClick={() => onTabChange(tab.id)}
          >
            {tab.label}
          </TabTrigger>
        );
      })}
    </div>
  );
}
