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
| 29e | DECISIONI 23/11 con Antonio (TUI e distribuzione; sostituiscono in 29b «resta allineabile all'upstream» e «due binari installati da ringil.sh»). (a) Fork di Codex CONGELATO al commit preso (studio: d4014cb del 21/11) e mantenuto da noi, niente sincronizzazione con l'upstream: si tolgono telemetria (otel, analytics, feedback) e ogni uscita verso OpenAI (login, backend, cloud, realtime); restano LICENSE Apache-2.0 e NOTICE; test che la TUI non apra connessioni di rete; watcher delle vulnerabilità sulle dipendenze Rust (cargo-audit/cargo-deny in CI). (b) Distribuzione separata: l'SDK è per sviluppatori e non porta con sé la TUI; la TUI è un'app (installi, metti la chiave, funziona) e porta con sé il proprio motore. (c) Il motore della TUI è un programma Go nel repo della TUI che importa l'SDK, registra i tool e le skill della TUI ed espone il protocollo app-server di Codex (implementato con i tipi generati dal loro JSON Schema); l'interfaccia Rust è solo client. Così la TUI è anche l'esempio d'uso vero della libreria; l'SDK resta senza politiche dell'app. (d) Pacchetti business da OpenSwarm nel motore della TUI (blueprint studio, ROADMAP 2.5): i sette specialisti (Docs, Slides, Deep Research, Data Analyst, Virtual Assistant, Image, Video) diventano skill + tool caricati al bisogno dallo stesso agente; niente agenti specializzati né orchestratore (la delega c'è già: worker generico con contesto pulito, la skill giusta e i tool che servono). Si studiano dal Python e si convertono migliorandoli: per ognuno prompt (procedura e criteri di qualità → skill), tool (Go nel motore, o script Python nel sandbox dove Go non ha equivalenti validi, es. python-pptx), template e asset (nella skill, con provenienza, revisione upstream e licenza). Un documento di studio per specialista, poi conversione e prova reale su un incarico business riproducibile. Le skill compaiono come slash command. (e) La TUI è OPINIONATA (l'SDK no): le scelte le facciamo noi e arrivano già fatte (memoria, MCP, A2A, pacchetti di skill, sandbox, provider di default); all'utente si chiede solo l'indispensabile (la chiave). Per l'utente funziona come se fosse in-process: la TUI avvia il motore Go come processo figlio su stdio; se `serve` gira già (canali, trigger) si collega a quello via socket unix e continua la stessa conversazione; nel fork si toglie il motore Rust in-process. MAPPA FATTA 23/11: `docs/tui-fork-map.md` (la TUI linka codex-core e legge config da sola: «zero import del motore» del 22/11 valeva solo per le dipendenze dirette; fork in tre fasi A client pulito, B fuori codex-core, C prodotto). ORDINE: mappa del fork di Codex (cosa si toglie, sostituisce, resta) discussa con Antonio prima del codice; poi motore Go + protocollo; poi OpenSwarm partendo da Docs e Slides, poi gli altri cinque | |
| 29f | DECISIONE 23/11 con Antonio: la TUI si scrive IN GO, niente fork (sostituisce 29e (a) e (c) per la parte fork; restano (b) distribuzione separata, (d) pacchetti OpenSwarm, (e) TUI opinionata). Motivo: Codex linka il proprio motore e legge la configurazione da sé (fork in tre fasi, ~100 file solo per togliere codex-core, 300k righe Rust altrui da sorvegliare); Gemini CLI peggio (interfaccia e motore senza confine, 293 file, Node, Clearcut). In Go: un linguaggio e un binario, il motore in-process quando serve o client di `serve` per `/rc`, niente protocollo di Codex da implementare (si usa l'API di `serve`), le nostre regole (audit, security test, gocyclo, CI), e la TUI è l'esempio d'uso vero dell'SDK. Strumenti: Orpheus per comandi, flag e onboarding, e il suo `term` (Sanitize su ogni testo di modello e tool, Width, Caps) dentro la TUI; Orpheus non ha ciclo di eventi, editing dell'input né layout a schermo: per quelli una libreria (candidata Bubble Tea, MIT) da proporre con il peso misurato. Codex resta RIFERIMENTO di comportamento (`docs/tui-fork-map.md`, copia di studio d4014cb): se ne copiano le scelte di UX, non il codice. Rischio principale: la rifinitura (a capo, markdown in streaming, scrollback, Windows). Stima: 10-15k righe, ~1 settimana per la prima versione buona, ~2 per la rifinitura. DISEGNO 23/11: `docs/tui-design.md` (Antonio: motore IN-PROCESS, client di `serve` solo se gira già o con /rc; slash command per cambiare dopo il primo avvio store dei segreti, motore di memoria, provider; primo avvio con Ollama, OpenAI, Google, Anthropic, compatibile OpenAI, ChatGPT). Da confermare: repo dell'app con `cmd/ringil` spostato dentro, default del primo avvio, Google via endpoint compatibile OpenAI | |
| 29g | DECISIONI 23/11 con Antonio sul disegno della TUI. (a) MONOREPO: l'app (CLI di oggi + TUI) resta nell'SDK, nel suo modulo `cmd/ringil` (go.mod a parte: chi importa la libreria non scarica le dipendenze della TUI); niente repo separato. (b) PRIMA si completa l'SDK con ciò che manca alla TUI: copia verificata tra store dei segreti; copia dei ricordi tra motori di memoria; ragionamento in streaming negli eventi; diff della modifica nell'evento di fine di edit/write; elenco/ripresa sessioni e ricarica della configurazione anche da `serve`. (c) Provider: Google entra fra le scelte (endpoint compatibile OpenAI di Gemini). Al posto della voce OpenRouter una categoria «compatibile OpenAI» con preimpostati OpenRouter, NVIDIA NIM, DeepSeek, Requesty, Z.ai, Moonshot (indirizzi e particolarità di ognuno, es. campi di ragionamento, verificati sulla documentazione prima del codice) più «altro» a indirizzo libero. (d) PIÙ PROVIDER INSIEME: impossibile prevederli tutti, quindi se ne configurano quanti si vuole (anche 4 compatibili OpenAI) con un nome ciascuno (tipo, indirizzo, nome della chiave) in una sezione `providers:` di ringil.yaml; settings e profili li richiamano per nome; `/model` passa dall'uno all'altro e ne aggiunge di nuovi. Oggi provider e ruolo stanno insieme nei profili: si separano. FATTO 23/11 (providers): sezione `providers:` e `provider:` per nome (cima, profili, transcribe/speech); preimpostati anthropic, chatgpt, openai, google, openrouter, nvidia, deepseek, requesty, zai, moonshot, ollama (indirizzi verificati sulle documentazioni); tolti `base`, `key_env`, `--base`, `--key-env`, `RINGIL_BASE_URL` (un file che li ha è rifiutato dicendo dove sono andati); onboarding con Google e sottomenu compatibili OpenAI, un server proprio diventa un provider col nome dell'host; la chiave di un provider va solo al suo indirizzo (mai il nome della chiave di un altro fornitore, un indirizzo diverso per un preimpostato vuole il suo key_env). Audit indipendente (5 punti, corretti) e prova dal vivo NIM + Claude. (e) ONBOARDING (Antonio 23/11): la TUI è autonoma, quindi le domande a righe di `ringil setup` diventano ridondanti; la LOGICA resta (modelli, verifica chiave, store, scrittura con Argus, Composio, proprietario Telegram, prova dal vivo) e si separa dalle domande quando si fa il primo avvio nella TUI; nello stesso cambio si cancellano le domande a righe e `ringil setup`. La CLI a righe (chat, run) resta per i test interni; `serve` e i comandi senza interfaccia restano nel binario. (f) CANALI dalla TUI: `/channels` collega e scollega Telegram (token nello store, proprietario dal primo /start, sezione serve.telegram con Argus) e propone di avviare `serve` in background; la riga di stato dice se il canale è acceso | |
| 29h | 23/11 SEGRETI E RETE (Antonio). FATTO: `secrets.Copy` e `secrets.WriteEnvFile` nell'SDK; `ringil secrets move keyring|file|vault|env` (copia, rilettura, switch di ringil.yaml con Argus, cancellazione dal vecchio solo su sì; env --to-file per i container); SHADOW ENV: con uno store, lo store vince e lo stesso nome anche nell'ambiente è errore (store.Resolver), le chiavi dei provider e i token di serve e Telegram vengono SOLO dallo store (store.StoreOnly), un provider nominato dall'ambiente è rifiutato (solo ringil.yaml o --provider), la fonte di ogni chiave va nell'audit di Argus; il setup e `secrets set` avvisano delle chiavi rimaste nell'ambiente; PROXY solo da ringil.yaml (`proxy.url`, `no_proxy`; HTTPS_PROXY/ALL_PROXY dell'ambiente ignorati, solo https tunnellato). Limite detto ad Antonio: chi controlla davvero l'ambiente del processo (LD_PRELOAD ecc.) non si ferma da dentro; le leve sono fuori (Cerberus su unit/profili/compose, serve con utente proprio). Audit a più giri, tutti chiusi. PRIMA DELLA TUI (Antonio 23/11): (a) RED TEAM dell'SDK in una sessione nuova, come per le cacce sui prodotti altrui (Antonio la apre); (b) studio di Microsoft Agent Framework per due motivi: ispirazione e verifica delle funzioni essenziali, e i CONFINI LOGICI dalla loro documentazione (cosa dichiarano fuori dal loro modello di minaccia: ci ferma dalla spirale dell'«indurire sempre qualcosa in più»); terzo motivo (Antonio 23/11): come strutturano la DOCUMENTAZIONE, logica e coerente senza essere prolissa (indice, livelli, cosa sta nei concetti e cosa nei riferimenti), modello per i nostri docs (backlog in project_ringil_docs_backlog) | |
| 29i | STUDIO MAF FATTO 23/11 (`docs/study-maf/`: features.md, boundaries.md, docs-structure.md; fonte: microsoft/agent-framework-go MIT, il repo principale non è più pubblico, e learn.microsoft.com). Posizionamento (Antonio 23/11): loro framework che lascia la sicurezza allo sviluppatore, noi SDK di un'azienda di cybersecurity che la porta dentro: piccolo ma completo, «coltellino svizzero». PROPOSTE, prima della TUI: (1) FATTO 23/11 (agent.Policy nell'SDK, permissions in ringil.yaml; marcatori Edits, SelfConfirms, Internal; un tool senza marcatori chiede; Approve nil = nessuno a cui chiedere, quindi rifiuta; regole lette solo sull'argomento dello schema; deny sulla shell = miglior sforzo, il confine di bash è la sandbox; remember senza conferma, decisione di Antonio; quattro giri di audit) `Approve` sicuro di default, sul modello degli agenti di codice che la gente conosce (Claude Code, Codex, Mistral) e non di MAF (Antonio 23/11): letture libere; modifiche ai file, bash, rete e tool MCP chiedono; regole allow/ask/deny per tool e per argomento (es. Bash(git status:*)), deny vince su ask che vince su allow, ricordate per sessione o progetto; modi: default, accept-edits, plan (sola lettura), bypass solo esplicito; senza nessuno a cui chiedere si rifiuta (fail closed); (2) FATTO 23/11 SECURITY.md con le tre colonne garantiamo / tocca all'host / fuori perimetro, e la regola: è una vulnerabilità solo ciò che rompe una riga di «garantiamo» (ferma la spirale); (3) FATTO 23/11 la frase di fiducia su ogni interfaccia di contesto (Config.Context, memoria, storia): il contenuto va al modello com'è; (4) FATTO 23/11 OUTPUT STRUTTURATO: agent.FormatFor[T], Config.Format, agent.Parse[T] (valida sempre, limiti compresi, errori senza la risposta); OpenAI response_format (strict se tutti i campi obbligatori), Anthropic output_config.format, ChatGPT text.format; un server compatibile senza response_format riceve lo schema a parole (ricordato), Parse controlla comunque. Prova dal vivo Haiku e Requesty (novita/ling-3.1-flash, senza structured output). DOPO o con la TUI: harness preset nell'SDK (agente completo in una chiamata, quello che oggi monta cmd/ringil: serve alla TUI); compattazione a strategie con eviction dei risultati dei tool; agente esposto come server MCP; OTel in modulo a parte (già deciso: quando serve). PIÙ AVANTI: workflow a grafo con condizioni, cicli e richieste umane a metà; group chat; AG-UI; strato vector store generico. DOCS IN INGLESE (Antonio 23/11): struttura loro in versione asciutta (README ~120 righe; docs/get-started numerati 01-07 specchiati in examples/01-get-started; concepts, capabilities, providers, ringil; godoc unico riferimento API; piani interni in docs/dev) | |
| 29j | DECISIONI 23/11 con Antonio (dopo il red team, commit 05882b7). (a) REPO SEPARATO per l'app (sostituisce 29g (a) monorepo): la TUI è un'app a sé che usa l'SDK, quindi un repo suo con dentro anche `cmd/ringil` (CLI di oggi, serve, segreti, onboarding: la stessa app). Motivo: da un altro repo `internal/` non si importa, quindi tutto ciò che serve all'app passa dall'API pubblica e la TUI prova che l'SDK basta; costo: un tag dell'SDK per le modifiche che toccano entrambi, in locale `go.work`. Da sistemare prima dello spostamento: `cmd/ringil` usa `internal/lockfile` (3 file) e `internal/safehttp` (1): si rendono pubblici o si rifanno nell'app. (b) SECONDO CANALE: l'interfaccia channel è provata solo su Telegram; il secondo è SLACK (thread, messaggi modificati, blocchi, Socket Mode invece del polling: quasi l'opposto di Telegram, ed è il canale delle aziende a cui ci rivolgiamo). Se l'interfaccia regge Slack senza cambiare forma è giusta, altrimenti si corregge prima della TUI (`/channels`). Prova dal vivo su un workspace di test con un'app Slack creata da Antonio. (c) AGENTI IN ALTRI LINGUAGGI (non indispensabile, DOPO la TUI): come l'SDK di Claude Code, il binario `ringil` ha una modalità a riga di comando che parla JSON su stdio con un protocollo versionato; client sottili (Python, TS, poi altri) lo avviano, mandano la configurazione, ricevono gli eventi; i tool scritti nel linguaggio dell'utente sono richiamati sullo stesso canale. Sandbox, segreti, permessi ed egress restano in Go: le garanzie non dipendono dal linguaggio. Costo: il protocollo più qualche centinaio di righe per client. Si parte quando l'app è stabile. RED TEAM 23/11: tenuti tetto ai task in background (16), errore delle credenziali senza testo, corpo di risposta lungo esattamente il massimo accettato, tetto 1 GiB al file di sessione, anti-replay dei webhook per trigger; TOLTO bash che percorre la home (7-33 s e 1296 percorsi nascosti per comando sulla home vera, contro 3 ms e 14): il limite è scritto in SECURITY.md (una home intera nasconde i segreti dove stanno di solito; per la garanzia piena si lavora in una cartella di progetto) |
| 29k | DECISIONI 23/11 con Antonio. (a) L'APP SI USA COME CLAUDE CODE: il 99% degli utenti non apre mai ringil.yaml; il setup si fa con gli slash command (`/channels`, `/model`, `/secrets`, `/memory`...), che salvano le scelte in ringil.yaml con Argus; il file resta per i power user. (b) A PROVA DI AGGIORNAMENTO (requisito della TUI): l'aggiornamento sostituisce solo il binario, i dati dell'utente non stanno mai accanto a lui. Già così: tutto sotto la cartella di configurazione dell'utente; memoria SQLite versionata (PRAGMA user_version, rifiuta un database più nuovo). Da fare: dati separati dalla configurazione come da standard (config in `~/.config/ringil`; memoria, sessioni, note, audit, skill dell'utente, scratchpad in `~/.local/share/ringil`, equivalenti su macOS e Windows) con migrazione dei file di oggi; numero di versione in ringil.yaml e in ogni formato su disco (sessioni, note, store dei segreti, audit); backup automatico prima di ogni migrazione; test di aggiornamento in CI (versione vecchia, dati dentro, aggiornamento, tutto presente e leggibile). (c) SPOSTAMENTO FATTO 23/11: `cmd/ringil` va nel repo `ringil` (~/Desktop/GitHub/ringil, modulo github.com/agilira/ringil, storia conservata con git subtree); i tre helper interni che usava (CheckPrivate, NoFollow, Loopback) rifatti nell'app; finché l'SDK non ha un tag l'app lo trova con replace su ../ringil-agent-sdk (in CI il checkout dei due repo affiancati) |
| 29l | DECISIONI 24/11 con Antonio (sostituiscono 29b, 29e (a)(c)(e) sulla TUI e 29e (d) «niente agenti specializzati né orchestratore» PER L'APP). (a) NIENTE TUI: il fork di Codex si abbandona; si fa direttamente l'APP, fatta bene. L'app si chiama RINGIL HARNESS (Ringil è l'azienda; sorella di Ringil Forge, quella per addestrare LLM). Il repo `agilira/ringil` è la CLI (ex `cmd/ringil`), NON l'app: se all'app serva è da decidere; va RINOMINATO (nessun repo deve chiamarsi solo «ringil», è il nome dell'azienda), non urgente. L'app è l'app, l'SDK è l'SDK: l'app vive solo nel suo repo, nuovo, si distribuisce da lì e importa l'SDK come qualunque altro utente; l'SDK non contiene né distribuisce nulla dell'app. L'app è insieme la nostra app e la vetrina di cosa si fa con l'SDK. Scelta tecnica: interfaccia web dell'app (browser in locale o da remoto, niente Rust); un eventuale guscio desktop impacchetta la stessa interfaccia. (b) ORCHESTRATORE NELL'APP, MAI NELL'SDK: l'SDK resta senza agenti specializzati né orchestratore (primitive: delegate, trigger, sessioni, a2a); l'app ha un agente primario (dell'admin) che crea agenti secondari subordinati, ognuno con modello e system prompt propri per specializzarlo; durata: effimeri (delegate), periodici (trigger), a lunga durata (sessione persistente). L'utente parla con ciascuno nella sua tab; il primario li controlla, li orchestra e può parlare con loro (tool spawn_agent, message_agent, list_agents, stop_agent). (c) INSTALLARE SKILL E MCP COME CLAUDE CODE, OPENCODE E CODEX (Antonio 24/11: la gente è abituata così, niente di diverso): nessun tool apposta; comandi dell'app (`skills add`, `mcp add`, anche come slash command) che l'utente lancia o che l'agente lancia con bash quando gli si incolla un link, con l'approvazione normale dei permessi. Nell'SDK non serve niente di nuovo: `skills.Install` c'è già, la configurazione MCP è dell'host. (d) CONNETTORI PRONTI nell'app: 4-5 server MCP ufficiali (Gmail, Google Docs, Microsoft 365, Zoho; da verificare quali esistono davvero e sono ufficiali) con campi per OAuth o credenziali dirette, così le cose base ci sono senza Composio. ORDINE: prima Slack come secondo canale nell'SDK (29j b), poi l'app | |
| 29m | INSTALLARE DA UN LINK, NELL'SDK (Antonio 25/11, sostituisce 29l (c) «nell'SDK non serve niente»: chi costruisce un agente non ha la nostra CLI, e scaricare e verificare è la sicurezza che l'SDK deve dare pronta). Come Claude Code, OpenCode e Codex: incolli il link, l'agente chiama un tool, l'approvazione è quella normale (Destructive + OpenWorld: in default chiede). (a) `skills.InstallGitHub` + tool `install_skill`: link GitHub (repo, tree, blob di SKILL.md, `owner/repo/path@ref`), ref risolto al commit, tarball di quel commit da codeload, estrazione con limiti (byte, file, niente link né file speciali, percorsi locali), una skill trovata = installata, più skill = elenco per chiedere all'utente quale; provenienza (repo, cartella, commit) nel manifesto. (b) `mcp.Add` + tool `add_mcp_server`: nome, comando senza shell o URL, env e header; gli header solo come riferimenti `${NOME}` (i segreti non finiscono nel file); controlla e passa all'host, che salva e collega e restituisce i tool. L'app aggiunge `/skills add` e `/mcp add` sulle stesse funzioni. FATTO 25/11: `skills.InstallGitHub`, `InstallTool`, `Catalog.Source`; `mcp.Entry`, `AddFunc`, `AddTool`; fuzz di link ed entry (il fuzz ha trovato un panic sul confronto dell'host con caratteri non ASCII, corretto); provato dal vivo su anthropics/skills (elenco delle 20 skill, poi pdf installata con commit e script). Banco di prova: la CLI (`agilira/ringil`) salva in ringil.yaml e prova il server (`mcp.Added.NextSession`); dal vivo 25/11 con un modello vero: «aggiungi https://mcp.deepwiki.com/mcp e installa <link raw di SKILL.md>» → server salvato con 3 tool, skill pdf installata; nella sessione dopo i tool DeepWiki usati. RICARICA A CALDO FATTA 25/11 (Antonio: «una volta installati non funzionano nella sessione corrente»): `agent.LiveRegistry` (il run prende i tool del momento prima di ogni chiamata del modello, ricostruisce il catalogo di tool_search; le chiamate di un turno girano sui tool mostrati) e `Catalog.Reload`; nella CLI l'elenco delle skill passa dal system prompt a un blocco di contesto (cambia solo quando cambia). Dal vivo: nella stessa richiesta DeepWiki aggiunto e usato, skill xlsx installata e caricata. `/reload` per le modifiche a mano va nell'app | |
| 29n | 25/11 con Antonio. (a) SLACK FATTO (29j b): `channel/slack` in Socket Mode con coder/websocket v1.8.15 (modulo a parte, approvato da Antonio); DM e thread nei canali, bottoni, slash command, file, 👀 come «sta scrivendo» (Slack non ce l'ha per le app; `assistant.threads.setStatus` da provare se si attiva «Agents & AI Apps»); il markdown del modello non può fare @here (provato dal vivo: `&lt;` fuori dal codice). L'interfaccia channel ha retto Slack senza cambiare forma. Prova dal vivo sul workspace AGILira (bot U0CLYHHN481): DM, approvazione coi bottoni, PDF letto. (b) DEFAULT COME GLI ALTRI AGENTI (Antonio: «imporre metodi nuovi non è mai un buon selling point»): pacchetto `dirs` e un giro su ~260 opzioni; chi non tocca nulla ha inbox, sessioni, archivio di serve, token OAuth, journal dei workflow e output delle skill nei posti giusti, e le cartelle dell'agente protette da bash e tool dei file. MaxTurns resta senza limite, come Claude Code; MCP locali senza sandbox di default, come Claude Code; check_later in memoria per scelta. Canale provato dal vivo 25/11: menzione in #general, risposta nel thread, seconda domanda nel thread senza menzione con il contesto della prima; senza profili dei membri il gruppo parla con il profilo dei proprietari (prima rispondeva «non sono configurato per i gruppi»). Prossimo: l'app | |
| 29o | COSTI E BUDGET (punto 1 della parità, 25/11). FATTO 25/11: `Usage.Cost` in dollari; pacchetto `price`: NIENTE PREZZI NEL CODICE (Antonio 25/11: «deve essere dinamico non hardcoded»): `Catalog` scarica il listino pubblico di OpenRouter (470 modelli, tutti i vendor: Google, xAI, Mistral, Moonshot, Z.ai, DeepSeek, Qwen…; senza chiave, HTTPS, tetto 32 MiB), lo salva in `<state>/prices.json` per 24 ore, se la rete manca usa la copia; cache letta/scritta e scaglioni dei prompt lunghi (`overrides`); `Lookup` per nome come lo scrive il provider (minuscole, punti come trattini, prefisso del vendor, data), un nome condiviso da due vendor a prezzi diversi è escluso, mai un modello al prezzo di un altro; `Table.Over` per i prezzi dell'host; `Wrap` tiene il costo dichiarato. `openai.Config.StatesCost` (OpenRouter dichiara il costo). `Config.MaxCost` per esecuzione come `max_budget_usd` dell'SDK di Claude Code (`StopMaxCost`); `budget.Limits.Cost`; `workflow.Budget.Cost`; metrica `ringil_cost_usd_total`; costo nel log. CLI: riga di fine turno con il costo (anche dei worker), `--max-cost`, `--workflow-cost` (rifiutati su un modello senza prezzo), `prices:` in ringil.yaml; modelli locali e ChatGPT a zero; provider pronti `xai` e `mistral`. Audit indipendente 25/11: nulla di grave, 6 punti minori corretti. Prossimo: punto 2, fallback su un altro modello | |
| 29p | FALLBACK SU UN ALTRO MODELLO (punto 2 della parità, 25/11; Antonio: «smart, solo se c'è più di un provider»). FATTO 25/11: `provider/fallback`: con un provider solo `New` lo restituisce com'è; con più, passa al successivo SUBITO quando il provider non risponde (UNAVAILABLE, RATE, INTERRUPTED, RESPONSE, AUTH, CIRCUIT_OPEN), mai per errori della richiesta (INPUT, CONTEXT, CONFIG); il caduto si salta per un cooldown (1 min, doppio a ogni caduta di fila, max 15) poi si riprova e si torna a lui; se tutti sono in pausa si provano lo stesso, il primo a tornare per primo; testo già mostrato mai ripetuto (`OnRestart` come retry); `OnSwitch` (dal, al, perché); `Info` del modello che risponderebbe. Ordine: retry fuori, fallback, poi per provider prezzo (e circuito). Fuzz: nessuna chiamata persa finché l'ultimo risponde. CLI (Antonio 25/11: «pensiamo alla UX futura», app e SaaS con righe chiave/endpoint/modello, + per aggiungere, primario e fallback): UNA LISTA ORDINATA `models: [provider:modello]` in ringil.yaml e per profilo, `--models`; il primo risponde, gli altri nell'ordine (niente toggle «primario»: conta l'ordine, «Rendi principale» = in cima, drag & drop riordina); connessione (endpoint+chiave, `providers:`) separata dal modello, così una chiave serve più modelli; `model:`/`provider:` rifiutati dicendo dove sono andati; `--provider`/`--model` mettono un altro modello primo; l'onboarding scrive `models:` col modello scelto in cima e gli altri dopo. Un modello senza chiave o non creabile lasciato fuori (detto); la conversazione va solo ai provider in lista, un profilo con i suoi `models:` usa solo quelli; il terminale dice chi risponde. Audit 25/11 (5 difetti, corretti): un profilo con il suo provider non prende la lista del flag; se il modello di riserva rifiuta la richiesta (troppo piccolo, senza tool, chiave) si restituisce l'errore del primo col suo Retry-After, così il retry aspetta come senza fallback; cadute in parallelo contano come una; pensiero di Claude legato a modello ed endpoint (`anthropic:<modello>[@base]`). Prossimo: punto 3, tool `read_document` | |
| 29q | TOOL `read_document` (punto 3 della parità, 25/11). FATTO 25/11 (parte Office): pacchetto `document` solo libreria standard (zip + XML): Word, Excel (fogli nell'ordine, righe con colonne «row 3: A=Totale | C=42», stringhe condivise, valori come salvati), PowerPoint (slide nell'ordine della presentazione), OpenDocument testo/foglio/presentazione; limiti contro zip bomb (parte 64 MiB, totale 256 MiB, testo 16 MiB con nota), celle/righe ripetute (vuote costano zero, piene max 256/64), XML profondo 256, entità solo le cinque standard, caratteri di controllo tolti; file cifrati e vecchi binari (.doc/.xls/.ppt) detti per nome. Tool in `tools/files` con la stessa politica di read_file (cartelle concesse, esclusi, niente cartelle) e lettura a pagine. Provato su file reali (Office, LibreOffice, OnlyOffice). Fuzz di zip e XML. PDF: DA DECIDERE con Antonio (libreria Go pura, pdftotext, o il PDF al modello) | |
| 30 | Istruzioni per agenti di codice e IDE (decisione 20/11), in fondo: AGENTS.md come fonte unica (lo leggono Codex, Cursor, Copilot, Gemini…), CLAUDE.md che lo richiama, `.vscode/` con impostazioni e task (`make check`) | dicono a un agente come CREARE e PERSONALIZZARE agenti con l'SDK (un agente o una squadra: provider, tool, MCP, skill, istruzioni, profili e ruoli, canali, trigger, memoria, delegate; dal `ringil.yaml` o dall'API Go, con esempi pronti) e come usarlo bene: il prompt base non si sostituisce, sicurezza di default, gocyclo < 10, `make check` prima di chiudere; brevi e mantenuti con il codice. Valutare se `ringil init` li genera anche nei progetti di chi usa l'SDK |

Conferma prima dei tool distruttivi e tool in sola lettura in parallelo entrano con il
punto 1, quando esistono i primi tool reali su cui provarli.
