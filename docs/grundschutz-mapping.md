# IT-Grundschutz mapping

> **Position.** ConvertFlow is software, not an ISMS. It cannot be "Grundschutz-compliant", "Grundschutz-certified" or "BSI-approved" by itself; IT-Grundschutz is applied by an organization to its Informationsverbund (Strukturanalyse, Schutzbedarfsfeststellung, Modellierung, IT-Grundschutz-Check, Risikoanalyse, Umsetzung, Audit). This document supports that work: it shows, requirement by requirement, what the product does, what the operator still has to do, and where the product deviates. It does not replace the operator's own IT-Grundschutz-Check and makes no statement about any particular installation. A "Teilweise" or "Erfüllt (Produkt)" entry means *the product provides a measure*, not that the requirement is fulfilled in your Informationsverbund.

**Kompendium edition used:** IT-Grundschutz-Kompendium, **Edition 2023** (page footer "Stand Februar 2023"), downloaded from the BSI web site (https://www.bsi.bund.de/DE/Themen/Unternehmen-und-Organisationen/Standards-und-Zertifizierung/IT-Grundschutz/IT-Grundschutz-Kompendium/it-grundschutz-kompendium_node.html, file `IT_Grundschutz_Kompendium_Edition2023.pdf`) on 2026-10-17. That was the newest edition listed on that page on that day. Requirement IDs, titles and levels below were extracted from that PDF by script and not typed by hand; requirement texts are not reproduced (the Kompendium is © BSI; read the originals). Requirements marked "ENTFALLEN" in the Kompendium are omitted from the tables and counted per Baustein. If the BSI publishes a newer edition, re-check this document ("NOT VERIFIED against editions after 2023").

Requirement level: **Basis** (B, MUSS), **Standard** (S, SOLLTE), **Hoch** (H, bei erhöhtem Schutzbedarf). The Bausteine here were chosen by the project; the Modellierung of your Informationsverbund may select others (for example CON.2 Datenschutz when media can contain personal data, OPS.1.1.6 Software-Tests und -Freigaben, SYS.1.3 for the host OS, INF.* for premises). Role tags in the Kompendium (for example [IT-Betrieb]) are not repeated.

Status values: **Erfüllt (Produkt)** the product implements the measure; **Teilweise** the product supports it and the operator completes it or there is a limitation; **Operator** organizational/technical measure outside the product; **Nicht umgesetzt** accepted deviation (no authentication, see [risk-acceptance-template.md](risk-acceptance-template.md)); **Nicht anwendbar** the requirement does not apply to this product.

Evidence: file paths are relative to the repository root; test names are Go tests (`go test ./...`) unless a script is named. The tests exist in the repository; the container checks and the API behavior referenced here were run on rootless podman 6.1.2. The Docker engine, Windows and the GitHub CI pipeline are **not verified**.

## Summary of deviations and gaps

| Topic | Affected requirements | Statement |
|---|---|---|
| No authentication / authorization / sessions | ORP.4 (A1-A4, A6, A7, A9, A12-A14, A16, A18, A21), CON.10.A1-A3/A15/A16, APP.3.1.A1 | Deliberate. Anyone who can reach the port can submit, read, cancel and delete every job. Compensating controls: loopback default, isolated segment, hardened containers, limits, retention, audit log; operator: network access control. Use the risk template. |
| TLS not on by default | APP.3.2.A11, CON.10.A7/A18 | Default is HTTP on loopback. TLS 1.2+ available in the application or via the operator's proxy. |
| Database connection not encrypted | APP.4.3.A16 | `sslmode=disable` on the internal network. |
| Database role is a superuser | APP.4.3.A3 | The application uses the image's initial role. |
| Plain unlink only | CON.6.A2, A12 | No secure wipe; use an encrypted volume. |
| No malware scanning of uploads | APP.3.2.A3, OPS.1.1.4 | Mitigated by isolation of parsing, not by scanning. |
| Client IP in audit log is the TCP peer | OPS.1.1.5, APP.3.2.A4 | `X-Forwarded-For` is not trusted; behind a proxy the proxy address is logged. |
| No HTTP access log, no central logging | APP.3.2.A4, OPS.1.1.5.A6 | Operator's proxy/log infrastructure. |
| Docker engine, Windows, macOS, hardware acceleration unverified | SYS.1.6 | Only rootless podman 6.1.2 on Linux was tested. |
| CI/release pipeline not yet run on GitHub | CON.8, SYS.1.6.A6, OPS.1.1.3.A10 | Scripts verified locally; signing path untested. |
| No independent penetration test or audit | APP.3.1.A22, APP.3.2.A16 | Operator to commission. |
| No HA for api and db | APP.3.2.A15, SYS.1.6.A25 | Single instance. |

## Bausteine


### CON.10 Entwicklung von Webanwendungen

Applies to the web application (UI and REST API) as a product. Several requirements are addressed to developers (Entwickelnde) and are assessed here as product properties.

Requirements in this Baustein (Edition 2023): 18 active, 0 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| CON.10.A1 | Authentisierung bei Webanwendungen | Basis | Nicht umgesetzt | No authentication by design. | Restrict network access; optional authentication at a reverse proxy; risk acceptance. | Deviation: nothing to authenticate against. See risk template. |
| CON.10.A2 | Zugriffskontrolle bei Webanwendungen | Basis | Nicht umgesetzt | No users, no authorization component; any client can access every job and result. | Network-level access control. | Deviation. Mitigation: job ids are random UUIDs (unguessable) but `GET /api/v1/jobs` lists them. |
| CON.10.A3 | Sicheres Session-Management | Basis | Nicht anwendbar | No sessions, no cookies, no session ids. | - | Not applicable because there are no sessions. |
| CON.10.A4 | Kontrolliertes Einbinden von Inhalten bei Webanwendungen | Basis | Erfüllt (Produkt) | Only embedded static assets and API responses are served; CSP `default-src 'none'; script-src 'self'; ...`; no redirect function; no external content (`internal/api/middleware.go`, `internal/ui`). | - | - |
| CON.10.A5 | Upload-Funktionen | Basis | Erfüllt (Produkt) | Storage path is server-generated (`/data/in/<uuid>`, O_EXCL create); the client cannot influence it. Upload limits are configurable at run time (`CONVERTFLOW_MAX_UPLOAD_BYTES` etc.). Tests: `TestUploadHostileFilenameStaysInsideDataDir`, `TestUploadHostileFilenamesNeverEscapeInputDir`, `TestUploadTooLarge`. | Tune the limits. | - |
| CON.10.A6 | Schutz vor unerlaubter automatisierter Nutzung von Webanwendungen | Basis | Teilweise | Caps on concurrent uploads, active jobs and SSE streams (`503` with `Retry-After`), idle-time limit on uploads. Tests: `TestUploadConcurrencyLimitAndRelease`, `TestUploadQueueCap`. | Rate limiting per client at proxy/firewall. | No per-client rate limit or CAPTCHA (no client identity). |
| CON.10.A7 | Schutz vertraulicher Daten | Basis | Teilweise | Uploads use POST; `Cache-Control: no-store` on `/api/`; `Referrer-Policy: no-referrer`; no credentials exist to protect. Source files are not served (only embedded UI assets). | TLS (see APP.3.2.A11). Do not upload confidential media over plain HTTP. | Uploaded media is stored unencrypted on the media volume until retention expires. |
| CON.10.A8 | Umfassende Eingabevalidierung und Ausgabekodierung | Basis | Erfüllt (Produkt) | All inputs validated server-side: preset id and options against fixed lists/ranges (`internal/jobs/options.go`), strict multipart field parsing, filename sanitization (`SanitizeFilename`), job ids validated as canonical UUIDs; the UI renders with text nodes/CSP without inline script. Tests: `TestUploadStrictFieldParsing`, `TestSanitizeFilenameHostile`, `internal/jobs/options_test.go`, `TestMalformedIDIs404EverywhereIncludingNonCanonical`. | - | - |
| CON.10.A9 | Schutz vor SQL-Injection | Basis | Erfüllt (Produkt) | All queries use bound parameters (pgx, `internal/jobs/store.go`); ids are validated before lookup. | - | - |
| CON.10.A10 | Restriktive Herausgabe sicherheitsrelevanter Informationen | Basis | Erfüllt (Produkt) | Problem responses carry fixed client-safe messages; internal errors only in the server log; FFmpeg error text is sanitized of paths; no Server header, no stack traces (`internal/api/problem.go`, `TestPanicBecomesGenericProblem`, `TestOutputRemovedIs410WithoutPathLeak`). | - | - |
| CON.10.A11 | Softwarearchitektur einer Webanwendung | Standard | Teilweise | Architecture documented in the design spec and README (components, trust boundaries: media parsing only in the worker). | Review for your environment. | No dedicated architecture/security-mechanism document beyond `docs/security.md` and the spec. |
| CON.10.A12 | Verifikation essenzieller Änderungen | Standard | Nicht anwendbar | No settings or accounts that need re-verification. | - | Not applicable. |
| CON.10.A13 | Fehlerbehandlung | Standard | Erfüllt (Produkt) | Errors leave a consistent state: uploads clean up partial files, semaphore released on every exit path, janitor removes orphans, worker fences terminal writes. Errors are logged. Tests: `TestUploadSemaphoreReleasedOnEveryExitPath`, `TestUploadTooLargeChunkedNoLeftovers`, `TestStrandedQueuedJobExpires`, `TestWorkerCrashRequeue`. | - | - |
| CON.10.A14 | Sichere HTTP-Konfiguration bei Webanwendungen | Standard | Erfüllt (Produkt) | Headers set on every response: CSP, X-Content-Type-Options, Referrer-Policy, X-Frame-Options, Cross-Origin-Resource-Policy; Strict-Transport-Security when the application terminates TLS; `Cache-Control: no-store` on `/api/`; correct Content-Type. No cookies are set. Tests: `TestSecurityHeaders`, `TestSecurityHeadersOnProblemAndAPI`. | Add HSTS at the reverse proxy if TLS is terminated there. | HSTS absent in plain-HTTP deployments. |
| CON.10.A15 | Verhinderung von Cross-Site-Request-Forgery | Standard | Nicht umgesetzt | No sessions and no secret token; a cross-origin form POST (upload) is not distinguished from an intended one. Cross-origin DELETE fails CORS preflight. | Network isolation. | Part of the no-authentication deviation; see `docs/security.md`. |
| CON.10.A16 | Mehr-Faktor-Authentisierung | Standard | Nicht umgesetzt | No authentication, hence no multi-factor authentication. | MFA at a reverse proxy/SSO if required. | Deviation. |
| CON.10.A17 | Verhinderung der Blockade von Ressourcen | Hoch | Teilweise | Resource-intensive operations (transcoding) are bounded: upload, duration, pixel, stream and output limits, job timeout, container CPU/memory/pids limits, queue and upload caps; container logs rotate (10 MB x 3). Tests: `TestDecompressionBombsRefused`. | Size limits and workers to the hardware; monitor the queue and the volume. | No per-client limits; a reachable client can fill the queue up to the caps. |
| CON.10.A18 | Kryptografische Absicherung vertraulicher Daten | Hoch | Teilweise | TLS 1.2+ for data in transit if enabled. Data at rest is not encrypted by the application. | Encrypted media/pgdata volume. | No application-level encryption at rest. |

### APP.3.1 Webanwendungen und Webservices

Applies to the operation of ConvertFlow as a web application/web service. Requirements addressed to the IT-Betrieb are split between the product defaults and the operator.

Requirements in this Baustein (Edition 2023): 11 active, 14 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| APP.3.1.A1 | Authentisierung | Basis | Nicht umgesetzt | No authentication by design. | Network controls; proxy-level authentication optional. | Deviation. See risk template. |
| APP.3.1.A4 | Kontrolliertes Einbinden von Dateien und Inhalten | Basis | Erfüllt (Produkt) | Upload function restricted: maximum size, server-chosen storage location, content probed by ffprobe, container format allow-list (`-format_whitelist`), playlist/concat/network inputs rejected. Any media type FFmpeg can decode within the whitelist is accepted (no extension-based file type filter). Tests: `TestPlaylistAndConcatInputsRejected`, `TestGarbageInputsFailCleanly`, `TestFormatWhitelistAcceptsSupportedContainers`. | - | Allowed file types are determined by the FFmpeg format whitelist, not by an operator-editable list. |
| APP.3.1.A7 | Schutz vor unerlaubter automatisierter Nutzung | Basis | Teilweise | Concurrency, queue and stream caps (see CON.10.A6). | Rate limiting/WAF at the network edge. | No automated-use detection. |
| APP.3.1.A8 | Systemarchitektur | Standard | Teilweise | Security considered in the design spec (section 3 hardening, trust boundary around the worker). | Include in your own architecture documentation. | - |
| APP.3.1.A9 | Beschaffung von Webanwendungen und Webservices | Standard | Operator | - | Procurement/acceptance by the operating organization; use this mapping and `docs/security.md` as input. | - |
| APP.3.1.A11 | Sichere Anbindung von Hintergrundsystemen | Standard | Erfüllt (Produkt) | Backend systems: only PostgreSQL, reached from api/worker only over the internal `backend` network with a file-based credential; no other back ends. | - | Database traffic is not TLS-encrypted (`sslmode=disable`), protected by the internal network only. |
| APP.3.1.A12 | Sichere Konfiguration | Standard | Erfüllt (Produkt) | Only intended endpoints are routed (`internal/api/server.go` route table, OpenAPI kept in sync by `TestOpenAPIMatchesRoutes`); unknown paths answer problem+json; no debug or admin endpoints; read-only root filesystem. | Keep `/metrics` off untrusted networks (it is unauthenticated). | - |
| APP.3.1.A14 | Schutz vertraulicher Daten | Basis | Nicht anwendbar | No credentials are stored by the application. The database password is a file secret (`scripts/gen-secrets.sh`). | - | Not applicable (no user credentials). |
| APP.3.1.A20 | Einsatz von Web Application Firewalls | Hoch | Operator | None built in. | Place a WAF at the network edge if required. | No WAF. |
| APP.3.1.A21 | Sichere HTTP-Konfiguration bei Webanwendungen | Standard | Erfüllt (Produkt) | See CON.10.A14. | HSTS at proxy when TLS is terminated there. | - |
| APP.3.1.A22 | Penetrationstest und Revision | Standard | Operator | Automated tests incl. hostile-input suite (`internal/e2e/hostile_test.go`), gosec, govulncheck in CI. No external penetration test has been performed. | Commission penetration tests/revisions for your deployment. | No independent penetration test or audit. |

### APP.3.2 Webserver

ConvertFlow embeds its own HTTP server (Go `net/http`); a separate web server is not used unless the operator adds a reverse proxy, to which the same requirements then apply.

Requirements in this Baustein (Edition 2023): 17 active, 3 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| APP.3.2.A1 | Sichere Konfiguration eines Webservers | Basis | Erfüllt (Produkt) | The Go HTTP server runs as uid 65532 (non-root), in a distroless image without shell, read-only rootfs, all capabilities dropped; TLS configuration, timeouts and header limits are set in code (`cmd/convertflow-api/main.go`). Verified by `tests/container/hardening.sh`. | Host and runtime hardening. | - |
| APP.3.2.A2 | Schutz der Webserver-Dateien | Basis | Erfüllt (Produkt) | Only embedded assets are served (no filesystem document root); API data files live in `/data` outside any served path; the data volume is mounted only into the api and worker containers (uid 65532); input files are created with mode 0600. | Host file permissions on volumes and secrets. | - |
| APP.3.2.A3 | Absicherung von Datei-Uploads und -Downloads | Basis | Teilweise | Maximum upload size (`CONVERTFLOW_MAX_UPLOAD_BYTES`), free space check before accepting an upload (`CONVERTFLOW_MIN_FREE_BYTES`, `507`), downloads limited to the job's recorded output file. Tests: `TestUploadInsufficientDiskSpace`, `TestUploadTooLarge`. | Malware scanning of uploaded or published files (if required by your policy). | ConvertFlow does **not** scan uploads or results for malware; uploaded media is parsed in an isolated worker instead. Published (downloaded) files are converted outputs of the user's own uploads. |
| APP.3.2.A4 | Protokollierung von Ereignissen | Basis | Teilweise | JSON audit events for uploads, rejections, deletions, cancellations (not every successful read). Client IP is the TCP peer. Failed accesses: 404/409/410 are returned but not all are logged by the application. See `docs/operations.md`. | Reverse proxy access log for full request logging; central log collection. | No HTTP access log with all successful and failed requests is produced by the application. |
| APP.3.2.A5 | Authentisierung | Basis | Nicht anwendbar | No passwords are used by the web server. | - | Not applicable. |
| APP.3.2.A7 | Rechtliche Rahmenbedingungen für Webangebote | Basis | Operator | - | Legal framework for offering the service (data protection, provider obligations) is the operator's. | - |
| APP.3.2.A8 | Planung des Einsatzes eines Webservers | Standard | Operator | Purpose and scope documented in README and spec. | Document planned use in your environment. | - |
| APP.3.2.A9 | Festlegung einer Sicherheitsrichtlinie für den Webserver | Standard | Operator | - | Security policy for the service. | - |
| APP.3.2.A10 | Auswahl eines geeigneten Webhosters | Standard | Nicht anwendbar | Self-hosted; no web hoster involved. | - | Not applicable. |
| APP.3.2.A11 | Verschlüsselung über TLS | Basis | Teilweise | Optional TLS 1.2+ in the application (`CONVERTFLOW_TLS_CERT_FILE`/`_KEY_FILE`, verified incl. refusal of TLS 1.1) or behind a reverse proxy; HSTS emitted when TLS is on. Default is plain HTTP on loopback. | MUST for connections through untrusted networks: enable TLS or a TLS-terminating proxy, manage certificates. | Default configuration has no TLS (loopback only). |
| APP.3.2.A12 | Geeigneter Umgang mit Fehlern und Fehlermeldungen | Standard | Erfüllt (Produkt) | Neither product name nor version is sent in headers (no `Server` header, verified in a live response); fixed error messages without system details. | - | - |
| APP.3.2.A13 | Zugriffskontrolle für Webcrawler | Standard | Nicht anwendbar | No public web content; internal tool. | Do not expose to crawlers; no robots.txt is served. | Not applicable to an isolated service. |
| APP.3.2.A14 | Integritätsprüfungen und Schutz vor Schadsoftware | Standard | Teilweise | Read-only rootfs and digest-pinned images prevent in-place modification; images are scanned (Trivy) and have SBOMs. | Runtime integrity monitoring, malware scanning of files if required. | No runtime integrity monitoring. |
| APP.3.2.A15 | Redundanz | Hoch | Operator | Single instance by default; multiple workers possible. | Redundancy/HA planning. | No HA for api and db. |
| APP.3.2.A16 | Penetrationstest und Revision | Standard | Operator | See APP.3.1.A22. | Penetration tests/revisions. | No independent test. |
| APP.3.2.A18 | Schutz vor Denial-of-Service-Angriffen | Hoch | Teilweise | Caps and limits (see CON.10.A17); `/metrics` for monitoring. | Network-level DoS protection and monitoring. | - |
| APP.3.2.A20 | Benennung von Anzusprechenden | Standard | Operator | Vulnerability reporting contact: `SECURITY.md` (project level). | Name contacts for the service in your organization. | - |

### CON.8 Software-Entwicklung

Relevant to the development process of ConvertFlow itself. For the operator these are supplier-assurance inputs; they are not requirements the operator can satisfy by configuration.

Requirements in this Baustein (Edition 2023): 18 active, 4 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| CON.8.A1 | Definition von Rollen und Zuständigkeiten | Standard | Teilweise | Single-maintainer open-source project; roles not formally defined. | - | No formal role definition. |
| CON.8.A2 | Auswahl eines Vorgehensmodells | Basis | Teilweise | Spec-first, plan-driven, test-driven development (`docs/superpowers/specs`, `docs/superpowers/plans`, `CONTRIBUTING.md`). | - | No formally selected process model. |
| CON.8.A3 | Auswahl einer Entwicklungsumgebung | Basis | Teilweise | Go version pinned in `go.mod`/`mise.toml`, CI tool versions in `scripts/tool-versions.env`, digest-pinned build images. | - | No documented selection criteria. |
| CON.8.A5 | Sicheres Systemdesign | Basis | Erfüllt (Produkt) | Input validated before processing, allow-lists, server-side checks, fail-closed flavor policy (`internal/presets`), least privilege containers, secure defaults (loopback, no egress, limits). See `docs/security.md`. | - | - |
| CON.8.A6 | Verwendung von externen Bibliotheken aus vertrauenswürdigen Quellen | Basis | Erfüllt (Produkt) | Few direct Go dependencies (pgx, uuid) from public module proxy with `go.sum` verification; permissive-license policy (`scripts/check-licenses.sh`); govulncheck and Dependabot; FFmpeg source verified by SHA-256 pinned after GPG verification (`docker/ffmpeg.env`); Debian libraries from the pinned base image's repositories. | - | Debian libraries are not individually pinned by version; the base image digest is. |
| CON.8.A7 | Durchführung von entwicklungsbegleitenden Software-Tests | Basis | Erfüllt (Produkt) | Unit, integration (real FFmpeg, real PostgreSQL), hostile-input, race and container tests; CI runs gofmt, vet, `go test -race`, gosec, govulncheck (`.github/workflows/ci.yml`). | - | CI has not yet run on GitHub (see `docs/supply-chain.md`); all scripts were run locally. |
| CON.8.A8 | Bereitstellung von Patches, Updates und Änderungen | Basis | Teilweise | Release process and Dependabot/digest-drift checks exist; there is no released version yet. | Apply updates by rebuilding/upgrading (`docs/operations.md`). | No security advisories or patch SLA beyond best effort (`SECURITY.md`). |
| CON.8.A10 | Versionsverwaltung des Quellcodes | Basis | Erfüllt (Produkt) | Git; commits are small and logged. | - | Repository access control and branch protection are settings of the hosting platform and not verified here. |
| CON.8.A11 | Erstellung einer Richtlinie für die Software-Entwicklung | Standard | Teilweise | `CONTRIBUTING.md` states the rules (TDD, security properties, license policy). | - | Short; not a full secure-development policy. |
| CON.8.A12 | Ausführliche Dokumentation | Standard | Erfüllt (Produkt) | README, `docs/operations.md`, `docs/api.md`, `docs/security.md`, OpenAPI document, container and supply-chain docs. | - | - |
| CON.8.A14 | Schulung des Entwicklungsteams zur Informationssicherheit | Standard | Operator | - | Training of the (organization's) development team. | - |
| CON.8.A16 | Geeignete Steuerung der Software-Entwicklung | Standard | Teilweise | Task plan with review gates in `docs/superpowers/plans`. | - | Not a formal project management model. |
| CON.8.A17 | Auswahl vertrauenswürdiger Entwicklungswerkzeuge | Hoch | Teilweise | Toolchain pinned and verified in CI (Go, esbuild/typescript from a lockfile with `npm ci --ignore-scripts`). | - | No assessment of tool vendors. |
| CON.8.A18 | Regelmäßige Sicherheitsaudits für die Entwicklungsumgebung | Hoch | Operator | - | Security audits of the development environment. | - |
| CON.8.A19 | Regelmäßige Integritätsprüfung der Entwicklungsumgebung | Hoch | Teilweise | Checksums for release artifacts, cosign signatures planned in the release workflow (not yet exercised). | - | Not verified in practice. |
| CON.8.A20 | Überprüfung von externen Komponenten | Basis | Erfüllt (Produkt) | Dependency vulnerabilities (govulncheck, Trivy), license scan, SBOMs (CycloneDX) per release (`scripts/security-scan.sh`, `scripts/trivy-scan.sh`, `scripts/gen-sbom.sh`). | - | Pipeline not yet run on GitHub. |
| CON.8.A21 | Bedrohungsmodellierung | Standard | Teilweise | Threat model summary in `docs/security.md`; hostile-input and injection tests derive from it. | - | Lightweight model, not a formal STRIDE document. |
| CON.8.A22 | Sicherer Software-Entwurf | Standard | Teilweise | Design spec and `docs/security.md` reflect the threat model (isolated parser, no shell, allow-lists). | - | - |

### OPS.1.1.5 Protokollierung

Applies to the operation of the service. The product produces the audit events; collection, time sync and retention are the operator's.

Requirements in this Baustein (Edition 2023): 11 active, 2 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| OPS.1.1.5.A1 | Erstellung einer Sicherheitsrichtlinie für die Protokollierung | Basis | Operator | Event catalogue in `docs/operations.md`. | Logging policy of the organization. | - |
| OPS.1.1.5.A3 | Konfiguration der Protokollierung auf System- und Netzebene | Basis | Teilweise | Structured JSON audit events (job created/started/finished/failed/canceled/deleted, upload rejected, janitor actions) to stdout; no file contents (`internal/audit`, `docs/operations.md`). Tests: `internal/audit/audit_test.go`, `TestAuditLogOnCreate`, `TestUploadRejectionsAreAudited`. | Configure log forwarding and system/network-level logging. | Not every request is logged (see APP.3.2.A4); there is no log of successful downloads or listings. |
| OPS.1.1.5.A4 | Zeitsynchronisation der IT-Systeme | Basis | Operator | UTC RFC 3339 timestamps in one format from all components. | Time synchronization of the host (NTP). | The product does not synchronize time itself. |
| OPS.1.1.5.A5 | Einhaltung rechtlicher Rahmenbedingungen | Basis | Operator | Filenames appear in the database and not in audit events; client IPs appear in the audit log (personal data). | Legal assessment (data protection, co-determination). | - |
| OPS.1.1.5.A6 | Aufbau einer zentralen Protokollierungsinfrastruktur | Standard | Operator | Stdout JSON suitable for log shippers. | Central log infrastructure. | No built-in central logging. |
| OPS.1.1.5.A8 | Archivierung von Protokollierungsdaten | Standard | Operator | - | Archiving per legal requirements. | Container logs rotate after 3 x 10 MB by default. |
| OPS.1.1.5.A9 | Bereitstellung von Protokollierungsdaten für die Auswertung | Standard | Operator | Machine-readable JSON. | Filter/normalize/correlate in the SIEM. | - |
| OPS.1.1.5.A10 | Zugriffsschutz für Protokollierungsdaten | Standard | Operator | - | Protect logs from administrators. | Logs are in the container runtime's json-file store on the host. |
| OPS.1.1.5.A11 | Steigerung des Protokollierungsumfangs | Hoch | Teilweise | Audit events cover job lifecycle; no debug logging of content. | Increase scope if the protection requirement demands. | - |
| OPS.1.1.5.A12 | Verschlüsselung der Protokollierungsdaten | Hoch | Operator | - | Encrypt/sign log transport and storage. | - |
| OPS.1.1.5.A13 | Hochverfügbare Protokollierungsinfrastruktur | Hoch | Operator | - | HA logging infrastructure. | - |

### CON.6 Löschen und Vernichten

The product deletes expired data with a plain unlink. Media-level sanitization is the operator's.

Requirements in this Baustein (Edition 2023): 8 active, 6 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| CON.6.A1 | Regelung für die Löschung und Vernichtung von Informationen | Basis | Teilweise | Retention-based deletion defined and configurable (`CONVERTFLOW_RETENTION`, 24 h), explicit `DELETE`, input deleted after processing. | Organization-wide deletion regulation covering this service and its volumes. | - |
| CON.6.A2 | Ordnungsgemäßes Löschen und Vernichten von schützenswerten Betriebsmitteln und Informationen | Basis | Teilweise | Janitor deletes files and metadata after retention; orphan sweep; input removed after the job. Tests: `TestRetentionJanitor`, `TestDeleteRemovesFiles`, `internal/janitor/janitor_test.go`. | Sanitization of the underlying storage when decommissioning. | Deletion is a plain `unlink`; no secure wipe, no overwriting. Journal/snapshots/backups of the volume may retain data. |
| CON.6.A4 | Auswahl geeigneter Verfahren zur Löschung oder Vernichtung von Datenträgern | Standard | Operator | Plain unlink only; encrypted volume recommended (`docs/operations.md`). | Choose a suitable method (e.g. encrypted volume with key destruction). | - |
| CON.6.A8 | Erstellung einer Richtlinie für die Löschung und Vernichtung von Informationen | Standard | Operator | - | Deletion policy. | - |
| CON.6.A11 | Löschung und Vernichtung von Datenträgern durch externe Dienstleistende | Basis | Operator | - | If external service providers handle disks. | - |
| CON.6.A12 | Mindestanforderungen an Verfahren zur Löschung und Vernichtung | Basis | Operator | - | Minimum deletion/destruction methods for media; the product only unlinks files. | Plain unlink does not meet media-level sanitization requirements by itself. |
| CON.6.A13 | Vernichtung defekter digitaler Datenträger | Standard | Operator | - | Destruction of defective disks. | - |
| CON.6.A14 | Vernichten von Datenträgern auf erhöhter Sicherheitsstufe | Hoch | Operator | - | Higher-level destruction. | - |

### CON.3 Datensicherungskonzept

Only the database needs backing up. The backup concept itself is the operator's.

Requirements in this Baustein (Edition 2023): 11 active, 4 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| CON.3.A1 | Erhebung der Einflussfaktoren für Datensicherungen | Basis | Teilweise | Documented: only the PostgreSQL database is relevant; media is ephemeral (`docs/operations.md`). | Determine influence factors for your site. | - |
| CON.3.A2 | Festlegung der Verfahrensweisen für die Datensicherung | Basis | Teilweise | `pg_dump`/restore procedure verified on podman (`docs/operations.md`). | Choose procedure, frequency, retention. | - |
| CON.3.A4 | Erstellung von Datensicherungsplänen | Basis | Operator | - | Backup plans. | - |
| CON.3.A5 | Regelmäßige Datensicherung | Basis | Operator | Dump procedure documented. | Run regular backups. | No built-in scheduler. |
| CON.3.A6 | Entwicklung eines Datensicherungskonzepts | Standard | Operator | - | Backup concept. | - |
| CON.3.A7 | Beschaffung eines geeigneten Datensicherungssystems | Standard | Operator | - | Backup system. | - |
| CON.3.A9 | Voraussetzungen für die Online-Datensicherung | Standard | Nicht anwendbar | No online backup interface beyond `pg_dump`. | - | Not applicable. |
| CON.3.A12 | Sichere Aufbewahrung der Speichermedien für die Datensicherungen | Basis | Operator | - | Safe storage of backup media. | - |
| CON.3.A13 | Einsatz kryptografischer Verfahren bei der Datensicherung | Hoch | Operator | Dumps contain filenames; store encrypted. | Cryptographic protection of backups. | - |
| CON.3.A14 | Schutz von Datensicherungen | Basis | Operator | - | Protect backups. | - |
| CON.3.A15 | Regelmäßiges Testen der Datensicherungen | Basis | Operator | Restore verified once on a throwaway stack. | Regular restore tests in your environment. | - |

### SYS.1.6 Containerisierung

Applies to the compose deployment. Rootless podman 6.1.2 is the only verified runtime; the Docker engine is not verified.

Requirements in this Baustein (Edition 2023): 26 active, 0 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| SYS.1.6.A1 | Planung des Container-Einsatzes | Basis | Teilweise | Purpose (isolation of untrusted media parsing, scaling workers) stated in the spec. | Planning in your environment. | - |
| SYS.1.6.A2 | Planung der Verwaltung von Containern | Basis | Operator | - | Lifecycle planning of container management. | - |
| SYS.1.6.A3 | Sicherer Einsatz containerisierter IT-Systeme | Basis | Erfüllt (Produkt) | Applications fit containers: stateless api/worker, state in PostgreSQL and a volume; protection requirement and isolation reflected in hardening. | Check against your protection requirement. | - |
| SYS.1.6.A4 | Planung der Bereitstellung und Verteilung von Images | Basis | Teilweise | Release bundle and process documented (`docs/supply-chain.md`, air-gap bundle). | Your own distribution process. | - |
| SYS.1.6.A5 | Separierung der Administrations- und Zugangsnetze bei Containern | Basis | Operator | Compose separates `frontend` (published port) and `backend` (internal) networks. | Separate host administration and access networks. | - |
| SYS.1.6.A6 | Verwendung sicherer Images | Basis | Erfüllt (Produkt) | Images are built from digest-pinned official bases (Debian, distroless, PostgreSQL) and checksum-verified FFmpeg source; images are signed and have SBOMs in the release workflow. Checked by `scripts/check-pinned-digests.sh` and `tests/container/hardening.sh`. | Verify signatures/checksums on import. | Signing and the CI/release pipeline have not run on GitHub yet. |
| SYS.1.6.A7 | Persistenz von Protokollierungsdaten der Container | Basis | Teilweise | Containers log to stdout; compose uses the `json-file` driver on the host with rotation. | Forward to central logging; retention. | - |
| SYS.1.6.A8 | Sichere Speicherung von Zugangsdaten bei Containern | Basis | Erfüllt (Produkt) | Database credentials are file secrets (`/run/secrets`), not in images or environment variables; checked by `tests/container/hardening.sh` (no password in environment). | Protect `./secrets` on the host. | The secret files are mode 0644 inside a 0700 directory so that uid 999 and 65532 can read them. |
| SYS.1.6.A9 | Eignung für Container-Betrieb | Standard | Erfüllt (Produkt) | Services tolerate unexpected termination: jobs are requeued by heartbeat, outputs are per attempt, shutdown grace period 45 s. | - | - |
| SYS.1.6.A10 | Richtlinie für Images und Container-Betrieb | Standard | Operator | - | Policy for images and container operation. | - |
| SYS.1.6.A11 | Nur ein Dienst pro Container | Standard | Erfüllt (Produkt) | One service per container (api, worker, db). | - | - |
| SYS.1.6.A12 | Verteilung sicherer Images | Standard | Teilweise | Image sources and verification documented (`docs/supply-chain.md`). | Document your trusted sources. | - |
| SYS.1.6.A13 | Freigabe von Images | Standard | Teilweise | CI/release gates (tests, scans, hardening test) before publishing. | Your own release approval (OPS.1.1.6). | - |
| SYS.1.6.A14 | Aktualisierung von Images | Standard | Teilweise | `scripts/pin-digests.sh`, weekly digest-drift workflow, upgrade procedure. | Patch process (OPS.1.1.3). | Pinned digests go stale until re-pinned. |
| SYS.1.6.A15 | Limitierung der Ressourcen pro Container | Standard | Erfüllt (Produkt) | CPU, memory and pids limits for every container (`compose.yaml`), tmpfs sizes, ulimits; checked by `tests/container/hardening.sh`. | Adjust to hardware; define behavior when limits are hit. | Disk quota on the media volume is not set by the product (free-space check only). |
| SYS.1.6.A16 | Administrativer Fernzugriff auf Container | Standard | Erfüllt (Produkt) | No administrative access from containers to the host (no privileged, no host namespaces, no devices, no docker socket); the API image has no shell. | - | - |
| SYS.1.6.A17 | Ausführung von Containern ohne Privilegien | Standard | Erfüllt (Produkt) | Rootless podman verified; containers run as non-root users (65532, 999); `no-new-privileges`, `cap_drop: ALL`. | Run the container runtime itself unprivileged (rootless). | Rootful Docker is not verified. |
| SYS.1.6.A18 | Accounts der Anwendungsdienste | Standard | Erfüllt (Produkt) | Container accounts have no host privileges; apps run as uid 65532. | - | - |
| SYS.1.6.A19 | Einbinden von Datenspeichern in Container | Standard | Erfüllt (Produkt) | Only the `media` volume (api, worker), `pgdata` (db) and secret files are mounted; no host paths. | - | - |
| SYS.1.6.A20 | Absicherung von Konfigurationsdaten | Standard | Erfüllt (Produkt) | Configuration is versioned in the repository (`compose.yaml`, Dockerfiles). | Keep your overrides in version control. | - |
| SYS.1.6.A21 | Erweiterte Sicherheitsrichtlinien | Hoch | Teilweise | Default seccomp profile and `no-new-privileges`; no custom MAC profile. | SELinux/AppArmor policies. | No custom MAC/seccomp profile; network egress of the `api` container is not restricted. |
| SYS.1.6.A22 | Vorsorge für Untersuchungen | Hoch | Operator | - | Forensic readiness (snapshots, images). | - |
| SYS.1.6.A23 | Unveränderlichkeit der Container | Hoch | Erfüllt (Produkt) | Read-only root filesystem for all containers; writes only to tmpfs and the volumes. Checked in the hardening test (including a diff of the container filesystem). | - | - |
| SYS.1.6.A24 | Hostbasierte Angriffserkennung | Hoch | Operator | - | Host-based intrusion detection. | - |
| SYS.1.6.A25 | Hochverfügbarkeit von containerisierten Anwendungen | Hoch | Operator | Multiple workers supported. | HA design. | api and db are single instances. |
| SYS.1.6.A26 | Weitergehende Isolation und Kapselung von Containern | Hoch | Teilweise | Parser isolated from the network-facing component; internal network. | Dedicated hosts, VM isolation/gVisor/Kata as required. | Containers share the host kernel; no additional sandbox. |

### OPS.1.1.4 Schutz vor Schadprogrammen

ConvertFlow is not an anti-virus product and does not scan uploads for malware. It reduces exposure by parsing untrusted media only in an isolated worker.

Requirements in this Baustein (Edition 2023): 12 active, 3 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| OPS.1.1.4.A1 | Erstellung eines Konzepts für den Schutz vor Schadprogrammen | Basis | Operator | - | Concept for protection against malware. | - |
| OPS.1.1.4.A2 | Nutzung systemspezifischer Schutzmechanismen | Basis | Teilweise | System-specific protections used: non-root, read-only, seccomp, limits. | - | - |
| OPS.1.1.4.A3 | Auswahl eines Virenschutzprogrammes | Basis | Operator | - | Anti-virus product selection for the hosts. | ConvertFlow includes no anti-virus scanner. |
| OPS.1.1.4.A5 | Betrieb und Konfiguration von Virenschutzprogrammen | Basis | Operator | - | Operation/configuration of AV on the host. | - |
| OPS.1.1.4.A6 | Regelmäßige Aktualisierung der eingesetzten Virenschutzprogramme | Basis | Operator | - | AV updates (not possible inside the air-gapped containers). | - |
| OPS.1.1.4.A7 | Sensibilisierung und Verpflichtung der Benutzenden | Basis | Operator | - | User awareness. | - |
| OPS.1.1.4.A9 | Meldung von Infektionen mit Schadprogrammen | Standard | Operator | - | Infection reporting. | - |
| OPS.1.1.4.A10 | Nutzung spezieller Analyseumgebungen | Hoch | Teilweise | Uploaded files are parsed only in an isolated worker container without network egress and with resource limits. | - | Not a malware sandbox: no behavior analysis. |
| OPS.1.1.4.A11 | Einsatz mehrerer Scan-Engines | Hoch | Operator | - | Multiple scan engines. | - |
| OPS.1.1.4.A12 | Einsatz von Datenträgerschleusen | Hoch | Operator | - | Media sluices. | - |
| OPS.1.1.4.A13 | Umgang mit nicht vertrauenswürdigen Dateien | Hoch | Teilweise | Untrusted files are opened only in the isolated worker and converted to a defined format; this matches the intent of the requirement for media files. | Isolated handling for other file types. | - |
| OPS.1.1.4.A14 | Auswahl und Einsatz von Cyber-Sicherheitsprodukten gegen gezielte Angriffe | Hoch | Operator | - | Cyber-security products against targeted attacks. | - |

### OPS.1.1.3 Patch- und Änderungsmanagement

Mostly operator processes; the product provides a documented upgrade path, checksums and digest tooling.

Requirements in this Baustein (Edition 2023): 14 active, 2 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| OPS.1.1.3.A1 | Konzept für das Patch- und Änderungsmanagement | Basis | Operator | Upgrade procedure documented (`docs/operations.md`). | Patch and change management concept. | - |
| OPS.1.1.3.A2 | Festlegung der Zuständigkeiten | Basis | Operator | - | Responsibilities. | - |
| OPS.1.1.3.A3 | Konfiguration von Autoupdate-Mechanismen | Basis | Erfüllt (Produkt) | No auto-update mechanism exists; updates are explicit image upgrades. | Decide how updates are performed. | - |
| OPS.1.1.3.A5 | Umgang mit Änderungsanforderungen | Standard | Operator | - | Handling of requests for change. | - |
| OPS.1.1.3.A6 | Abstimmung von Änderungsanforderungen | Standard | Operator | - | Coordination of changes. | - |
| OPS.1.1.3.A7 | Integration des Änderungsmanagements in die Geschäftsprozesse | Standard | Operator | - | Integration into business processes. | - |
| OPS.1.1.3.A8 | Sicherer Einsatz von Werkzeugen für das Patch- und Änderungsmanagement | Standard | Operator | - | Tools for patch/change management. | - |
| OPS.1.1.3.A9 | Test- und Abnahmeverfahren für neue Hardware | Standard | Nicht anwendbar | No hardware. | - | Not applicable. |
| OPS.1.1.3.A10 | Sicherstellung der Integrität und Authentizität von Softwarepaketen | Standard | Erfüllt (Produkt) | Checksums for bundle and images, cosign signatures in the release workflow, FFmpeg source checksum, digest-pinned bases (`docs/supply-chain.md`). | Verify on import. | Signing path not yet exercised on GitHub. |
| OPS.1.1.3.A11 | Kontinuierliche Dokumentation der Informationsverarbeitung | Standard | Teilweise | Git history and release notes. | Document changes in your environment. | - |
| OPS.1.1.3.A12 | Einsatz von Werkzeugen beim Änderungsmanagement | Hoch | Operator | - | Change-management tooling. | - |
| OPS.1.1.3.A13 | Erfolgsmessung von Änderungsanforderungen | Hoch | Operator | `scripts/smoke.sh` and `tests/container/hardening.sh` support post-change tests. | Post-change tests. | - |
| OPS.1.1.3.A14 | Synchronisierung innerhalb des Änderungsmanagements | Hoch | Nicht anwendbar | No unreachable devices in scope. | - | Not applicable. |
| OPS.1.1.3.A15 | Regelmäßige Aktualisierung von IT-Systemen und Software | Basis | Teilweise | Weekly digest-drift check, Dependabot for actions, `scripts/pin-digests.sh`. New releases must be rebuilt to include OS and library fixes. | Apply updates to ConvertFlow, host and runtime promptly. | No automatic patching; the pinned base digests lag behind security updates until re-pinned. |

### APP.4.3 Relationale Datenbanken

PostgreSQL is part of the stack (queue and state). Most requirements concern the operation of the DBMS and rest with the operator; the compose stack provides a restricted default setup.

Requirements in this Baustein (Edition 2023): 17 active, 8 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| APP.4.3.A1 | Erstellung einer Sicherheitsrichtlinie für Datenbanksysteme | Basis | Operator | - | Security policy for database systems. | - |
| APP.4.3.A3 | Basishärtung des Datenbankmanagementsystems | Basis | Teilweise | Official PostgreSQL image, non-root user, read-only rootfs, internal network only, no published port, password from file secret. | Further DBMS hardening (pg_hba, privileges, extensions). | The application connects as the database's initial superuser role `convertflow` (created by the image); pg_hba and settings are image defaults and were not reviewed. |
| APP.4.3.A4 | Geregeltes Anlegen neuer Datenbanken | Basis | Teilweise | The application creates its schema automatically (`internal/db/migrate.go`); one database. | Process for new databases. | - |
| APP.4.3.A9 | Datensicherung eines Datenbanksystems | Basis | Teilweise | `pg_dump`/restore verified (`docs/operations.md`). | Backups. | - |
| APP.4.3.A11 | Ausreichende Dimensionierung der Hardware | Standard | Teilweise | Resource limits given in compose (1 CPU, 1 GiB). | Size to workload. | - |
| APP.4.3.A12 | Einheitlicher Konfigurationsstandard von Datenbankmanagementsystemen | Standard | Operator | - | Common configuration standard for DBMS. | - |
| APP.4.3.A13 | Restriktive Handhabung von Datenbank-Links | Standard | Nicht anwendbar | No database links. | - | Not applicable. |
| APP.4.3.A16 | Verschlüsselung der Datenbankanbindung | Standard | Nicht umgesetzt | Connection URL uses `sslmode=disable`; traffic stays on the internal `backend` network. | Enable TLS on PostgreSQL and change the URL if the network is not trusted. | No encryption of the database connection by default. |
| APP.4.3.A17 | Datenübernahme oder Migration | Standard | Nicht anwendbar | Migrations are part of the application; no data import. | - | Not applicable. |
| APP.4.3.A18 | Überwachung des Datenbankmanagementsystems | Standard | Teilweise | `/healthz` pings the database; container healthcheck; `/metrics` job counts. | Database monitoring. | - |
| APP.4.3.A19 | Schutz vor schädlichen Datenbank-Skripten | Standard | Erfüllt (Produkt) | No user-supplied scripts; migrations are embedded and fixed. | - | - |
| APP.4.3.A20 | Regelmäßige Audits | Standard | Operator | - | Regular audits. | - |
| APP.4.3.A21 | Einsatz von Datenbank Security Tools | Hoch | Operator | - | Database security tools. | - |
| APP.4.3.A22 | Notfallvorsorge | Hoch | Operator | - | Emergency preparedness. | - |
| APP.4.3.A23 | Archivierung | Hoch | Nicht anwendbar | The database holds ephemeral job metadata only. | - | Not applicable. |
| APP.4.3.A24 | Datenverschlüsselung in der Datenbank | Hoch | Operator | - | Encrypted volume for `pgdata`. | No data encryption inside the database. |
| APP.4.3.A25 | Sicherheitsprüfungen von Datenbanksystemen | Hoch | Operator | - | Security checks. | - |

### ORP.4 Identitäts- und Berechtigungsmanagement

ConvertFlow has no authentication or authorization (explicit operator decision). Requirements addressed to IAM of IT systems and applications are therefore not implemented; they are listed so the deviation is explicit.

Requirements in this Baustein (Edition 2023): 24 active, 0 marked ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level | Status | How ConvertFlow implements it (evidence) | Operator responsibility | Gap / deviation |
|---|---|---|---|---|---|---|
| ORP.4.A1 | Regelung für die Einrichtung und Löschung von Benutzenden und Benutzendengruppen | Basis | Nicht umgesetzt | There are no users or groups to create or delete. | Regulate access to the network segment and the host instead. | Deviation: no identity management. |
| ORP.4.A2 | Einrichtung, Änderung und Entzug von Berechtigungen | Basis | Nicht umgesetzt | No rights or roles; all clients are equal. | Network-level authorization. | Deviation. |
| ORP.4.A3 | Dokumentation der Benutzendenkennungen und Rechteprofile | Basis | Nicht umgesetzt | No users, accounts or credentials exist in the product. | Document who may reach the service (firewall rules). | Deviation. |
| ORP.4.A4 | Aufgabenverteilung und Funktionstrennung | Basis | Nicht umgesetzt | No roles, no separation of duties in the product. | Administrative duties on the host/runtime. | Deviation. |
| ORP.4.A5 | Vergabe von Zutrittsberechtigungen | Basis | Operator | - | Physical access to the hosts. | - |
| ORP.4.A6 | Vergabe von Zugangsberechtigungen | Basis | Nicht umgesetzt | No access authorization per user: every client reaching the port is authorized. | Access to the network path, host and container runtime. | Deviation. |
| ORP.4.A7 | Vergabe von Zugriffsrechten | Basis | Nicht umgesetzt | No access rights: every client may read, cancel and delete every job. | Network-level controls. | Deviation. |
| ORP.4.A8 | Regelung des Passwortgebrauchs | Basis | Nicht anwendbar | No passwords. | Rules for host and database administrators' passwords. | Not applicable to the product itself. |
| ORP.4.A9 | Identifikation und Authentisierung | Basis | Nicht umgesetzt | No identification or authentication of clients. | Network-level or proxy-level identification if required. | Deviation. |
| ORP.4.A10 | Schutz von Benutzendenkennungen mit weitreichenden Berechtigungen | Standard | Operator | - | Protect privileged host/runtime accounts. | - |
| ORP.4.A11 | Zurücksetzen von Passwörtern | Standard | Nicht anwendbar | No passwords. | - | Not applicable. |
| ORP.4.A12 | Entwicklung eines Authentisierungskonzeptes für IT-Systeme und Anwendungen | Standard | Nicht umgesetzt | No authentication concept; recorded decision (spec section 1). | Authentication concept for the network/proxy layer. | Deviation. |
| ORP.4.A13 | Geeignete Auswahl von Authentisierungsmechanismen | Standard | Nicht umgesetzt | No authentication mechanisms. | Mechanism selection at a proxy. | Deviation. |
| ORP.4.A14 | Kontrolle der Wirksamkeit der Benutzendentrennung am IT-System bzw. an der Anwendung | Standard | Nicht umgesetzt | No user separation exists to be checked; job ids are random UUIDs only. | - | Deviation. |
| ORP.4.A15 | Vorgehensweise und Konzeption der Prozesse beim Identitäts- und Berechtigungsmanagement | Standard | Operator | - | Process design for identity and access management. | - |
| ORP.4.A16 | Richtlinien für die Zugriffs- und Zugangskontrolle | Standard | Nicht umgesetzt | No policy enforced by the product. | Access policies for the network path. | Deviation. |
| ORP.4.A17 | Geeignete Auswahl von Identitäts- und Berechtigungsmanagement-Systemen | Standard | Operator | - | IAM system selection. | - |
| ORP.4.A18 | Einsatz eines zentralen Authentisierungsdienstes | Standard | Nicht umgesetzt | No central authentication service integration. | SSO at a reverse proxy if desired. | Deviation. |
| ORP.4.A19 | Einweisung aller Mitarbeitenden in den Umgang mit Authentisierungsverfahren und -mechanismen | Standard | Operator | - | Instruct staff. | - |
| ORP.4.A20 | Notfallvorsorge für das Identitäts- und Berechtigungsmanagement-System | Hoch | Operator | - | IAM emergency preparedness. | - |
| ORP.4.A21 | Mehr-Faktor-Authentisierung | Hoch | Nicht umgesetzt | No multi-factor authentication. | MFA at a proxy. | Deviation. |
| ORP.4.A22 | Regelung zur Passwortqualität | Basis | Nicht anwendbar | No passwords. | - | Not applicable. |
| ORP.4.A23 | Regelung für passwortverarbeitende Anwendungen und IT-Systeme | Basis | Nicht anwendbar | The application processes no passwords. | - | Not applicable. |
| ORP.4.A24 | Vier-Augen-Prinzip für administrative Tätigkeiten | Hoch | Operator | - | Four-eyes principle for administrative activities. | - |

## Bausteine that are entirely the operator's responsibility

These Bausteine usually belong to the operator's Modellierung for the host and network that run ConvertFlow. The product neither implements nor hinders them; the requirement IDs and titles are listed so that the operator can see the scope. Status for all rows: **Operator**.

### SYS.1.1 Allgemeiner Server

The host(s) running the containers are servers in the sense of SYS.1.1. All requirements are the operator's; ConvertFlow adds none and removes none. Read together with SYS.1.6 (container host) and the Linux/Unix server Bausteine that apply to the operator's OS (SYS.1.3 for Linux and Unix servers is outside this document and not checked).

28 active requirements, 11 ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level |
|---|---|---|
| SYS.1.1.A1 | Zugriffsschutz und Nutzung | Basis |
| SYS.1.1.A2 | Authentisierung an Servern | Basis |
| SYS.1.1.A5 | Schutz von Schnittstellen | Basis |
| SYS.1.1.A6 | Deaktivierung nicht benötigter Dienste | Basis |
| SYS.1.1.A9 | Einsatz von Virenschutz-Programmen auf Servern | Basis |
| SYS.1.1.A10 | Protokollierung | Basis |
| SYS.1.1.A11 | Festlegung einer Sicherheitsrichtlinie für Server | Standard |
| SYS.1.1.A12 | Planung des Server-Einsatzes | Standard |
| SYS.1.1.A13 | Beschaffung von Servern | Standard |
| SYS.1.1.A15 | Unterbrechungsfreie und stabile Stromversorgung | Standard |
| SYS.1.1.A16 | Sichere Installation und Grundkonfiguration von Servern | Standard |
| SYS.1.1.A19 | Einrichtung lokaler Paketfilter | Standard |
| SYS.1.1.A21 | Betriebsdokumentation für Server | Standard |
| SYS.1.1.A22 | Einbindung in die Notfallplanung | Standard |
| SYS.1.1.A23 | Systemüberwachung und Monitoring von Servern | Standard |
| SYS.1.1.A24 | Sicherheitsprüfungen für Server | Standard |
| SYS.1.1.A25 | Geregelte Außerbetriebnahme eines Servers | Standard |
| SYS.1.1.A27 | Hostbasierte Angriffserkennung | Hoch |
| SYS.1.1.A28 | Steigerung der Verfügbarkeit durch Redundanz | Hoch |
| SYS.1.1.A30 | Ein Dienst pro Server | Hoch |
| SYS.1.1.A31 | Einsatz von Ausführungskontrolle | Hoch |
| SYS.1.1.A33 | Aktive Verwaltung der Wurzelzertifikate | Hoch |
| SYS.1.1.A34 | Festplattenverschlüsselung | Hoch |
| SYS.1.1.A35 | Erstellung und Pflege eines Betriebshandbuchs | Standard |
| SYS.1.1.A36 | Absicherung des Bootvorgangs | Hoch |
| SYS.1.1.A37 | Kapselung von sicherheitskritischen Anwendungen und Betriebssystemkomponenten | Standard |
| SYS.1.1.A38 | Härtung des Host-Systems mittels Read-Only-Dateisystem | Hoch |
| SYS.1.1.A39 | Zentrale Verwaltung der Sicherheitsrichtlinien von Servern | Standard |

### OPS.1.1.2 Ordnungsgemäße IT-Administration

Administration of the hosts, container runtime and the compose stack is the operator's.

21 active requirements, 9 ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level |
|---|---|---|
| OPS.1.1.2.A2 | Vertretungsregelungen | Basis |
| OPS.1.1.2.A4 | Beendigung der Tätigkeit in der IT-Administration | Basis |
| OPS.1.1.2.A5 | Nachweisbarkeit von administrativen Tätigkeiten | Basis |
| OPS.1.1.2.A6 | Schutz administrativer Tätigkeiten | Basis |
| OPS.1.1.2.A7 | Regelung der IT-Administrationstätigkeit | Standard |
| OPS.1.1.2.A8 | Administration von Fachanwendungen | Standard |
| OPS.1.1.2.A11 | Dokumentation von IT-Administrationstätigkeiten | Standard |
| OPS.1.1.2.A16 | Erweiterte Sicherheitsmaßnahmen für Administrationszugänge | Standard |
| OPS.1.1.2.A17 | IT-Administration im Vier-Augen-Prinzip | Hoch |
| OPS.1.1.2.A18 | Durchgängige Protokollierung administrativer Tätigkeiten | Hoch |
| OPS.1.1.2.A19 | Nutzung hochverfügbarer IT-Administrationswerkzeuge | Hoch |
| OPS.1.1.2.A21 | Regelung der IT-Administrationsrollen | Basis |
| OPS.1.1.2.A22 | Trennung von administrativen und anderen Tätigkeiten | Basis |
| OPS.1.1.2.A23 | Rollen- und Berechtigungskonzept für administrative Zugriffe | Standard |
| OPS.1.1.2.A24 | Prüfen von Administrationstätigkeiten | Standard |
| OPS.1.1.2.A25 | Zeitfenster für schwerwiegende Administrationstätigkeiten | Standard |
| OPS.1.1.2.A26 | Backup der Konfiguration | Standard |
| OPS.1.1.2.A27 | Ersatz für zentrale IT-Administrationswerkzeuge | Standard |
| OPS.1.1.2.A28 | Protokollierung administrativer Tätigkeiten | Standard |
| OPS.1.1.2.A29 | Monitoring der IT-Administrationswerkzeuge | Hoch |
| OPS.1.1.2.A30 | Sicherheitsmonitoring administrativer Tätigkeiten | Hoch |

### NET.1.1 Netzarchitektur und -design

Network segmentation, firewalling and the placement of the service in the architecture are the operator's. The product assumes an isolated segment and publishes on loopback by default.

36 active requirements, 0 ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level |
|---|---|---|
| NET.1.1.A1 | Sicherheitsrichtlinie für das Netz | Basis |
| NET.1.1.A2 | Dokumentation des Netzes | Basis |
| NET.1.1.A3 | Anforderungsspezifikation für das Netz | Basis |
| NET.1.1.A4 | Netztrennung in Zonen | Basis |
| NET.1.1.A5 | Client-Server-Segmentierung | Basis |
| NET.1.1.A6 | Endgeräte-Segmentierung im internen Netz | Basis |
| NET.1.1.A7 | Absicherung von schützenswerten Informationen | Basis |
| NET.1.1.A8 | Grundlegende Absicherung des Internetzugangs | Basis |
| NET.1.1.A9 | Grundlegende Absicherung der Kommunikation mit nicht vertrauenswürdigen Netzen | Basis |
| NET.1.1.A10 | DMZ-Segmentierung für Zugriffe aus dem Internet | Basis |
| NET.1.1.A11 | Absicherung eingehender Kommunikation vom Internet in das interne Netz | Basis |
| NET.1.1.A12 | Absicherung ausgehender interner Kommunikation zum Internet | Basis |
| NET.1.1.A13 | Netzplanung | Basis |
| NET.1.1.A14 | Umsetzung der Netzplanung | Basis |
| NET.1.1.A15 | Regelmäßiger Soll-Ist-Vergleich | Basis |
| NET.1.1.A16 | Spezifikation der Netzarchitektur | Standard |
| NET.1.1.A17 | Spezifikation des Netzdesigns | Standard |
| NET.1.1.A18 | P-A-P-Struktur für die Internet-Anbindung | Standard |
| NET.1.1.A19 | Separierung der Infrastrukturdienste | Standard |
| NET.1.1.A20 | Zuweisung dedizierter Subnetze für IPv4/IPv6-Endgerätegruppen | Standard |
| NET.1.1.A21 | Separierung des Management-Bereichs | Standard |
| NET.1.1.A22 | Spezifikation des Segmentierungskonzepts | Standard |
| NET.1.1.A23 | Trennung von Netzsegmenten | Standard |
| NET.1.1.A24 | Sichere logische Trennung mittels VLAN | Standard |
| NET.1.1.A25 | Fein- und Umsetzungsplanung von Netzarchitektur und -design | Standard |
| NET.1.1.A26 | Spezifikation von Betriebsprozessen für das Netz | Standard |
| NET.1.1.A27 | Einbindung der Netzarchitektur in die Notfallplanung | Standard |
| NET.1.1.A28 | Hochverfügbare Netz- und Sicherheitskomponenten | Hoch |
| NET.1.1.A29 | Hochverfügbare Realisierung von Netzanbindungen | Hoch |
| NET.1.1.A30 | Schutz vor Distributed-Denial-of-Service | Hoch |
| NET.1.1.A31 | Physische Trennung von Netzsegmenten | Hoch |
| NET.1.1.A32 | Physische Trennung von Management-Netzsegmenten | Hoch |
| NET.1.1.A33 | Mikrosegmentierung des Netzes | Hoch |
| NET.1.1.A34 | Einsatz kryptografischer Verfahren auf Netzebene | Hoch |
| NET.1.1.A35 | Einsatz von netzbasiertem DLP | Hoch |
| NET.1.1.A36 | Trennung mittels VLAN bei sehr hohem Schutzbedarf | Hoch |

### CON.1 Kryptokonzept

The cryptographic concept (TLS settings, certificates, disk encryption) is the operator's. The product offers TLS 1.2+ with Go's default cipher suites and uses no other cryptography of its own beyond what PostgreSQL, Go and FFmpeg provide.

13 active requirements, 7 ENTFALLEN (omitted).

| ID | Title (Kompendium) | Level |
|---|---|---|
| CON.1.A1 | Auswahl geeigneter kryptografischer Verfahren | Basis |
| CON.1.A2 | Datensicherung beim Einsatz kryptografischer Verfahren | Basis |
| CON.1.A4 | Geeignetes Schlüsselmanagement | Basis |
| CON.1.A5 | Sicheres Löschen und Vernichten von kryptografischen Schlüsseln | Standard |
| CON.1.A9 | Festlegung von Kriterien für die Auswahl von Hard- oder Software mit kryptografischen Funktionen | Standard |
| CON.1.A10 | Erstellung eines Kryptokonzepts | Standard |
| CON.1.A11 | Test von Hardware mit kryptografischen Funktionen | Hoch |
| CON.1.A15 | Reaktion auf praktische Schwächung eines Kryptoverfahrens | Standard |
| CON.1.A16 | Physische Absicherung von Hardware mit kryptografischen Funktionen | Hoch |
| CON.1.A17 | Abstrahlsicherheit | Hoch |
| CON.1.A18 | Kryptografische Ersatzhardware | Hoch |
| CON.1.A19 | Erstellung eines Krypto-Katasters | Standard |
| CON.1.A20 | Manipulationserkennung für Hard- oder Software mit kryptografischen Funktionen | Hoch |

## Not covered

The following were not mapped: Bausteine for the operating system of the host, the premises (INF), the organization (ISMS.1, ORP.1-ORP.3, ORP.5), incident handling (DER.*), and CON.2 Datenschutz. If the media or the audit log (client IPs, filenames) contain personal data, CON.2 and the applicable data protection law must be assessed by the operator.

## Counts


Product-side status counts over the 187 rows in the detailed Bausteine: Erfüllt (Produkt) 35, Nicht umgesetzt 19, Operator 71, Teilweise 46, Nicht anwendbar 16.

## How this document was produced and checked

- Requirement IDs, titles and levels were extracted programmatically from the text of `IT_Grundschutz_Kompendium_Edition2023.pdf` (BSI) and spot-checked against the PDF; none were typed by hand. Requirements whose heading could not be parsed would have been listed as "NOT VERIFIED - fetch from Kompendium edition 2023"; the extraction parsed all headings for the listed Bausteine.
- The "how implemented" column was written from the code, configuration and tests in this repository and checked against a running rootless podman stack (see [operations.md](operations.md), [api.md](api.md)); it is a judgment, not a BSI-endorsed assessment.
- Related: [security.md](security.md), [operations.md](operations.md), [risk-acceptance-template.md](risk-acceptance-template.md), [licensing.md](licensing.md).
