# ringil-agent-sdk

SDK Go per costruire agenti: un modello, dei tool, un ciclo che li alterna, una
conversazione che sopravvive al processo.

## Installazione

```sh
go get github.com/agilira/ringil-agent-sdk
```

Richiede Go 1.27.

## Un agente completo

Modello, tool tipizzato, conversazione salvata su disco, risposta in streaming:
[`examples/agent`](examples/agent/main.go), una sessantina di righe.

```sh
export RINGIL_BASE_URL=http://localhost:11434/v1 RINGIL_MODEL=qwen2.5:3b
go run ./examples/agent "Ricorda che il mio colore preferito è il verde"
go run ./examples/agent "Qual è il mio colore preferito, e che ore sono a New York?"
```

Le protezioni sono accese di default (argomenti dei tool validati, errori dei tool
filtrati, sessioni con ripresa sicura, compattazione); limiti e politiche sono
opzioni, non obblighi.

## CLI

```sh
make chat                                   # chat con l'endpoint del .env (RINGIL_*)
./cmd/ringil/ringil run "che ore sono a Tokyo?"
./cmd/ringil/ringil -S lavoro chat          # sessione salvata: la riapri e riprende
./cmd/ringil/ringil sessions                # elenco; "sessions rm <nome>" per cancellare
./cmd/ringil/ringil run "trova dove è definito X"   # tool sui file nella cartella di lancio;
                                                   # per altre cartelle l'agente chiede il permesso
./cmd/ringil/ringil sandbox --mount ~/dati -- python3 script.py               # esecuzione isolata
```

Flag principali: `--base`, `--model`, `--key-env`, `--instructions`, `--max-turns`,
`--allow-http`, `--no-tools`, `--session`/`-S`, `--sessions-dir`.

## Agente

```go
a, err := agent.New(agent.Config{Provider: model, Tools: tools})

store, err := file.New(dir)                 // session/file
sess, err := store.Open(ctx, "lavoro")      // un solo aperto alla volta
defer sess.Close()

for ev, err := range a.Stream(ctx, sess, agent.UserText("ciao")) {
    // EventText, EventToolStart, EventToolEnd, EventRecovered, EventDone
}
```

Il ciclo scrive ogni messaggio nella conversazione appena esiste: la richiesta
dell'utente, il messaggio del modello prima che parta qualunque tool, ogni
risultato dopo. Se il processo muore a metà di un tool, alla ripresa la chiamata
non viene rieseguita: il modello riceve un risultato «interrotto, verifica prima
di ripetere» (con `StrictResume` la sessione viene invece rifiutata).

Le sessioni su file sono JSON Lines in sola aggiunta, con una catena di hash che
rivela righe modificate, tolte o riordinate; una riga a metà lasciata da un crash
viene scartata. Nessun limite di lunghezza.

## Uso del provider

Una chiamata in streaming verso qualunque server compatibile OpenAI:

```go
p, err := openai.New(openai.Config{
    BaseURL: "http://localhost:11434/v1", // Ollama; oppure https://api.openai.com/v1, vLLM, OpenRouter...
    Model:   "qwen2.5:3b",
    APIKey:  os.Getenv("OPENAI_API_KEY"), // vuota per i server locali
})
if err != nil {
    return err
}
model := retry.Wrap(p, retry.Config{})

req := &agent.Request{Messages: []agent.Message{agent.UserText("ciao")}}
resp, err := model.Generate(ctx, req, func(delta string) error {
    fmt.Print(delta)
    return nil
})
```

Esempio eseguibile:

```sh
go run ./examples/chat -base http://localhost:11434/v1 -model qwen2.5:3b "ciao"
go run ./examples/chat -tool "che tempo fa a Roma?"
```

Per un server HTTP non locale (LAN, pod) impostare `AllowHTTP: true`.

## Tool

Un tool è una funzione Go tipizzata; lo schema JSON viene generato dalla struct:

```go
type MeteoIn struct {
    Citta string `json:"citta" desc:"Nome della città" min:"1"`
    Unita string `json:"unita,omitempty" enum:"c,f"`
}

meteo := agent.NewTool("meteo", "Temperatura attuale di una città",
    func(ctx context.Context, in MeteoIn) (string, error) {
        if in.Citta == "Atlantide" {
            return "", agent.ToolError("NOT_FOUND", "Città sconosciuta.")
        }
        return "21 gradi", nil
    },
    agent.ReadOnly())

tools, err := agent.NewRegistry(meteo)
req.Tools = tools.Specs()
// per ogni tool call del modello:
result, err := tools.Execute(ctx, call)
```

Tag: `json`, `desc`, `enum` (stringhe), `min`/`max` (valore dei numeri,
lunghezza delle stringhe, elementi degli array). Un campo è obbligatorio se non
è un puntatore e non ha `omitempty`/`omitzero`.

Prima che la funzione parta, gli argomenti sono controllati contro lo schema:
campi sconosciuti (anche con maiuscole diverse), tipi sbagliati, numeri fuori
range e campi mancanti sono rifiutati. Il modello riceve i nomi dei campi da
correggere, mai i valori che ha mandato. Un errore normale arriva al modello solo
come codice; il testo lo vede solo se il tool usa `agent.ToolError`. Un panic in
un tool diventa un errore e non ferma l'agente.

Per tool con schema noto solo a runtime (MCP, tool generati) si implementa
direttamente l'interfaccia `agent.Tool`.

## Tool sui file

```go
ws, err := files.New(files.Config{Dirs: []string{"/home/me/progetto", "/home/me/documenti"}})
tools, err := agent.NewRegistry(ws.ReadTools()...)   // read_file, list_directory, glob, grep
```

Percorsi assoluti dentro qualunque cartella concessa, o relativi alla prima. Con
`Config.Ask` l'host decide al momento sulle altre cartelle (la CLI chiede nel
terminale, un'app mostra una finestra); senza, sono rifiutate. Nessuna
uscita dalle cartelle (`..`, symlink); file dei segreti (`.env`, chiavi SSH, `*.pem`,
credenziali cloud) esclusi di default, modificabile con `Config.Exclude`.

## Memoria

```go
mem, err := local.New(dir)                         // memory/local: file privato, ricerca BM25
a, err := agent.New(agent.Config{Provider: p, Tools: tools, Memory: mem})
tools, err = tools.With(memory.Tools(mem)...)      // remember, recall, forget per il modello
```

Con un `agent.Embedder` la memoria trova anche per significato: `memory/vector` usa lo
stesso archivio di `memory/local` e ci affianca i vettori; la ricerca fonde parole
(BM25) e significato (coseno) per rango reciproco. Se gli embedding non rispondono, la
memoria continua per parole e lo segnala. `openai.NewEmbedder` parla con qualunque
endpoint `/embeddings` compatibile; un vector store esterno è un'altra implementazione
di `agent.Memory`.

```go
emb, err := openai.NewEmbedder(openai.Config{BaseURL: base, Model: "openai/text-embedding-3-small", KeyFunc: key})
mem, err := vector.New(dir, emb)
```

A ogni domanda i ricordi pertinenti entrano nel contesto di quel turno, delimitati e
marcati come dati, mai nella conversazione salvata. Nella CLI la memoria è attiva di
default (`--no-memory` per toglierla), per significato con `--embed-model` o
`RINGIL_EMBED_MODEL`; `ringil memory`, `memory search`, `memory rm`.

## Delega ai sub-agenti

`delegate.Tool` dà al modello sub-agenti a cui passare incarichi autonomi: ognuno parte
con un contesto pulito (vede solo il compito), lavora con i suoi tool e restituisce un
solo rapporto, così ricerche e letture lunghe non riempiono il contesto del padre. Più
incarichi dello stesso turno girano in parallelo: il ciclo esegue insieme le chiamate
di tool in sola lettura o marcati `agent.Parallel()`, chiedendo le conferme una alla
volta prima di partire.

```go
dt, err := delegate.Tool(delegate.Team{Provider: model, Approve: approve, Workers: []delegate.Worker{
	{Name: "explore", Description: "searches files, read-only", Tools: readOnly},
}})
```

Ogni tipo di worker ha istruzioni e tool propri (mai la delega stessa, salvo scelta
dell'host); `Prepare` dà a ogni incarico uno scratchpad suo; le conferme passano
dall'hook del padre. CLI: `explore` (sola lettura) e `general` (i tool del padre),
ciascuno con scratchpad proprio, note di progetto in sola lettura e regole del progetto.

### Background e ripresa

Con `background: true` il sub-agente parte e il padre continua: il rapporto arriva più
tardi da `Crew.Next`, e `delegate.Notice` lo porta al padre come messaggio marcato
(non parole dell'utente). Con `continue_task` il padre manda un seguito a un compito
finito: il worker risponde con il suo contesto intatto. `NewCrew` dà l'oggetto completo
(compiti, background, `Close`); `delegate.Tool` è la scorciatoia.

## Workflow

`workflow.Runner` esegue un piano: passaggi con nome, ognuno un compito per un worker
del crew, con i passaggi di cui ha bisogno. Un passaggio parte quando i suoi bisogni sono
finiti e i passaggi pronti girano insieme; ognuno vede solo il suo compito, il brief e i
rapporti di cui ha bisogno, marcati come dati.

Con un `Journal` (`workflow.NewFileJournal`) l'intenzione di un passaggio si salva prima
di avviarlo e il rapporto prima di andare avanti. La ripresa salta i passaggi finiti; un
passaggio avviato e non finito ferma la ripresa (`WORKFLOW_UNCERTAIN`), perché i suoi
effetti potrebbero esserci già: si verifica e si registra l'esito con `workflow.Resolve`.
Il journal non riesegue mai nulla di sua iniziativa.

`Runner.Budget` limita chiamate e token di un workflow nel suo insieme: la spesa di ogni
passaggio si salva nel journal, quindi la ripresa continua a contare; un passaggio parte
solo finché il budget non è esaurito. CLI: `--workflow-calls`, `--workflow-tokens`.

`Runner.Tool()` dà al modello `run_workflow`, per i piani che scrive lui: è marcato
distruttivo, quindi l'host mostra il piano prima di eseguirlo.
CLI: il piano compare con i passaggi e chiede conferma; `ringil workflow`,
`ringil workflow resume <id>`, `ringil workflow resolve <id> <passaggio>` (esito da stdin).

## Artifact

`artifact` conserva i risultati che gli agenti producono (un rapporto, una tabella, un
file) perché altri compiti, sessioni successive o l'utente li usino. Un artifact ha un nome
in uno scope scelto dall'host e delle versioni: ripubblicare un nome aggiunge una versione,
non sovrascrive mai. Ogni versione è salvata per SHA-256 e controllata a ogni lettura.
`artifact.Tools` dà al modello `list_artifacts`, `read_artifact` e, con il permesso,
`publish_artifact`; altri scope si possono concedere in sola lettura.
CLI: scope = progetto della cartella di lancio, condiviso da agente, sub-agenti e
workflow; `ringil artifact`, `artifact show <nome>`, `artifact rm <nome>`.

## Backup e ripristino

`backup.Create` salva in un tar.gz le cartelle di stato indicate dall'host, con un manifest
di digest; `backup.Verify` controlla un archivio senza estrarlo; `backup.Restore` lo
ripristina solo in una cartella nuova, marcata `QUARANTINE.md`: un'istantanea non dice nulla
di ciò che è successo dopo (effetti già fatti, spese già fatte), quindi niente viene
riattivato da solo. CLI: `ringil state backup <file>`, `state verify <file>`,
`state restore <file> <cartella nuova>`; sono esclusi le skill installate e i token di
login.

## Regole di progetto

`rules` legge le istruzioni che le persone scrivono per gli agenti, così un agente
fatto con l'SDK rispetta le stesse regole di quelli che già usano: `AGENTS.md` (lo
standard aperto) e i file degli altri agenti, `CLAUDE.md`, `GEMINI.md`,
`.github/copilot-instructions.md`, `.cursorrules`, `.cursor/rules/`, `.windsurfrules`.
Si leggono dalla radice del repository fino alla cartella di lavoro (il più vicino
affina il più ampio), più un file globale dell'utente; i link si seguono ma mai fuori
dal repository né verso file segreti. Entrano nel contesto a ogni turno come regole da
seguire:

```go
cfg.Context = rules.Context(rules.Config{Dir: cwd, Global: globalAgentsMD})
```

`files.DefaultProtected` elenca i percorsi da non toccare senza conferma esplicita
(regole degli agenti, `.vscode`, `.idea`, `.claude`, `.codex`, `.cursor`, `.git`,
`.github`...): i tool che scrivono file chiedono sempre prima di modificarli.
CLI: `ringil rules` mostra quali file l'agente sta seguendo.

## Todo e note

`todo.Tool()` dà al modello una lista di compiti per il lavoro lungo: la riscrive intera
a ogni passo, una voce in corso alla volta. La lista vive nella conversazione (gli
argomenti dell'ultima chiamata) e `todo.Current` la rilegge.

`notes` dà quaderni Markdown con un solo tool, `notes`, come il memory tool su cui i
modelli sono addestrati: `view`, `create`, `str_replace`, `insert`, `delete`, `rename`
su percorsi come `/scratchpad/plan.md` o `/project/decisions.md`. Ogni scope è una
cartella scelta dall'host, anche in sola lettura; i file restano Markdown che l'utente
apre e modifica. L'`INDEX.md` di ogni scope entra nel contesto a ogni turno tramite
`Config.Context`, un aggancio per il contesto che cambia tra un turno e l'altro.

```go
nb, err := notes.New(notes.Scope{Name: "scratchpad", Dir: scratch}, notes.Scope{Name: "project", Dir: proj})
cfg.Context = nb.Context
tools, err = tools.With(nb.Tool(), todo.Tool())
```

CLI: la todo list compare a ogni aggiornamento; lo scratchpad vale per la sessione
(`--session`) o per l'esecuzione, le note di progetto per la cartella di lancio, in
`<config>/ringil/projects/`.

## Skill

Formato standard Agent Skills: una cartella con `SKILL.md` (front matter `name`,
`description`) più file di riferimento e script. Installare è un gesto dell'utente,
mai del modello; ogni file ha il suo SHA-256 e una skill modificata dopo
l'installazione viene rifiutata.

```go
info, err := skills.Install(ctx, "examples/skills/review", dir)
cat, err := skills.Open(dir)
cfg.System = agent.DefaultSystem + cat.Instructions() // la base resta; delle skill solo nomi e descrizioni
tools, err = tools.With(skills.Tools(cat, skills.Options{Sandbox: sb})...)
```

Il modello ha `load_skill`, `read_skill_file` e, con una sandbox, `run_skill_script`:
qualunque script della skill (Python, shell, JavaScript, Ruby, Perl o con riga `#!`)
gira su una copia verificata, senza rete. `Options.Mounts` ed `Options.Env` danno agli
script cartelle e librerie; un `sandbox.Mount` può nascondere percorsi (`Hide`), e
`files.Hidden` elenca quelli che i tool sui file escludono.
Gli script lavorano in una cartella vuota (`/work`, anche `$OUTPUT_DIR`); con
`Options.Output` i file che creano vengono consegnati in una cartella nuova per ogni
esecuzione (`<skill>-<data-ora>`), senza nomi nascosti e con limiti su numero e
dimensione. `Options.Runtimes` indica interpreti installati
fuori da `/usr`.
La CLI mostra agli script la cartella di lancio in sola lettura con i segreti nascosti,
il virtualenv attivo, le librerie Python dell'utente e gli interpreti per-utente
(node da nvm, ruby, perl, python); i file prodotti arrivano in `~/ringil-output/`,
fuori da ogni progetto, perché git o un test runner non li eseguano.
CLI: `ringil skill install <dir>`, `ringil skill`, `ringil skill rm <nome>`.

## MCP

Modulo a parte (`github.com/agilira/ringil-agent-sdk/mcp`, sopra l'SDK ufficiale
`modelcontextprotocol/go-sdk`): il nucleo resta senza dipendenze.

```go
c, err := mcp.Connect(ctx, mcp.Server{Name: "notes", Command: []string{"notes", dir}, Sandbox: sb})
c, err := mcp.Connect(ctx, mcp.Server{Name: "crm", URL: "https://crm.example/mcp", Headers: headers, OAuth: oauth})
tools, err = tools.With(c.Tools()...)              // nomi "<server>__<tool>"
defer c.Close()
```

Server locali via stdio, nella sandbox se data; remoti via Streamable HTTP, solo HTTPS
(loopback escluso), credenziali per richiesta dal resolver dell'host, redirect verso altre
origini mai seguiti. Le annotazioni di un server sono suggerimenti suoi: senza
`TrustAnnotations` ogni tool MCP conta come distruttivo e passa dall'hook `Approve`.
Ogni chiamata ha un timeout, ogni risultato e ogni risposta HTTP un tetto, e `Close`
annulla le chiamate in corso.

Login OAuth da desktop o terminale: `mcp.NewLogin` apre un listener su 127.0.0.1,
registra il client (dinamicamente o con un client ID dato), mostra il link e conserva i
token con `mcp.FileTokens`, così la volta dopo non serve rifare il login.

CLI: `<config>/ringil/mcp.json` (o `--mcp-config`) nel formato comune
`{"mcpServers": {...}}`; nei valori `${NOME}` viene risolto da ambiente ed `--env-file`.
Per un server locale `"sandbox": true` con `"network"` e `"mounts"` lo isola;
`"oauth": {}` fa il login dal browser (token in `<config>/ringil/oauth/`);
`"trustAnnotations": true` evita la conferma sui tool che il server dichiara di sola
lettura; `"disabled": true` lo tiene spento. Alla conferma, `a` consente quel tool per
tutta la sessione.
Esempio di server: `mcp/examples/notes`.

## Cronologia dopo la compattazione

La compattazione cambia solo ciò che si manda al modello: la conversazione resta intera.
Riassunti e risultati accorciati dicono quali messaggi coprono («messaggi 1-18»,
«messaggio 7»), e da quando la vista ha tolto qualcosa il ciclo offre `read_history`, che
restituisce gli originali per numero, entro un limite per lettura.

## Discovery dei tool

I tool marcati `agent.Deferrable()` (quelli MCP lo sono sempre) vengono mostrati al
modello solo per nome quando i loro schemi superano `Config.DiscoveryThreshold`
(16 KiB di default). Il modello li carica con `tool_search`, per nome esatto o
descrivendo il compito, e da lì li chiama normalmente; ciò che ha caricato resta per
tutta la conversazione. `DiscoveryAlways` e `DiscoveryOff` forzano le due strade.

## Composio

`mcp/composio` collega Composio Connect con la consumer key dell'utente (`ck_…`):

```go
c, err := mcp.Connect(ctx, composio.Connect(func(ctx context.Context) (string, error) { return key, nil }))
```

Tutti i meta-tool di Composio sono offerti (ricerca, esecuzione, connessioni,
workbench); `Server.Tools` li restringe. Nella CLI basta un server HTTP in
`mcp.json` con l'header `"x-consumer-api-key": "${COMPOSIO_KEY}"`.

## A2A (agenti remoti)

Modulo a parte (`github.com/agilira/ringil-agent-sdk/a2a`, sopra l'SDK ufficiale
`a2a-go`): un agente remoto Agent2Agent diventa un tool del nostro agente.

```go
wh, err := a2a.Connect(ctx, a2a.Remote{Name: "warehouse", URL: "https://wh.example", Headers: headers})
tools, err = tools.With(wh.Tool())
```

La card dell'agente si legge dal suo URL; le credenziali vanno solo a quell'origine (una
card che indica un endpoint altrove è rifiutata senza `AllowOtherOrigin`). Il tool aspetta
il task remoto fino alla fine, a una domanda o al timeout (poi lo annulla); il modello
continua una conversazione con `context_id` e risponde a una domanda con `task_id`.
Conta come distruttivo: l'hook `Approve` vede ogni chiamata.
CLI: `<config>/ringil/agents.json` (o `--agents-config`) con `{"agents": {...}}`.
Il lato server (esporre il nostro agente via A2A) arriva con `serve`.

## Decoratori

Tutti opzionali; un campo a zero vuol dire nessun limite. Ordine consigliato,
dall'esterno all'interno:

```go
spend, _ := budget.New(budget.Limits{Calls: 500, Tokens: 2_000_000})
pace, _ := ratelimit.New(ratelimit.Config{RequestsPerMinute: 60, MaxConcurrent: 4})
guard, _ := circuit.New(circuit.Config{})

model := retry.Wrap(guard.Wrap(pace.Wrap(spend.Wrap(p))), retry.Config{})
```

- `retry` fuori da tutto: riprova anche il circuito aperto, aspettando la sonda.
- `circuit` vede ogni tentativo; `ratelimit` li distanzia; `budget` li conta tutti.
- Lo stesso `budget` o `ratelimit` può avvolgere più provider (agente, worker,
  riassuntore) che pescano dalla stessa quota.

## Sviluppo

```sh
make check          # fmt, vet, lint, sicurezza, test
make race
make fuzz FUZZTIME=5m
make live           # prove reali con l'endpoint in .env (AGENT_LIVE_*)
```

## Licenza

MPL-2.0.
