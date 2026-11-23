# Mappa del fork di Codex per la TUI di Ringil

> 23/11: il fork NON si fa (plan-core 29f), la TUI si scrive in Go. Questa mappa resta come riferimento: cosa fa la TUI di Codex, quali scelte di UX copiare, cosa non rifare (telemetria, uscite di rete).

Data: 2026-11-23. Base: openai/codex `d4014cb` (21/11), Apache-2.0, congelato (plan-core 29e).
Fonte: lettura statica del sorgente (tre analisi indipendenti); niente build. Percorsi relativi a `codex-rs/`.

## Cosa abbiamo davanti

- `tui/src`: 1291 file, ~540k righe (~330k senza i file di test).
- La TUI **non è un client puro**: dipende da `codex-app-server-client`, che linka `codex-app-server` e
  `codex-core` (~294k righe, 74 crate codex). Il default è il motore Rust in-process (`AppServerTarget::Embedded`,
  `tui/src/lib.rs:346`); esistono anche `LocalDaemon`, `Cloud` e `Remote` (`--remote ws://|unix://`).
- La TUI legge e scrive da sola `~/.codex/config.toml` tramite `codex_core::config` (`legacy_core::`, 138 usi in 74 file),
  la storia dei messaggi e i file delle sessioni su disco.
- Il client remoto ha solo WebSocket e socket unix: **stdio non c'è**.

## Decisioni proposte

1. **Trasporto: socket unix per tutti e due i casi.** La TUI avvia il motore Go con un socket in una cartella privata
   (0700) e ci si collega; se `serve` gira già, si collega al suo socket. Un solo trasporto, niente stdio da scrivere
   in Rust. WebSocket resta per l'uso remoto.
2. **Il fork procede in tre fasi**, ognuna con la TUI funzionante alla fine:
   - **A — client pulito (~25 file + i moduli cancellati).** Solo `Remote`: via Embedded, LocalDaemon, Cloud,
     `codex-app-server-daemon`. Via tutte le uscite di rete (tabella sotto). La TUI parla già col nostro motore.
   - **B — fuori `codex-core` (~100 file).** Un tipo di configurazione nostro lato client al posto di
     `legacy_core::config::Config`; la configurazione si legge e scrive dal motore (`config/read`, `config/value/write`);
     sessioni da `thread/list|read|turns/list|items/list` invece dei file rollout. Poi si potano `codex-protocol`,
     `codex-config`, `codex-rollout` e `app-server-protocol` (dipendono da http-client, network-proxy, otel, sqlx):
     finché restano, `reqwest` resta linkato.
   - **C — prodotto Ringil.** Marchio, `~/.ringil`, slash command nostri, un comando per skill, schermate OpenAI tolte.
3. **Stima rivista**: la fase B da sola è più dei 2–4 giorni scritti il 22/11 per tutto il fork. Resta comunque meno
   che scrivere da zero una TUI di questa qualità (rendering, diff, approvazioni, pager, keymap, test snapshot).

## Uscite di rete e telemetria da togliere (fase A)

| Cosa | Dove | Quando | Come |
|---|---|---|---|
| Metriche Statsig/OTel verso `ab.chatgpt.com/otlp` con chiave fissa | `startup_orchestration.rs:697-745`, `otel/src/config.rs:11` | avvio, solo build release | via `codex-otel`; stub di `SessionTelemetry` |
| Controllo aggiornamenti: GitHub releases, Homebrew, npm; l'aggiornamento esegue `curl chatgpt.com/codex/install.sh \| sh` | `updates*.rs`, `update_*.rs`, `npm_registry.rs` | avvio, ogni 20 h | cancellare i moduli |
| Annuncio da `raw.githubusercontent.com/openai/codex/main/announcement_tip.toml` | `tooltips.rs:17,181` | sempre all'avvio | cancellare |
| Sprite dei pet da `persistent.oaistatic.com` | `pets/asset_pack.rs` | se configurati | cancellare `pets/` |
| Statistiche d'uso da `chatgpt.com/backend-api/wham` col token | `analytics/client.rs` | `/usage` | cancellare, via `codex-backend-client` |
| Configurazione gestita da `chatgpt.com/backend-api` (+ login OAuth enterprise) | `startup_orchestration.rs:317` | avvio e ricarica | via `codex-cloud-config` |
| Voce realtime (SDP verso OpenAI, helper WebRTC) | `realtime_call.rs`, `chatwidget/realtime*` | comando voce | cancellare, via `codex-api`, `codex-realtime-webrtc` |
| Sentry con DSN fisso | `feedback/src/lib.rs:62` | linkato | via `codex-feedback` |
| Analytics eventi verso `chatgpt.com/backend-api/codex/analytics-events` | crate `codex-analytics` via `codex-core-plugins` | linkato | inline delle costanti, via il crate |
| Probe LM Studio/Ollama, `ollama pull` | `oss_selection.rs` | `--oss` | cancellare |
| Apertura browser: link cliccati, help openai.com, login | `history_ui.rs:303`, `pager_overlay/transcript.rs:559`, onboarding | su azione | solo URL dati dal motore (login dei nostri provider) e link cliccati dall'utente |
| Visualizzazioni HTML che caricano da CDN | `inline_visualization/viewer.rs` | all'apertura | asset inclusi, o via |

Resta in ascolto solo un server MCP interno su `127.0.0.1:<porta>` con token (`dynamic_tools_mcp.rs`), che dà al
motore i tool per creare/forkare thread e worktree: si tiene solo se il nostro motore lo usa, altrimenti via con
`rmcp` e `axum`.

Crate da togliere da `tui/Cargo.toml`: otel, feedback, login, backend-client, cloud-config, realtime-webrtc, api,
http-client, models-manager, connectors, model-provider, model-provider-info, core-plugins, utils-oss,
install-context, app-server-daemon, exec-server, app-server-client (resta app-server-remote-client), webbrowser
(o solo dietro controllo dell'URL).

## Test di rete zero (CI)

1. **Albero delle dipendenze** (offline): `cargo tree -p <tui> -e normal --target all` fallisce se compaiono reqwest,
   hyper-rustls, sentry, opentelemetry-otlp/http, oauth2, tiny_http, tonic, curl, hickory o i crate codex sopra.
   Ammessi: tokio-tungstenite (solo dal client remoto), axum/hyper solo se resta l'MCP interno. Più un controllo
   grep su `https?://` in `tui/src` fuori da testi d'aiuto e test.
2. **A runtime** (Linux, build release perché Statsig è spento in debug): `unshare -rn` + `strace -f -e
   trace=socket,connect,bind,sendto,sendmsg`, TUI in un pty contro un motore finto su socket unix, sessione scritta
   (avvio, prompt, `/usage`, voce, uscita). Fallisce su ogni socket AF_INET/AF_INET6 (salvo il bind MCP se resta),
   DNS, o socket unix diverso da quello del motore.
3. **Vulnerabilità**: `cargo-audit` e `cargo-deny` in CI sulle dipendenze rimaste (il repo ha già un `deny.toml`).

## Protocollo che il motore Go deve parlare

Tipi generati dal JSON Schema di `app-server-protocol`. Uso reale della TUI:

| | Necessari | Solo OpenAI | Facoltativi |
|---|---|---|---|
| Richieste client→server (105) | 30 | 35 | 40 |
| Notifiche server→client (89) | 29 | 20 | 40 |
| Richieste server→client (10) | 5 | 2 | 3 |

- **Necessari**: `thread/start|resume|fork|list|read|loaded/list|turns/list|items/list|archive|unarchive|delete|name/set|compact/start|unsubscribe|settings/update|metadata/update|inject_items`, `turn/start|interrupt|steer`, `model/list`, `config/read|value/write|batchWrite`, `skills/list`, `mcpServerStatus/list`, `permissionProfile/list`, `collaborationMode/list`, `thread/memoryMode/set`, `memory/reset`; notifiche `item/*` (delta di messaggio, ragionamento, piano, output dei comandi, file, MCP), `turn/*`, `thread/*`, `serverRequest/resolved`, `error`, `warning`; approvazioni comando, file e permessi, `requestUserInput`, elicitation MCP.
- **Avvio**: `initialize` → `account/read` (rispondiamo «nessun login richiesto»: salta la schermata OpenAI) →
  `model/list` (non vuoto, o la TUI si ferma), `configRequirements/read` (errore bloccante se manca),
  `collaborationMode/list`, `hooks/list` → `thread/start`. Da verificare: se la TUI regge «metodo inesistente» sui
  metodi solo OpenAI chiamati all'avvio; nella fase A si tolgono comunque quelle chiamate.
- **Fuori dal protocollo**: la storia dei messaggi del compositore (freccia su) si legge da un file locale; o resta
  locale in `~/.ringil`, o si aggiunge un metodo nostro.

## Slash command (fase C)

Codex ne ha 69 (`tui/src/slash_command.rs:12`); le skill si chiamano con `$` e non come comandi.

- **Restano**: clear, compact, resume, model, mcp, agents, skills, config, init.
- **Rinominati**: permissions → /approvals, memories → /memory, usage → /cost (rifatto su `thread/tokenUsage`).
- **Da aggiungere**: /help, /sessions, /profile, /notes, /tasks, /secrets, /rc, e un comando per skill
  (nuova variante `SlashCommandItem::Skill` alimentata da `skills/list`).
- **Via**: daybreak, app, apps, plugins, logout, feedback, voice, import, elevate-sandbox, experimental, daemon, pets,
  rollout, test-approval, debug-*.
- **Da decidere uno per uno** (utili, nessun costo di rete): new, fork, rename, archive, delete, diff, review, plan,
  goal, side, copy, export, keymap, vim, theme, statusline, ps, stop, cd, pwd, worktree, title.

## Marchio e percorsi (fase C)

«Codex» in ~150 righe di 74 file, «OpenAI» ~31, «ChatGPT» ~37, ~25 URL fissi di openai/chatgpt/github; 368 snapshot
dei test da rigenerare. `CODEX_HOME`/`~/.codex` (`utils/home-dir/src/lib.rs:13`) → `RINGIL_HOME`/`~/.ringil`.
`clientInfo` dell'handshake da `codex-tui` a `ringil-tui`. Schermate da togliere: onboarding/login (auth, gateway,
bedrock, gcp), limiti e piano (`rate_limits.rs`, `usage_notice.rs`, `subscription.rs`, `status/card.rs`),
banner e inviti all'upgrade (`backend_banners.rs`, `model_migration.rs`, `daybreak.rs`).

## Licenza

Restano LICENSE Apache-2.0 e NOTICE di Codex; ogni file modificato porta la nota di modifica richiesta dalla
licenza (§4b). Il nostro codice nuovo nel repo della TUI può avere la nostra licenza nei file nostri.

## Scelte di default della TUI (opinionata) — da decidere con Antonio

Proposta: memoria SQLite locale, sandbox acceso, provider e chiave chiesti all'onboarding (unica domanda), Composio
se c'è la chiave, pacchetti di skill da OpenSwarm installati, A2A spento finché non c'è un peer da usare.
