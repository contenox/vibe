# Nutzungsbedingungen / Terms of Service

**Version `terms-2026-08-13-r2` — gültig ab 13. August 2026**

Maßgeblich veröffentlicht unter <https://contenox.com/legal/terms>.
Canonically published at <https://contenox.com/legal/terms>.

Die deutsche Fassung ist maßgeblich. The German version prevails; the English
text below is a mirror provided for convenience.

---

# Teil A — Deutsch (maßgeblich)

## Kurzfassung ohne Fachbegriffe

Diese Zusammenfassung erklärt, worum es geht. Verbindlich sind die nummerierten
Abschnitte darunter.

Contenox besteht aus drei Dingen, die rechtlich **getrennt** sind:

1. **Die Software.** Ein Programm, das Sie kostenlos herunterladen und auf
Ihrem eigenen Rechner betreiben. Es verbindet Ihren Rechner mit dem KI-Modell,
das **Sie** ausgewählt haben, und führt die Arbeitsschritte aus, die **Sie**
vorher aufgeschrieben und freigegeben haben. Dafür gilt die Apache-Lizenz 2.0,
nicht dieser Vertrag.
2. **Der Zugangsdienst** (das hier Vereinbarte). Ein Fernzugang: Er lässt Sie
aus dem Browser auf die Programme zugreifen, die auf Ihren eigenen Rechnern
bereits laufen. Er entscheidet nichts, wählt kein KI-Modell und führt keinen
Arbeitsschritt aus.
3. **Rechenkapazität in Ihrem Auftrag.** Falls wir gesondert vereinbaren, dass
wir Rechner für Sie anmieten oder betreiben, kaufen wir diese in Ihrem Namen
und für Ihre Rechnung ein. Anbieter der Rechenleistung sind dann nicht wir.

**Was das für die Verantwortung heißt.** Was ein KI-Agent tut, ergibt sich aus
Ihrer Konfiguration, Ihrem KI-Modell und Ihren Vorgaben. Das ist keine
Leistung, die wir Ihnen schulden — wir schulden den Zugang dorthin. Wo das
Gesetz trotzdem eine Haftung anordnet, gilt sie; wir schließen nichts aus, was
sich nicht ausschließen lässt (Abschnitt 10).

**Wer nach der KI-Verordnung verantwortlich ist.** Sie wählen das KI-Modell,
Sie schreiben die Regeln, Sie legen fest, was einen Ablauf auslöst — und Sie
geben das alles frei, indem Sie es verwenden. Genau diese Entscheidungen machen
nach der KI-Verordnung und vergleichbaren Vorschriften **Sie** zur
verantwortlichen Stelle, nicht uns (Abschnitt 6).

**Was Sie nicht damit tun sollten.** Kein Einsatz in Medizin, Personalauswahl,
Kreditvergabe, Strafverfolgung, kritischer Infrastruktur oder behördlichen
Entscheidungen ohne eigene rechtliche Prüfung (Abschnitt 6).

---

## 1. Anbieter und Geltungsbereich

Anbieter ist Alexander Ertli, Jungfernstieg, 20354 Hamburg, Deutschland
(hello@contenox.com; im Folgenden „wir"). Diese Bedingungen gelten für den
gehosteten Zugangsdienst unter relay.contenox.com und app.contenox.com samt der
dort ausgelieferten Web-App, gegenüber Verbrauchern und Unternehmern.

Vollständige Anbieterangaben nach § 5 DDG stehen im Impressum.

## 2. Die drei Ebenen

Contenox ist kein einzelnes Produkt, sondern drei getrennte Dinge mit
getrennten Rechtsverhältnissen. Welche Ebene betroffen ist, entscheidet, wer
wofür einsteht.

### 2.1 Die Software — nicht Gegenstand dieses Vertrags

Die contenox-Software ist quelloffen unter der Apache-Lizenz 2.0. Er
läuft auf Ihren eigenen Maschinen, benötigt weder Konto noch Registrierung noch
Entgelt, und für ihn gilt allein seine Lizenz.

**Was die Software leistet — und was damit zugesichert ist.** Die Software
verschafft Zugriff auf **das KI-Modell, das Sie auswählen**, unter **der
Konfiguration, die Sie geschrieben haben**. Zugesichert ist genau dies und
nicht mehr:

- dass er konfigurierbar ist,
- dass er ausschließlich das von Ihnen benannte KI-Modell anspricht, so wie
  Ihre Regeln es vorgeben,
- dass er die Verarbeitungs- und Prüfschritte ausführt, die Sie über die
  Ablaufsteuerung deklariert, überprüft und durch deren Verwendung freigegeben
  haben.

Die Bedienoberflächen — im Terminal wie im Browser — bauen auf dieser
Ablaufsteuerung auf. Sie sind
**Beispielimplementierungen** des contenox-Governance-Systems — also des
Regelformats, der Freigaben durch einen Menschen und der festgelegten
Auslöser —
und nicht als geprüfte Endprodukte für einen bestimmten Einsatzzweck zu
verstehen.

**Was die Software nicht leistet.** Sie trifft keine inhaltlichen
Entscheidungen. Die Auswahl des KI-Modells, die Eingaben, die Regeln, die
Freigaben und die Frage, worauf ein KI-Agent auf Ihrer Maschine zugreifen darf,
sind Ihre Festlegungen. Die Richtigkeit der Ausgaben des KI-Modells ist weder
prüfbar noch zugesichert.

Welche Teile des Systems von uns stammen und welche von Dritten — die
KI-Modelle, die Programme, die sie ausführen, die Schnittstellen-Standards MCP
und ACP, die Programmbibliotheken — steht auf der Rechtsseite von contenox.com. Wir sichern
unseren eigenen Teil zu und nichts darüber hinaus.

### 2.2 Der Zugangsdienst — das hier Vereinbarte

Der Dienst ist eine **Zugangsschicht**. Er verschafft Ihnen Zugang zu

- Sitzungen Ihrer KI-Agenten, die Sie selbst oder jemand in Ihrem Auftrag auf Ihren
  eigenen Maschinen bereitgestellt hat, erreichbar aus dem Browser, und
- den Auslösern und Meldequellen, die **Sie** über das Relay festgelegt haben.

Die Maschine wählt sich beim Relay ein; es wird kein Port geöffnet. Das Relay
leitet Sitzungsdaten weiter und **speichert sie nicht** — keine Eingaben, keine
Antworten, keine Dateien. Einzelheiten in der Datenschutzerklärung.

Der Dienst wählt kein KI-Modell aus, formuliert keine Eingabe, trifft keine
Freigabeentscheidung und führt keinen Schritt eines KI-Agenten aus. Er stellt die
Verbindung her und verwaltet Konto, Team und Abrechnung.

**Wo Ihre Daten liegen.** Der gehostete Dienst speichert seine Daten in der
Europäischen Union — Google Cloud, Region `europe-west3`, Frankfurt am Main.
Eine andere Region bieten wir derzeit nicht an. Wenn eine Speicherung in der EU
den für Sie geltenden Anforderungen nicht genügt, nutzen Sie den gehosteten
Dienst bitte nicht: Die Software selbst benötigt kein Konto, und dann erreicht
uns nichts von Ihnen.

### 2.3 Rechenkapazität in Ihrem Auftrag

Vereinbaren wir gesondert, dass wir Rechenkapazität für Sie beschaffen oder
betreiben, geschieht dies **in Ihrem Auftrag und für Ihre Rechnung**. Wir sind
nicht Anbieter dieser Rechenkapazität. Es gelten zusätzlich die Bedingungen des
jeweiligen Rechenzentrums- oder KI-Modell-Anbieters, auf die wir Sie vor der
Beauftragung hinweisen. Für Verfügbarkeit, Preise und Leistungsumfang
steht der jeweilige Anbieter ein, nicht wir.

Ohne eine solche gesonderte Vereinbarung ist keine Rechenkapazität Bestandteil
dieses Vertrags.

## 3. Tarife

| Tarif | Preis | Gekoppelte Maschinen | Plätze |
|---|---|---|---|
| Free | € 0 | eine je aktivem Mitglied, gemeinsam genutzt | bis zu 3 |
| Pro | € 29/Monat pauschal, inkl. USt. | 25 | 25 |

Pro ist ein Pauschaltarif, kein Preis je Platz oder je Maschine. Die jeweils
geltenden Kontingente zeigt auch die Preisdarstellung im Produkt.

## 4. Ihre Verantwortung — Sie bestimmen, was KI-Agenten dürfen

Die KI-Agenten laufen auf Ihren Maschinen, mit Ihren Dateien, Ihren Schlüsseln und
unter Richtlinien, die Sie festlegen. **Sie schreiben die Vorgaben; wir
betreiben die Vermittlung.** Das heißt:

- Sie konfigurieren und genehmigen, was KI-Agenten auf Ihren Maschinen tun dürfen;
  deren Handlungen und Ausgaben erfolgen unter Ihrer Kontrolle.
- Für Inhalte, die über von Ihnen konfigurierte Quellen eingeliefert werden
  (Meldungen anderer Systeme, Formulare auf Ihren Websites), sind Sie verantwortlich —
  einschließlich der datenschutzrechtlichen Zulässigkeit. Verarbeiten wir dabei
  personenbezogene Daten Dritter für Sie, geschieht das weisungsgebunden; einen
  Vertrag zur Auftragsverarbeitung nach Art. 28 DSGVO stellen wir auf Anfrage
  bereit.
- Sie halten Zugangsdaten (Passwort, Kopplungsschlüssel) geheim und melden uns
  einen Verdacht auf Missbrauch.

## 5. Verfügbarkeit

Der Dienst wird ohne Verfügbarkeitszusage erbracht (kein SLA); wir bemühen uns
nach besten Kräften. Ein Ausfall des Relays kostet die Erreichbarkeit von
unterwegs — die Arbeit auf Ihren Maschinen
läuft lokal weiter und geht dadurch nicht verloren. Wartungen können den Dienst
kurzzeitig unterbrechen.

## 6. Anwendungsgrenzen und bekannte Risiken

Dieser Abschnitt beschreibt, wofür der Dienst nicht gebaut ist und welche
Risiken bekannt sind. Er beschränkt keine gesetzliche Haftung, sondern grenzt
ab, was wir schulden.

**Nicht bestimmt für.** Der Dienst ist nicht für Einsatzfelder bestimmt, in
denen ein Fehler zu Personenschäden führt oder in denen automatisierte
Verarbeitung unmittelbar über Menschen entscheidet — insbesondere Medizin und
Gesundheitsversorgung, Personalauswahl, Kreditvergabe und Bonitätsbewertung,
Strafverfolgung, Migration und Grenzkontrolle, kritische Infrastruktur sowie
behördliche Entscheidungen.

Wer ihn dort dennoch einsetzt, prüft die Zulässigkeit selbst und erfüllt die
dafür vorgeschriebenen Pflichten.

### Ihre Rolle und Ihre Pflichten — je nachdem, wo Sie ansässig sind

**Contenox ist nicht regional beschränkt.** Sie können es überall betreiben,
und welchen Vorschriften Sie unterliegen, hängt davon ab, wo Sie ansässig sind,
wo Ihre Nutzer ansässig sind und wen Ihr Einsatz betrifft. Nachfolgend stellen
wir die KI-Verordnung der EU dar, weil wir hier ansässig sind und sie das
nächstliegende Beispiel ist. Außerhalb der EU gelten andere
Regelwerke: KI-Gesetze einzelner US-Bundesstaaten, sektorspezifische Aufsicht,
Berufsrecht sowie Datenschutz- und Verbraucherrecht Ihres Landes. Welche davon
für Sie gelten, müssen Sie selbst feststellen.

Diese deutsche Fassung behandelt das deutsche und europäische Recht. Wenn Sie
außerhalb Deutschlands ansässig sind oder Nutzer außerhalb Deutschlands haben,
finden Sie in **Teil B, Abschnitt 6** eine nach Regionen geordnete Übersicht —
Vereinigtes Königreich, USA einschließlich Kalifornien und Colorado,
Australien, Indien und China — sowie den Hinweis, welche dortigen
Verbraucherrechte durch diesen Vertrag nicht eingeschränkt werden.

Die Verordnung (EU) 2024/1689 (KI-Verordnung, „AI Act") knüpft Pflichten an
**Rollen**, nicht an Software. Wer ein KI-System unter eigener Verantwortung
einsetzt, ist dessen **Betreiber**. Wer es unter eigenem Namen bereitstellt,
für einen eigenen Zweck zusammenstellt oder wesentlich verändert, wird dessen
**Anbieter** — mit deutlich weiter reichenden Pflichten.

Bei contenox treffen **Sie** genau die Entscheidungen, die diese Rolle
begründen:

- **Sie wählen das KI-Modell.** Welches KI-Modell antwortet, ist Ihre Auswahl;
  die Software spricht nur das an, was Sie benennen.
- **Sie schreiben die Konfiguration.** Regeln, Freigaben, Budgets und Grenzen
  sind Ihr Text, in Ihrem eigenen Projektverzeichnis, von Ihnen geprüft.
- **Sie legen die Auslöser fest.** Was einen Ablauf startet — ein Zeitplan, die
  Meldung eines anderen Systems, ein ausgefülltes Formular — bestimmen Sie.
- **Sie erklären Ihre Freigabe durch die Verwendung.** Ein Ablauf startet, weil
  Sie ihn gestartet oder eingeplant haben; eine Regeldatei gilt, weil Sie sie
  geschrieben und in Betrieb genommen haben. Es gibt keinen Schritt, den wir
  für Sie genehmigen.

Damit bestimmen Sie Zweck und Mittel des Einsatzes. Setzen Sie das Ergebnis in
einem der oben genannten Bereiche ein oder stellen Sie es Dritten bereit,
können Sie **Betreiber oder Anbieter eines Hochrisiko-KI-Systems** werden — mit
eigenen Pflichten, unter anderem Risikomanagement, Daten-Governance, technische
Dokumentation, Protokollierung, menschliche Aufsicht, Transparenz gegenüber
betroffenen Personen sowie gegebenenfalls Konformitätsbewertung und
Registrierung.

Unabhängig davon trifft Anbieter **und** Betreiber bereits die Pflicht zur
KI-Kompetenz nach Art. 4 KI-VO: Wer KI-Systeme betreibt, muss für ausreichende
Kenntnisse der damit befassten Personen sorgen.

**Diese Pflichten treffen Sie. Wir übernehmen sie nicht, und die Nutzung von
contenox erfüllt sie nicht.** Was contenox liefert, sind Kontrollen, auf die
eine eigene Bewertung zeigen kann: von Ihnen verfasste Regeln, dauerhaft
festgehaltene Freigaben und erfasster Ausführungszustand. Die Bewertung selbst
bleibt Ihre.

Neben der KI-Verordnung können weitere Regelungen greifen — etwa Art. 22 DSGVO
bei automatisierten Entscheidungen mit rechtlicher Wirkung, Sektor- und
Berufsrecht, Produktsicherheits- und Arbeitsrecht. Dieser Abschnitt ist eine
Warnung, keine Rechtsberatung.

**Bekannte Risiken:**

- **Die Ausgaben des KI-Modells können falsch sein.** KI-Sprachmodelle erzeugen
  plausible, aber unzutreffende Ergebnisse. Eine Prüfung findet nicht statt und
  ist technisch nicht möglich.
- **Automatisierte Schritte können irreversibel wirken.** Ein KI-Agent, dem Sie
  erlauben, schreibt Dateien, führt Kommandos aus und ruft fremde Systeme auf.
  Deshalb sind Verbotsregeln und Freigaben durch einen Menschen Teil des Systems
und
  sollten genutzt werden.
- **Eingelieferte Inhalte können Anweisungen enthalten** (eingeschleuste Anweisungen).
  Wer eine Einlieferungsquelle öffnet, öffnet einen Kanal, über den Dritte Text
  in einen Ablauf bringen können.
- **Ihre eigenen Regeln können falsch sein.** Das System setzt durch, was Sie
  geschrieben haben, nicht, was Sie gemeint haben.

## 7. Zulässige Nutzung

Untersagt sind rechtswidrige Nutzung, Eingriffe in den Betrieb des Dienstes (u.
a. Umgehen von Raten- und Kontingentgrenzen, Missbrauch der
Einlieferungsendpunkte, unbefugter Zugriff auf fremde Konten oder Maschinen)
und die Nutzung zur Verbreitung von Schadsoftware oder Spam. Bei Verstößen
können wir Konten sperren; berechtigte Interessen des Nutzers werden dabei
berücksichtigt.

## 8. Preise, Zahlung, Laufzeit

Pro wird monatlich über den Zahlungsdienstleister Stripe abgerechnet und ist
jederzeit zum Ende des Abrechnungszeitraums über das Kundenportal kündbar; bis
dahin bleibt der Leistungsumfang bestehen. Geschäftskonten werden auf Rechnung
abgerechnet. Preisänderungen gelten nur für künftige Abrechnungszeiträume und
werden vorher mitgeteilt.

Für Verbraucher: Widerrufsrecht und dessen vorzeitiges Erlöschen sind in der
Widerrufsbelehrung geregelt, die bei der Registrierung gesondert verlinkt und
bestätigt wird.

## 9. Beendigung und Löschung

Sie können Ihr Konto jederzeit selbst löschen; die Löschfolgen und die zwei
Ausnahmen (letzter Owner eines Kontos mit weiteren Mitgliedern; laufendes
Abonnement — erst kündigen) beschreibt die Datenschutzerklärung. Wir können den
Vertrag mit angemessener Frist kündigen, aus wichtigem Grund fristlos.

Nach Beendigung bleibt die Software auf Ihren Maschinen nutzbar. Sie verlieren
den Fernzugang, nicht Ihre Arbeit.

## 10. Haftung

**Vorbemerkung in einem Satz:** Wir schließen Haftung nur aus, soweit das
gesetzlich zulässig ist — und für einiges ist es das nicht.

1. **Unbeschränkt haften wir** für Vorsatz und grobe Fahrlässigkeit, für
Schäden aus der Verletzung des Lebens, des Körpers oder der Gesundheit, nach
dem Produkthaftungsgesetz, nach Art. 82 DSGVO und sonstigem zwingenden
Datenschutzrecht sowie im Umfang einer ausdrücklich übernommenen Garantie.
2. **Bei einfacher Fahrlässigkeit** haften wir nur für die Verletzung
wesentlicher Vertragspflichten (Kardinalpflichten) — Pflichten, deren Erfüllung
die ordnungsgemäße Durchführung des Vertrags überhaupt erst ermöglicht und auf
deren Einhaltung Sie regelmäßig vertrauen dürfen — und der Höhe nach begrenzt
auf den vertragstypischen, bei Vertragsschluss vorhersehbaren Schaden.
3. **Im Übrigen** ist die Haftung ausgeschlossen, soweit gesetzlich zulässig.
Für unentgeltliche Leistungen (Free-Tarif) bleibt es — unbeschadet Nr. 1 — bei
der Haftung für Vorsatz und grobe Fahrlässigkeit.
4. **Abgrenzung, keine Freizeichnung.** Handlungen und Ausgaben von KI-Agenten,
die auf Ihren Maschinen unter Ihrer Konfiguration und mit dem von Ihnen
gewählten KI-Modell laufen (Abschnitte 2.1 und 4), sind **keine von uns
geschuldete Leistung**. Sie sind damit kein Gegenstand unserer Leistungspflicht
— was uns nicht von der Haftung nach Nr. 1 befreit und nicht von der Haftung
für die Zugangsschicht, die wir tatsächlich schulden.
5. Nr. 1 bis 4 gelten auch zugunsten unserer Erfüllungsgehilfen.

## 11. Änderungen dieser Bedingungen

Änderungen veröffentlichen wir mit neuer Versionskennung und teilen sie aktiven
Nutzern vorher in Textform mit. Für bestehende Verträge gelten sie nur, wenn
Sie zustimmen oder die Änderung aus triftigem Grund erforderlich und für Sie
zumutbar ist. Widersprechen Sie einer Änderung, können Sie den Vertrag zum
Wirksamwerden der Änderung kündigen.

## 12. Schlussbestimmungen

Es gilt deutsches Recht. Sind Sie Verbraucher, bleiben die zwingenden
Schutzvorschriften des Staates Ihres gewöhnlichen Aufenthalts unberührt.
Gerichtsstand gegenüber Kaufleuten ist Hamburg. Sollten einzelne Bestimmungen
unwirksam sein, bleibt der Vertrag im Übrigen wirksam.

---

# Part B — English (mirror of Part A)

## Plain-language summary

This summary explains the shape of the thing. The numbered sections below are
what binds.

Contenox is three legally **separate** things:

1. **The software.** A program you download for free and run on your own
machine. It connects your machine to the AI model **you** chose and carries out
the steps **you** wrote down and approved beforehand. It is governed by the
Apache License 2.0, not by this contract.
2. **The access service** (what is agreed here). Remote reach: it lets you get
at the programs already running on your own machines, from a browser. It
decides nothing, chooses no AI model, and executes no step.
3. **Compute on your behalf.** If we separately agree that we rent or operate
machines for you, we buy them in your name and for your account. We are then
not the provider of that compute.

**What that means for responsibility.** What an AI agent does follows from your
configuration, your AI model and your instructions. That is not a service we
owe you — what we owe is the access to it. Where the law imposes liability
anyway, it applies; we exclude nothing that cannot be excluded (section 10).

**Who is responsible under the AI Act.** You choose the AI model, you write
rules, you decide what starts a run — and you approve all of it by using it.
Those decisions are exactly what makes **you** the responsible party under the
AI Act and comparable rules, not us (section 6).

**What you should not use it for.** Not for medicine, hiring, credit scoring,
law enforcement, critical infrastructure or official decisions without your own
legal assessment (section 6).

---

## 1. Provider and scope

The provider is Alexander Ertli, Jungfernstieg, 20354 Hamburg, Germany
(hello@contenox.com; "we"). These terms govern the hosted access service at
relay.contenox.com and app.contenox.com including the web app served there, for
consumers and businesses alike.

Full provider details under § 5 DDG are in the imprint.

## 2. The three layers

Contenox is not one product but three separate things in three separate legal
relationships. Which layer is involved decides who answers for what.

### 2.1 The software — not governed by this contract

The contenox software is open source under the Apache License 2.0. It
runs on your own machines, needs no account, no registration and no payment,
and is governed solely by that licence.

**What the software does — and what is therefore warranted.** The contenox
software gives access to **the AI model you pick**, under **the configuration you wrote**.
Exactly this is warranted, and no more:

- that it is configurable,
- that it reaches only the AI model you named, as your rules specify,
- that it carries out the processing and checking steps you declared through the
  task engine, reviewed, and approved by using it.

The interfaces — in the terminal and in the browser — build on that task
engine. They are
**example implementations** of the contenox governance system — the rule
format, approvals by a human, and the triggers you declare — and are not to be
understood as validated end products for any particular purpose.

**What the software does not do.** It makes no substantive decisions. Model
choice, prompts, rules, approvals and what an AI agent may touch on your machine
are your determinations. The correctness of the AI model's output is neither
checkable nor warranted.

Which parts of the system are ours and which are third-party — the AI models,
the programs that run them, the MCP and ACP interface standards, the software
libraries — is set out
on contenox.com's legal page. We warrant our own part and nothing beyond it.

### 2.2 The access service — what is agreed here

The service is an **access layer**. It gives you access to

- AI agent sessions that you, or someone acting for you, deployed on your own
  machines, reachable from a browser, and
- the triggers and ingestion sources **you** declared through the relay.

The machine dials out to the relay; no port is opened. The relay routes session
data and **does not store it** — no inputs, no outputs, no files. Details in
the privacy policy.

The service selects no AI model, composes no input, makes no approval decision
and executes no step of an AI agent. It establishes the connection and administers
account, team and billing.

**Where your data is held.** The hosted service stores its data in the European
Union — Google Cloud, region `europe-west3`, Frankfurt am Main. There is no
other region on offer today. If EU storage does not satisfy the requirements
that apply to you, do not use the hosted service: the software itself needs no
account, and then nothing of yours reaches us at all.

### 2.3 Compute on your behalf

If we separately agree that we procure or operate compute for you, this happens
**on your behalf and for your account**. We are not the provider of that
compute. The terms of the respective data-centre or AI-model provider apply in
addition, and we will point you to them before you instruct us. They answer for
its availability, pricing and content of service, not us.

Absent such a separate agreement, no compute forms part of this contract.

## 3. Tiers

| Tier | Price | Paired machines | Seats |
|---|---|---|---|
| Free | €0 | one per active member, pooled | up to 3 |
| Pro | €29/month flat, VAT included | 25 | 25 |

Pro is a flat rate, not a per-seat or per-machine price. The allowances in
force are also shown in the product's pricing display.

## 4. Your responsibility — you decide what AI agents may do

The AI agents run on your machines, with your files, your keys, and under policies
you set. **You write the rules; we operate the machinery.** That means:

- You configure and approve what AI agents may do on your machines; their actions
  and outputs happen under your control.
- You are responsible for content delivered through sources you configured
  (messages from other systems, forms on your websites) — including its lawfulness under data
  protection law. Where we process third parties' personal data for you in
  doing so, we act on your instructions; a data processing agreement under Art.
  28 GDPR is available on request.
- You keep credentials (password, pairing keys) secret and report suspected
  misuse to us.

## 5. Availability

The service is provided without an availability commitment (no SLA); we make
best efforts. A relay outage costs you remote reach — the work on your machines continues locally and is not lost.
Maintenance may briefly interrupt the service.

## 6. Limits of application, and known risks

This section describes what the service is not built for and which risks are
known. It limits no statutory liability; it defines what we owe.

**Not intended for.** The service is not intended for fields in which an error
causes personal injury, or in which automated processing decides directly about
people — in particular medicine and healthcare, hiring, credit scoring and
creditworthiness assessment, law enforcement, migration and border control,
critical infrastructure, and official decisions.

Anyone deploying it there assesses admissibility themselves and carries the
duties prescribed for it.

### Your role and your obligations — wherever you are based

**Contenox is not region-locked.** You can run it anywhere, and which rules
reach you depends on where you are, where your users are, and whom your
deployment affects. The EU AI Act is set out below because we are based here
and it is the nearest example. Outside the EU
other regimes apply: US state AI statutes, sector regulators, professional
rules, and your own country's data protection and consumer law. Which of them
bind you is yours to establish. We cannot know that for you, and we do not.

#### Where the rules come from, by region

Orientation only, and not exhaustive.

| If you are in | AI-specific rules to check | Privacy law | Consumer protections you cannot sign away |
|---|---|---|---|
| **EU / EEA** | AI Act (EU) 2024/1689 — set out below | GDPR | Mandatory consumer law of your country of residence |
| **United Kingdom** | No single AI act; the ICO, FCA and sector regulators apply existing law. UK GDPR Art. 22 covers automated decisions | UK GDPR + Data Protection Act 2018 | Consumer Rights Act 2015 |
| **United States** | No federal AI act. State law instead — Colorado's AI Act for consequential automated decisions, California's training-data and AI-disclosure statutes, New York City's rules for automated hiring tools. The FTC treats unsupported AI claims as deceptive practice | State privacy laws; California's CCPA/CPRA is the broadest | Implied warranties and state consumer statutes; several states restrict how far they can be disclaimed |
| **Australia** | No AI act yet; the Privacy Act and sector regulators apply | Privacy Act 1988 and the Australian Privacy Principles | **Australian Consumer Law guarantees cannot be excluded.** Nothing in these terms limits them |
| **India** | The DPDP Act and MeitY advisories | Digital Personal Data Protection Act 2023 | Consumer Protection Act 2019 |
| **China** | PIPL, the generative-AI measures, and algorithm filing requirements | PIPL | Local consumer law |
| **Anywhere else** | Assume something applies and check | Assume something applies and check | Local mandatory law prevails over these terms |

**Which of these bind you is yours to establish.** It depends on where you are,
who your users are, and what you build. The table is a starting point, not an
assessment, and it will go out of date.

**Nothing here overrides your local mandatory protections.** Section 12 chooses
German law, and that choice cannot take away rights your own law gives you and
does not allow to be contracted out — Australian Consumer Law guarantees and UK
statutory consumer rights among them.

Regulation (EU) 2024/1689 (the AI Act) attaches duties to **roles**, not to
software. Whoever puts an AI system into use under their own authority is its
**deployer**. Whoever places it on the market under their own name, assembles
it for a purpose of their own, or substantially modifies it becomes its
**provider** — with considerably heavier duties.

With contenox, **you** make precisely the decisions that create that role:

- **You choose the AI model.** Which AI model answers is your selection; the
  software reaches only what you name.
- **You write the configuration.** Rules, approvals, budgets and limits are
  your text, in your repository, reviewed by you.
- **You declare the triggers.** What starts a run — a schedule, a message from
  another system, a submitted form — is yours to decide.
- **You express approval by using it.** A run starts because you started or
  scheduled it; a rule file applies because you wrote it and put it into
  service. There is no step we approve on your behalf.

You therefore determine the purpose and the means of the deployment. If you use
the result in one of the fields named above, or make it available to third
parties, you may become the **deployer or provider of a high-risk AI system** —
with your own duties, among them risk management, data governance, technical
documentation, record-keeping, human oversight, transparency towards affected
persons, and where applicable conformity assessment and registration.

Separately, both providers **and** deployers already carry the AI-literacy duty
under Art. 4 AI Act: whoever operates AI systems must ensure a sufficient level
of competence among the people involved.

**Those duties fall on you. We do not assume them, and using contenox does not
discharge them.** What contenox provides is controls an assessment of your own
can point at: rules you wrote, approvals recorded durably, and captured
execution state. The assessment itself remains yours.

Beyond the AI Act, other rules may apply — Art. 22 GDPR for automated decisions
with legal effect, sector and professional law, product safety and employment
law among them. This section is a warning, not legal advice.

**Known risks:**

- **The AI model's output can be wrong.** AI language models produce plausible
  but incorrect results. No check takes place, and none is technically
  possible.
- **Automated steps can be irreversible.** An AI agent you permit will write
  files, run commands and call third-party systems. Deny rules and
  human-in-the-loop approvals are part of the system for that reason and should
  be used.
- **Ingested content can contain instructions** (injected instructions). Opening an
  ingestion source opens a channel through which third parties can put text
  into a run.
- **Your own rules can be wrong.** The system enforces what you wrote, not what
  you meant.

## 7. Acceptable use

Prohibited: unlawful use, interference with the operation of the service
(including circumventing rate and allowance limits, abusing the ingestion
endpoints, unauthorised access to others' accounts or machines), and use for
distributing malware or spam. On violations we may suspend accounts; the user's
legitimate interests are taken into account.

## 8. Prices, payment, term

Pro is billed monthly through the payment provider Stripe and can be cancelled
at any time, effective at the end of the billing period, via the customer
portal; the service level remains until then. Business accounts are billed by
invoice. Price changes apply only to future billing periods and are announced
in advance.

For consumers: the right of withdrawal and its early lapse are set out in the
withdrawal notice, which is linked and confirmed separately at registration.

## 9. Termination and deletion

You can delete your account yourself at any time; the consequences and the two
exceptions (last owner of an account with other members; a live subscription —
cancel first) are described in the privacy policy. We may terminate with
reasonable notice, and without notice for cause.

After termination the software on your machines keeps working. You lose remote
reach, not your work.

## 10. Liability

**In one sentence:** we exclude liability only as far as the law allows — and
for some things it does not.

1. **We are liable without limit** for intent and gross negligence, for damage
from injury to life, body or health, under the German Product Liability Act,
under Art. 82 GDPR and other mandatory data protection law, and to the extent
of an expressly assumed guarantee.
2. **For simple negligence** we are liable only for breach of essential
contractual obligations (cardinal obligations) — obligations whose fulfilment
makes proper performance of the contract possible at all and on whose
observance you may regularly rely — and limited in amount to the foreseeable
damage typical of this kind of contract at the time of conclusion.
3. **Otherwise** liability is excluded as far as the law allows. For services
provided free of charge (the Free tier), liability — clause 1 unaffected —
remains limited to intent and gross negligence.
4. **Scope, not a disclaimer.** Actions and outputs of AI agents running on your
machines under your configuration and with the AI model you chose (sections 2.1
and 4) are **not a service we owe**. They are therefore outside our obligation
to perform — which does not release us from liability under clause 1, nor from
liability for the access layer we actually do owe.
5. Clauses 1 to 4 also apply in favour of our agents and subcontractors.

## 11. Changes to these terms

We publish changes under a new version identifier and notify active users in
text form in advance. For existing contracts they apply only if you consent, or
if the change is necessary for a valid reason and reasonable for you. If you
object to a change, you may terminate the contract with effect from the date
the change takes effect.

## 12. Final provisions

German law applies. If you are a consumer, the mandatory protections of the
state of your habitual residence remain unaffected. The place of jurisdiction
towards merchants is Hamburg. Should individual provisions be invalid, the
remainder of the contract remains in force.
