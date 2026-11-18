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
