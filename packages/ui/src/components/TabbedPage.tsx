import React, { useState, useEffect, useRef } from "react";
import { TabTrigger } from "./TabTrigger";

export interface TabbedPageTab {
  id: string;
  label: string;
  content: React.ReactNode;
}

interface TabbedPageProps extends React.HTMLAttributes<HTMLDivElement> {
  tabs: TabbedPageTab[];
  defaultActiveTab?: string;
  activeTab?: string;
  onTabChange?: (tabId: string) => void;
  /**
   * When true, only the active tab's content is mounted. Inactive panels render
   * empty placeholders so hidden tabs do not run effects or fetch data.
   */
  mountActivePanelOnly?: boolean;
}

export function TabbedPage({
  tabs,
  defaultActiveTab,
  activeTab: controlledActiveTab,
  onTabChange,
  mountActivePanelOnly = false,
  ...props
}: TabbedPageProps) {
  const [activeTab, setActiveTab] = useState(
    controlledActiveTab ?? defaultActiveTab ?? tabs[0]?.id,
  );

  // refs for roving focus — keyed by tab id
  const tabRefs = useRef<Record<string, HTMLButtonElement | null>>({});

  useEffect(() => {
    if (
      controlledActiveTab !== undefined &&
      controlledActiveTab !== activeTab
    ) {
      setActiveTab(controlledActiveTab);
    }
  }, [controlledActiveTab, activeTab]);

  const setAndNotify = (id: string) => {
    setActiveTab(id);
    onTabChange?.(id);
  };

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
    setAndNotify(nextId);
    tabRefs.current[nextId]?.focus();
  };

  return (
    <div {...props}>
      <div
        role="tablist"
        aria-orientation="horizontal"
        className="flex gap-1"
        onKeyDown={onKeyDown}
      >
        {tabs.map((tab) => {
          const isActive = tab.id === activeTab;
          return (
            <TabTrigger
              key={tab.id}
              ref={(el) => {
                tabRefs.current[tab.id] = el ?? null;
              }}
              active={isActive}
              aria-controls={`panel-${tab.id}`}
              id={`tab-${tab.id}`}
              tabIndex={isActive ? 0 : -1}
              onClick={() => setAndNotify(tab.id)}
            >
              {tab.label}
            </TabTrigger>
          );
        })}
      </div>

      <div className="mt-4">
        {tabs.map(({ id, content }) => (
          <div
            key={id}
            role="tabpanel"
            id={`panel-${id}`}
            aria-labelledby={`tab-${id}`}
            className={id === activeTab ? "block" : "hidden"}
            hidden={id !== activeTab}
          >
            {mountActivePanelOnly && id !== activeTab ? null : content}
          </div>
        ))}
      </div>
    </div>
  );
}
