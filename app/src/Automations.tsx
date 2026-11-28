import { useEffect, useState } from "react";
import { api, enc, message } from "./api";
import type { Obj, Profile, Run } from "./types";
import { useI18n, type Key } from "./i18n";
import { Copy, ErrorBox, Markdown } from "./ui";
import { agentTitle } from "./state";
interface Trigger extends Obj {
  name: string;
  kind: string;
  when?: string;
  prompt?: string;
  profile?: string;
  paused?: boolean;
  running?: boolean;
  next?: string;
  status?: { last_run?: string; last_error?: string; last_text?: string };
  has_secret?: boolean;
}
export function schedule(value: string) {
  let m = value
    .toLowerCase()
    .trim()
    .match(
      /^(?:every weekday at|ogni giorno feriale alle) (\d{1,2})(?::(\d{2}))?$/,
    );
  if (m) return `${m[2] ?? 0} ${m[1]} * * 1-5`;
  m = value
    .toLowerCase()
    .trim()
    .match(/^(?:every day at|ogni giorno alle) (\d{1,2})(?::(\d{2}))?$/);
  if (m) return `${m[2] ?? 0} ${m[1]} * * *`;
  m = value
    .toLowerCase()
    .trim()
    .match(/^(?:every|ogni) (\d+) (?:minutes|minuti)$/);
  if (m) return `@every ${m[1]}m`;
  return value.trim();
}
export function Automations({
  profiles,
  onChat,
}: {
  profiles: Profile[];
  onChat: (id: string) => void;
}) {
  const { t } = useI18n();
  const [items, setItems] = useState<Trigger[]>([]);
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState("");
  const [body, setBody] = useState<Obj>({
    kind: "time",
    profile: "default",
    prompt: "",
    when: "",
    time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone,
  });
  const [error, setError] = useState("");
  const [secret, setSecret] = useState("");
  const [advanced, setAdvanced] = useState("");
  const [lastRun, setLastRun] = useState<Run>();
  useEffect(() => {
    if (!lastRun || !["running", "waiting"].includes(lastRun.state)) return;
    const timer = setInterval(
      () =>
        api<Run>(`/v1/runs/${enc(lastRun.id)}`)
          .then(setLastRun)
          .catch((e) => setError(message(e))),
      3000,
    );
    return () => clearInterval(timer);
  }, [lastRun?.id, lastRun?.state]);
  function reload() {
    api<Trigger[]>("/v1/triggers")
      .then((x) => setItems(x ?? []))
      .catch((e) => setError(message(e)));
  }
  useEffect(() => {
    reload();
    const timer = setInterval(() => {
      if (document.visibilityState === "visible") reload();
    }, 8000);
    return () => clearInterval(timer);
  }, []);
  async function action(name: string, action: string) {
    try {
      await api(
        `/v1/triggers/${enc(name)}${action ? "/" + action : ""}`,
        action ? "POST" : "DELETE",
      );
      reload();
    } catch (e) {
      setError(message(e));
    }
  }
  function edit(item?: Trigger) {
    setEditing(true);
    setName(item?.name ?? "");
    if (item) {
      const { name, paused, running, next, status, has_secret, ...rest } = item;
      const {
        kind,
        when,
        prompt,
        profile,
        time_zone,
        tool,
        path,
        chat,
        ...extra
      } = rest;
      setBody({ kind, when, prompt, profile, time_zone, tool, path, chat });
      setAdvanced(JSON.stringify(extra, null, 2));
    } else {
      setBody({
        kind: "time",
        when: "",
        prompt: "",
        profile: "default",
        time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      });
      setAdvanced("{}");
    }
  }
  return (
    <>
      <div className="page-header">
        <h1>{t("automations")}</h1>
        <button className="primary" onClick={() => edit()}>
          {t("create")}
        </button>
      </div>
      <ErrorBox error={error} />
      {secret && (
        <section className="approval">
          <p>{t("secretOnce")}</p>
          <pre>{secret}</pre>
          <Copy text={secret} />
          <button onClick={() => setSecret("")}>{t("close")}</button>
        </section>
      )}
      {editing && (
        <form
          className="surface stack"
          onSubmit={(e) => {
            e.preventDefault();
            setError("");
            try {
              const extra = JSON.parse(advanced) as Obj;
              api<{ secret?: string }>(`/v1/triggers/${enc(name)}`, "PUT", {
                ...extra,
                ...body,
                ...(body.when ? { when: schedule(String(body.when)) } : {}),
              })
                .then((r) => {
                  if (r?.secret) setSecret(r.secret);
                  setEditing(false);
                  reload();
                })
                .catch((e) => setError(message(e)));
            } catch (e) {
              setError(message(e));
            }
          }}
        >
          <label>
            {t("name")}
            <input
              required
              pattern="[A-Za-z0-9_-]{1,48}"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label>
            {t("kind")}
            <select
              value={String(body.kind ?? "time")}
              onChange={(e) => {
                const next: Obj = { ...body, kind: e.target.value };
                if (
                  e.target.value === "webhook" ||
                  e.target.value === "message"
                )
                  delete next.when;
                setBody(next);
              }}
            >
              {(["time", "tool", "file", "message", "webhook"] as const).map(
                (k) => (
                  <option value={k} key={k}>
                    {t(k)}
                  </option>
                ),
              )}
            </select>
          </label>
          {!["message", "webhook"].includes(String(body.kind)) && (
            <>
              <label>
                {t("schedule")}
                <input
                  value={String(body.when ?? "")}
                  onChange={(e) => setBody({ ...body, when: e.target.value })}
                />
              </label>
              <p className="muted">{t("scheduleHelp")}</p>
              <label>
                {t("timezone")}
                <input
                  value={String(body.time_zone ?? "")}
                  onChange={(e) =>
                    setBody({ ...body, time_zone: e.target.value })
                  }
                />
              </label>
            </>
          )}
          {["file", "tool", "message"].includes(String(body.kind)) && (
            <label>
              {t(
                body.kind === "file"
                  ? "path"
                  : body.kind === "tool"
                    ? "tools"
                    : "channels",
              )}
              <input
                required
                value={String(
                  body[
                    body.kind === "file"
                      ? "path"
                      : body.kind === "tool"
                        ? "tool"
                        : "chat"
                  ] ?? "",
                )}
                onChange={(e) =>
                  setBody({
                    ...body,
                    [body.kind === "file"
                      ? "path"
                      : body.kind === "tool"
                        ? "tool"
                        : "chat"]: e.target.value,
                  })
                }
              />
            </label>
          )}
          <label>
            {t("agent")}
            <select
              value={String(body.profile ?? "default")}
              onChange={(e) => setBody({ ...body, profile: e.target.value })}
            >
              {profiles.map((p) => (
                <option key={p.name} value={p.name}>
                  {agentTitle(p, t)}
                </option>
              ))}
            </select>
          </label>
          <label>
            {t("prompt")}
            <textarea
              required
              value={String(body.prompt ?? "")}
              onChange={(e) => setBody({ ...body, prompt: e.target.value })}
            />
          </label>
          <details>
            <summary>{t("jsonHelp")}</summary>
            <label>
              {t("jsonHelp")}
              <textarea
                className="json"
                value={advanced}
                onChange={(e) => setAdvanced(e.target.value)}
              />
            </label>
          </details>
          <div className="actions">
            <button>{t("save")}</button>
            <button type="button" onClick={() => setEditing(false)}>
              {t("cancel")}
            </button>
          </div>
        </form>
      )}
      {items.map((item) => (
        <section className="surface" key={item.name}>
          <h2>{item.name}</h2>
          <p>{item.prompt}</p>
          <p className="muted">
            {item.kind} · {item.when}
          </p>
          {item.next && (
            <p>
              {t("next")}: {new Date(item.next).toLocaleString()}
            </p>
          )}
          {item.status?.last_error && (
            <ErrorBox error={item.status.last_error} />
          )}
          <p>{item.status?.last_text}</p>
          <div className="actions">
            <button onClick={() => edit(item)}>{t("edit")}</button>
            <button
              disabled={item.running}
              onClick={() => action(item.name, "run")}
            >
              {t("runNow")}
            </button>
            <button
              onClick={() =>
                action(item.name, item.paused ? "resume" : "pause")
              }
            >
              {t(item.paused ? "resume" : "pause")}
            </button>
            <button
              onClick={() => {
                if (confirm(t("deleteConfirm"))) void action(item.name, "");
              }}
            >
              {t("delete")}
            </button>
            {item.status?.last_run && (
              <button
                onClick={() =>
                  api<Run>(`/v1/runs/${enc(item.status!.last_run!)}`)
                    .then((r) => {
                      if (r.conversation) onChat(r.conversation);
                      else setLastRun(r);
                    })
                    .catch((e) => setError(message(e)))
                }
              >
                {t("lastRun")}
              </button>
            )}
          </div>
        </section>
      ))}
      {lastRun && (
        <section className="surface" aria-label={t("lastRun")}>
          <div className="page-header">
            <h2>{t("lastRun")}</h2>
            <button onClick={() => setLastRun(undefined)}>{t("close")}</button>
          </div>
          <p>{t(lastRun.state as Key) || lastRun.state}</p>
          <ErrorBox error={lastRun.error?.message ?? ""} />
          {lastRun.text && <Markdown text={lastRun.text} />}
        </section>
      )}
      {!items.length && !editing && <p className="muted">{t("empty")}</p>}
    </>
  );
}
