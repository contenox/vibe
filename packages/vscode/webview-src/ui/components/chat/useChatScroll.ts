import {
  type DependencyList,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";

export type UseChatScrollOptions = {
  /** Values that should trigger an auto-scroll check (e.g. message list) */
  deps: DependencyList;
  /** Pixels from bottom to still count as "near bottom" (default 160) */
  thresholdPx?: number;
  behavior?: ScrollBehavior;
};

export function useChatScroll({
  deps,
  thresholdPx = 160,
  behavior = "smooth",
}: UseChatScrollOptions) {
  const containerRef = useRef<HTMLDivElement>(null);
  const endRef = useRef<HTMLDivElement>(null);
  const [isNearBottom, setIsNearBottom] = useState(true);

  const checkNearBottom = useCallback(() => {
    const el = containerRef.current;
    if (!el) return true;
    return el.scrollHeight - el.scrollTop - el.clientHeight < thresholdPx;
  }, [thresholdPx]);

  const scrollToEnd = useCallback(() => {
    endRef.current?.scrollIntoView({ behavior });
  }, [behavior]);

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    const handleScroll = () => {
      setIsNearBottom(checkNearBottom());
    };

    el.addEventListener("scroll", handleScroll, { passive: true });
    return () => el.removeEventListener("scroll", handleScroll);
  }, [checkNearBottom]);

  useEffect(() => {
    if (checkNearBottom()) {
      endRef.current?.scrollIntoView({ behavior });
    }
  }, [thresholdPx, behavior, checkNearBottom, ...deps]);

  return { containerRef, endRef, scrollToEnd, isNearBottom };
}
