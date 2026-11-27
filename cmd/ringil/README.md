# ringil

L'app ringil: CLI, `serve` (canali, trigger, API) e, in arrivo, la TUI. Usa
[ringil-agent-sdk](https://github.com/agilira/ringil-agent-sdk); le garanzie di sicurezza
sono quelle dell'SDK ([SECURITY.md](https://github.com/agilira/ringil-agent-sdk/blob/main/SECURITY.md)).

Finché l'SDK non ha un tag, l'app lo trova accanto a sé: clona i due repo nella stessa
cartella (`ringil` e `ringil-agent-sdk`).

## CLI

```sh
make chat                                   # chat con le chiavi di ENV_FILE (default ../ringil-agent-sdk/.env)
./ringil run "che ore sono a Tokyo?"
./ringil -S lavoro chat          # sessione salvata: la riapri e riprende
./ringil sessions                # elenco; "sessions rm <nome>" per cancellare
./ringil run "trova dove è definito X"   # tool sui file nella cartella di lancio;
                                                   # per altre cartelle l'agente chiede il permesso
./ringil sandbox --mount ~/dati -- python3 script.py               # esecuzione isolata
```

Flag principali: `--provider`, `--model`, `--instructions`, `--max-turns`, `--max-cost`,
`--workflow-cost`, `--allow-http`, `--no-tools`, `--session`/`-S`, `--sessions-dir`,
`--config`/`-c`.

Il costo in dollari di ogni richiesta compare nella riga di fine turno. I prezzi vengono dal
listino pubblico di OpenRouter (tutti i vendor), scaricato una volta al giorno in
`<data>/ringil/prices.json`; OpenRouter dichiara il costo di ogni chiamata, che vince; i
modelli su questa macchina e l'abbonamento ChatGPT costano zero. `prices:` in ringil.yaml
aggiunge o corregge un modello. `--max-cost` e `--workflow-cost` su un modello senza prezzo
sono rifiutati, perché non fermerebbero nulla.

I modelli sono una lista ordinata, `models:` in ringil.yaml (o `--models`), di
`provider:modello`: il primo risponde, gli altri nell'ordine quando non risponde (server
giù, troppe richieste, risposta interrotta o rotta, chiave rifiutata). Si passa al successivo
subito; il caduto si salta per un minuto (che raddoppia se ricade) e poi si torna a lui. Una
richiesta rifiutata per sé (troppo lunga, non valida) non va a un altro. Con un modello solo
non cambia nulla; un modello senza la sua chiave è lasciato fuori. La conversazione va solo
ai provider della lista; un profilo con i suoi `models:` usa solo quelli. `--provider` e
`--model` mettono un altro modello primo, con la lista dopo di lui.

Con `OTEL_EXPORTER_OTLP_ENDPOINT` (e se serve `OTEL_EXPORTER_OTLP_HEADERS`) ogni esecuzione
diventa una traccia OpenTelemetry: chiamate al modello con token e costo, strumenti, worker;
mai il testo della conversazione. Anche ogni revisione di ringil.yaml (caricata, applicata,
rifiutata) è uno span, con le chiavi cambiate e mai i valori.

`serve.app` in ringil.yaml (la cartella della build di un'app web) la serve sullo stesso
indirizzo dell'API, e all'avvio `ringil serve` stampa il link per entrare (monouso, 10 minuti).
L'app usa le API di `serve` e quelle delle impostazioni (`/v1/settings`): modelli in ordine,
limiti di costo, connessioni, chiavi (si scrivono, non si rileggono), server MCP, skill.
Ogni modifica è controllata sul file intero prima di finire in ringil.yaml.
Avviato da un terminale (o con `--open`, come fa un installer) `ringil serve` apre l'app nel
browser già dentro; `--no-browser` stampa solo il link. Il link si chiede anche dopo, a serve
avviato: `ringil serve link`.

Primo avvio dall'app: `GET /v1/setup` dice cosa manca (un modello, la sua chiave, le istruzioni
dell'utente); `PUT /v1/settings/models`, `PUT /v1/settings/secrets/{NOME}` e
`PUT /v1/settings/instructions` bastano. Le istruzioni dell'utente si aggiungono al prompt di
ringil, non lo sostituiscono (un file, `<config>/ringil/instructions.md` se ringil.yaml non ne
nomina uno). Alla prima chiave, se nessuno store dei segreti è configurato, ringil ne prepara
uno: il keyring del sistema dove funziona, altrimenti il file cifrato in
`<config>/ringil/secrets/` con una passphrase che crea lì (un container, una macchina senza
sessione grafica): accanto al file, protegge solo quando il file viene copiato da solo, non da
chi legge tutta la cartella, come un file privato. Né le chiavi né la passphrase entrano in un
export, e gli strumenti sui file dell'agente non le vedono.

In Docker:

```sh
docker build -t ringil .
docker run -d --name ringil -p 127.0.0.1:8765:8765 -v ringil:/data ringil
docker exec ringil ringil serve link
```

Nel container serve ascolta su tutte le sue interfacce (`serve.container`, o
`RINGIL_CONTAINER=1` dentro un container, come imposta l'immagine) ma risponde solo al
loopback e all'host, cioè al gateway del container (`serve.peers` per cambiarlo): un altro
container sulla rete, o la LAN se la porta è pubblicata per errore su tutte le interfacce,
è rifiutato prima del token. La porta va pubblicata solo sul loopback dell'host. Lo stato sta
nel volume `/data`, che sopravvive al container e agli aggiornamenti.

```yaml
models:
  - anthropic:claude-opus-5-5
  - openrouter:anthropic/claude-sonnet-5.5
  - ollama:qwen3:8b
```

### ringil.yaml

Tutta la configurazione sta in un file, `<config>/ringil/ringil.yaml` (o `--config`):
impostazioni, profili, server MCP, agenti remoti e `serve`. Le chiavi sono i nomi dei
flag con `_` al posto di `-`; un flag vince sul file per quel comando, e
`RINGIL_PROVIDER`, `RINGIL_MODEL`, `RINGIL_EMBED_MODEL` nell'ambiente pure. I segreti
non ci stanno mai: `key_env` e `${NOME}` dicono quale variabile li contiene. Il file
dev'essere tuo e non scrivibile da altri; i percorsi relativi delle istruzioni e di
`sessions_dir` partono dalla sua cartella. Esempio completo: `docs/ringil.example.yaml`.

Il provider di ogni modello si nomina prima dei due punti (`anthropic:…`, `--provider`, `RINGIL_PROVIDER`).
Pronti senza configurazione, con il segreto della chiave: `anthropic` (`ANTHROPIC_API_KEY`),
`chatgpt` (abbonamento, `ringil login chatgpt`), `openai` (`OPENAI_API_KEY`), `google`
(Gemini, `GEMINI_API_KEY`), `openrouter`, `nvidia` (NIM), `deepseek`, `requesty`, `zai`,
`moonshot`, `xai` (Grok), `mistral` (`<NOME>_API_KEY`), `ollama` (questa macchina, senza chiave). Altri, quanti se ne
vuole, nella sezione `providers:` con un nome, l'indirizzo e il segreto della chiave; una
voce con il nome di uno pronto ne cambia indirizzo o segreto. Profili e modelli audio
scelgono il loro provider per nome. Ogni chiave va solo al suo provider.

Una chiave sconosciuta è un errore, non un default silenzioso. Il file, i file di
istruzioni che nomina e le skill installate si leggono insieme, come una revisione
(Argus): mentre gira, ringil segue le modifiche di tutti; una che passa i controlli
vale dalla prossima run (profili, istruzioni, impostazioni, chi parla con il bot), una
che non li passa è segnalata e resta la revisione precedente. Ogni run di `serve` usa
una sola revisione dall'inizio alla fine e il suo log ne riporta l'impronta
(`config=...`); ogni revisione finisce in `audit.db` (`audit_file`, `no_audit`) per
nome e hash, mai il testo: SQLite con la catena di hash di Argus, che scopre un record
cambiato o tolto (un file `.jsonl` è a righe, senza catena). Un percorso di istruzioni non può contenere `*`, `?` o `[`. Server MCP, agenti remoti e il resto di `serve` valgono al
prossimo avvio, e il log lo dice. Gli strumenti sui file dell'agente non toccano
`ringil.yaml` né i file di istruzioni senza chiedere.


## Sviluppo

```sh
make check          # fmt, vet, lint, sicurezza, test
make race
make fuzz FUZZTIME=5m
```

## Licenza

MPL-2.0.
