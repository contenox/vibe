import React, { useCallback, useState, useRef, useEffect } from "react";
import { Button, Section } from "../..";
import { TabTrigger } from "../TabTrigger";

export interface TabbedFormTab {
  id: string;
  label: string;
  content: React.ReactNode;
  disabled?: boolean;
}

export interface TabbedFormProps {
  title: string;
  description?: string;
  tabs: TabbedFormTab[];
  onSave: () => void;
  onCancel: () => void;
  onDelete?: () => void;
  className?: string;
}

export const TabbedForm: React.FC<TabbedFormProps> = ({
  title,
  description,
  tabs,
  onSave,
  onCancel,
  onDelete,
  className,
}) => {
  const [activeTab, setActiveTab] = useState<string>(tabs[0]?.id);

  const tabRefs = useRef<Record<string, HTMLButtonElement | null>>({});

  const handleTabChange = useCallback((tabId: string) => {
    setActiveTab(tabId);
  }, []);

  const activeTabContent = tabs.find((t) => t.id === activeTab)?.content;

  // keyboard nav: ← → Home End
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
    setActiveTab(nextId);
    tabRefs.current[nextId]?.focus();
  };

  return (
    <div className={`flex h-full flex-col ${className ?? ""}`}>
      <Section title={title} description={description} className="shrink-0">
        <div
          role="tablist"
          aria-orientation="horizontal"
          className="mb-4 flex gap-1"
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
                onClick={() => handleTabChange(tab.id)}
                disabled={tab.disabled}
              >
                {tab.label}
              </TabTrigger>
            );
          })}
        </div>
      </Section>

      {/* Active tab content */}
      <div className="flex-1 overflow-y-auto">
        {activeTabContent && (
          <div
            role="tabpanel"
            id={`panel-${activeTab}`}
            aria-labelledby={`tab-${activeTab}`}
            className="block"
          >
            {activeTabContent}
          </div>
        )}
      </div>

      {/* Footer actions */}
      <div className="border-border mt-6 shrink-0 border-t border-surface-300 dark:border-dark-surface-300 pt-4">
        <div className="flex items-center justify-between">
          <div>
            {onDelete && (
              <Button variant="secondary" onClick={onDelete}>
                Delete
              </Button>
            )}
          </div>
          <div className="flex gap-2">
            <Button variant="secondary" onClick={onCancel}>
              Cancel
            </Button>
            <Button variant="primary" onClick={onSave}>
              Save
            </Button>
          </div>
        </div>
      </div>
    </div>
  );
};

export default TabbedForm;
