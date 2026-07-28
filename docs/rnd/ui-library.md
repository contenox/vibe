---
title: "@contenox/ui: the component library"
description: A versioned, Storybook-catalogued React 19 + Tailwind design system spanning chat, terminal, and visual workflow components, built to power Beam.
---

# @contenox/ui: the component library

`@contenox/ui` was Contenox's own design system: a versioned, independently published React 19 and Tailwind 4 component library, built with `tsup` into dual CJS/ESM bundles with full type declarations, and catalogued component-by-component in Storybook. It shipped its own theme layer — `styles.css`, `theme.css`, `fonts.css` with a bundled Geist variable font — so any consumer got a consistent look without redefining tokens.

The catalog ran to roughly seventy components across five families: interaction primitives (buttons, inputs, selects, dialogs, tabs, tables, forms, pagination); a full chat kit (`ChatThread`, `ChatComposer`, `ChatMessage`, `ApprovalCard`, `ToolCallCard`, typing indicators, transcript embeds); a terminal kit (`TerminalOutput`, `TerminalLine`, ANSI rendering, a terminal prompt input); a visual workflow kit built on `dagre` graph layout (`WorkflowVisualizer`, `WorkflowNode`, `WorkflowEdge`, `ExecutionTimeline`, `StateVisualizer`); and structural pieces — resizable panels, a file tree, drag-and-drop, a command bar, a setup wizard. It was the library Beam's entire admin and chat surface was built from.

![The @contenox/ui component catalog in Storybook](/ui-library-storybook.png)

## What it proved

- **One design language stretched from admin tables to a terminal-native chat kit** — proof that agent/HITL UX doesn't need a bespoke component for every screen.
- **Cataloguing components in Storybook, versioned independently of the app**, let the design system be built and reviewed in isolation from Beam itself.
- **Purpose-built primitives for agent work — approval cards, tool-call display, diff-first review — are reusable interaction patterns**, not one-off screens, once named and componentized.

## Where this lives now

The interaction patterns it proved out — approval cards, tool-call display, diff-first review — now render as text and panels in the terminal beam experience, no React runtime required.
