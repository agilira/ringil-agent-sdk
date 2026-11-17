# ringil-agent-sdk

SDK Go per costruire agenti: un modello, dei tool, un ciclo che li alterna, una
conversazione che sopravvive al processo.

## Installazione

```sh
go get github.com/agilira/ringil-agent-sdk
```

Richiede Go 1.27.

## Uso

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
```

## Licenza

MPL-2.0.
