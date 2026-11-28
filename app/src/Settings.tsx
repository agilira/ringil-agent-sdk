import { waitForServer } from "./restart";
import { Channels } from "./Channels";
import { useEffect, useState, type FormEvent } from "react";
import { api, enc, message, response } from "./api";
import type { Settings as SettingsData, Provider, Obj } from "./types";
import { useI18n, type Key, type Language } from "./i18n";
import { Logo } from "./icons";
import { ErrorBox, JsonEditor } from "./ui";
export function Setup({ onReady }: { onReady: () => void }) {
  const { t } = useI18n();
  const [settings, setSettings] = useState<SettingsData>();
  const [provider, setProvider] = useState("");
  const [key, setKey] = useState("");
  const [model, setModel] = useState("");
  const [models, setModels] = useState<string[]>([]);
  const [instructions, setInstructions] = useState("");
  const [step, setStep] = useState(0);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    api<SettingsData>("/v1/settings")
      .then((s) => {
        setSettings(s);
        setProvider(s.models?.[0]?.split(":")[0] || s.providers[0]?.name || "");
      })
      .catch((e) => setError(message(e)));
  }, []);
  async function next(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (step === 0) {
        const p = settings?.providers.find((p) => p.name === provider);
        if (key && p?.key_env) {
          await api(`/v1/settings/secrets/${enc(p.key_env)}`, "PUT", {
            value: key,
          });
          setKey("");
        }
        const data = await api<{ models: string[] }>(
          `/v1/settings/providers/${enc(provider)}/models`,
        );
        setModels(data.models ?? []);
        setModel(data.models?.[0] || "");
        setStep(1);
      } else if (step === 1) {
        await api("/v1/settings/models", "PUT", {
          models: [`${provider}:${model}`],
        });
        setStep(2);
      } else {
        if (instructions.trim())
          await api("/v1/settings/instructions", "PUT", { text: instructions });
        onReady();
      }
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <main className="signin">
      <div className="sigil">
        <Logo size={40} />
      </div>
      <h1>{t("setup")}</h1>
      <p>{t("setupText")}</p>
      <form className="stack" onSubmit={next}>
        {step === 0 ? (
          <>
            <label>
              {t("provider")}
              <select
                value={provider}
                onChange={(e) => setProvider(e.target.value)}
              >
                {settings?.providers.map((p) => (
                  <option key={p.name}>{p.name}</option>
                ))}
              </select>
            </label>
            {provider === "chatgpt" ? (
              <p>{t("chatgptTerminal")}</p>
            ) : (
              settings?.providers.find((p) => p.name === provider)?.key_env && (
                <label>
                  {t("key")}
                  <input
                    type="password"
                    autoComplete="off"
                    value={key}
                    onChange={(e) => setKey(e.target.value)}
                  />
                </label>
              )
            )}
          </>
        ) : step === 1 ? (
          <label>
            {t("model")}
            <input
              required
              list="setup-models"
              value={model}
              onChange={(e) => setModel(e.target.value)}
            />
            <datalist id="setup-models">
              {models.map((m) => (
                <option key={m}>{m}</option>
              ))}
            </datalist>
          </label>
        ) : (
          <label>
            {t("instructions")}
            <textarea
              value={instructions}
              onChange={(e) => setInstructions(e.target.value)}
            />
          </label>
        )}
        <ErrorBox error={error} />
        <button className="primary" disabled={busy}>
          {t(step === 2 ? "save" : "continue")}
        </button>
        {step === 2 && (
          <button type="button" onClick={onReady}>
            {t("skip")}
          </button>
        )}
      </form>
    </main>
  );
}
// The settings sections, in the order the sidebar lists them.
export const settingsTabs: Key[] = [
  "models",
  "keys",
  "limits",
  "instructions",
  "voice",
  "memory",
  "channels",
  "composio",
  "hooks",
  "state",
  "appearance",
];
export function Settings({
  tab,
  language,
  theme,
  onLanguage,
  onTheme,
  onRefresh,
}: {
  tab: Key;
  language: Language;
  theme: string;
  onLanguage: (l: Language) => void;
  onTheme: (s: string) => void;
  onRefresh: () => void;
}) {
  const { t } = useI18n();
  const [data, setData] = useState<SettingsData>();
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [models, setModels] = useState("");
  const [max, setMax] = useState(0);
  const [workflow, setWorkflow] = useState(0);
  const [instructions, setInstructions] = useState("");
  const [provider, setProvider] = useState("");
  const [key, setKey] = useState("");
  const [restarting, setRestarting] = useState(false);
  const [memories, setMemories] = useState<
    { id: string; content: string; created: string }[]
  >([]);
  const [query, setQuery] = useState("");
  const [memory, setMemory] = useState("");
  function reload() {
    api<SettingsData>("/v1/settings")
      .then((s) => {
        setData(s);
        setModels((s.models ?? []).join("\n"));
        setMax(s.max_cost ?? 0);
        setWorkflow(s.workflow_cost ?? 0);
        setProvider((p) => p || s.providers[0]?.name || "");
      })
      .catch((e) => setError(message(e)));
  }
  function search() {
    api<typeof memories>(`/v1/memories?q=${enc(query)}`)
      .then((m) => setMemories(m ?? []))
      .catch((e) => setError(message(e)));
  }
  useEffect(reload, []);
  useEffect(() => {
    setSaved(false);
    setError("");
    if (tab === "instructions")
      api<{ text: string }>("/v1/settings/instructions")
        .then((x) => setInstructions(x.text))
        .catch((e) => setError(message(e)));
    if (tab === "memory") search();
  }, [tab]);
  async function save(path: string, body: unknown) {
    setError("");
    setSaved(false);
    try {
      await api(path, "PUT", body);
      setSaved(true);
      onRefresh();
      reload();
    } catch (e) {
      setError(message(e));
    }
  }
  async function restart() {
    setRestarting(true);
    setError("");
    try {
      await api("/v1/restart", "POST");
      await waitForServer(t("serverTimeout"));
      onRefresh();
      setSaved(true);
    } catch (e) {
      setError(message(e));
    } finally {
      setRestarting(false);
    }
  }
  async function exportState() {
    try {
      const r = await response("/v1/state/export");
      const url = URL.createObjectURL(await r.blob());
      const a = document.createElement("a");
      a.href = url;
      a.download = "ringil-state.tar.gz";
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) {
      setError(message(e));
    }
  }
  async function importState(file: File) {
    if (!confirm(t("importConfirm"))) return;
    setRestarting(true);
    try {
      await response("/v1/state/import", {
        method: "POST",
        headers: { "Content-Type": "application/gzip" },
        body: file,
      });
      await waitForServer(t("serverTimeout"));
      const result = await api<{ ok: boolean; error?: string }>(
        "/v1/state/import",
      );
      if (!result.ok) throw new Error(result.error || t("error"));
      reload();
      onRefresh();
      setSaved(true);
    } catch (e) {
      setError(message(e));
    } finally {
      setRestarting(false);
    }
  }
  const p = data?.providers.find((p) => p.name === provider);
  return (
    <>
      <div className="page-header">
        <h1>{t(tab)}</h1>
        <button
          onClick={() =>
            api("/v1/signout", "POST")
              .then(() => window.dispatchEvent(new Event("signedout")))
              .catch((e) => setError(message(e)))
          }
        >
          {t("signout")}
        </button>
      </div>
      <ErrorBox error={error} />
      <p role="status">
        {restarting ? t("restarting") : saved ? t("saved") : ""}
      </p>
      <section aria-label={t(tab)}>
        {tab === "appearance" ? (
          <div className="surface stack">
            <label>
              {t("language")}
              <select
                value={language}
                onChange={(e) => onLanguage(e.target.value as Language)}
              >
                <option value="en">English</option>
                <option value="it">Italiano</option>
              </select>
            </label>
            <label>
              {t("theme")}
              <select value={theme} onChange={(e) => onTheme(e.target.value)}>
                {(["light", "dark", "system"] as const).map((k) => (
                  <option value={k} key={k}>
                    {t(k)}
                  </option>
                ))}
              </select>
            </label>
          </div>
        ) : tab === "models" ? (
          <form
            className="surface stack"
            onSubmit={(e) => {
              e.preventDefault();
              void save("/v1/settings/models", {
                models: models
                  .split("\n")
                  .map((s) => s.trim())
                  .filter(Boolean),
              });
            }}
          >
            <p>{t("modelsHelp")}</p>
            <label>
              {t("models")}
              <textarea
                value={models}
                onChange={(e) => setModels(e.target.value)}
              />
            </label>
            <button>{t("save")}</button>
          </form>
        ) : tab === "keys" ? (
          <form
            className="surface stack"
            onSubmit={(e) => {
              e.preventDefault();
              if (p?.key_env)
                void save(`/v1/settings/secrets/${enc(p.key_env)}`, {
                  value: key,
                }).then(() => setKey(""));
            }}
          >
            <label>
              {t("provider")}
              <select
                value={provider}
                onChange={(e) => setProvider(e.target.value)}
              >
                {data?.providers.map((p) => (
                  <option key={p.name}>{p.name}</option>
                ))}
              </select>
            </label>
            <p>{t(p?.key_set ? "keySet" : "keyMissing")}</p>
            {provider === "chatgpt" ? (
              <p>{t("chatgptTerminal")}</p>
            ) : (
              p?.key_env && (
                <>
                  <label>
                    {t("key")}
                    <input
                      type="password"
                      autoComplete="off"
                      required
                      value={key}
                      onChange={(e) => setKey(e.target.value)}
                    />
                  </label>
                  <button>{t("save")}</button>
                  <button
                    type="button"
                    onClick={() =>
                      api(`/v1/settings/secrets/${enc(p.key_env!)}`, "DELETE")
                        .then(reload)
                        .catch((e) => setError(message(e)))
                    }
                  >
                    {t("delete")}
                  </button>
                </>
              )
            )}
          </form>
        ) : tab === "limits" ? (
          <form
            className="surface stack"
            onSubmit={(e) => {
              e.preventDefault();
              void save("/v1/settings/limits", {
                max_cost: max,
                workflow_cost: workflow,
              });
            }}
          >
            <label>
              {t("maxCost")}
              <input
                type="number"
                min="0"
                step="0.01"
                value={max}
                onChange={(e) => setMax(Number(e.target.value))}
              />
            </label>
            <label>
              {t("workflowCost")}
              <input
                type="number"
                min="0"
                step="0.01"
                value={workflow}
                onChange={(e) => setWorkflow(Number(e.target.value))}
              />
            </label>
            <button>{t("save")}</button>
          </form>
        ) : tab === "instructions" ? (
          <form
            className="surface stack"
            onSubmit={(e) => {
              e.preventDefault();
              void save("/v1/settings/instructions", { text: instructions });
            }}
          >
            <label>
              {t("instructions")}
              <textarea
                rows={12}
                value={instructions}
                onChange={(e) => setInstructions(e.target.value)}
              />
            </label>
            <button>{t("save")}</button>
          </form>
        ) : tab === "voice" ? (
          <Voice data={data} save={save} />
        ) : tab === "memory" ? (
          <>
            <form
              className="surface stack"
              onSubmit={(e) => {
                e.preventDefault();
                search();
              }}
            >
              <label>
                {t("search")}
                <input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                />
              </label>
              <button>{t("search")}</button>
            </form>
            <form
              className="surface stack"
              onSubmit={(e) => {
                e.preventDefault();
                api("/v1/memories", "POST", { content: memory })
                  .then(() => {
                    setMemory("");
                    search();
                  })
                  .catch((e) => setError(message(e)));
              }}
            >
              <label>
                {t("content")}
                <textarea
                  required
                  value={memory}
                  onChange={(e) => setMemory(e.target.value)}
                />
              </label>
              <button>{t("create")}</button>
              {memories.map((m) => (
                <div className="list-row" key={m.id}>
                  <p>{m.content}</p>
                  <button
                    type="button"
                    onClick={() =>
                      api(`/v1/memories/${enc(m.id)}`, "DELETE")
                        .then(search)
                        .catch((e) => setError(message(e)))
                    }
                  >
                    {t("forget")}
                  </button>
                </div>
              ))}
            </form>
            <JsonEditor path="/v1/settings/memory" title={t("memoryStore")} />
          </>
        ) : tab === "channels" ? (
          <Channels />
        ) : tab === "hooks" ? (
          <JsonEditor
            path="/v1/settings/hooks"
            title={t("hooks")}
            heading={false}
          />
        ) : tab === "composio" ? (
          <Composio />
        ) : tab === "state" ? (
          <div className="surface stack">
            <button onClick={exportState}>{t("export")}</button>
            <label>
              {t("import")}
              <input
                type="file"
                accept=".gz,.tgz"
                onChange={(e) => {
                  if (e.target.files?.[0]) void importState(e.target.files[0]);
                  e.target.value = "";
                }}
              />
            </label>
          </div>
        ) : null}
      </section>
      <div className="surface">
        <p>{t("restartHelp")}</p>
        <button disabled={restarting} onClick={restart}>
          {t("restart")}
        </button>
      </div>
    </>
  );
}
function Voice({
  data,
  save,
}: {
  data?: SettingsData;
  save: (path: string, body: unknown) => Promise<void>;
}) {
  const { t } = useI18n();
  const [transcribe, setTranscribe] = useState("");
  const [speech, setSpeech] = useState("");
  const [voice, setVoice] = useState("");
  useEffect(() => {
    if (data) {
      setTranscribe(
        data.transcribe.model
          ? `${data.transcribe.provider}:${data.transcribe.model}`
          : "",
      );
      setSpeech(
        data.speech.model ? `${data.speech.provider}:${data.speech.model}` : "",
      );
      setVoice(String(data.speech.voice ?? ""));
    }
  }, [data]);
  function model(s: string) {
    const i = s.indexOf(":");
    return i < 0 ? {} : { provider: s.slice(0, i), model: s.slice(i + 1) };
  }
  return (
    <form
      className="surface stack"
      onSubmit={(e) => {
        e.preventDefault();
        void save("/v1/settings/voice", {
          transcribe: model(transcribe),
          speech: speech
            ? { ...model(speech), ...(voice ? { voice } : {}) }
            : {},
        });
      }}
    >
      <label>
        {t("mic")} (provider:model)
        <input
          value={transcribe}
          onChange={(e) => setTranscribe(e.target.value)}
        />
      </label>
      <label>
        {t("read")} (provider:model)
        <input value={speech} onChange={(e) => setSpeech(e.target.value)} />
      </label>
      <label>
        {t("voice")}
        <input value={voice} onChange={(e) => setVoice(e.target.value)} />
      </label>
      <button>{t("save")}</button>
    </form>
  );
}
function Composio() {
  const { t } = useI18n();
  const [key, setKey] = useState("");
  const [set, setSet] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    api<{ key_set: boolean }>("/v1/settings/composio")
      .then((x) => setSet(x.key_set))
      .catch((e) => setError(message(e)));
  }, []);
  return (
    <form
      className="surface stack"
      onSubmit={(e) => {
        e.preventDefault();
        api("/v1/settings/composio", "PUT", { key })
          .then(() => {
            setKey("");
            setSet(true);
          })
          .catch((e) => setError(message(e)));
      }}
    >
      <p>{t(set ? "keySet" : "keyMissing")}</p>
      <label>
        {t("key")}
        <input
          type="password"
          autoComplete="off"
          required
          value={key}
          onChange={(e) => setKey(e.target.value)}
        />
      </label>
      <ErrorBox error={error} />
      <button>{t("save")}</button>
      <button
        type="button"
        onClick={() =>
          api("/v1/settings/composio", "DELETE")
            .then(() => setSet(false))
            .catch((e) => setError(message(e)))
        }
      >
        {t("delete")}
      </button>
    </form>
  );
}
