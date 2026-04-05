import {
  CSSProperties,
  forwardRef,
  ReactNode,
  useImperativeHandle,
  useState,
} from "react";
import { cn } from "../utils";
import { P } from "./Typography";

export interface CommandPanelHandle {
  updateContent: (newContent: ReactNode) => void;
  resetContent: () => void;
}

export interface CommandPanelProps {
  initialContent?: ReactNode;
  className?: string;
  style?: CSSProperties;
}

export const CommandPanel = forwardRef<CommandPanelHandle, CommandPanelProps>(
  (props, ref) => {
    const { initialContent = <P>Hi</P>, className = "", style = {} } = props;

    const [content, setContent] = useState<ReactNode>(initialContent);

    useImperativeHandle(
      ref,
      () => ({
        updateContent(newContent: ReactNode) {
          setContent(newContent);
        },
        resetContent() {
          setContent(initialContent);
        },
      }),
      [initialContent],
    );

    return (
      <div
        className={cn("flex items-center justify-between gap-4 p-4", className)}
        style={style}
      >
        {content}
      </div>
    );
  },
);

CommandPanel.displayName = "CommandPanel";
