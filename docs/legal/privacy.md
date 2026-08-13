# Datenschutzerklärung / Privacy Policy

**Version `privacy-2026-08-13-r2` — gültig ab 13. August 2026**

Maßgeblich veröffentlicht unter <https://contenox.com/legal/privacy>.
Canonically published at <https://contenox.com/legal/privacy>.

Die deutsche Fassung ist maßgeblich. The German version prevails; the English
text below is a mirror provided for convenience.

---

# Teil A — Deutsch (maßgeblich)

## 1. Verantwortlicher

Alexander Ertli, Jungfernstieg, 20354 Hamburg, Deutschland. E-Mail:
hello@contenox.com. (Angaben wie im Impressum.)

Ein Datenschutzbeauftragter ist nicht benannt.

## 2. Was der Dienst ist — und was er nicht speichert

Das contenox-Relay verbindet Ihren Browser mit Maschinen, die Sie selbst
betreiben. Die Maschine wählt sich beim Relay ein; das Relay leitet die
Sitzungsdaten zwischen Browser und Maschine weiter und **speichert keine
Sitzungsinhalte** — keine Eingaben, keine Antworten, keine Dateien. Die
Komponenten, die Sitzungsrahmen sehen, haben keinen Schreibzugriff auf die
Datenbank. Gespeichert wird die Kontrollebene: wer ein Konto hat, welche
Maschinen gekoppelt sind, was daran verwaltet wurde.

**Wir trainieren nicht mit Ihren Daten.** Weder Ihre Inhalte noch die Ausgaben
der KI-Modelle verwenden wir, um Modelle zu trainieren — nicht selbst und nicht
durch Dritte in unserem Auftrag. Sitzungsinhalte werden gar nicht erst
gespeichert.

**Für den Anbieter des KI-Modells, den Sie auswählen, gilt das nicht
automatisch.** Eingaben und Dateien gehen unmittelbar an diesen Anbieter. Ob er
sie zum Training verwendet, richtet sich allein nach seinen Bedingungen und
Ihren dortigen Einstellungen — nicht nach dieser Erklärung, und wir haben darauf
keinen Einfluss und keine Einsicht. Wenn Training ausgeschlossen sein muss,
prüfen Sie die Bedingungen Ihres Anbieters oder betreiben Sie das Modell
lokal.

## 3. Kategorien verarbeiteter Daten

**a. Konto und Anmeldung.** E-Mail-Adresse, Anzeigename (optional), Passwort
als nicht umkehrbare Prüfsumme (nie im Klartext), Zeitpunkt der letzten Anmeldung.

**b. Sitzungen (Anmeldung im Browser).** Genau ein Cookie
(`contenox_relay_session`). Es kann von Skripten im Browser nicht gelesen
werden, wird nur über verschlüsselte Verbindungen übertragen und nicht an
fremde Websites mitgeschickt. Auf dem Server liegt davon nur eine Prüfsumme,
nie der Wert selbst. Dazu die Kennung Ihres Browsers (gekürzt, damit Sie eine
Sitzung in Ihrer Sitzungsliste wiedererkennen) und Zeitstempel. Eine Sitzung
gilt 30 Tage.

**c. Mitgliedschaften und Rollen.** Wer zu welchem Konto gehört, mit welcher
Rolle; Änderungen daran im Ereignisprotokoll (nur Rollen, Akteurs-IDs und
Zeitpunkte — keine Adressen).

**d. Einladungen.** Die eingeladene E-Mail-Adresse, die Rolle, das Ablaufdatum
und eine Prüfsumme des Einmal-Schlüssels (nie der Schlüssel selbst). Das zugehörige Protokollereignis trägt
ausdrücklich **nicht** die Adresse, damit die unten genannte Löschfrist sie
wirklich entfernt.

**e. Gekoppelte Maschinen.** Pro Maschine: der **Hostname, wie die Maschine ihn
beim Koppeln vorschlägt** (das kann ein selbstgewählter, personenbeziehbarer
Name sein), ein öffentlicher Schlüssel, eine Prüfsumme des Zugangsschlüssels, Zeitstempel
(gekoppelt, zuletzt gesehen, widerrufen) und wer gekoppelt hat.

**f. Geschäftskonten.** Nur bei Registrierung als Unternehmen: Firmenname,
Rechnungsanschrift, optional USt-IdNr. Verbraucherkonten speichern hiervon
nichts.

**g. Zustimmungsnachweise.** Pro akzeptiertem Dokument (Nutzungsbedingungen,
Datenschutzerklärung, Widerrufsbelehrung): Dokumentname, Versionskennung,
Zeitpunkt, Konto und Nutzer. **Keine IP-Adresse.**

**h. Ereignisprotokoll (Kontrollebene).** Ein konteneigenes Protokoll:
gekoppelt/widerrufen/gesehen, Mitgliedschafts- und Einladungsvorgänge,
Kontoumbenennungen, Abonnementänderungen. Die Nutzlast-Regeln sind bewusst
datensparsam: keine E-Mail-Adressen, keine Zugangsschlüssel, keine Prüfsummen;
Abrechnungsereignisse tragen Stripe-Abo-, Checkout- und Charge-IDs, nie
Kartendaten und nie die Kunden-E-Mail.

**i. Extern eingelieferte Ereignisse (Ingest).** Hier ist die Ausnahme von der
Datensparsamkeit, und sie wird ausdrücklich benannt. Kontoinhaber können
Einlieferungsquellen konfigurieren, deren Ereignisse selbstkonfigurierte
Abläufe auf ihren eigenen Maschinen auslösen. Zwei Formen:

- **Signierte Einlieferung:** der **rohe Request-Body wird wörtlich
  gespeichert** (JSON, vom Absender signiert).
- **Browser-Formulare:** die **abgeschickten Formularfelder werden wörtlich
  gespeichert** — Text anonymer Absender auf der Website des Kontoinhabers
  (herkunftsgeprüft per Origin, mit Größen-, Feld- und Frequenz-Obergrenzen und
  einer Falle für automatisierte Einsendungen).

Was darin steht, bestimmt die Quelle, die der Kontoinhaber konfiguriert hat —
es **kann personenbezogene Daten Dritter enthalten**. Der Kontoinhaber
entscheidet über Zwecke und Mittel dieser Einlieferung.

**j. Transaktionale E-Mails.** Fünf Nachrichtentypen: Passwort-Zurücksetzen,
Team-Einladung, Willkommensnachricht, Hinweis auf fehlgeschlagene Zahlung sowie
eine interne Betreiber-Benachrichtigung bei Geschäftsregistrierungen. Die
Versand-Warteschlange ist das Ereignisprotokoll; **kein Eintrag trägt einen
Schlüssel, einen Link oder eine Adresse** — Links werden erst im Moment des
Versands erzeugt. Das Versandregister speichert nur Versuchszähler und
Zeitpunkte, keine Inhalte und keine Adressen.

**k. Abrechnung.** Gespeichert werden Abonnementzustand, -quelle und die
Stripe-Abonnement-Referenz. **Nicht gespeichert:** Kartendaten,
Stripe-Kunden-ID, Rechnungsdaten — die liegen bei Stripe (siehe Abschnitt 5).

**l. IP-Adressen.** In der Datenbank des Dienstes gibt es **keine** IP-Spalte.
IP-Adressen werden flüchtig im Arbeitsspeicher für Missbrauchs-Schranken
verwendet, um Missbrauch abzuwehren (Begrenzung der Anfragerate bei Anmeldung,
Zurücksetzen und Kopplung). Dauerhaft gespeichert werden sie nicht. Die Zugriffsprotokolle der vorgelagerten
Infrastruktur enthalten IP-Adresse, Pfad und User-Agent; sie dienen allein dem
Betrieb und der Missbrauchsabwehr, werden nicht mit Konten zusammengeführt und
nur für die betriebliche Rotationsdauer der Knoten vorgehalten.

## 4. Zwecke und Rechtsgrundlagen

- **Vertragserfüllung bzw. vorvertragliche Maßnahmen (Art. 6 Abs. 1 lit. b
  DSGVO):** Konto, Anmeldung, Sitzungen, Kopplung und Erreichbarkeit der
  Maschinen, Team-Verwaltung, Einlieferungs-Funktionen, transaktionale E-Mails,
  Abrechnung.
- **Berechtigtes Interesse (Art. 6 Abs. 1 lit. f DSGVO):** Schutz vor
  Missbrauch und Betriebssicherheit — Begrenzung der Anfragerate, Obergrenzen an unauthentifizierten
  Endpunkten, Protokollierung sicherheitsrelevanter Ereignisse.
- **Rechtliche Verpflichtung (Art. 6 Abs. 1 lit. c DSGVO):** steuer- und
  handelsrechtliche Aufbewahrung von Abrechnungsunterlagen.

Kein Profiling, keine automatisierte Einzelentscheidung (Art. 22 DSGVO), kein
Tracking, keine Analyse- oder Werbedienste, keine Weitergabe zu Werbezwecken.

## 5. Empfänger und Auftragsverarbeiter

- **Google Ireland Limited** (Gordon House, Barrow Street, Dublin 4, Irland) —
  Hosting. Das Projekt läuft in der Region **europe-west3 (Frankfurt am
  Main)**; Speicherung und Verarbeitung finden dort statt. Google Ireland
  gehört zu einem US-Konzern; auch bei Speicherung in der EU lässt sich ein
  Zugriffsverlangen nach US-Recht nicht grundsätzlich ausschließen. Wenn das
  für Sie nicht tragbar ist, nutzen Sie bitte nur die Software auf Ihren
  eigenen Maschinen.
- **E-Mail-Zustelldienstleister** — Zustellung der transaktionalen E-Mails, auf
  Grundlage eines Auftragsverarbeitungsvertrags.
- **Stripe Technology Company Limited**, One Wilton Park, Wilton Place, Dublin
  2, D02 FX04, Irland — Checkout, Abonnement- und Rechnungsverwaltung,
  Kundenportal, soweit Zahlungsfunktionen genutzt werden. Stripe erhält
  Zahlungs- und Kontaktdaten unmittelbar von Ihnen; der Dienst selbst speichert
  davon nur die Abonnement-Referenz. Eine Übermittlung innerhalb der
  Stripe-Unternehmensgruppe in Drittländer erfolgt auf Grundlage der
  Standardvertragsklauseln der EU-Kommission.

Keine weiteren Empfänger. Es gibt keine Analyse-, Werbe- oder
Social-Media-Einbindungen.

## 6. Drittlandübermittlung

Standardmäßig keine: Hosting und Speicherung erfolgen in der EU (europe-west3,
Frankfurt am Main). Ausnahme ist die Zahlungsabwicklung über Stripe im Rahmen
des vorstehenden Abschnitts.

## 7. Speicherdauer

Die Fristen sind im Code festgelegt und werden von einem stündlichen
Aufräumprozess durchgesetzt:

| Datum | Lebensdauer | Danach |
|---|---|---|
| Browser-Sitzung | 30 Tage gültig | abgelaufene Zeile nach weiteren 24 h gelöscht |
| Passwort-Reset | 30 Minuten gültig | nach weiterer 1 h gelöscht |
| Einladung | 7 Tage gültig | Zeile mit der E-Mail-Adresse 30 Tage nach Ablauf gelöscht |
| Kopplungsschlüssel | 10 Minuten gültig | nach weiterer 1 h gelöscht |
| Register der Zahlungsmeldungen von Stripe (nur Kennung und Art des Vorgangs) | 90 Tage | gelöscht |
| Überholte Heartbeat-Ereignisse | 30 Tage | gelöscht |
| Eingelieferte Ereignisse | mindestens 30 Tage nach dem jüngsten Ereignis der jeweils vollen Protokollseite | seitenweise gelöscht; einzelne Einträge können länger bestehen, bis ihre Seite voll und abgelaufen ist |
| E-Mail-Zustellversuche (nur Zähler) | 24 h | gelöscht |
| Konto-, Mitglieds-, Maschinen- und Protokolldaten | bis zur Löschung des Kontos bzw. der Person | mit der Löschung entfernt (Abschnitt 8) |
| Zustimmungsnachweise | Lebensdauer des Kontos | beim Löschen einer Person bleibt der Nachweis des Kontos ohne Personenbezug bestehen; mit dem Konto gelöscht |

## 8. Datensicherheit (Art. 32 DSGVO)

Angemessene Sicherheit ist eine gesetzliche Pflicht, keine Zusage, auf die Sie
verzichten könnten. Was konkret umgesetzt ist:

- **Übertragung.** Alles läuft über TLS. Die Verbindung zwischen Relay und
  Ihrer Maschine ist zusätzlich auf Anwendungsebene authentifiziert: die
  Maschine prüft eine fest im Programm hinterlegte Ausweis des Relays, so
  dass ein kompromittierter TLS-Kanal allein keinen Zugriff verschafft.
- **Passwörter** werden ausschließlich in einer nicht umkehrbaren Prüfsumme
  gespeichert, nie im Klartext. Aus dem gespeicherten Wert lässt sich das
  Passwort nicht zurückrechnen.
- **Sitzungen.** Das Anmelde-Cookie kann von Skripten im Browser nicht gelesen
  werden, wird nur über verschlüsselte Verbindungen übertragen und nicht an
  fremde Websites mitgeschickt. Auf dem Server liegt davon nur eine Prüfsumme,
  nie der Wert selbst.
- **Einmal-Schlüssel** für Kopplung, Einladung und Passwort-Zurücksetzen werden
  nur als Prüfsumme gespeichert. Der Klartext existiert nur in dem Moment, in dem er
  ausgegeben wird; Links entstehen erst beim Versand der E-Mail.
- **Schlüsseltrennung.** Der Schlüssel zum Hashen von Zugangsdaten ist ein
  anderer als die Identität des Relays; das System weist eine Konfiguration
  zurück, in der beide gleich wären.
- **Datensparsamkeit als Sicherheitsmaßnahme.** Es gibt keine IP-Spalte in der
  Datenbank. Das Ereignisprotokoll trägt keine Adressen, keine Tokens und keine
  Prüfsummen.
- **Trennung der Zuständigkeiten.** Die Komponenten, die Sitzungsdaten
  durchleiten, haben keinen Schreibzugriff auf die Datenbank.
- **Schutz vor Missbrauch.** Begrenzung der Anfragerate bei Anmeldung,
  Zurücksetzen und Kopplung; Obergrenzen an allen unauthentifizierten Endpunkten; Prüfung der Signatur
  von Zahlungsmeldungen gegen den Rohtext der Anfrage, bevor irgendetwas
  ausgewertet wird.
- **Zugangsdaten und Schlüssel des Betriebs** liegen ausschließlich in einem
  verschlüsselten Schlüsselspeicher der Serverumgebung, werden automatisiert
  eingespielt und stehen nirgends im Quelltext.
- **Speicherort und Löschung.** Verarbeitung und Speicherung in der EU
  (europe-west3, Frankfurt am Main) auf verschlüsselten Datenträgern der
  Cloud-Infrastruktur. Die Fristen aus Abschnitt 7 setzt ein stündlich
  laufender Prozess durch.

**Schwachstellen melden.** Sicherheitsmeldungen erreichen uns unter
hello@contenox.com; das Verfahren steht auf der
[Sicherheitsseite](https://contenox.com/legal/security).

**Nicht abdingbar.** Unsere Pflichten und Ihre Rechte aus der DSGVO —
insbesondere aus Art. 5 bis 7, 13, 32 und 82 — bestehen unabhängig von den
Nutzungsbedingungen. Sie lassen sich weder durch eine Klausel ausschließen noch
durch Ihre Zustimmung abbedingen, und wir versuchen das an keiner Stelle.

## 9. Ihre Rechte

- **Auskunft und Datenübertragbarkeit (Art. 15, 20 DSGVO):** eingebaut und in
  Selbstbedienung — ein Export liefert als ein JSON-Dokument alles, was der
  Dienst über Sie hält: Identität, Sitzungen, Anmeldeverfahren (ohne
  Geheimnisse), Konten, Maschinen, Zustimmungshistorie und das
  Ereignisprotokoll Ihrer Konten.
- **Löschung (Art. 17 DSGVO):** eingebaut und in Selbstbedienung. Konten, deren
  einziges aktives Mitglied Sie sind, werden mitsamt Maschinen, Einladungen,
  Nachweisen und Protokoll gelöscht, und laufende Maschinenverbindungen werden
  getrennt. Zwei Grenzen: sind weitere aktive Mitglieder vorhanden und Sie sind
  letzter Owner, wird die Löschung abgelehnt, bis die Inhaberschaft übertragen
  ist; mit laufendem Abonnement wird sie abgelehnt — bitte zuerst im
  Zahlungsportal kündigen.
- **Berichtigung (Art. 16 DSGVO):** eingebaut — E-Mail-Adresse und Anzeigename
  änderbar, Passwortwechsel, Sitzungsliste mit Einzel- und Sammel-Abmeldung.
- **Widerspruch (Art. 21) und Einschränkung (Art. 18):** per E-Mail an
  hello@contenox.com.
- **Beschwerde (Art. 77 DSGVO):** bei einer Datenschutz-Aufsichtsbehörde, für
  uns Der Hamburgische Beauftragte für Datenschutz und Informationsfreiheit.

**Information Dritter (Art. 14 DSGVO):** Die E-Mail-Adresse einer eingeladenen
Person verarbeiten wir auf Veranlassung des Kontoinhabers, um die Einladung
zuzustellen; sie wird mit der Einladung informiert, und die Adresse wird 30
Tage nach Ablauf der Einladung gelöscht.

**Wenn Sie außerhalb der EU ansässig sind:** Diese deutsche Fassung behandelt
die DSGVO. Für Kalifornien (CCPA/CPRA), das Vereinigte Königreich, Australien
und Indien steht in **Teil B, Abschnitt 9** unter „If you are outside the EU",
welche Rechte dort zusätzlich gelten und an wen Sie sich wenden können. Die in
diesem Abschnitt genannten Rechte bieten wir unabhängig vom Wohnsitz allen an.

## 10. Cookies und lokaler Speicher

Genau **ein** Cookie: `contenox_relay_session`, für Skripte im Browser nicht
lesbar, ausschließlich für die Anmeldung — unbedingt erforderlich (§ 25 Abs. 2 Nr. 2 TDDDG), daher ohne
Einwilligungsbanner. Kein Tracking-, Analyse- oder Dritt-Cookie. Die App merkt
sich die Farbschema-Einstellung lokal im Browser; dieser Wert verlässt den
Browser nicht. Die App lädt keine Ressourcen von Dritten; Schriften sind selbst
gehostet.

## 11. Stand und Änderungen

Version `privacy-2026-08-13-r2`. Die Kennung wird bei der Registrierung
mitgespeichert, sodass nachvollziehbar bleibt, welcher Text akzeptiert wurde.
Änderungen werden mit neuer Versionskennung veröffentlicht.

---

# Part B — English (mirror of Part A)

## 1. Controller

Alexander Ertli, Jungfernstieg, 20354 Hamburg, Germany. E-mail:
hello@contenox.com. (As in the Impressum.)

No data protection officer is appointed.

## 2. What the service is — and what it does not store

The contenox relay connects your browser to machines you operate yourself. The
machine dials out to the relay; the relay routes session frames between browser
and machine and **stores no session content** — no prompts, no outputs, no
files. The components that see frames have no write path to the database. What
is stored is the control plane: who has an account, which machines are paired,
and what was administered about them.

**We do not train on your data.** We use neither your content nor the AI
models' output to train models — not ourselves, and not through third parties
acting for us. Session content is not stored in the first place.

**That does not automatically extend to the AI model provider you choose.**
Inputs and files go directly to that provider. Whether they use them for
training is governed by their terms and your settings with them, not by this
policy — and we have neither influence over it nor visibility into it. If
training must be ruled out, check your provider's terms or run the model
locally.

## 3. Categories of data processed

**a. Account and sign-in.** E-mail address, display name (optional), password
as an irreversible checksum (never plaintext), time of last sign-in.

**b. Sessions (signing in through the browser).** Exactly one cookie
(`contenox_relay_session`). It cannot be read by scripts in the browser, is
sent only over encrypted connections, and is not sent to other websites; the
server holds only a checksum of it, never the value itself. Alongside it, an
identifier for your browser (shortened, so you can recognise a session in your
session list) and timestamps. A session lasts 30 days.

**c. Memberships and roles.** Who belongs to which account, in which role;
changes are recorded in the event log (roles, actor ids and timestamps only —
never addresses).

**d. Invitations.** The invited e-mail address, the role, the expiry, and a
checksum of the one-time key (never the key itself). The corresponding log event deliberately does
**not** carry the address, so the deletion window below genuinely removes it.

**e. Paired machines.** Per machine: the **hostname as the machine proposes it
at pairing** (which can be a self-chosen, person-identifying name), a public
key, a checksum of the access key, timestamps (paired, last seen, revoked), and who paired
it.

**f. Business accounts.** Only for business registrations: legal name, billing
address, optional VAT ID. Consumer accounts store none of this.

**g. Consent records.** Per accepted document (terms, privacy policy,
withdrawal notice): document name, version identifier, timestamp, account and
user. **No IP address.**

**h. Event log (control plane).** A per-account log: paired/revoked/seen,
membership and invitation actions, account renames, subscription changes.
Payload rules are deliberately minimal: no e-mail addresses, no access keys, no
checksums; billing events carry Stripe subscription, checkout and charge ids,
never card data and never the customer e-mail.

**i. Externally ingested events.** This is the stated exception to data
minimisation. Account holders can configure ingestion sources whose events
trigger self-configured chains on their own machines. Two forms:

- **Signed ingestion:** the **raw request body is stored verbatim** (JSON,
  signed by the sender).
- **Browser forms:** the **submitted form fields are stored verbatim** — text
  from anonymous submitters on the account holder's own website
  (origin-checked, with size, field-count and rate caps and a trap for
  automated submissions).

What these contain is determined by the source the account holder configured —
it **may include third parties' personal data**. The account holder decides the
purposes and means of that ingestion.

**j. Transactional mail.** Five message types: password reset, team invitation,
welcome message, payment-failure notice, and an internal operator notice for
business registrations. The send queue is the event log; **no entry carries a key, a
link or an address** — links are minted only at the moment of sending.
The delivery ledger stores attempt counts and timestamps only, no content and
no addresses.

**k. Billing.** Stored: subscription state, source, and the Stripe subscription
reference. **Not stored:** card data, Stripe customer id, invoices — those live
at Stripe (see section 5).

**l. IP addresses.** The service's database has **no** IP column. IP addresses
are used transiently in memory for abuse floors (rate limiting of sign-in,
reset and pairing; discarded after roughly 30 minutes of inactivity). The
upstream infrastructure's access logs contain IP address, path and User-Agent;
they serve operation and abuse defence only, are not joined to accounts, and
are kept only for the nodes' operational rotation window.

## 4. Purposes and legal bases

- **Contract performance / pre-contractual steps (Art. 6(1)(b) GDPR):**
  account, sign-in, sessions, pairing and machine reachability, team
  management, ingestion features, transactional mail, billing.
- **Legitimate interest (Art. 6(1)(f) GDPR):** abuse and operational security —
  rate limiting, caps on unauthenticated endpoints, logging of
  security-relevant events.
- **Legal obligation (Art. 6(1)(c) GDPR):** tax- and commercial-law retention
  of billing records.

No profiling, no automated individual decision-making (Art. 22 GDPR), no
tracking, no analytics or advertising services, no sharing for advertising.

## 5. Recipients and processors

- **Google Ireland Limited** (Gordon House, Barrow Street, Dublin 4, Ireland) —
  hosting. The project runs in region **europe-west3 (Frankfurt am Main)**;
  storage and processing happen there. Google Ireland belongs to a
  US-headquartered group, and EU storage does not in principle rule out an
  access request under US law. If that is not acceptable to you, use only the
  software on your own machines.
- **E-mail delivery provider** — delivery of transactional mail, under a data
  processing agreement.
- **Stripe Technology Company Limited**, One Wilton Park, Wilton Place, Dublin
  2, D02 FX04, Ireland — checkout, subscription and invoice management,
  customer portal, where payment features are used. Stripe receives payment and
  contact data directly from you; the service itself stores only the
  subscription reference. Transfers within the Stripe group to third countries
  take place on the basis of the European Commission's Standard Contractual
  Clauses.

No further recipients. There are no analytics, advertising or social-media
embeds.

## 6. Third-country transfers

None by default: hosting and storage are in the EU (europe-west3, Frankfurt am
Main). The exception is payment processing via Stripe, within the scope of the
preceding section.

## 7. Retention

The windows are set in code and enforced by an hourly housekeeping process:

| Data | Lifetime | Then |
|---|---|---|
| Browser session | valid 30 days | expired row deleted after a further 24 h |
| Password reset | valid 30 minutes | deleted after a further 1 h |
| Invitation | valid 7 days | row with the e-mail address deleted 30 days after expiry |
| Pairing key | valid 10 minutes | deleted after a further 1 h |
| Register of payment messages from Stripe (identifier and type of event only) | 90 days | deleted |
| Superseded heartbeat events | 30 days | deleted |
| Ingested events | at least 30 days past the newest event of each filled log page | dropped page-wise; individual entries can persist longer, until their page fills and ages out |
| Mail delivery attempts (counters only) | 24 h | deleted |
| Account, membership, machine and log data | until erasure of the account or person | removed with the erasure (section 8) |
| Consent records | lifetime of the account | when a person is erased, the account's record survives with the personal link removed; deleted with the account |

## 8. Data security (Art. 32 GDPR)

Appropriate security is an obligation, not an assurance we could have you waive.
What is actually in place:

- **Transport.** Everything runs over TLS. The connection between the relay and
  your machine is additionally authenticated at the application layer: the
  machine checks the relay's identity, fixed into the program, so a broken
  TLS channel alone grants no access.
- **Passwords** are stored only as an irreversible checksum, never in plaintext.
  The password cannot be worked back out of the stored value.
- **Sessions.** The sign-in cookie cannot be read by scripts in the browser, is
  sent only over encrypted connections, and is not sent to other websites. The
  server holds only a checksum of it, never the value itself.
- **One-time keys** for pairing, invitations and password resets are stored
  only as a checksum. The plaintext exists only at the moment it is issued; links are minted
  when the mail is sent.
- **Key separation.** The key used to hash credentials is not the relay's
  identity key; the system refuses a configuration in which the two are equal.
- **Data minimisation as a security measure.** There is no IP column in the
  database. The event log carries no addresses, no access keys and no
  checksums.
- **Separation of duties.** The components that route session data have no
  write path to the database.
- **Defences against abuse.** Rate limiting on sign-in, reset and pairing; caps on every
  unauthenticated endpoint; the signature on payment messages verified against the raw
  request body before anything is parsed.
- **Operational credentials and keys** live only in an encrypted key store on
  the server, are delivered automatically, and appear nowhere in the source
  code.
- **Location and deletion.** Processing and storage in the EU (europe-west3,
  Frankfurt am Main) on encrypted cloud infrastructure volumes. The retention
  windows in section 7 are enforced by an hourly process.

**Reporting a vulnerability.** Security reports reach us at
hello@contenox.com; the process is on the
[security page](https://contenox.com/legal/security).

**Not waivable.** Our obligations and your rights under the GDPR — in particular
Arts. 5 to 7, 13, 32 and 82 — exist independently of the terms of service. They
cannot be excluded by a clause or signed away by your consent, and we do not
attempt either.

## 9. Your rights

- **Access and portability (Art. 15, 20 GDPR):** built in and self-serve — an
  export returns, as one JSON document, everything the service holds about you:
  identity, sessions, sign-in mechanisms (without secrets), accounts, machines,
  consent history and your accounts' event logs.
- **Erasure (Art. 17 GDPR):** built in and self-serve. Accounts whose only
  active member you are are deleted with their machines, invitations, consent
  records and event log, and live machine connections are dropped. Two limits:
  if an account has other active members and you are its last owner, erasure is
  refused until ownership is transferred; with a live subscription it is
  refused — please cancel in the billing portal first.
- **Rectification (Art. 16 GDPR):** built in — e-mail address and display name
  can be changed, password change, a session list with single and bulk
  sign-out.
- **Objection (Art. 21) and restriction (Art. 18):** by e-mail to
  hello@contenox.com.
- **Complaint (Art. 77 GDPR):** with a supervisory authority; for us the
  Hamburg Commissioner for Data Protection and Freedom of Information.

**Information of third parties (Art. 14 GDPR):** an invited person's e-mail
address is processed at the account holder's initiative to deliver the
invitation; they are informed by the invitation itself, and the address is
deleted 30 days after the invitation expires.

### If you are outside the EU

The service is operated from Germany and your data is stored in the EU
(Frankfurt am Main). If you are elsewhere, that means your data is transferred
**to** the EU and held under GDPR — which in most cases gives you more than
your local law requires, not less. The rights in this section are offered to
everyone, wherever they live, and the routes below are in addition to them.

**California.** Under the CCPA/CPRA you may ask what we hold, ask us to delete
or correct it, and are protected against being treated worse for asking. Both
requests are self-serve, in your account, and need no e-mail. Two points that
matter more than the rest:

- **We do not sell your personal information, and we do not share it for
  cross-context behavioural advertising.** Not as a policy we could change
  quietly — there is no advertising, no analytics and no tracking anywhere in
  the product, so there is nothing to sell and no one to share with.
- **We do not use your data to train AI models.** See section 2, including what
  that does and does not say about the model provider you chose.

We do not knowingly process the personal information of anyone under 16.

**United Kingdom.** Your rights are the same as those in this section, under
the UK GDPR. Complaints go to the Information Commissioner's Office (ICO).

**Australia.** We handle personal information in line with this policy and, so
far as it applies to us, the Australian Privacy Principles. You may ask for
access and correction using the same self-serve routes. Complaints can go to
the Office of the Australian Information Commissioner (OAIC). Where a data
breach is likely to cause serious harm, we will notify affected people and the
relevant authority without undue delay.

**India.** Under the Digital Personal Data Protection Act 2023 you may access,
correct and erase your data, and nominate someone to act for you. For
grievances, write to hello@contenox.com; that address is also the contact for
answering questions about this policy.

**Anywhere else.** Write to hello@contenox.com.

## 10. Cookies and local storage

Exactly **one** cookie: `contenox_relay_session`, unreadable to scripts in the
browser, solely for signing in — strictly necessary (§ 25(2) no. 2 TDDDG), hence no consent banner. No
tracking, analytics or third-party cookies. The app remembers the colour-scheme
preference locally in the browser; that value never leaves the browser. The app
loads no third-party resources; fonts are self-hosted.

## 11. Version and changes

Version `privacy-2026-08-13-r2`. The identifier is stored with each registration,
so it remains traceable which text was accepted. Changes are published under a
new version identifier.
