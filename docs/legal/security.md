# Sicherheit / Security

**Stand / As of: 13. August 2026**

Maßgeblich veröffentlicht unter <https://contenox.com/legal/security>.
Canonically published at <https://contenox.com/legal/security>.

---

## Teil A — Deutsch

### Eine Schwachstelle melden

Schreiben Sie an **hello@contenox.com** mit „Security" im Betreff. Bitte
schildern Sie, was Sie gefunden haben und wie es reproduzierbar ist.

- Wir bestätigen den Eingang innerhalb von **3 Werktagen**.
- Wir melden uns mit einer ersten Einschätzung innerhalb von **10 Werktagen**.
- Wir nennen Ihnen, sobald absehbar, wann ein Fix geplant ist, und sagen es
  ebenso, wenn wir nichts tun werden und warum.

Wir zahlen keine Prämien für Meldungen. Wenn Sie es wünschen, nennen wir Sie
beim Beheben namentlich.

**Bitte nicht:** auf fremde Konten oder fremde Maschinen zugreifen, Daten
exfiltrieren, den Dienst lahmlegen, oder eine Schwachstelle veröffentlichen,
bevor sie behoben ist. Gegen Meldungen, die sich daran halten, unternehmen wir
nichts.

### Wie das System gebaut ist

Die konkreten technischen und organisatorischen Maßnahmen stehen in der
[Datenschutzerklärung, Abschnitt 8](/legal/privacy) — Verschlüsselung,
Passwort- und Token-Behandlung, Schlüsseltrennung, Missbrauchsschranken,
Speicherort und automatische Löschung. Sie sind dort und nicht hier
beschrieben, weil sie Teil unserer Pflicht nach Art. 32 DSGVO sind und mit den
Verarbeitungsangaben zusammen gelesen werden müssen.

Dazu kommen zwei Eigenschaften des Aufbaus:

- **Das Relay speichert keine Sitzungsinhalte.** Keine Eingaben, keine
  Antworten, keine Dateien. Ein Angriff auf das Relay kann nicht offenlegen,
  was nie dort lag.
- **Die Verbindung ist unabhängig von TLS authentifiziert.** Ihre Maschine
  prüft eine fest im Programm hinterlegte Ausweis des Relays. Ein
  kompromittierter TLS-Kanal reicht deshalb nicht aus, um sich als Relay
  auszugeben.

### Die Software auf Ihren Maschinen

Für den quelloffene Software gilt: Er läuft mit Ihren Rechten auf Ihrer
Maschine, und worauf ein KI-Agent dort zugreifen darf, entscheiden Ihre Regeln. Die
Mechanik dazu — bereinigte Umgebung, erlaubte Werkzeuge, Arbeitsverzeichnis-Grenzen,
Verbotsregeln, Freigabe durch einen Menschen — steht in den Guides, nicht in diesem Dokument.
Sicherheitsmeldungen zur Software gehen denselben Weg wie oben.

---

## Part B — English

### Reporting a vulnerability

Write to **hello@contenox.com** with "Security" in the subject. Please describe
what you found and how to reproduce it.

- We acknowledge receipt within **3 working days**.
- We come back with a first assessment within **10 working days**.
- We tell you when a fix is planned as soon as we know — and equally, if we are
  not going to act, we say so and why.

We do not pay rewards for reports. We will credit you by name on the fix if
you want that.

**Please do not:** touch other people's accounts or machines, exfiltrate data,
take the service down, or disclose a vulnerability before it is fixed. We will
not pursue reports that stay within those lines.

### How the system is built

The concrete technical and organisational measures are in the
[privacy policy, section 8](/legal/privacy) — encryption, password and token
handling, key separation, abuse floors, location and automatic deletion. They
live there rather than here because they are part of our Art. 32 GDPR duty and
have to be read together with what is processed.

Two properties of the design:

- **The relay stores no session content.** No inputs, no outputs, no files. An
  attack on the relay cannot disclose what was never held there.
- **The connection is authenticated independently of TLS.** Your machine checks
  the relay's identity, fixed into the program, so breaking TLS is not
  enough to impersonate the relay.

### The software on your machines

For the open-source software: it runs with your privileges on your machine, and
what an AI agent may touch there is decided by your rules. The mechanics —
a cleaned environment, permitted tools, working-directory
boundaries, forbidding rules, approval by a human — are in the guides rather than in this document. Security
reports about the software take the same route as above.
