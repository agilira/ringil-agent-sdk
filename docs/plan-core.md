# ringil-agent-sdk — piano del nucleo

Data: 2026-11-17. **Stato: proposta, da approvare.** Base: `~/Desktop/studio` (0.52, ~40k LOC)
come guida di *cosa* deve fare, non di *come*. Nessuna riga di studio entra così com'è.

Questo documento fissa il nucleo dell'SDK: **provider, ciclo dell'agente, API dei tool,
sessioni**, più il canale di eventi che li tiene insieme. Tutto il resto (MCP, tool nativi,
segreti, memoria, delega, workflow, cron, controllo HTTP, A2A) si appoggia su questi
contratti e arriva dopo (§9).

## 1. Cosa è

Un SDK Go per costruire agenti: un modello, dei tool, un ciclo che li alterna, una
conversazione che sopravvive al processo. Si comporta come un agente normale (Claude
Code, Codex, LangChain): niente attriti inutili. È **utilizzabile da solo**, senza i
nostri modelli, senza Harness, senza la TUI: chi prova il resto lo fa perché l'SDK gli
piace, non perché deve.

La differenza che vendiamo: è scritto da vulnerability researcher. Ogni input non fidato
(risposta del modello, argomenti dei tool, file di sessione) ha un limite e un fuzz test;
i default sono sicuri; nessun contenuto finisce nei log.

## 2. Principi invariabili

Battono ogni singola funzione. Se una funzione li viola, si toglie la funzione.

1. **Niente muri.** I limiti sono default con un'opzione, non costanti. Una conversazione
   non muore mai perché è lunga: si compatta, si arrotola, continua.
2. **Sicurezza = default, non divieto.** L'SDK non è opinionato: le opinioni stanno
   nelle app (Harness, TUI). Validazione, cap e sandbox sono accesi di default e l'host
   può cambiarli o spegnerli tutti; il **modello** non può mai. Fisse restano solo le
   regole di correttezza del protocollo (accoppiamento call/risultato, ID unici) e il
   divieto di rieseguire da soli un'azione dall'esito incerto, che nessun framework fa.
3. **Il nucleo non conosce i fornitori.** I tipi del motore sono nostri; nessun wire type
   OpenAI/Anthropic attraversa il confine dell'adapter.
4. **Nessun replay.** Un'azione dall'esito incerto non viene mai rieseguita in automatico.
5. **Dipendenze:** nucleo = stdlib + `golang.org/x/*` + `go-errors`. Gli adapter con
   dipendenze pesanti stanno in moduli separati: le loro dipendenze non raggiungono chi
   non li usa.
6. **Tre OS di prima classe:** Linux, macOS, Windows (Harness è obbligatorio su Windows).
   CI su tutti e tre dal primo commit.
7. **Qualità:** GOCYCLO < 10 (test inclusi), nessun errore ignorato, nessun panic, TDD
   3:1, race/vet/gofmt verdi. Ogni difetto trovato in studio diventa un test scritto
   **prima** del codice.
8. **Mai nei log:** prompt, argomenti, risultati dei tool, credenziali, corpi di errore
   upstream.

### Niente tetti di prodotto

L'SDK non mette tetti a giri, chiamate, prompt, output o lunghezza delle conversazioni:
gli altri SDK non lo fanno e chi li vuole li mette nell'app (`MaxTurns`, budget, hook).
Restano solo limiti **interni e invisibili** sul parsing di byte non fidati (una riga SSE
da 1 GB non deve esaurire la RAM), dimensionati ben sopra qualunque uso reale: sono
igiene della memoria, non funzioni del prodotto.

## 3. Struttura

```text
github.com/agilira/ringil-agent-sdk        package agent — tipi, Provider, Tool, Agent, Session, Event
  provider/openai        OpenAI-compatible (vLLM/Eos, Ollama, OpenRouter, Groq, DeepSeek, free tier)
  provider/anthropic     Messages API nativa (v0.2: è il test che l'astrazione regge)
  provider/fake          provider scriptato per i test, pubblico (serve anche a chi usa l'SDK)
  session/file           backend JSONL su disco
  session/mem            backend in memoria
  internal/schema        generazione schema JSON dai tipi Go
  internal/jsonl         lettura/scrittura append-only con catena di hash
```

Il package si chiama `agent`: si legge `agent.New`, `agent.NewTool`, `agent.Run`.
Go 1.27 (le 1.25 e precedenti hanno vulnerabilità note nella stdlib). Modulo sotto `agilira/` finché è privato;
all'apertura si trasferisce sotto l'organizzazione Ringil. Licenza MPL-2.0, come il resto
dello stack.

## 4. Modello dati

I tipi stanno nel nucleo, pensati contro **due** wire diversi fin dal primo giorno
(fixture OpenAI e Anthropic nei test), così il secondo adapter non rompe l'API.

```go
type Role string // "user", "assistant", "tool"

type Message struct {
    Role    Role
    Parts   []Part     // v0.1: solo TextPart; ImagePart riservata, così l'API non cambia dopo
    Calls   []ToolCall // solo assistant
    Result  *ToolResult // solo tool
}

type ToolCall   struct { ID, Name string; Input json.RawMessage }
type ToolResult struct { CallID string; Content string; IsError bool }

type Usage struct { Input, Output, CacheRead, CacheWrite int }
type StopReason string // "end", "tool_use", "max_tokens", "max_turns", "cancelled"
```

Differenze da studio: niente `Type: "function"` e `Arguments string` (forma OpenAI) nel
nucleo; il system prompt **non** è un messaggio ma un campo della richiesta (Anthropic lo
vuole separato, e così la memoria non finisce in cronologia).

## 5. Provider

### 5.1 Contratto

```go
type Provider interface {
    Generate(ctx context.Context, req *Request, sink TextSink) (*Response, error)
    Info() ModelInfo // nome, finestra di contesto, capacità (tool, immagini, cache)
}

type Request struct {
    System    string
    Messages  []Message
    Tools     []ToolSpec
    MaxTokens int
    Options   map[string]any // temperatura ecc.; l'adapter prende ciò che capisce
}

type TextSink func(delta string) error // nil = niente streaming
```

Un solo metodo. Lo streaming è esplicito (studio lo nascondeva nel `context`); il testo
esce man mano, le chiamate ai tool vengono **assemblate e validate dall'adapter** prima di
tornare: il ciclo non vede mai una tool call a metà.

### 5.2 Errori classificati

L'adapter traduce ogni errore in un codice `go-errors`; il corpo upstream non esce mai.

| Codice | Significato | Chi reagisce |
|---|---|---|
| `PROVIDER_AUTH` | chiave assente o rifiutata | host |
| `PROVIDER_RATE` | 429, con `RetryAfter` | decoratore retry |
| `PROVIDER_UNAVAILABLE` | 5xx, rete prima della risposta | decoratore retry |
| `PROVIDER_CONTEXT` | input troppo lungo | ciclo → compattazione (§7.4) |
| `PROVIDER_INPUT` | richiesta rifiutata | host |
| `PROVIDER_INTERRUPTED` | stream troncato a metà | ciclo (§5.4) |

### 5.3 Decoratori

`Budget`, `Admission` (rate limiter condiviso), `CircuitBreaker`, `Retry` restano quelli di
studio, ma come decoratori `func(Provider) Provider` in un sotto-package, composti
dall'host. La compattazione **esce** dai decoratori (studio: `CompactingProvider`) e va nel
ciclo, perché per compattare il turno attivo serve sapere cos'è un turno.

### 5.4 Retry

Una chiamata al modello non ha effetti esterni: ripeterla è sicuro, costa solo token.
Default: `Retry` riprova `RATE`/`UNAVAILABLE` fino a 3 volte con backoff e `Retry-After`.
Su `INTERRUPTED` riprova solo se nessun testo è già uscito verso il sink; altrimenti emette
l'evento `ModelRestarted` e riprova, così la UI sa di dover ripulire. Configurabile.

### 5.5 Adapter `openai`

Da studio si tiene il lavoro di sicurezza, come default: HTTPS salvo loopback (con
`AllowHTTP` per un vLLM in LAN o su un pod), niente redirect, niente credenziali/query nell'URL, corpo limitato, parser SSE con cap.
Si aggiunge: chiave da `KeyFunc` (aggancio per `secrets.Resolver`, keyring, Vault),
`MaxTokens` senza tetto fisso a 32k, `Info()` dalla config. Fuzz: parser SSE, risposta
JSON, accumulo degli argomenti delle tool call.

## 6. API dei tool

### 6.1 Due livelli

**Alto livello** — quello che usa il 95% della gente:

```go
type MeteoIn struct {
    Citta string `json:"citta" desc:"Nome della città"`
    Unita string `json:"unita,omitempty" enum:"c,f" desc:"Scala, default c"`
}

meteo := agent.NewTool("meteo", "Temperatura attuale di una città",
    func(ctx context.Context, in MeteoIn) (string, error) { ... })
```

- Schema JSON generato dal tipo `In` alla costruzione: `json`, `desc`, `enum`, `min`,
  `max`, `maxLength`; campo senza `omitempty` e non puntatore = obbligatorio.
- Tipi ammessi: scalari, slice, struct annidate, puntatori, `time.Time`. Interfacce,
  canali, funzioni, mappe non stringa = errore.
- `Out`: `string` passa così com'è, qualunque altro tipo va in JSON.
- **Niente panic**: `NewTool` ritorna sempre un `Tool`; uno schema non valido diventa
  errore alla registrazione (`NewRegistry`). Una riga per tool, nessun `Must`.

**Basso livello** — per MCP, tool dinamici, chi vuole lo schema a mano:

```go
type Tool interface {
    Spec() ToolSpec // Name, Description, InputSchema, Annotations
    Call(ctx context.Context, input json.RawMessage) (ToolOutput, error)
}
```

### 6.2 Decodifica stretta

Prima della funzione dell'utente, sempre: campi sconosciuti rifiutati, tipi sbagliati
rifiutati, obbligatori presenti, enum e limiti dello schema rispettati. Il
messaggio al modello nomina **solo i campi** dello schema, mai i valori inviati, così il
modello si corregge senza che rifletta input ostile. Fuzz sulla decodifica.

### 6.3 Errori verso il modello

In studio un unico `switch` conosceva i codici di tutti i tool (`executionFailure`): ogni
tool nuovo toccava il motore. Si toglie. Regola nuova:

- `agent.ToolError(code, testo)` → il modello riceve `testo [code]`: è il tool a decidere
  cosa è sicuro dire;
- qualunque altro `error` → il modello riceve `Tool failed [CODE]` (o generico), l'errore
  vero va solo all'host via evento.

### 6.4 Annotazioni e approvazione

`Annotations{ReadOnly, Destructive, OpenWorld}` sullo spec. Il ciclo chiama
`Hooks.Approve(ctx, call, spec)` prima di eseguire; default = consenti (l'SDK è un agente
normale). Harness ci attacca la sua UI di conferma; la TUI la sua.

### 6.5 Registry

Immutabile come in studio: `NewRegistry`, `With`, `Select` (i worker ricevono sottoinsiemi,
mai di più). Nomi `^[a-zA-Z0-9_-]{1,64}$`, duplicati rifiutati. Chi scarica il binario e non
compila usa MCP (deciso il 16/11): stesso `Tool`, nessun secondo sistema.

## 7. Ciclo dell'agente

### 7.1 API

```go
a, err := agent.New(agent.Config{
    Provider: p,
    Tools:    reg,
    System:   "",          // vuoto = prompt base; testo = lo sostituisce
    Hooks:    agent.Hooks{Approve: ..., Context: ...},
})

// Semplice: aspetta la fine.
res, err := a.Run(ctx, sess, agent.Text("riassumi la posta di oggi"))

// Con eventi: iteratore, il consumatore detta il ritmo.
for ev, err := range a.Stream(ctx, sess, agent.Text("...")) { ... }
```

`Stream` ritorna `iter.Seq2[Event, error]`: niente goroutine da far morire, niente buffer,
niente politica di scarto; `break` = cancella. È il canale di eventi pubblico. Chi deve
distribuirlo a più ascoltatori (SSE di Harness) lo fa sopra, nel suo bus. `Run` è `Stream`
consumato fino in fondo.

`sess` può essere `nil`: conversazione usa e getta.

### 7.2 Un giro

```text
1. vista   = System + contesto host (memoria, regole) + cronologia, compattata se serve
2. risposta = Provider.Generate(vista, sink → eventi TextDelta)
3. valida  le tool call: ID unici, nomi validi, input JSON, accoppiamento con la cronologia
4. salva   l'intento (checkpoint) PRIMA di qualunque effetto
5. per ogni call: Approve → loop detector → esegui (ReadOnly in parallelo, il resto in fila)
6. salva   i risultati, poi giro successivo
fine: nessuna tool call, MaxTurns, budget, ctx cancellato
```

### 7.3 Cosa cambia da studio

| Studio | SDK |
|---|---|
| `MaxSteps` 1..100 obbligatorio | `MaxTurns` opzionale, default illimitato (il freno vero è il budget, se l'host lo mette); al limite si **ferma pulito** (`StopReason=max_turns`), la sessione resta continuabile |
| tetti fissi ovunque (8 call per giro, 64 KiB di prompt, 1..8 worker...) | **tolti**: come negli altri SDK, l'unico limite è la finestra di contesto, gestita dalla compattazione |
| prompt base fisso nel codice | default sostituibile (`System`, o file letto da Harness) |
| memorie salvate come messaggi `user` in cronologia, duplicate a ogni giro | la memoria entra solo nella **vista** (hook `Context`), mai nella sessione |
| memoria + prompt = due `user` di fila | sparisce col punto sopra; l'adapter normalizza comunque l'alternanza |
| output tool > 64 KiB = errore | nessun tetto: un output enorme lo gestisce la compattazione (§7.4) |
| tool sempre in serie | ReadOnly in parallelo (ordine dei risultati conservato; concorrenza regolabile) |
| loop detector: blocca la stessa call ripetuta | default **avvisa** il modello, blocco opzionale; riconosce anche argomenti alterni e cicli di 3 (difetto del 15/11) |
| due sequenze di eventi | una sola, numerata dal ciclo |

### 7.4 Compattazione

Opzionale, nel ciclo, tocca **solo la vista**: la sessione conserva gli originali.

1. **Turni conclusi** → riassunto chiesto al provider, marcato non fidato, con sha256
   della sorgente (come studio).
2. **Turno attivo** (mancava in studio: un compito lungo moriva con `BUDGET_CONTEXT`) →
   i risultati di tool più vecchi diventano stub `[risultato omesso: N byte, sha256]`,
   gli ultimi K restano interi; il modello può rileggerli con un tool di storia.

Scatta su stima > soglia della finestra (`Info().ContextWindow`) **oppure** su
`PROVIDER_CONTEXT`, poi una sola nuova prova. Contratto `Compactor` sostituibile.

## 8. Sessioni

### 8.1 Formato

Studio riscriveva l'istantanea intera a ogni checkpoint (I/O quadratico) e moriva a
512 messaggi / 1 MiB. Si passa a un **log append-only**:

```text
sessione = JSONL, una voce per riga: {seq, prev_sha256, kind, data}
kind     = message | compaction | meta
```

- Append + fsync per checkpoint: costo costante, nessun tetto di lunghezza.
- Catena di hash: una riga alterata o tagliata si vede al caricamento. Ultima riga
  monca (crash durante la scrittura) = scartata, il resto vale.
- Campo `version` in testa; migrazioni testate (un aggiornamento di Harness non tocca i
  dati dell'utente).

### 8.2 Contratto

```go
type Store interface {
    Open(ctx context.Context, id string) (Session, error) // crea se assente, prende il lease
    List(ctx context.Context) ([]SessionInfo, error)
    Delete(ctx context.Context, id string) error
}

type Session interface {
    Entries() iter.Seq2[Entry, error]
    Append(ctx context.Context, entries ...Entry) error
    Close() error // rilascia il lease
}
```

Niente `scope`: multi-utente = un'istanza per utente (deciso il 16/11), quindi lo spazio
dei nomi è la directory dello store. Un run per sessione alla volta: lease con `flock` su
Unix, `LockFileEx` su Windows. Più avanti `Fork(seq)` per ramificare una conversazione.

### 8.3 Ripresa dopo un'interruzione

Studio rifiutava la sessione (`SESSION_INCOMPLETE`): conversazione persa. Default nuovo:
alle call senza risultato il ciclo aggiunge un risultato sintetico *«interrotto: esito
sconosciuto, l'azione può essere avvenuta o no; verifica prima di ripeterla»* e continua.
**La call non viene mai rieseguita** (principio 4). `StrictResume: true` riporta il
comportamento di studio per chi lo vuole.

### 8.4 Eventi

Tipati, una sequenza unica per run:

```text
RunStarted · TextDelta · ModelCompleted(usage) · ModelRestarted
ToolStarted(name, input) · ToolFinished(output, isError, durata) · ToolDenied
Compacted · Checkpointed · RunFinished(stopReason, usage totale)
```

Gli eventi che riceve chi chiama `Stream` **contengono** testo e argomenti: sono dati suoi
e la UI ne ha bisogno. L'adapter `slog` invece registra solo metadati (principio 8).

## 9. Fasi

Ogni fase finisce con release, changelog, CI verde sui tre OS.

| Fase | Contenuto | Fatto quando |
|---|---|---|
| **N1** | modello dati, `Provider`, `provider/fake`, `provider/openai`, decoratori | chat in streaming contro Ollama; fuzz SSE 10 min pulito |
| **N2** | `NewTool`, schema, decodifica stretta, registry, `ToolError` | fuzz decodifica pulito; nessun valore dell'input nei messaggi d'errore (test) |
| **N3** | ciclo, `Run`/`Stream`, hook, loop detector, parallelo ReadOnly | i difetti di §7.3 coperti da test scritti prima |
| **N4** | sessioni JSONL, lease, ripresa, `session/mem` | `kill -9` a metà tool → ripresa senza replay; 10k messaggi senza rallentare |
| **N5** | compattazione conclusi + turno attivo | compito con 200 tool call lunghe arriva in fondo |
| N6 | `provider/anthropic` | stessi test di N1–N5 verdi senza toccare il nucleo |

**Condizione di nucleo fatto:** un esempio di ~40 righe (provider, un tool custom,
sessione su file) che gira uguale su Linux, macOS e Windows, con modello locale o chiave
di un free tier.

### Todo e workflow scritti dall'agente

- **Todo (in N3/N4).** Un tool standard `todo` con cui l'agente scrive e aggiorna la sua
  lista (voce, stato: da fare / in corso / fatto). La lista vive nella sessione come voce
  `meta`, quindi sopravvive alla ripresa, ed esce come evento `TodoUpdated` perché la UI la
  mostri. Opt-in come ogni tool.
- **Workflow (subito dopo la delega).** Un tool `workflow` con cui il modello emette un
  `WorkflowPlan` (passi con worker, compito, dipendenze). Il piano viene validato ed
  eseguito dallo **stesso motore** dei workflow statici (DAG + journal di studio), non da un
  secondo sistema. L'host può chiedere conferma prima dell'avvio con l'hook `Approve`.

Dopo il nucleo, nell'ordine già concordato (il provider SystemOne per Eos è un adapter come gli altri, fuori dal nucleo): segreti (keyring, Vault, env file — il
`# commento` inline va tolto dal valore), MCP, tool nativi (bash/git/fs con le note
delle review del 16-17/11), memoria con `VectorStore` (Chroma primo adapter), delega e
workflow, scheduler, controllo HTTP, A2A.

## 10. Decisioni prese

1. Modulo `github.com/agilira/ringil-agent-sdk`, trasferito sotto Ringil all'apertura (17/11).
2. Licenza MPL-2.0 (17/11).

## 11. Dopo il nucleo: l'ordine del blueprint studio

Data: 2026-11-18. Studio è il blueprint (`~/Desktop/studio`, README e ROADMAP); qui si
segue il suo ordine di costruzione, non uno nuovo. Gli adapter (Anthropic, SystemOne,
Vault, keyring) si attaccano a interfacce esistenti e non entrano in sequenza.

**Già nell'SDK** (equivalenti, rifatti con gli audit): agente e provider sostituibile,
streaming (0.25), API dei tool, sessioni con ripresa (0.19 superata: nessun tetto),
budget di chiamate/token (0.11), compattazione con sintesi e stima (0.12, 0.18),
rate limiter e circuit breaker (0.26), CLI.

**Da costruire**, componenti di studio in ordine di dipendenza (prima ciò su cui gli altri
poggiano) — ogni pezzo collegato alla CLI e visto girare prima del successivo, CI verde
prima del commit dopo:

| # | Componente (versione studio) | Note dalle review da applicare |
|---|---|---|
| 1 | Segreti: `Resolver`, ambiente, env file privato, catena senza fallback silenziosi (0.28) — FATTO 18/11. Adapter Vault KV v2, file cifrato, keyring OS (da eos-legacy) dopo: si attaccano a `Resolver` | il `# commento` inline non entra nel valore; la chiave API passa da qui, mai flag |
| 2 | Sandbox condiviso (`sandbox`: bwrap, seccomp, prlimit) per script, MCP stdio e Bash — FATTO 18/11, audit corretto. Da aggiungere sulla stessa interfaccia: tetto totale con cgroup, filtro egress per la rete isolata, backend macOS (seatbelt) e Windows (AppContainer) | rete in netns propria; seccomp anche bpf/perf_event_open/userfaultfd |
| 3 | Tool filesystem in lettura con grant `--workspace` (primo tool di studio) — FATTO 18/11: read_file, list_directory, glob, grep; percorsi assoluti in più cartelle | os.Root, no symlink/traversal, nessun byte di file nei log |
| 4 | Memoria ricercabile `Memory` (scope dall'host) — FATTO 18/11: BM25 locale, richiamo nella vista, tool remember/recall/forget | richiamo nella vista, mai nella sessione; `VectorStore` + Chroma più avanti |
| 5 | Skills (nomi sempre esposti, corpi e script al bisogno, script solo nel sandbox) | FATTO 18/11: formato Agent Skills, hash per file, script su copia verificata nel sandbox |
| 6 | MCP stdio/HTTP + OAuth | FATTO 18/11: modulo mcp/ sull'SDK ufficiale; stdio nel sandbox; nessun replay; consenso solo da operatore; login OAuth con token privati |
| 7 | Delega ai worker (sottoinsiemi di tool, budget condiviso, contesto fresco) | FATTO 18/11: pacchetto delegate (tipi di worker, contesto pulito, rapporto unico, Prepare per scratchpad, Approve del padre), esecuzione parallela di tool ReadOnly/Parallel nel nucleo. `Spawner` per istanze separate più avanti |
| 8 | Workflow locali + journal durevole (+ workflow scritti dall'agente) | FATTO 18/11: workflow.Runner (passaggi pronti in parallelo, journal intent-before-run, ripresa senza replay, Resolve), FileJournal con lock, run_workflow per i piani del modello; delega in background e continue_task |
| 9 | Scratchpad e memoria di progetto Markdown (0.10) + todo dell'agente | FATTO 18/11: tool notes (semantica memory tool), scope scelti dall'host anche read-only, INDEX.md nel contesto via Config.Context; todo_write con lista nella conversazione |
| 10 | Ruolo e provider per profilo, regole di progetto, `AGENTS.md` automatico (0.13-0.15) | REGOLE FATTE 18/11: pacchetto rules (AGENTS.md + CLAUDE/GEMINI/Copilot/Cursor/Windsurf, dalla radice del repo, globale utente) via Config.Context; files.DefaultProtected per la scrittura (18). Ruolo/provider per profilo: con serve |
| 11 | Lettura selettiva della storia citata dalle sintesi (0.16) | FATTO 18/11: riassunti e stub numerati, read_history offerto dopo la compattazione, eseguito dal ciclo |
| 12 | Budget durevole per i workflow (0.17) | FATTO 18/11: spesa per passaggio (anche fallito) nel journal, Runner.Budget calls/token, ripresa che continua il conto |
| 13 | Artefatti, backup e ripristino in quarantena (0.20-0.24) | FATTO 18/11: artifact (scope, versioni immutabili, digest, tool con scope concessi), backup tar.gz con manifest, verify, restore solo in cartella nuova in quarantena |
| 14 | Rilevatore di loop (0.27) | default avviso; argomenti alterni e cicli di 3 |
| 15 | Bus di eventi e adapter slog solo metadati (0.29) | una sola sequenza |
| 16 | Controller, API HTTP loopback, `serve` (0.30-0.32) | TTL dei run conclusi, lock per job, `omitzero` |
| 17 | Composio, setup e catalogo con discovery (2.4, 2.4.1) | FATTO 18/11 (Connect, consumer key) + discovery nel nucleo (tool_search); modalità project con sessione REST: da fare con una project key |
| 18 | Tool filesystem in scrittura e Git strutturato (2.4.2) | grafts disattivati; output in streaming, non nel cap; fsync prima del ref |
| 19 | Bash sandbox/host (2.6) | host mode senza fuga di env da /proc/$PPID; timeout che uccide anche i figli setsid |
| 20 | Pianificazione (4.1): trigger a tempo e a evento, consegna, retry + notifica | DST con libreria matura; ogni difetto di studio diventa un test |
| 21 | A2A (5.1) | CLIENT FATTO 18/11: modulo a2a/ su a2a-go v2.8.0, agente remoto come tool, credenziali solo all'origine della card; server con serve (16) |
| 22 | Canali — API e Telegram FATTI 19/11 (pacchetto channel, Bridge, serve.Watch, `ringil serve --telegram-allow`), in uso da Antonio ogni giorno; poi WhatsApp, Slack, iMessage, WeChat come adapter | ogni chat = una conversazione di serve; approvazioni (bash Confirm, askDomain, serve.Approve) in chat con bottoni sì/no |
| 23 | Trigger + `create_skill` (decisione 19/11; nome «trigger» al posto di «probe», scelto il 19/11). PASSO 1 FATTO 19/11: pacchetto `trigger` (ex schedule, sostituito), tipi tempo/tool/file con controllo periodico (niente fsnotify: nessuna dipendenza, va su dischi di rete), Fire/Heard per messaggio e webhook, Timeout per run, serve.StartFrom/SourceOf, Allow vincolato alla run; `ringil trigger`. create_skill FATTO 19/11; create_trigger FATTO 20/11, solo nelle run di `/automate` (chat, solo proprietario in privato) e `ringil automate` (decisione di Antonio: niente trigger per sbaglio); tipi time/tool/file (message e webhook li crea l'utente). Lista/cancellazione (list_triggers, delete_trigger), periodi from/until e consegna in chat FATTI 20/11. check_later FATTO 20/11 (pacchetto `later`: controlli temporanei nella stessa conversazione, limiti 1m/24h/48 volte/3 per conversazione, [quiet] = niente messaggio; terminale e chat private). Webhook FATTO 20/11 (`serve --webhooks ADDR`, listener a parte, firma X-Hub-Signature-256 come GitHub, stesso corpo una volta al giorno, 503 finché il runner non è su) e ascolto chat FATTO 20/11 (solo proprietari e membri già noti, niente comandi, nessuna chiamata di rete per ascoltare). RIGA 23 CHIUSA | due tool: `create_trigger` e `create_skill`. Cinque tipi fissi e deterministici: tempo (lo scheduler attuale), tool (chiama un tool configurato, MCP/Composio/fetch/bash in sandbox, con argomenti fissi; copre URL e comandi), file (fsnotify), messaggio sul canale, webhook (endpoint di serve con segreto/firma). Ogni trigger emette `trigger_fired` e lancia una skill normale in markdown. Pre-autorizzati: le azioni pericolose si approvano una volta alla creazione; fuori scope = rifiuto + notifica, mai attese appese. Unico limite: run precedente in corso = salto. API Go per i power user (stessa struct del tool), trigger persistiti e riavviati da serve |
| 24 | Canali aziendali (decisione 19/11): ruoli proprietari / membri / pubblico, gruppi vs chat singole | proprietari: profilo completo, unici ad approvare; membri (impiegati) per ID, numero (contatto condiviso: FATTO 19/11, `--telegram-member-phones`, solo il proprio contatto in chat privata, revoca togliendo il numero) o iscrizione a un gruppo aziendale, profilo `team`; pubblico opzionale con profilo ristretto e tetti per utente; nei gruppi si risponde solo se chiamati (@bot, risposta, comando), una conversazione per gruppo, approvazioni solo da proprietari; memoria separata per chat (+ base aziendale in sola lettura) |
| 25 | Plugin in qualsiasi linguaggio (decisione 19/11; RIMANDATO a una versione successiva, decisione 20/11): hashicorp go-plugin | un solo proto gRPC per Channel, Provider, Transcriber, Synthesizer e poi Trigger; due trasporti: go-plugin (processo figlio locale) e gRPC in rete (mTLS obbligatorio); tool e agenti restano su MCP e A2A. Modulo separato (grpc, protobuf, yamux, hclog fuori dal core). Si fa DOPO probe e documentazione, a interfacce stabili: il proto le congela e si versiona |
| 26 | Istruzioni personalizzate (decisione 20/11): il prompt dell'azienda o dell'utente si accoda a quello base, non lo sostituisce; da file, per profilo | nella CLI solo l'accodamento (il base porta le regole di sicurezza); sostituirlo resta solo nell'API Go; tetto di dimensione; il file si rilegge a ogni run |
| 27 | FATTO 20/11 (manca il commit e la release di Argus v1.8.0): `ringil.yaml` unico (`--config`), profiles/mcp/agents.json e flag di serve cancellati; Argus esteso alla radice (Bind di mappe, liste, puntatori; Strict; Validate; YAML a un solo documento); ricarica a caldo di profili, istruzioni, impostazioni e persone delle chat (Bridge.SetPeople); il resto chiede riavvio e il log lo dice. Setup dichiarativo YAML con Argus (decisione 20/11): un solo `ringil.yaml` al posto di profiles.json, mcp.json, agents.json e dei flag di canali e trigger; versionabile; base dello spawning multi-utente (un YAML per utente) | i campi sono le struct Go dell'SDK, nessuna configurazione parallela; i vecchi file si cancellano (niente alternative); segreti mai nel file, solo il nome della variabile; hot reload solo dei campi sicuri (profili, ruoli, liste di consenso, istruzioni), gli altri chiedono riavvio; un file sbagliato non si applica, resta il precedente con l'errore nel log |
| 28 | FATTO 22/11 (Anthropic, Mem0, Chroma; segreti: secrets.Store + secrets/keyring con backend nostri, secrets/vault KV v2, secrets/encfile; ognuno con audit indipendente e prova dal vivo, keyring e Vault anche in CI). Seconda implementazione per ogni interfaccia (decisione 20/11), per provare le API: provider Anthropic nativo, segreti (Vault KV v2, keyring OS, file cifrato da eos-legacy), vector store Chroma, memoria Mem0 (poi la memoria di Antonio) | un'interfaccia che non regge la seconda implementazione si corregge prima della TUI; altri canali e browser RIMANDATI |
| 28b | FATTO 21/11 (audit indipendente e correzioni fatti, commit 21/11). SDK non opinionato e allineato agli agenti maturi (revisione del 21/11; Antonio: «è un SDK, non un'app: non deve essere opinionato»). Misure: cache dal 2º turno 97% letta (prima 0%, ~8× meno per turno); stub a scaglioni 15→≤5 cambi su 30 chiamate. In ordine di impatto: (1) system prompt stabile: memorie richiamate, Config.Context e discovery fuori dal system, in coda alla conversazione o come messaggio di sistema a metà; poi il pensiero di Claude si conserva tra i turni; (2) immagini precedenti tenute (togliere solo in compattazione, opzione); (3) tool_search senza riscrivere l'array dei tool (defer_loading nativo dove c'è); (4) messaggi con parti di ragionamento, ruolo system e risultati dei tool non solo testo (immagini MCP, screenshot); (5) Approve con decisione ricca (input modificato, messaggio, interruzione) + hook PreTool/PostTool; (6) nudge della risposta vuota fuori dal system e disattivabile; (7) provider OpenAI: max_completion_tokens, reasoning_effort, campi extra; (8) compattazione a scaglioni stabili, prompt del riassunto configurabile; (9) rilevatore di loop solo eventi di default, testi configurabili; (10) bash: tetto di timeout configurabile, comandi in background, descrizione sostituibile, un solo callback di permesso; (11) delegate: system prompt del worker sostituibile e config passata; (12) politiche dell'app nell'SDK: ringil.yaml in DefaultProtected, limiti fissi di rules, modello delle persone del canale dietro un'interfaccia | ogni punto: misura prima/dopo dove c'è un costo (cache, latenza), test, audit; niente alternative deprecate |
| 29 | DECISIONI 22/11 con Antonio. ORDINE (Antonio 22/11): onboarding FATTO 22/11 (con Composio); `ringil.sh`/`ringil.ps1` PER ULTIMI, quando roadmap e TUI sono finite e la distribuzione è decisa; prossimo: la TUI. (a) Onboarding: `ringil` in Go, guidato con Orpheus, solo le domande necessarie, in ordine: store dei segreti (keyring, vault, file), provider e modello, chiave API (salvata nello store, mai in chiaro), file del system prompt (registrato in ringil.yaml), memoria (sqlite, chroma, mem0), canale Telegram facoltativo (token nello store; owner preso dal primo /start al bot); scrive ringil.yaml e prova il modello dal vivo. Sostituisce la CLI attuale; `serve`, `secrets`, `sessions` restano suoi comandi. `ringil.sh` (e `ringil.ps1` per Windows) scarica dalla release, verifica il checksum, lancia l'onboarding. (b) TUI = fork di Codex (openai/codex, Apache-2.0): il suo crate tui ha zero import del motore e parla solo il protocollo JSON-RPC dell'app-server (WebSocket/unix/stdio); usa 107 richieste, 89 notifiche, 15 richieste dal server, molte di account/cloud OpenAI da spegnere. Strategia: NON cambiare il loro protocollo, implementarlo in `serve` (tipi generati dal loro JSON Schema); il fork cambia poco (marchio, flussi OpenAI spenti, i nostri slash command via metodi nostri aggiunti) così resta allineabile all'upstream. Due binari (TUI Rust + ringil Go), installati insieme da ringil.sh; la TUI avvia serve se non gira. Stima: adattatore Go 3–5 giorni, fork 2–4. Slash command di base: /help /clear /compact /resume /sessions /model /profile /memory /notes /mcp /agents /tasks /skills /approvals /secrets /config /cost /init /rc + una per skill (lista confermata 22/11; se ne aggiungono quando servono). TUI + installer (decisione 20/11): si distribuisce la TUI, la CLI resta per lo sviluppo; installer interattivo con Orpheus (segreti compresi) che scrive il `ringil.yaml` | la TUI risolve i limiti del prompt a righe: messaggi dei controlli mentre si scrive, approvazioni senza contesa con l'input. La TUI è un CLIENT di `serve` (decisione 20/11), non un agente in processo: una sola conversazione per tutti i fronti. `/rc` (passaggio di mano, decisione 20/11): dalla TUI si sceglie il canale come si sceglie un modello e la chat continua la stessa conversazione (storia, controlli aperti, approvazioni in sospeso); e al contrario dal telefono al PC; un fronte alla volta segue la conversazione (niente doppioni né notifiche ovunque). Le risposte tornano dove si è chiesto (già così) |
| 29b | Abbonamento ChatGPT (Antonio 22/11: prima della TUI, insieme a health e metrics). FATTO 22/11: `provider/chatgpt` (login browser PKCE e a codice, come Codex; Session con rinnovo a 5 minuti dalla scadenza sotto FileLock tra processi e rilettura dello store, perché OpenAI ruota il refresh token; revoca al logout; Responses API del backend ChatGPT in streaming, store:false, ragionamento cifrato rimandato; Models con client_version 99.0.0, sotto 1.0.0 il backend non elenca nulla); `internal/httpcall` estratto da provider/openai (errori HTTP, stall, eventi SSE), usage_limit_reached non ritentato; CLI `ringil login/logout chatgpt`, `provider: chatgpt`, voce nell'onboarding. Provato dal vivo 22/11 (accesso browser, modelli gpt-6-luna/gpt-5.6-terra/gpt-5.6-luna, tool su 3 chiamate, ragionamento su due turni, rinnovo forzato). Prossimo: health e metrics, poi TUI | |
| 29c | Health e metrics (Antonio 22/11: prima della TUI). FATTO 22/11: pacchetto `metrics` (solo stdlib, Observer per profilo, formato di testo Prometheus: run avviate/attive/finite con motivo o codice, task delegati, chiamate al modello e durate, token input/output/cache, compattazioni, loop, tool per nome ed esito con tetto 256 nomi e `unknown` per i nomi inventati, durate; nessun contenuto, niente prezzi); `serve` GET /health senza token (solo ok/closing, loopback, niente Origin); `ringil serve` monta GET /metrics dietro il token. OTel: modulo a parte, solo quando serve davvero (decisione mia: le metriche Prometheus coprono l'uso di oggi). Prossimo: TUI | |
| 29d | DECISIONI 22/11 con Antonio (Cerberus, Argus, orchestratore). (a) L'SDK è compagna nativa di Cerberus: l'installer (per ultimo, con ringil.sh) chiede se attivare il controllo d'integrità e configura Cerberus; ringil genera l'elenco di cosa sorvegliare (ringil.yaml, system prompt, skill, configurazione MCP, binari). (b) Argus passa a SQLite in Go puro (modernc, lo stesso driver della memoria): oggi ringil contiene due driver, mattn con cgo pesa 85 MB contro 37 e senza cgo l'audit ripiega in silenzio sul JSONL perdendo la catena di hash; insieme yaml v3 → v4. (c) I drift di Cerberus vanno nell'audit di Argus con catena di hash: da decidere nel design dove sta il registro (di root, o l'ultimo anello custodito dal demone), sennò l'utente stesso può riscriverlo. (d) Orchestratore futuro che avvia istanze dell'SDK come caselle mail, ognuna isolata al 100% (cartella di configurazione, store dei segreti, audit, /health e /metrics propri; un utente di sistema e la sandbox per istanza); Cerberus fornisce i dati alla dashboard dell'orchestratore con le sue metriche. ORDINE: Argus a Go puro + yaml v4 subito, poi la TUI. FATTO 22/11: Argus v1.9.0 (modernc, yaml v4), v1.10.0/v1.10.1 (ConfigWriter che modifica il documento YAML, cinque audit); l'SDK usa Argus v1.10.1, l'audit di ringil è audit.db con catena di hash, e l'onboarding scrive ringil.yaml con il ConfigWriter di Argus (l'SDK non importa più yaml: DRY e separazione, la regola del formato sta in Argus) | |
| 30 | Istruzioni per agenti di codice e IDE (decisione 20/11), in fondo: AGENTS.md come fonte unica (lo leggono Codex, Cursor, Copilot, Gemini…), CLAUDE.md che lo richiama, `.vscode/` con impostazioni e task (`make check`) | dicono a un agente come CREARE e PERSONALIZZARE agenti con l'SDK (un agente o una squadra: provider, tool, MCP, skill, istruzioni, profili e ruoli, canali, trigger, memoria, delegate; dal `ringil.yaml` o dall'API Go, con esempi pronti) e come usarlo bene: il prompt base non si sostituisce, sicurezza di default, gocyclo < 10, `make check` prima di chiudere; brevi e mantenuti con il codice. Valutare se `ringil init` li genera anche nei progetti di chi usa l'SDK |

Conferma prima dei tool distruttivi e tool in sola lettura in parallelo entrano con il
punto 1, quando esistono i primi tool reali su cui provarli.
