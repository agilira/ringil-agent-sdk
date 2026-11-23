# La TUI di Ringil — disegno

Data: 2026-11-23. Decisione: plan-core 29f (TUI in Go). Riferimento di comportamento: `docs/tui-fork-map.md` (Codex).

## 1. Che cos'è

L'app di Ringil: installi, rispondi a poche domande, lavori. Opinionata (plan-core 29e e): le scelte arrivano fatte.
Un solo binario `ringil`. Senza argomenti apre la TUI; con un comando fa il resto (`ringil serve`, `ringil run`,
`ringil secrets`...). È il caso d'uso vero dell'SDK: un agente costruito sulla libreria, con i suoi tool e le sue skill.

## 2. Dove vive il codice — da confermare con Antonio

Oggi l'app sta in `ringil-agent-sdk/cmd/ringil` (package main: configurazione, onboarding, serve, canali, segreti).
La TUI ne ha bisogno tutta, e un package main non si importa. Proposta:

- **repo nuovo dell'app** (`ringil`), che importa l'SDK; ci si sposta `cmd/ringil` intero e ci si aggiunge la TUI;
- **l'SDK** resta libreria + esempi piccoli in `examples/` (un agente in 20 righe, un serve, un canale);
- la separazione 29e (b) diventa fisica: chi usa l'SDK non scarica né la TUI né l'onboarding.

## 3. Motore: in-process, con due eccezioni

Interfaccia unica dentro la TUI, due implementazioni:

```
type Engine interface {
    Start(ctx, Prompt) (Run, error)        // eventi in streaming
    Decide(runID, approvalID, Decision) error
    Cancel(runID) error
    Sessions() / Resume(name) / ...        // storia
}
```

- **in-process (default)**: l'agente dell'SDK nello stesso processo; gli eventi (`ringil.Event`) arrivano su un canale Go.
  Niente socket, niente token, niente serializzazione.
- **client di `serve`**: se `ringil serve` gira già (Telegram, trigger, webhook), la TUI si collega a quello
  (loopback + token, eventi SSE di `/v1/runs/{id}/events`) e continua la stessa conversazione dei canali.
- **`/rc`**: dalla TUI passi la conversazione a un canale: se `serve` non gira, la TUI lo avvia in background
  (PID file nella cartella di stato), gli consegna la sessione e si ricollega come client.

Chiudere la TUI in-process ferma il motore; i trigger e i canali vivono solo in `serve`. La TUI lo dice nella
riga di stato («canali spenti: /rc per accenderli»).

## 4. Primo avvio (onboarding nella TUI)

Le stesse domande dell'onboarding di oggi, in schermate della TUI invece che a righe:

1. **Provider**: Anthropic · ChatGPT con l'abbonamento · OpenAI · Google (Gemini) · OpenRouter · Ollama su questa
   macchina · altro server compatibile OpenAI. *Google è nuovo*: Gemini ha un endpoint compatibile OpenAI
   (`generativelanguage.googleapis.com/v1beta/openai`), quindi passa dal provider openai, come OpenRouter.
2. **Chiave** (o login ChatGPT, o niente per Ollama) → salvata nello store di default.
3. **Modello**: dalla lista del provider, con quello consigliato già selezionato.
4. **Prova dal vivo** del modello; poi si lavora.

Tutto il resto ha un default deciso da noi, senza domanda: store dei segreti = keyring del sistema (file cifrato
dove il keyring non c'è), memoria = SQLite locale, sandbox acceso, skill di base installate, Composio/Telegram/A2A
spenti. Ognuno si cambia dopo, con il suo slash command.

## 5. Slash command

Ogni comando che cambia la configurazione la scrive con il ConfigWriter di Argus (commenti e ordine restano) e
ricarica il motore senza uscire. Quelli che toccano i segreti non mostrano mai i valori.

| Comando | Cosa fa |
|---|---|
| `/help` | comandi e tasti |
| `/model` | cambia provider e modello (stesso flusso del primo avvio) |
| `/login`, `/logout` | ChatGPT |
| `/secrets` | elenco dei nomi; aggiungi o togli; **cambia store**: keyring · Vault · file cifrato · env file. Il cambio copia ogni segreto nel nuovo store, lo rilegge e lo confronta, aggiorna `ringil.yaml`, e solo dopo chiede se cancellare dal vecchio |
| `/memory` | cerca, elenca, dimentica; **cambia motore**: SQLite · Chroma · Mem0 (i ricordi esistenti si copiano, con conferma) |
| `/mcp` | server collegati, stato, aggiungi o togli (Composio compreso) |
| `/agents` | agenti remoti A2A |
| `/skills` | skill installate; ognuna è anche un comando `/<skill>` |
| `/sessions`, `/resume`, `/clear` | storia: elenco, riprendi, ricomincia |
| `/compact` | compattazione ora |
| `/tasks` | lavori delegati e workflow in corso |
| `/approvals` | cosa chiede conferma e cosa no |
| `/profile` | ruoli di `ringil.yaml` |
| `/notes` | note di progetto |
| `/config` | apre `ringil.yaml` (percorso) e lo rilegge |
| `/cost` | token e chiamate della sessione |
| `/rc` | passa la conversazione a un canale (sezione 3) |
| `/init` | crea `AGENTS.md` per il progetto |
| `/channels` | Telegram: collega, scollega |

Mancano oggi nell'SDK e vanno fatti: la **copia tra store dei segreti** (le interfacce ci sono: `secrets.Store`
legge e scrive, serve solo il passaggio con verifica) e la **copia tra motori di memoria** (esportazione dei ricordi
con il loro scope).

## 6. Schermata

Inline, non a schermo intero, come Codex: la conversazione finisce nello scrollback del terminale (si scorre e si
copia con il terminale), e in basso resta solo la parte viva.

```
 … conversazione passata nello scrollback …
 ● risposta in streaming (markdown, codice colorato)
 ▸ bash  go test ./...                           ✓ 2.1s
 ▸ edit  serve/http.go  +12 −3   [diff a scomparsa]
┌──────────────────────────────────────────────────────┐
│ ›  il compositore, più righe, @file, /comandi        │
└──────────────────────────────────────────────────────┘
 claude-sonnet-5-5 · ~/progetto · 12k token · canali spenti
```

Approvazioni: un riquadro sopra il compositore con il comando o il diff, `s` sì, `n` no, `a` sempre per questa
sessione; l'input non si contende (è il limite della CLI a righe che la TUI risolve).

## 7. Componenti

| Componente | Cosa | Con |
|---|---|---|
| ciclo e layout | eventi, ridimensionamento, rendering | Bubble Tea v2, Lip Gloss v2 |
| compositore | più righe, storia (freccia su), incolla, `@file` con ricerca, menu `/` | Bubbles (textarea) + nostro |
| markdown in streaming | si rende solo il blocco chiuso, l'ultimo resta grezzo finché non si chiude | goldmark + nostro renderer |
| codice | colori | Chroma |
| diff | unified, colorato, a scomparsa | nostro (l'SDK sa già fare il diff per edit_file) |
| sicurezza del testo | ogni testo di modello, tool, MCP, file passa da `term.Sanitize` di Orpheus prima dello schermo | Orpheus |
| selettori | modello, sessione, store, skill, con ricerca | Bubbles (list) |

**Peso misurato** (Bubble Tea 2.1.2, Lip Gloss 2.1.1, Bubbles 2.2.2, goldmark 1.8.7, Chroma 2.27.0): +34 moduli,
binario da 1,2 a 9,3 MB (quasi tutto i lessici di Chroma), `govulncheck` pulito. Tutte MIT o BSD.

## 8. Cosa serve dall'SDK e da `serve`

- **Eventi**: oggi `text`, `tool_start`, `tool_end`, `model`, `compacted`, `done`, `failed`... Mancano il
  **ragionamento in streaming** (lo mostriamo ridotto, come Codex) e il **diff di una modifica** nell'evento
  `tool_end` di edit/write (per renderlo senza rileggere il file).
- **`serve`**, per il modo client: elenco e ripresa delle sessioni, cambio modello e configurazione ricaricata.
  In-process non servono: la TUI chiama l'SDK.
- **Segreti e memoria**: le due copie con verifica della sezione 5.

## 9. Sicurezza e qualità

- Nessuna uscita di rete della TUI: parlano solo il motore e i provider configurati. Niente telemetria, niente
  controllo aggiornamenti, niente asset scaricati (la mappa di Codex elenca cosa non rifare).
- Sanitize su tutto ciò che va a schermo (fuzz già in Orpheus); link aperti solo su azione dell'utente.
- Ogni pacchetto con `security_test.go`; gocyclo < 10; audit indipendente per componente; `make check`.
- Test: modello della TUI pilotato con messaggi finti e confronto dell'output (golden) per ogni componente;
  una prova dal vivo per fase con un modello piccolo.

## 10. Ordine

1. Repo dell'app e spostamento di `cmd/ringil` (se confermato), CI verde.
2. Scheletro: Engine in-process, conversazione in streaming, compositore, riga di stato.
3. Tool ed approvazioni: blocchi dei tool, diff, riquadro di conferma.
4. Primo avvio nella TUI; `/model`, `/login`.
5. Sessioni, `/compact`, `/cost`, `/skills` e comandi per skill.
6. `/secrets` e `/memory` con il cambio di store e di motore.
7. Modo client di `serve` e `/rc`.
8. Windows e macOS; poi i pacchetti OpenSwarm (Docs e Slides per primi).

## Da decidere con Antonio

1. Repo dell'app separato con `cmd/ringil` spostato dentro (sezione 2).
2. I default della sezione 4 (keyring, SQLite, sandbox acceso, integrazioni spente).
3. Google come scelta del primo avvio, via endpoint compatibile OpenAI.
