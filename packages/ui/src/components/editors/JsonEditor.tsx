import {
  Button,
  Card,
  FormField,
  GridLayout,
  Panel,
  Textarea,
  cn,
} from "../..";
import { useEffect, useState } from "react";

export interface JsonEditorProps {
  value: object;
  onSave: (value: object) => void;
  onCancel: () => void;
  title?: string;
  description?: string;
  validate?: (json: object) => { isValid: boolean; error?: string };
  exampleJson?: string;
  className?: string;
}

export const JsonEditor: React.FC<JsonEditorProps> = ({
  value,
  onSave,
  onCancel,
  title = "JSON Editor",
  description = "Edit the JSON data below. Use the format button to automatically format the JSON.",
  validate,
  exampleJson = `{
  "key": "value"
}`,
  className,
}) => {
  const [jsonInput, setJsonInput] = useState("");
  const [error, setError] = useState<string | undefined>(undefined);
  const [isValid, setIsValid] = useState(true);

  useEffect(() => {
    try {
      setJsonInput(JSON.stringify(value, null, 2));
      setError(undefined);
      setIsValid(true);
    } catch {
      setError("Failed to initialize JSON editor");
      setIsValid(false);
    }
  }, [value]);

  const validateJson = (jsonString: string): boolean => {
    try {
      if (!jsonString.trim()) {
        setError("JSON cannot be empty");
        return false;
      }
      const parsed = JSON.parse(jsonString);

      if (validate) {
        const res = validate(parsed);
        if (!res.isValid) {
          setError(res.error || "Validation failed");
          return false;
        }
      }

      setError(undefined);
      return true;
    } catch (err) {
      setError(`Invalid JSON: ${(err as Error).message}`);
      return false;
    }
  };

  const handleJsonChange = (value: string) => {
    setJsonInput(value);
    setIsValid(validateJson(value));
  };

  const handleSave = () => {
    if (!validateJson(jsonInput)) return;
    try {
      const parsedJson = JSON.parse(jsonInput);
      onSave(parsedJson);
    } catch (err) {
      setError(`Failed to save JSON: ${(err as Error).message}`);
    }
  };

  const handleFormat = () => {
    try {
      const parsed = JSON.parse(jsonInput);
      setJsonInput(JSON.stringify(parsed, null, 2));
      setIsValid(true);
      setError(undefined);
    } catch (err) {
      setError(`Failed to format JSON: ${(err as Error).message}`);
      setIsValid(false);
    }
  };

  return (
    <GridLayout
      className={cn("h-full min-h-0 gap-4", className)} // fill the page
      responsive={{ base: 1, lg: 2 }}
    >
      {/* LEFT: Main editor */}
      <Card className="flex h-full min-h-0 flex-col overflow-hidden p-4">
        <div className="flex-shrink-0">
          <h3 className="mb-2 text-lg font-semibold text-text dark:text-dark-text">
            {title}
          </h3>
          <p className="text-text-muted mb-4 text-sm dark:text-dark-text-muted">
            {description}
          </p>
          {error && (
            <Panel variant="error" className="mb-4">
              {error}
            </Panel>
          )}
        </div>
        <FormField
          label="JSON Data"
          error={error}
          className="flex min-h-0 flex-1 flex-col"
        >
          <div className="flex min-h-0 min-w-0 flex-1 flex-col">
            <div className="mb-2 flex items-center justify-between">
              <span className="text-sm font-medium text-text dark:text-dark-text">
                JSON
              </span>
              <Button size="sm" variant="outline" onClick={handleFormat}>
                Format JSON
              </Button>
            </div>

            <div className="relative min-h-0 min-w-0 flex-1 overflow-hidden rounded-lg border border-surface-300 dark:border-dark-surface-300">
              <Textarea
                value={jsonInput}
                onChange={(e) => handleJsonChange(e.target.value)}
                className="h-full w-full font-mono text-sm whitespace-pre overflow-auto"
                placeholder="Enter JSON here..."
              />
            </div>
          </div>
        </FormField>
        <div className="mt-4 flex flex-shrink-0 items-center justify-between border-t border-surface-300 pt-4 dark:border-dark-surface-300">
          <div className="flex items-center gap-2">
            {isValid ? (
              <span className="text-success-500 dark:text-dark-success-500 text-sm">
                ✓ Valid JSON
              </span>
            ) : (
              <span className="text-error-500 dark:text-dark-error-500 text-sm">
                ✗ Invalid JSON
              </span>
            )}
          </div>

          <div className="flex gap-2">
            <Button variant="secondary" onClick={onCancel}>
              Cancel
            </Button>
            <Button variant="primary" onClick={handleSave} disabled={!isValid}>
              Save
            </Button>
          </div>
        </div>
      </Card>

      <div className="flex h-full min-h-0 flex-col overflow-hidden">
        <Panel
          variant="surface"
          className="flex h-full min-h-0 flex-col overflow-hidden p-4"
        >
          <h4 className="mb-2 flex-shrink-0 font-medium text-text dark:text-dark-text">
            Example JSON Structure
          </h4>
          <div className="min-h-0 flex-1 overflow-hidden">
            <pre className="min-h-full rounded bg-surface-100 p-3 font-mono text-xs text-text dark:bg-dark-surface-100 dark:text-dark-text">
              {exampleJson}
            </pre>
          </div>
        </Panel>
      </div>
    </GridLayout>
  );
};

export default JsonEditor;
