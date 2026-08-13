---
title: Legal
description: All Contenox legal documents in one place — terms of service, privacy policy, right of withdrawal, imprint, security and sub-processors for the hosted service, plus the notices for this website and the open-source software.
---

# Legal

Everything legal, in one place. Two groups: the documents for the **hosted
service** you sign up to, and the notices for **this website and the
open-source software**, which need no account at all.

## The hosted service (app.contenox.com)

| Document | What it covers |
|---|---|
| [Terms of service](/legal/terms) | The contract: the three layers, what we owe, liability, and which rules reach you where you are |
| [Privacy policy](/legal/privacy) | What is processed, on what legal basis, how long it is kept, how it is secured, and your rights — including outside the EU |
| [Right of withdrawal](/legal/withdrawal) | For consumers: the fourteen-day right, the model form, and when it lapses |
| [Imprint](/legal/imprint) | § 5 DDG provider identification for the service |
| [Security](/legal/security) | How to report a vulnerability, and what we do with it |
| [Sub-processors](/legal/subprocessors) | Every third party that processes data, and how changes are announced |

All six are published here. The copies served inside the app are mirrors of
these.

## This website and the software

The rest of this page. Using the open-source software needs no account, and
none of the documents above apply to it.

## Legal notice (Impressum gem. § 5 DDG)

**Alexander Ertli**\
Jungfernstieg\
20354 Hamburg, Germany

E-mail: <hello@contenox.com>\
Web: [ertli.com](https://ertli.com)

**Editorial responsibility (§ 18 Abs. 2 MStV)**\
Alexander Ertli, address as above.

VAT ID (USt-IdNr.): `DE429161583`\
Tax number: `2247005603265`

Online dispute resolution pursuant to Art. 14 (1) ODR-VO:
[consumer-redress.ec.europa.eu](https://consumer-redress.ec.europa.eu/dispute-resolution-bodies).
We are not obligated and not willing to participate in dispute resolution
proceedings before a consumer arbitration board.

## License, warranty and liability

Contenox is open-source software released under the [Apache License
2.0](https://www.apache.org/licenses/LICENSE-2.0). That licence governs your
use of the software. The sections below describe what it means in practice;
they do not replace it.

### What the software does — and what is warranted

The contenox software gives access to **the AI model you pick**, under **the
configuration you wrote**. Exactly this is warranted, and no more:

- that it is configurable;
- that it reaches only the AI model you named, as your rules specify;
- that it carries out the processing and checking steps you declared through the
  task engine, reviewed, and approved by using it.

The interfaces — in the terminal and in the browser — build on that task
engine. They are
example implementations of the contenox governance system — the rule
format, approvals by a human, and the triggers you declare — not validated end
products for any particular purpose.

The software makes no substantive decisions. The choice of AI model, the
prompts, the rules, the approvals and what an AI agent may touch on your machine
are your determinations. The correctness of the AI model's output is neither
checkable nor warranted.

### What is ours, and what is not

Contenox is an assembly, and being precise about which part is our work decides
what we can fairly be asked to stand behind.

**Our own work** — the governance system and the parts that carry it:

- the rule format in which you write rules, budgets and limits;
- approval by a human, with a durable saved state — a run pauses, the process is released, and it resumes exactly once when the
  question is answered, days later if need be;
- the system of declared triggers;
- the task engine, and the interfaces that build on it;
- the procedure by which a machine pairs to the relay, and the relay's fixed
  identity;
- `modeld`, the layer between contenox and the programs that run AI models on
  your own computer.

**Not ours** — used under their own licences and terms, and warranted by their
authors rather than by us:

- the AI models themselves, and the services that serve them — Ollama, vLLM,
  OpenAI, Anthropic, Google Vertex, AWS Bedrock, Mistral, OpenRouter and
  whatever else you configure;
- the programs that run those models on your computer, such as llama.cpp and
  OpenVINO;
- the open interface standards contenox follows — the Model Context Protocol
  (MCP) and the Agent Client Protocol (ACP) — and the editors and tools that
  also use them;
- the open-source libraries the build depends on, listed in `go.mod`.

We warrant our own part as described above and nothing beyond it. For anything
in the second list, your relationship is with its author or provider, on their
terms.

### Warranty

The software is provided "as is", without warranty of any kind — express or
implied — including but not limited to warranties of merchantability, fitness
for a particular purpose, or non-infringement.

### Liability

Liability is excluded **only as far as the law permits**. It is not excluded —
and under German law cannot be — for intent and gross negligence, for damage
arising from injury to life, body or health, under the German Product Liability
Act, under Art. 82 GDPR and other mandatory data protection law, or to the
extent of an expressly assumed guarantee.

### Your role and your obligations — wherever you are based

**Contenox is not region-locked.** You can run it anywhere, and which rules
reach you depends on where you are, where your users are, and whom your
deployment affects. The EU AI Act is set out below because we are based here
and it is the nearest example. Outside the EU
other regimes apply: US state AI statutes, sector regulators, professional
rules, and your own country's data protection and consumer law. Which of them
bind you is yours to establish. We cannot know that for you, and we do not.

Regulation (EU) 2024/1689 attaches duties to **roles**, not to software.
Whoever puts an AI system into use under their own authority is its
**deployer**; whoever places it on the market under their own name, assembles
it for a purpose of their own, or substantially modifies it becomes its
**provider**.

Running contenox, **you** make the decisions that create that role: you choose
the AI model, you write the configuration, you declare the triggers, and you
approve all of it by putting it into service. There is no step approved on your
behalf. You therefore determine the purpose and the means — and if you deploy
the result in a regulated field or make it available to others, the duties are
yours: risk management, documentation, record-keeping, human oversight,
transparency, and where applicable conformity assessment. Art. 4 AI Act's
AI-literacy duty already applies to providers and deployers alike.

Using contenox does not discharge any of that. What it gives you is controls an
assessment of your own can point at: rules you wrote, approvals recorded
durably, captured execution state. The assessment stays yours.

### Known limits and risks

The AI model's output can be wrong
and is not checked; automated steps can be irreversible once you permit them,
which forbidding rules and approvals by a human limit; incoming
content can carry injected instructions; and the system enforces the
rules you wrote rather than the ones you meant.

Contenox is not built for fields where an error causes personal injury, or
where automated processing decides directly about people — medicine, hiring,
credit scoring, law enforcement, critical infrastructure, official decisions.
Deploying it there is your assessment to make.

None of this is legal advice.

## Data & privacy

**The software runs on your machine.** Contenox stores its state (sessions,
chains, configuration) locally. Inputs and files you include in a request go
only to the AI model provider you configured — no server of ours processes your
workload. Using the software needs no account and no registration.

**The hosted relay is a separate, optional service.** If you create an account
at [app.contenox.com](https://app.contenox.com), you can reach the machines you
already run from a browser. That service does hold data about you — an account,
which machines are paired, and, if you subscribe, a billing reference. It
stores no session content: no inputs, no outputs, no files. What it holds, for
how long, and how to export or erase it is set out in its own documents:
[privacy policy](/legal/privacy), [terms of service](/legal/terms), and, for
consumers, [right of withdrawal](/legal/withdrawal), and the service
[imprint](/legal/imprint). How the service is secured and how to report a
vulnerability is on the [security page](/legal/security), and the processors it
uses are listed under [sub-processors](/legal/subprocessors). Nothing on this page applies to it, and nothing
there is needed to use the open-source software.

**This website is static.** contenox.com is a static site. It sets no cookies,
runs no analytics, and requires no account. Your color-scheme preference is
stored locally in your browser (`localStorage`) and never transmitted. Search
runs entirely in your browser against a locally downloaded index.

**Third-party requests.** The homepage loads release/star badges from
`img.shields.io` and fonts from Google Fonts; those requests are subject to the
respective providers' privacy terms. No other third-party resources are
embedded.

**E-mail.** If you contact <hello@contenox.com>, we process the information you
send to answer your request and for no other purpose.

*Last updated: 13 August 2026*
