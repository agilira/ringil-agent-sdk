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

Flag principali: `--base`, `--model`, `--key-env`, `--system`, `--max-turns`,
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

A ogni domanda i ricordi pertinenti entrano nel contesto di quel turno, delimitati e
marcati come dati, mai nella conversazione salvata. Nella CLI la memoria è attiva di
default (`--no-memory` per toglierla); `ringil memory`, `memory search`, `memory rm`.

## Skill

Formato standard Agent Skills: una cartella con `SKILL.md` (front matter `name`,
`description`) più file di riferimento e script. Installare è un gesto dell'utente,
mai del modello; ogni file ha il suo SHA-256 e una skill modificata dopo
l'installazione viene rifiutata.

```go
info, err := skills.Install(ctx, "examples/skills/review", dir)
cat, err := skills.Open(dir)
cfg.System += cat.Instructions()                     // solo nomi e descrizioni
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
origini mai seguiti, OAuth con l'handler dell'SDK. Le annotazioni seguono i default MCP:
un tool non marcato read-only è distruttivo, quindi passa dall'hook `Approve`.

CLI: `<config>/ringil/mcp.json` (o `--mcp-config`) nel formato comune
`{"mcpServers": {...}}`; nei valori `${NOME}` viene risolto da ambiente ed `--env-file`.
Per un server locale `"sandbox": true` con `"network"` e `"mounts"` lo isola.
Esempio di server: `mcp/examples/notes`.

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
