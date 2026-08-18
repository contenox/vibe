---
title: KI-Agenten, lokal und souverän — AI-Souveränität mit Contenox
description: "AI-Souveränität operativ: KI-Agenten self-hosted mit Ollama oder vLLM, oder EU-Region auf eigenen Schlüsseln. KI-Governance als Datei, Human-in-the-Loop-Freigaben, harte Budgets, lokale SQLite — Open Source, kein Konto. Und was die KI-Verordnung damit zu tun hat."
eyebrow: AI-Souveränität
ogType: article
en: /docs/guide/sovereignty/
---

# KI-Agenten, lokal und souverän

Agentenbasierte KI-Systeme bleiben nicht im Chatfenster: Sie führen
Shell-Kommandos aus, schreiben Dateien, rufen APIs auf. Damit ist
AI-Souveränität keine Grundsatzdebatte mehr, sondern eine Betriebsfrage in fünf
Teilen — wo läuft die Inferenz, wo liegt der Zustand, wer hält die Schlüssel,
was kann der Agent erreichen, und wer entscheidet, wann ein Mensch eingreift.

Contenox ist so gebaut, dass jede dieser Antworten dir gehört. Nicht der
Anbieter entscheidet, wie sich der Agent auf deinem Rechner verhält — du. Jede
Antwort ist eine Datei, ein Flag oder ein Grant: lesbar, versionierbar,
widerrufbar.

## Was digitale Souveränität hier operativ heißt

- **Zustand bleibt lokal.** Sessions, Konfiguration, Run-Logs und erfasster
  Ausführungszustand liegen in SQLite auf deinem Rechner. Kein Konto, kein
  contenox-Dienst dazwischen — außer du [pairst](/docs/guide/pairing/) bewusst
  mit dem optionalen Relay; Telemetrie ist opt-in und standardmäßig aus.
- **Secrets bleiben in deiner Umgebung.** Backends referenzieren Credentials per
  Umgebungsvariable; der Wert wird zur Anfragezeit gelesen und landet nie in
  einer Config auf der Platte.
- **Der Agent sieht, was du gewährst — nicht, was du hast.** Jede
  agent-erreichbare Shell bekommt eine
  [bereinigte Least-Privilege-Umgebung](/docs/guide/confinement/environment/),
  Tools sind pro Aufruf allowlistet, und Sessions laufen nur in dem **einen**
  Workspace, mit dem die Instanz gestartet wurde — dem Startverzeichnis bei
  `beam` und `run`, dem angegebenen Pfad bei `serve`, dem vom Editor geöffneten
  Projekt bei `acp` — und nie in Config, Datenbank oder Policies der Runtime
  selbst.

## Self-hosted und lokale KI — oder EU-Region auf deinen Schlüsseln

Inferenz ist Konfiguration, nicht Architektur. Wenn nichts dein Netzwerk
verlassen darf, läuft alles lokal:
[Ollama](/docs/integrations/providers/ollama/) auf deinem Rechner oder
[vLLM](/docs/integrations/providers/openai/) auf eigenen GPUs — kein Prompt und
keine Antwort verlässt dein Netzwerk. Das ist die stärkste
Souveränitäts-Haltung, die contenox kennt, und der Standardweg im
[Quickstart](/docs/guide/quickstart/). So wird contenox zur selbst-gehosteten
Copilot-Alternative: deine Regeln, deine Modelle, deine Maschine — statt eines
Assistenten, dessen Verhalten und Telemetrie dem Anbieter gehören.

Für gehostete Modelle gilt: eigene Keys, gepinnte Region.
[AWS Bedrock in Frankfurt](/docs/integrations/providers/bedrock/#eu-regions)
(`eu-central-1`),
[Vertex AI in `europe-west3` oder `europe-west4`](/docs/integrations/providers/vertex/#eu-regions-and-data-residency),
[OpenAI über `eu.api.openai.com`](/docs/integrations/providers/openai/#eu-data-residency).
Ehrlich eingeordnet: Eine EU-Region beim US-Anbieter ist die schwächere Haltung
— dessen Infrastruktur und Vertragsbedingungen bleiben in der Schleife. Aber
Konto, Region und Schlüssel bleiben deine, und der Wechsel zu lokaler Inferenz
ist später eine Konfigurationsänderung, kein Umbau.

## KI-Governance für dich selbst, nicht für ein Gremium

KI-Governance klingt nach Ausschusssitzung. Gemeint ist hier etwas anderes: Du
willst einem KI-Agenten um drei Uhr nachts eine Mission überlassen können — und
wissen, dass er Arbeitsergebnisse nicht als „Cleanup" wegräumt, weil deine
Policy die Pfade, die zählen, per Deny-Regel unantastbar macht.

Das Instrument dafür ist der Envelope: eine JSON-Policy, die du geschrieben
hast. Sie benennt, was still durchläuft, was für einen Menschen pausiert und was
verweigert wird; was keine Regel trifft, schlägt geschlossen fehl — es fragt.
Dazu kommen Budgets: Obergrenzen für Turns, Tool-Aufrufe und Tokens, Allowlists
für Modelle und Backends. Eine Mission, die eine Grenze reißt, endet als
„stuck", statt weiterzulaufen. Die Datei liegt in deinem Repo und wird reviewt
wie jede andere Änderung; `contenox vet` prüft sie, bevor irgendetwas unter ihr
läuft.

```json
{
  "default_action": "approve",
  "rules": [],
  "compute": {
    "maxTurns": 40,
    "maxToolCalls": 200,
    "maxTokens": 2000000,
    "modelAllowlist": ["qwen3:8b"],
    "backendAllowlist": ["ollama"],
    "onExhausted": "finish_stuck"
  },
  "attention": { "allowAgentAnswers": false }
}
```

Dieser Envelope pinnt eine unbeaufsichtigte Mission auf lokale Inferenz: Die
Unit kann sich nicht selbst auf ein Modell oder Backend umschalten, das du nicht
benannt hast.

Und wenn der Run scheitert, rätst du nicht — du liest nach. `contenox state`
zeigt den erfassten Ausführungszustand vergangener Runs, Schritt für Schritt;
`--trace` liefert strukturierte Telemetrie auf stderr; jedes beantwortete Ask
hält dauerhaft fest, wer geantwortet hat — Mensch oder Agent. Vertrauenswürdige,
robuste KI ist als Formel ziemlich abgenutzt; mechanisch heißt sie hier:
geschriebene Regeln statt versteckter Prompts, Budgets statt Hoffnung, Traces
statt Vermutung.

## Human-in-the-Loop heißt: Die Freigabe wartet auf dich

Human-in-the-Loop ist bei contenox kein Dialog mit Timeout. Eine Frage, die die
Unit nicht allein entscheiden darf, checkpointet den Run: Das Ask wird
gespeichert, der Prozess freigegeben. Tage später beantwortest du es aus
irgendeinem Terminal — `contenox approvals respond` — und der Run läuft genau
einmal weiter. Standardmäßig darf nur ein Mensch antworten; ein Envelope kann
eine begrenzte Zahl von Routinefragen an den feuernden Agenten delegieren, und
der Datensatz zeigt immer, wer es war. Das vollständige Policy-Format steht im
[HITL-Guide](/docs/guide/hitl/).

## Die KI-Verordnung, ohne Beratersprech

Die KI-Verordnung — die deutsche Bezeichnung des EU AI Act, Verordnung (EU)
2024/1689 — verlangt für Hochrisiko-Systeme unter anderem wirksame menschliche
Aufsicht: Die Menschen dahinter sollen das System verstehen, in seinen Betrieb
eingreifen und es unterbrechen können. Man muss die Verordnung nicht mögen, um
ihre Fragen gut zu finden — es sind dieselben, die du dir selbst stellst, bevor
du einem Agenten etwas überlässt. Envelope, durable Freigaben, Budgets und
Traces sind die operativen Antworten darauf, geschrieben von dir statt von einem
Anbieter-Default.

Klar gesagt: Das ist keine Rechtsberatung, und contenox macht ein Deployment
nicht „compliant". Ob die Verordnung dein System überhaupt betrifft, hängt davon
ab, was du baust und ausrollst — die Bewertung gehört dir und deiner
Rechtsberatung. Was contenox liefert, sind Kontrollen, auf die eine solche
Bewertung zeigen kann: von dir verfasst, versioniert, inspizierbar.

## Open Source, eine Binärdatei, kein Konto

Contenox ist Open Source unter Apache-2.0. Der Code, der deine Regeln
durchsetzt, ist der Code, den du lesen kannst — dieselbe Transparenz, die du vom
Envelope erwartest, eine Ebene tiefer. Eine Binärdatei, lokale SQLite, kein
Konto, nichts telefoniert nach Hause ohne Opt-in. Und wenn du morgen aufhörst,
contenox zu benutzen, bleiben deine Chains, Policies und Logs, was sie immer
waren: Dateien auf deiner Platte.

## Weiter in die Tiefe (englisch)

- [AI Sovereignty & the EU AI Act](/docs/guide/sovereignty/) — das englische
  Gegenstück dieser Seite, mit der vollständigen Zuordnung von Aufsichts-Themen
  zu Mechanismen.
- [HITL Policies](/docs/guide/hitl/) — Policy-Format, Bedingungsoperatoren,
  Presets und Attention-Bounds.
- [Ollama](/docs/integrations/providers/ollama/) ·
  [vLLM / OpenAI-kompatibel](/docs/integrations/providers/openai/) ·
  [Bedrock EU-Regionen](/docs/integrations/providers/bedrock/#eu-regions) ·
  [Vertex EU-Regionen](/docs/integrations/providers/vertex/#eu-regions-and-data-residency) ·
  [OpenAI EU-Residency](/docs/integrations/providers/openai/#eu-data-residency)
  — die Provider-Anleitungen hinter „lokal und souverän".
- [Quickstart](/docs/guide/quickstart/) — Installation und die erste Mission.
