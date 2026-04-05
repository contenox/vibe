import { Badge, Button, Input, Label, Panel, Select, Textarea } from "../..";
import { Clock, GitBranch, RefreshCw, Save, Trash2, X } from "lucide-react";
import React, { useEffect, useState } from "react";

export interface DetailsPanelProps {
  title: string;
  data: Record<string, unknown>;
  fields: Array<{
    key: string;
    label: string;
    type: "text" | "textarea" | "select" | "badge" | "custom";
    options?: Array<{ value: string; label: string }>;
    render?: (value: unknown) => React.ReactNode;
  }>;
  onClose: () => void;
  onSave?: (data: Record<string, unknown>) => void;
  onDelete?: () => void;
  isEditing?: boolean;
  onEditToggle?: (editing: boolean) => void;
  onFieldUpdate?: (updates: Record<string, unknown>) => void;
  className?: string;
}

export const DetailsPanel: React.FC<DetailsPanelProps> = ({
  title,
  data,
  fields,
  onClose,
  onSave,
  onDelete,
  isEditing = false,
  onEditToggle,
  onFieldUpdate,
  className,
}) => {
  const [editedData, setEditedData] = useState<Record<string, unknown>>({});
  const [isEditMode, setIsEditMode] = useState(isEditing);

  useEffect(() => {
    setEditedData({ ...data });
  }, [data]);

  const handleSave = () => {
    onSave?.(editedData);
    setIsEditMode(false);
    onEditToggle?.(false);
  };

  const handleCancel = () => {
    setEditedData({ ...data });
    setIsEditMode(false);
    onEditToggle?.(false);
  };

  const updateField = (key: string, value: unknown) => {
    const updates = { [key]: value };
    setEditedData((prev) => ({ ...prev, ...updates }));
    onFieldUpdate?.(updates);
  };

  const renderField = (field: (typeof fields)[0]) => {
    const rawValue = isEditMode ? editedData[field.key] : data[field.key];

    if (field.render) {
      return field.render(rawValue);
    }

    const value = typeof rawValue === "string" ? rawValue : String(rawValue ?? "");

    switch (field.type) {
      case "badge":
        return <Badge>{value}</Badge>;
      case "select":
        return isEditMode ? (
          <Select
            value={value}
            onChange={(e) => updateField(field.key, e.target.value)}
            options={field.options || []}
            className="bg-surface-50 dark:bg-dark-surface-50 border-surface-300 dark:border-dark-surface-300 focus:border-primary-500 dark:focus:border-dark-primary-500 focus:ring-primary-500 dark:focus:ring-dark-primary-500"
          />
        ) : (
          <div className="text-sm text-text dark:text-dark-text">{value}</div>
        );
      case "textarea":
        return isEditMode ? (
          <Textarea
            value={value}
            onChange={(e) => updateField(field.key, e.target.value)}
            className="min-h-[80px] bg-surface-50 dark:bg-dark-surface-50 border-surface-300 dark:border-dark-surface-300 focus:border-primary-500 dark:focus:border-dark-primary-500 focus:ring-primary-500 dark:focus:ring-dark-primary-500"
          />
        ) : (
          <div className="bg-surface-100 dark:bg-dark-surface-100 rounded p-2 font-mono text-sm text-text dark:text-dark-text">
            {value}
          </div>
        );
      default:
        return isEditMode ? (
          <Input
            value={value}
            onChange={(e) => updateField(field.key, e.target.value)}
            className="bg-surface-50 dark:bg-dark-surface-50 border-surface-300 dark:border-dark-surface-300 focus:border-primary-500 dark:focus:border-dark-primary-500 focus:ring-primary-500 dark:focus:ring-dark-primary-500"
          />
        ) : (
          <div className="text-sm text-text dark:text-dark-text">{value}</div>
        );
    }
  };

  return (
    <div
      className={`bg-surface-50 dark:bg-dark-surface-50 flex h-full w-96 flex-col border-l border-surface-300 dark:border-dark-surface-300 shadow-xl ${className}`}
    >
      {/* Header */}
      <div className="flex items-center justify-between border-b border-surface-300 dark:border-dark-surface-300 p-4">
        <div>
          <h4 className="text-lg font-semibold text-text dark:text-dark-text">
            {title}
          </h4>
          {!isEditMode && <Badge className="mt-1">View Mode</Badge>}
        </div>
        <div className="flex gap-1">
          {!isEditMode && onSave && (
            <Button
              size="sm"
              variant="secondary"
              onClick={() => {
                setIsEditMode(true);
                onEditToggle?.(true);
              }}
            >
              Edit
            </Button>
          )}
          {onDelete && (
            <Button size="sm" variant="ghost" onClick={onDelete}>
              <Trash2 className="h-4 w-4 text-error-500 dark:text-dark-error-500" />
            </Button>
          )}
          <Button size="icon" variant="ghost" onClick={onClose}>
            <X className="h-4 w-4 text-text dark:text-dark-text" />
          </Button>
        </div>
      </div>

      {/* Content */}
      <div className="flex-1 space-y-4 overflow-y-auto p-4">
        {fields.map((field) => (
          <Panel key={field.key} variant="surface">
            <Label className="text-text dark:text-dark-text">
              {field.label}
            </Label>
            <div className="mt-2">{renderField(field)}</div>
          </Panel>
        ))}
      </div>

      {/* Action Buttons */}
      {isEditMode && (
        <div className="border-t border-surface-300 dark:border-dark-surface-300 p-4">
          <div className="flex gap-2">
            <Button
              variant="secondary"
              onClick={handleCancel}
              className="flex-1"
            >
              Cancel
            </Button>
            <Button variant="primary" onClick={handleSave} className="flex-1">
              <Save className="mr-2 h-4 w-4" />
              Save Changes
            </Button>
          </div>
        </div>
      )}
    </div>
  );
};

export default DetailsPanel;
