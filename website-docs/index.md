---
layout: home
title: Contenox Documentation

hero:
  name: "Contenox"
  text: "Sovereign GenAI Workflows"
  tagline: Build, run, and observe AI workflows as explicit state machines — on your infra, with full control.
  image:
    src: /chain_flow_diagram.png
    alt: Contenox workflow diagram
  actions:
    - theme: brand
      text: Get Started →
      link: /guide/introduction
    - theme: alt
      text: vibe CLI Quickstart
      link: /guide/quickstart
    - theme: alt
      text: API Reference
      link: https://contenox.com/docs/openapi.html
      target: _blank

features:
  - icon: 🧠
    title: vibe — local AI agent CLI
    details: Run AI workflows locally with full observability. Autonomous planning, stateless execution, remote hooks. No cloud required.
    link: /guide/quickstart
    linkText: Get started with vibe
  - icon: 🔗
    title: Task chains
    details: Define AI behaviour as composable JSON state machines — not prompt soup. Every step, branch, and tool call is explicit.
    link: /chains/
    linkText: Learn about chains
  - icon: 🔌
    title: Remote hooks
    details: Point vibe at any OpenAPI service and the model gets its operations as callable tools automatically.
    link: /hooks/remote
    linkText: Add your first hook
  - icon: 🛡️
    title: Vendor-agnostic
    details: Ollama, OpenAI, vLLM, Gemini — swap providers per task. No lock-in.
    link: /reference/config
    linkText: Configure backends
---
