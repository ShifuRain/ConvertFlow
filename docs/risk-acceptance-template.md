# Risk acceptance template: no authentication / Risikoakzeptanz: fehlende Authentisierung

Template for the operator's information security officer (ISB). It is **not pre-filled and not signed**. Every `[ORGANISATION: ...]` placeholder must be completed or deleted by the operating organization; the product facts in the text were taken from the code and compose file of this repository and must be re-checked against the version you deploy. This template is not legal advice and does not make the deployment "compliant" with anything. Related: [security.md](security.md), [grundschutz-mapping.md](grundschutz-mapping.md), [operations.md](operations.md).

Vorlage für die oder den Informationssicherheitsbeauftragte(n) (ISB) der betreibenden Organisation. Sie ist **nicht vorausgefüllt und nicht unterschrieben**. Alle Platzhalter `[ORGANISATION: ...]` müssen von der Organisation ausgefüllt oder gestrichen werden; die Produktangaben wurden aus Code und Compose-Datei dieses Repositories übernommen und sind gegen die eingesetzte Version zu prüfen. Diese Vorlage ist keine Rechtsberatung.

- [English](#english)
- [Deutsch](#deutsch)

---

<a id="english"></a>
# English

## 1. Subject

| Item | Value |
|---|---|
| Product | ConvertFlow (self-hosted FFmpeg transcoding web app and REST API) |
| Version / image digests | [ORGANISATION: version and `IMAGES.txt` digests of the deployed release] |
| FFmpeg flavor | [ORGANISATION: `lgpl` (default) or `full`] |
| Host(s) | [ORGANISATION: host names / inventory IDs] |
| Network segment | [ORGANISATION: segment / VLAN / zone] |
| Information classification of processed media | [ORGANISATION: protection requirement (Schutzbedarf) for confidentiality, integrity, availability] |
| Responsible business owner (Fachverantwortliche) | [ORGANISATION: name / role] |
| Operator (IT-Betrieb) | [ORGANISATION: name / role] |

## 2. Deviation

ConvertFlow has **no identity and access management**. There are no users, no roles, no sessions, no tokens and no API keys. Anyone with network access to the service port can upload media, list all jobs, download any result within the retention period, and cancel or delete any job. This is an explicit decision recorded in the design specification, not an oversight. It deviates from the following requirements of the IT-Grundschutz-Kompendium (Edition 2023); see [grundschutz-mapping.md](grundschutz-mapping.md) for the full list:

- ORP.4 Identitäts- und Berechtigungsmanagement (in particular ORP.4.A1, A2, A6, A7, A9)
- CON.10.A1 Authentisierung bei Webanwendungen, CON.10.A2 Zugriffskontrolle bei Webanwendungen, CON.10.A3 Sicheres Session-Management
- APP.3.1.A1 Authentisierung, APP.3.2.A5 Authentisierung

## 3. Threat and impact analysis

### 3.1 What a client can do (exposed capabilities)

| Capability | Endpoint | Note |
|---|---|---|
| Submit media for conversion | `POST /api/v1/jobs` | consumes CPU, memory and disk; bounded by the limits in section 4 |
| List all jobs (latest 200) with ids, filenames, status | `GET /api/v1/jobs` | other users' filenames are visible |
| Read status/metadata of a job | `GET /api/v1/jobs/{id}` | |
| Download any finished result | `GET /api/v1/jobs/{id}/output` | within the retention period (default 24 h) |
| Cancel or delete any job | `DELETE /api/v1/jobs/{id}` | |
| Read metrics | `GET /metrics` | counts only, no filenames |

### 3.2 What a client cannot do

No shell access; no arbitrary FFmpeg arguments (only a preset id and four validated numeric options); no access to the filesystem outside the data volume; no access to the database; no network access from the worker (internal network, no egress); no persistence beyond the retention period (except the database dump if backed up).

### 3.3 Threat scenarios

| # | Scenario | Confidentiality | Integrity | Availability |
|---|---|---|---|---|
| T1 | A user of the segment reads another user's converted file or filename | affected: other users' files and filenames visible within retention (default 24 h) | | |
| T2 | A user or a malicious web page in a user's browser cancels or deletes other users' jobs, or submits uploads | | affected: loss of in-progress or finished results | affected: forced re-work |
| T3 | Queue flooding / resource exhaustion by uploading many large files | | | affected: CPU exhaustion, disk fill, queue full (`503`/`507`) up to the configured caps |
| T4 | Hostile media file exploits an FFmpeg/codec flaw in the worker | affected: files of the media volume and the database credentials are reachable by code running in the worker | affected | affected |
| T5 | Eavesdropping on the unencrypted HTTP connection | affected if TLS is not used | possible manipulation | |
| T6 | Lack of accountability: actions cannot be attributed to persons | | | audit shows the TCP peer address only (the proxy address behind a reverse proxy) |
| T7 | Data remnants after deletion (plain unlink) | affected if the volume is not encrypted | | |

[ORGANISATION: add scenarios specific to your environment and rate likelihood and impact per your risk methodology (e.g. BSI-Standard 200-3).]

### 3.4 Impact assessment

[ORGANISATION: for each scenario, damage category and rating, based on the Schutzbedarf of the media to be processed. Recommendation: do not process media above the protection requirement `[ORGANISATION: normal/high/very high]` with this instance.]

## 4. Compensating controls

Controls **implemented in the product** (with configuration references; defaults shown):

| Control | Reference |
|---|---|
| Loopback-only publication by default (`127.0.0.1:8080`); exposure is a deliberate edit of the `ports:` line in `compose.yaml` | `compose.yaml`; verified by `tests/container/hardening.sh` ("published port bound to 127.0.0.1 only") |
| Internal-only network for worker and database (no egress); worker FFmpeg built with `--disable-network` and run with `-protocol_whitelist file` | `compose.yaml` (`backend: internal: true`), `Dockerfile.worker`, `internal/ffmpeg/args.go` |
| Hardened containers: non-root, read-only root filesystem, all capabilities dropped, `no-new-privileges`, seccomp, pids/memory/CPU limits, secrets as files | `compose.yaml`, `tests/container/hardening.sh` |
| Upload and resource limits: `CONVERTFLOW_MAX_UPLOAD_BYTES` (8 GiB), `CONVERTFLOW_MAX_CONCURRENT_UPLOADS` (4), `CONVERTFLOW_MAX_QUEUED_JOBS` (100), `CONVERTFLOW_MIN_FREE_BYTES`, `CONVERTFLOW_MAX_SSE_STREAMS` (64) | [operations.md](operations.md#configuration-reference) |
| Media limits: `CONVERTFLOW_MAX_DURATION_SEC`, `CONVERTFLOW_MAX_PIXELS`, `CONVERTFLOW_MAX_STREAMS`, `CONVERTFLOW_MAX_OUTPUT_BYTES`, `CONVERTFLOW_JOB_TIMEOUT` | same |
| Retention: finished jobs and files deleted after `CONVERTFLOW_RETENTION` (24 h); explicit delete; orphan sweep | [operations.md](operations.md#deletion-storage-and-encryption) |
| Audit log: JSON events (job created/started/finished/failed/deleted/cancel requested, upload rejected) with the peer IP; file contents are never logged | [operations.md](operations.md#logging-and-audit-events) |
| Input validation, fixed option allow-list, security headers and CSP, TLS 1.2+ available | [security.md](security.md) |

Controls the **operator must provide** (the product cannot):

| Required control | Value / decision |
|---|---|
| Network isolation: the service is reachable only from `[ORGANISATION: allowed source networks / hosts]` via host firewall rules and/or network segmentation | [ORGANISATION: rule IDs / references] |
| TLS: terminated by the application or a reverse proxy; certificate management | [ORGANISATION: option chosen, certificate process] |
| Reverse proxy or network device that provides access control, if users beyond the isolated segment must be served (e.g. client certificates, SSO at the proxy) | [ORGANISATION: yes/no, details] |
| Central log collection and retention for container logs and proxy access logs (client attribution is only possible via the proxy log) | [ORGANISATION: log system, retention] |
| Time synchronization of the host | [ORGANISATION: NTP source] |
| Encrypted storage for the `media` and `pgdata` volumes (the product uses plain unlink) | [ORGANISATION: LUKS/other, key management] |
| Database backup and restore tests; confidential handling of dumps | [ORGANISATION: schedule, location] |
| Patch and change management for host, container runtime and ConvertFlow images (rebuild on dependency updates) | [ORGANISATION: process reference] |
| Restriction of who may reach the host and the container runtime (administration) | [ORGANISATION: reference] |
| Rules on which media may be processed (protection requirement, copyright, patent licensing for the `full` flavor) | [ORGANISATION: policy reference] |

## 5. Residual risk assessment

With the controls above the remaining risk is that **every party that can reach the service port can read, delete and cancel every job and can consume the capacity of the service**, and that a flaw in media parsing can expose the contents of the media volume (in-retention files) and the database credentials.

| Risk | Residual rating |
|---|---|
| T1 Disclosure of other users' media | [ORGANISATION: rating and justification] |
| T2 Deletion / cancellation / unwanted uploads | [ORGANISATION: ...] |
| T3 Resource exhaustion | [ORGANISATION: ...] |
| T4 Exploitation of a media-parsing flaw | [ORGANISATION: ...] |
| T5 Eavesdropping | [ORGANISATION: ...] |
| T6 Lack of accountability | [ORGANISATION: ...] |
| T7 Data remnants | [ORGANISATION: ...] |

Overall residual risk: [ORGANISATION: low / medium / high] - [ORGANISATION: justification]

## 6. Decision

[ORGANISATION: accepted / accepted with conditions / rejected. Conditions, if any.]

| Item | Entry |
|---|---|
| Risk owner | [ORGANISATION: name, role] |
| Information security officer (ISB) | [ORGANISATION: name] |
| Data protection officer (if personal data may be processed) | [ORGANISATION: name] |
| Date of decision | [ORGANISATION: date] |
| Valid until / next review | [ORGANISATION: date; suggested: at most yearly, and on every trigger in section 7] |

| Role | Name | Date | Signature |
|---|---|---|---|
| Risk owner | | | |
| ISB | | | |
| Operator (IT-Betrieb) | | | |

## 7. Triggers for re-assessment

Reassess the acceptance when any of the following occurs:

- the service is made reachable beyond the isolated segment named in section 1, or the firewall rules change;
- media with a higher protection requirement than assessed are to be processed;
- users with different clearance or different organizational units start using the same instance;
- TLS or the reverse proxy configuration changes, or the instance is placed behind a different proxy;
- the FFmpeg flavor changes to `full`, hardware acceleration or device passthrough is added, or the container hardening is relaxed;
- a vulnerability in FFmpeg, a codec library or ConvertFlow with relevant impact becomes known;
- a security incident involving the service occurs;
- the review date in section 6 is reached.

---

<a id="deutsch"></a>
# Deutsch

## 1. Gegenstand

| Punkt | Wert |
|---|---|
| Produkt | ConvertFlow (selbst betriebene FFmpeg-Transcodier-Webanwendung mit REST-API) |
| Version / Image-Digests | [ORGANISATION: Version und Digests aus `IMAGES.txt` der eingesetzten Version] |
| FFmpeg-Variante | [ORGANISATION: `lgpl` (Standard) oder `full`] |
| Host(s) | [ORGANISATION: Hostnamen / Inventar-IDs] |
| Netzsegment | [ORGANISATION: Segment / VLAN / Zone] |
| Einstufung der verarbeiteten Medien | [ORGANISATION: Schutzbedarf Vertraulichkeit, Integrität, Verfügbarkeit] |
| Fachverantwortliche(r) | [ORGANISATION: Name / Rolle] |
| IT-Betrieb | [ORGANISATION: Name / Rolle] |

## 2. Abweichung

ConvertFlow hat **kein Identitäts- und Berechtigungsmanagement**. Es gibt keine Benutzenden, Rollen, Sitzungen, Token oder API-Schlüssel. Wer Netzzugriff auf den Dienst-Port hat, kann Medien hochladen, alle Aufträge auflisten, jedes fertige Ergebnis innerhalb der Aufbewahrungsfrist herunterladen sowie jeden Auftrag abbrechen oder löschen. Dies ist eine ausdrückliche, in der Entwurfsspezifikation dokumentierte Entscheidung. Sie weicht von folgenden Anforderungen des IT-Grundschutz-Kompendiums (Edition 2023) ab; die vollständige Liste steht in [grundschutz-mapping.md](grundschutz-mapping.md):

- ORP.4 Identitäts- und Berechtigungsmanagement (insbesondere ORP.4.A1, A2, A6, A7, A9)
- CON.10.A1 Authentisierung bei Webanwendungen, CON.10.A2 Zugriffskontrolle bei Webanwendungen, CON.10.A3 Sicheres Session-Management
- APP.3.1.A1 Authentisierung, APP.3.2.A5 Authentisierung

## 3. Bedrohungs- und Auswirkungsanalyse

### 3.1 Was ein Client tun kann

| Fähigkeit | Endpunkt | Hinweis |
|---|---|---|
| Medien zur Konvertierung einreichen | `POST /api/v1/jobs` | verbraucht CPU, Speicher, Platte; begrenzt durch die Limits in Abschnitt 4 |
| Alle Aufträge auflisten (letzte 200) mit IDs, Dateinamen, Status | `GET /api/v1/jobs` | Dateinamen anderer sind sichtbar |
| Status/Metadaten eines Auftrags lesen | `GET /api/v1/jobs/{id}` | |
| Beliebige fertige Ergebnisse herunterladen | `GET /api/v1/jobs/{id}/output` | innerhalb der Aufbewahrungsfrist (Standard 24 h) |
| Beliebige Aufträge abbrechen oder löschen | `DELETE /api/v1/jobs/{id}` | |
| Metriken lesen | `GET /metrics` | nur Zähler, keine Dateinamen |

### 3.2 Was ein Client nicht tun kann

Kein Shell-Zugriff; keine beliebigen FFmpeg-Argumente (nur eine Preset-ID und vier validierte Zahlenoptionen); kein Zugriff auf das Dateisystem außerhalb des Datenvolumens; kein Datenbankzugriff; kein Netzzugriff des Workers (internes Netz ohne Egress); keine Persistenz über die Aufbewahrungsfrist hinaus (ausser in einem Datenbank-Dump, falls gesichert).

### 3.3 Bedrohungsszenarien

| Nr. | Szenario | Vertraulichkeit | Integrität | Verfügbarkeit |
|---|---|---|---|---|
| T1 | Nutzende des Segments lesen Dateien oder Dateinamen anderer | betroffen: fremde Dateien und Dateinamen innerhalb der Aufbewahrung (Standard 24 h) sichtbar | | |
| T2 | Nutzende oder eine schädliche Webseite im Browser brechen fremde Aufträge ab/löschen sie oder reichen Uploads ein | | betroffen: Verlust laufender oder fertiger Ergebnisse | betroffen: Mehraufwand |
| T3 | Überlastung durch viele große Uploads | | | betroffen: CPU-Auslastung, Platte voll, Warteschlange voll (`503`/`507`) bis zu den konfigurierten Grenzen |
| T4 | Schadhafte Mediendatei nutzt eine Schwachstelle in FFmpeg/Codec im Worker | betroffen: Dateien des Medienvolumens und Datenbank-Zugangsdaten für Code im Worker erreichbar | betroffen | betroffen |
| T5 | Mithören der unverschlüsselten HTTP-Verbindung | betroffen ohne TLS | Manipulation möglich | |
| T6 | Fehlende Zurechenbarkeit von Aktionen | | | Audit-Log enthält nur die TCP-Peer-Adresse (hinter einem Reverse Proxy: Adresse des Proxys) |
| T7 | Datenreste nach dem Löschen (einfaches Unlink) | betroffen, wenn das Volume nicht verschlüsselt ist | | |

[ORGANISATION: umgebungsspezifische Szenarien ergänzen; Eintrittshäufigkeit und Auswirkung nach der eigenen Risikomethodik bewerten (z. B. BSI-Standard 200-3).]

### 3.4 Auswirkungsbewertung

[ORGANISATION: je Szenario Schadenskategorie und Bewertung auf Basis des Schutzbedarfs der zu verarbeitenden Medien. Empfehlung: Medien mit höherem Schutzbedarf als `[ORGANISATION: normal/hoch/sehr hoch]` nicht mit dieser Instanz verarbeiten.]

## 4. Kompensierende Maßnahmen

**Im Produkt umgesetzt** (mit Konfigurationsbezug; Standardwerte):

| Maßnahme | Referenz |
|---|---|
| Standardmäßig nur Loopback (`127.0.0.1:8080`); Freigabe nach außen nur durch bewusste Änderung der Zeile `ports:` in `compose.yaml` | `compose.yaml`; geprüft durch `tests/container/hardening.sh` |
| Internes Netz für Worker und Datenbank (kein Egress); FFmpeg mit `--disable-network` gebaut und mit `-protocol_whitelist file` gestartet | `compose.yaml`, `Dockerfile.worker`, `internal/ffmpeg/args.go` |
| Gehärtete Container: Nicht-Root, Root-Dateisystem schreibgeschützt, alle Capabilities entfernt, `no-new-privileges`, seccomp, pids-/Speicher-/CPU-Limits, Secrets als Dateien | `compose.yaml`, `tests/container/hardening.sh` |
| Upload- und Ressourcenlimits: `CONVERTFLOW_MAX_UPLOAD_BYTES` (8 GiB), `CONVERTFLOW_MAX_CONCURRENT_UPLOADS` (4), `CONVERTFLOW_MAX_QUEUED_JOBS` (100), `CONVERTFLOW_MIN_FREE_BYTES`, `CONVERTFLOW_MAX_SSE_STREAMS` (64) | [operations.md](operations.md#configuration-reference) |
| Medienlimits: `CONVERTFLOW_MAX_DURATION_SEC`, `CONVERTFLOW_MAX_PIXELS`, `CONVERTFLOW_MAX_STREAMS`, `CONVERTFLOW_MAX_OUTPUT_BYTES`, `CONVERTFLOW_JOB_TIMEOUT` | ebenda |
| Aufbewahrung: Aufträge und Dateien werden nach `CONVERTFLOW_RETENTION` (24 h) gelöscht; explizites Löschen; Waisendatei-Bereinigung | [operations.md](operations.md#deletion-storage-and-encryption) |
| Audit-Log: JSON-Ereignisse (Auftrag angelegt/gestartet/beendet/fehlgeschlagen/gelöscht/Abbruch angefordert, Upload abgelehnt) mit Peer-IP; Dateiinhalte werden nie protokolliert | [operations.md](operations.md#logging-and-audit-events) |
| Eingabevalidierung, feste Optionen-Allowlist, Sicherheits-Header und CSP, TLS 1.2+ verfügbar | [security.md](security.md) |

**Vom Betrieb zu erbringen** (das Produkt kann das nicht leisten):

| Erforderliche Maßnahme | Wert / Entscheidung |
|---|---|
| Netzisolation: Dienst nur erreichbar von `[ORGANISATION: erlaubte Quellnetze / Hosts]` per Host-Firewall und/oder Segmentierung | [ORGANISATION: Regel-IDs / Verweise] |
| TLS: Terminierung in der Anwendung oder im Reverse Proxy; Zertifikatsverwaltung | [ORGANISATION: gewählte Option, Zertifikatsprozess] |
| Reverse Proxy oder Netzkomponente mit Zugriffskontrolle, falls Nutzende außerhalb des isolierten Segments bedient werden (z. B. Client-Zertifikate, SSO am Proxy) | [ORGANISATION: ja/nein, Details] |
| Zentrale Protokollsammlung und -aufbewahrung für Container- und Proxy-Zugriffsprotokolle (Zurechnung zu Clients nur über das Proxy-Log möglich) | [ORGANISATION: Log-System, Aufbewahrung] |
| Zeitsynchronisation des Hosts | [ORGANISATION: NTP-Quelle] |
| Verschlüsselter Speicher für die Volumes `media` und `pgdata` (das Produkt nutzt einfaches Unlink) | [ORGANISATION: LUKS/andere, Schlüsselverwaltung] |
| Datenbank-Backup und Restore-Tests; vertraulicher Umgang mit Dumps | [ORGANISATION: Rhythmus, Ablageort] |
| Patch- und Änderungsmanagement für Host, Container-Runtime und ConvertFlow-Images (Neubau bei Abhängigkeits-Updates) | [ORGANISATION: Prozessverweis] |
| Beschränkung des Zugangs zu Host und Container-Runtime (Administration) | [ORGANISATION: Verweis] |
| Regeln, welche Medien verarbeitet werden dürfen (Schutzbedarf, Urheberrecht, Patentlizenzen bei der Variante `full`) | [ORGANISATION: Richtlinienverweis] |

## 5. Restrisikobewertung

Mit den genannten Maßnahmen verbleibt das Risiko, dass **jede Stelle, die den Dienst-Port erreicht, alle Aufträge lesen, löschen und abbrechen sowie Kapazität des Dienstes verbrauchen kann**, und dass eine Schwachstelle beim Parsen von Medien den Inhalt des Medienvolumens (Dateien innerhalb der Aufbewahrung) und die Datenbank-Zugangsdaten offenlegen kann.

| Risiko | Restrisiko-Bewertung |
|---|---|
| T1 Offenlegung fremder Medien | [ORGANISATION: Bewertung und Begründung] |
| T2 Löschen / Abbrechen / unerwünschte Uploads | [ORGANISATION: ...] |
| T3 Ressourcenerschöpfung | [ORGANISATION: ...] |
| T4 Ausnutzung einer Schwachstelle beim Medien-Parsing | [ORGANISATION: ...] |
| T5 Mithören | [ORGANISATION: ...] |
| T6 Fehlende Zurechenbarkeit | [ORGANISATION: ...] |
| T7 Datenreste | [ORGANISATION: ...] |

Gesamtrestrisiko: [ORGANISATION: gering / mittel / hoch] - [ORGANISATION: Begründung]

## 6. Entscheidung

[ORGANISATION: akzeptiert / akzeptiert mit Auflagen / abgelehnt. Ggf. Auflagen.]

| Punkt | Eintrag |
|---|---|
| Risikoeigentümer(in) | [ORGANISATION: Name, Rolle] |
| Informationssicherheitsbeauftragte(r) (ISB) | [ORGANISATION: Name] |
| Datenschutzbeauftragte(r) (falls personenbezogene Daten verarbeitet werden können) | [ORGANISATION: Name] |
| Datum der Entscheidung | [ORGANISATION: Datum] |
| Gültig bis / nächste Überprüfung | [ORGANISATION: Datum; Empfehlung: höchstens jährlich und bei jedem Auslöser aus Abschnitt 7] |

| Rolle | Name | Datum | Unterschrift |
|---|---|---|---|
| Risikoeigentümer(in) | | | |
| ISB | | | |
| IT-Betrieb | | | |

## 7. Auslöser für eine erneute Bewertung

Die Risikoakzeptanz ist erneut zu bewerten, wenn eines der folgenden Ereignisse eintritt:

- der Dienst wird über das in Abschnitt 1 genannte isolierte Segment hinaus erreichbar, oder die Firewall-Regeln ändern sich;
- Medien mit höherem Schutzbedarf als bewertet sollen verarbeitet werden;
- Nutzende mit anderer Berechtigung oder aus anderen Organisationseinheiten nutzen dieselbe Instanz;
- TLS- oder Reverse-Proxy-Konfiguration ändern sich oder die Instanz wird hinter einen anderen Proxy gestellt;
- die FFmpeg-Variante wechselt zu `full`, Hardwarebeschleunigung bzw. Gerätedurchreichung wird ergänzt oder die Container-Härtung wird gelockert;
- eine Schwachstelle in FFmpeg, einer Codec-Bibliothek oder ConvertFlow mit relevanter Auswirkung wird bekannt;
- ein Sicherheitsvorfall mit Bezug zum Dienst tritt auf;
- das Überprüfungsdatum aus Abschnitt 6 ist erreicht.
