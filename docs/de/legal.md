---
title: Rechtliches
description: Alle Rechtsdokumente von Contenox an einer Stelle — Nutzungsbedingungen, Datenschutzerklärung, Widerrufsbelehrung, Impressum, Sicherheit und Unterauftragsverarbeiter für den gehosteten Dienst, dazu die Angaben zu dieser Website und der quelloffenen Software.
en: /legal
---

# Rechtliches

Alles Rechtliche an einer Stelle. Zwei Gruppen: die Dokumente für den
**gehosteten Dienst**, für den Sie sich anmelden, und die Angaben zu **dieser
Website und der quelloffenen Software**, für die es kein Konto braucht.

## Der gehostete Dienst (app.contenox.com)

| Dokument | Worum es geht |
|---|---|
| [Nutzungsbedingungen](/legal/terms) | Der Vertrag: die drei Ebenen, was wir schulden, die Haftung, und welche Vorschriften Sie wo treffen |
| [Datenschutzerklärung](/legal/privacy) | Was verarbeitet wird, auf welcher Rechtsgrundlage, wie lange, wie es gesichert ist, und Ihre Rechte |
| [Widerrufsbelehrung](/legal/withdrawal) | Für Verbraucher: das vierzehntägige Widerrufsrecht, das Musterformular und wann es erlischt |
| [Impressum des Dienstes](/legal/imprint) | Anbieterkennzeichnung nach § 5 DDG für den Dienst |
| [Sicherheit](/legal/security) | Wie Sie eine Schwachstelle melden und was damit geschieht |
| [Unterauftragsverarbeiter](/legal/subprocessors) | Jeder Dritte, der Daten verarbeitet, und wie Änderungen angekündigt werden |

Alle sechs werden hier veröffentlicht. Die Fassungen in der App sind Kopien
davon.

## Diese Website und die Software

Der Rest dieser Seite. Für die Nutzung der quelloffenen Software braucht es
kein Konto, und keines der obigen Dokumente gilt für sie.

## Impressum gem. § 5 DDG

**Alexander Ertli**\
Jungfernstieg\
20354 Hamburg, Deutschland

E-Mail: <hello@contenox.com>\
Web: [ertli.com](https://ertli.com)

**Inhaltlich verantwortlich (§ 18 Abs. 2 MStV)**\
Alexander Ertli, Anschrift wie oben.

Umsatzsteuer-Identifikationsnummer (§ 27a UStG): `DE429161583`\
Steuernummer: `2247005603265`

Plattform der EU-Kommission zur Online-Streitbeilegung gemäß Art. 14 Abs. 1
ODR-VO:
[consumer-redress.ec.europa.eu](https://consumer-redress.ec.europa.eu/dispute-resolution-bodies).
Wir sind nicht bereit und nicht verpflichtet, an Streitbeilegungsverfahren vor
einer Verbraucherschlichtungsstelle teilzunehmen (§ 36 VSBG).

## Lizenz, Gewährleistung und Haftung

Contenox ist quelloffene Software unter der [Apache-Lizenz
2.0](https://www.apache.org/licenses/LICENSE-2.0). Diese Lizenz regelt Ihre
Nutzung der Software. Die folgenden Abschnitte beschreiben, was das praktisch
bedeutet, und ersetzen sie nicht.

### Was die Software leistet — und was zugesichert ist

Die Software verschafft Zugriff auf **das KI-Modell, das Sie auswählen**, unter
**der Konfiguration, die Sie geschrieben haben**. Zugesichert ist genau dies
und nicht mehr:

- dass er konfigurierbar ist;
- dass er ausschließlich das von Ihnen benannte KI-Modell anspricht, so wie
  Ihre Regeln es vorgeben;
- dass er die Verarbeitungs- und Prüfschritte ausführt, die Sie über die
  Ablaufsteuerung deklariert, überprüft und durch deren Verwendung freigegeben
  haben.

Die Bedienoberflächen — im Terminal wie im Browser — bauen auf dieser
Ablaufsteuerung auf. Sie sind
Beispielimplementierungen des contenox-Governance-Systems — also des Regelformats, der
Freigaben durch einen Menschen und der festgelegten Auslöser — und keine geprüften
Endprodukte für einen bestimmten Einsatzzweck.

Die Software trifft keine inhaltlichen Entscheidungen. Die Auswahl des
KI-Modells, Eingaben, Regeln, Freigaben und die Frage, was ein KI-Agent auf Ihrer
Maschine zugreifen darf, sind Ihre Festlegungen. Die Richtigkeit der Ausgaben
des KI-Modells ist weder prüfbar noch zugesichert.

### Was von uns stammt und was nicht

Contenox ist eine Zusammenstellung. Genau zu sagen, welcher Teil unsere Arbeit
ist, entscheidet darüber, wofür wir billigerweise einstehen können.

**Unsere eigene Arbeit** — das contenox-Governance-System und was es trägt:

- das Regelformat, in dem Sie Regeln, Budgets und Grenzen aufschreiben;
- die Festlegung der Freigaben: welche Handlungen ohne einen Menschen nicht
  weiterlaufen;
- das System der festgelegten Auslöser;
- die Ablaufsteuerung sowie die Bedienoberflächen, die auf ihr aufsetzen;
- das Verfahren, mit dem sich eine Maschine an das Relay koppelt, und dessen
  fest hinterlegter Ausweis;
- `modeld`, die Zwischenschicht zu Programmen, die KI-Modelle auf Ihrem eigenen
  Rechner ausführen.

**Nicht von uns** — genutzt unter eigenen Lizenzen und Bedingungen, zugesichert
von deren Urhebern und nicht von uns:

- die KI-Modelle selbst und die Dienste, die sie bereitstellen — Ollama, vLLM,
  OpenAI, Anthropic, Google Vertex, AWS Bedrock, Mistral, OpenRouter und was
  Sie sonst konfigurieren;
- die Programme, die diese Modelle auf Ihrem Rechner ausführen, etwa llama.cpp
  und OpenVINO;
- die offenen Schnittstellen-Standards, an die contenox sich hält — das Model
  Context Protocol (MCP) und das Agent Client Protocol (ACP) — sowie die
  Editoren und Werkzeuge, die sie ebenfalls verwenden;
- die Open-Source-Bibliotheken, von denen der Build abhängt, aufgeführt in
  `go.mod`.

Wir sichern unseren eigenen Teil wie oben beschrieben zu und nichts darüber
hinaus. Für alles aus der zweiten Liste besteht Ihr Verhältnis zu dessen
Urheber oder Anbieter, zu dessen Bedingungen.

### Gewährleistung

Die Software wird „wie besehen" bereitgestellt, ohne Gewährleistung jeglicher
Art — ausdrücklich oder stillschweigend, einschließlich der Gewährleistung der
Marktgängigkeit, der Eignung für einen bestimmten Zweck oder der
Nichtverletzung von Rechten Dritter.

### Haftung

Die Haftung ist ausgeschlossen, **soweit dies gesetzlich zulässig ist**. Nicht
ausgeschlossen — und nach deutschem Recht nicht ausschließbar — ist die Haftung
für Vorsatz und grobe Fahrlässigkeit, für Schäden aus der Verletzung des
Lebens, des Körpers oder der Gesundheit, nach dem Produkthaftungsgesetz, nach
Art. 82 DSGVO und sonstigem zwingenden Datenschutzrecht sowie im Umfang einer
ausdrücklich übernommenen Garantie.

### Ihre Rolle und Ihre Pflichten — je nachdem, wo Sie ansässig sind

**Contenox ist nicht regional beschränkt.** Sie können es überall betreiben,
und welchen Vorschriften Sie unterliegen, hängt davon ab, wo Sie ansässig sind,
wo Ihre Nutzer ansässig sind und wen Ihr Einsatz betrifft. Nachfolgend stellen wir die KI-Verordnung
der EU dar, weil wir hier ansässig sind und sie das nächstliegende Beispiel
ist. Außerhalb der EU gelten andere
Regelwerke: KI-Gesetze einzelner US-Bundesstaaten, sektorspezifische Aufsicht,
Berufsrecht sowie Datenschutz- und Verbraucherrecht Ihres Landes. Welche davon
für Sie gelten, müssen Sie selbst feststellen.

Die Verordnung (EU) 2024/1689 knüpft Pflichten an **Rollen**, nicht an
Software. Wer ein KI-System unter eigener Verantwortung einsetzt, ist dessen
**Betreiber**; wer es unter eigenem Namen bereitstellt, für einen eigenen Zweck
zusammenstellt oder wesentlich verändert, wird dessen **Anbieter**.

Wenn Sie contenox betreiben, treffen **Sie** die Entscheidungen, die diese
Rolle begründen: Sie wählen das KI-Modell, Sie schreiben die Konfiguration, Sie
legen die Auslöser fest, und Sie geben all das frei, indem Sie es in Betrieb
nehmen. Es gibt keinen Schritt, den wir für Sie genehmigen. Damit bestimmen Sie
Zweck und Mittel — und wenn Sie das Ergebnis in einem regulierten Bereich
einsetzen oder Dritten bereitstellen, treffen die Pflichten Sie:
Risikomanagement, Dokumentation, Protokollierung, menschliche Aufsicht,
Transparenz und gegebenenfalls Konformitätsbewertung. Die Pflicht zur
KI-Kompetenz nach Art. 4 KI-VO gilt bereits heute für Anbieter und Betreiber
gleichermaßen.

Die Nutzung von contenox erfüllt nichts davon. Sie liefert Kontrollen, auf die
eine eigene Bewertung zeigen kann: von Ihnen verfasste Regeln, dauerhaft
festgehaltene Freigaben, erfasster Ausführungszustand. Die Bewertung bleibt
Ihre.

### Bekannte Grenzen und Risiken

Die Ausgaben des KI-Modells
können falsch sein und werden nicht geprüft; automatisierte Schritte können
irreversibel wirken, sobald Sie sie erlauben — dafür gibt es Verbotsregeln und Freigaben
durch einen Menschen; eingelieferte Inhalte können Anweisungen enthalten
(eingeschleuste Anweisungen); und das System setzt die Regeln durch, die Sie geschrieben
haben, nicht die, die Sie gemeint haben.

Contenox ist nicht für Bereiche gebaut, in denen ein Fehler zu Personenschäden
führt oder automatisierte Verarbeitung unmittelbar über Menschen entscheidet —
Medizin, Personalauswahl, Kreditvergabe, Strafverfolgung, kritische
Infrastruktur, behördliche Entscheidungen. Ein Einsatz dort ist Ihre eigene
Bewertung.

Dies ist keine Rechtsberatung.

## Daten und Datenschutz

**Die Software läuft auf Ihrer Maschine.** Contenox speichert seinen Zustand
(Sitzungen, Abläufe, Konfiguration) lokal. Eingaben und Dateien, die Sie in eine
Anfrage einbeziehen, gehen ausschließlich an den von Ihnen konfigurierten
Anbieter des KI-Modells — kein Server von uns verarbeitet Ihre Arbeitslast. Die
Nutzung der Software erfordert weder Konto noch Registrierung.

**Das gehostete Relay ist ein davon getrennter, optionaler Dienst.** Wenn Sie
ein Konto unter [app.contenox.com](https://app.contenox.com) anlegen, können
Sie Ihre eigenen Maschinen aus dem Browser erreichen. Dieser Dienst hält Daten
über Sie — ein Konto, welche Maschinen gekoppelt sind und, bei einem
Abonnement, eine Abrechnungsreferenz. Sitzungsinhalte speichert er nicht: keine
Eingaben, keine Antworten, keine Dateien. Was er hält, wie lange, und wie Sie es
exportieren oder löschen, steht in seinen eigenen Dokumenten:
[Datenschutzerklärung](/legal/privacy), [Nutzungsbedingungen](/legal/terms)
und, für Verbraucher, [Widerrufsbelehrung](/legal/withdrawal) sowie das
[Impressum des Dienstes](/legal/imprint). Wie der Dienst abgesichert ist und
wie Sie eine Schwachstelle melden, steht auf der
[Sicherheitsseite](/legal/security); die eingesetzten Dienstleister listet
[Unterauftragsverarbeiter](/legal/subprocessors). Nichts auf dieser Seite gilt für
jenen Dienst, und nichts dort wird für die Nutzung der quelloffenen Software
benötigt.

**Diese Website ist statisch.** contenox.com setzt keine Cookies, führt keine
Analyse durch und verlangt kein Konto. Ihre Farbschema-Einstellung wird lokal
im Browser gespeichert (`localStorage`) und nicht übertragen. Die Suche läuft
vollständig in Ihrem Browser gegen einen lokal geladenen Index.

**Anfragen an Dritte.** Die Startseite lädt Release- und Star-Badges von
`img.shields.io` sowie Schriften von Google Fonts; für diese Anfragen gelten
die Datenschutzbestimmungen der jeweiligen Anbieter. Weitere Ressourcen Dritter
sind nicht eingebunden.

**E-Mail.** Wenn Sie <hello@contenox.com> schreiben, verarbeiten wir die
übermittelten Angaben ausschließlich zur Beantwortung Ihrer Anfrage.

*Stand: 13. August 2026*
