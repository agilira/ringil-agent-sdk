import { useEffect, useState } from "react";
import { api, enc, message } from "./api";
import type { Settings } from "./types";
import { useI18n } from "./i18n";
import { ErrorBox, RestartControl } from "./ui";
import { RemoteAgents } from "./RemoteAgents";
export function Connections() {
  const { t } = useI18n();
  const [data, setData] = useState<Settings>();
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [command, setCommand] = useState("");
  const [skill, setSkill] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  function reload() {
    api<Settings>("/v1/settings")
      .then(setData)
      .catch((e) => setError(message(e)));
  }
  useEffect(reload, []);
  return (
    <>
      <h1>{t("connections")}</h1>
      <ErrorBox error={error} />
      <p role="status">{notice}</p>
      <section className="surface">
        <h2>{t("mcp")}</h2>
        {data?.mcp_servers?.map((s) => (
          <div className="list-row" key={s.name}>
            <div>
              <strong>{s.name}</strong>
              <p className="muted">
                {String(
                  s.url ?? (s.command as string[] | undefined)?.join(" ") ?? "",
                )}
              </p>
            </div>
            <button
              onClick={() => {
                if (confirm(t("deleteConfirm")))
                  api(`/v1/settings/mcp_servers/${enc(s.name)}`, "DELETE")
                    .then(reload)
                    .catch((e) => setError(message(e)));
              }}
            >
              {t("delete")}
            </button>
          </div>
        ))}
        <form
          className="stack"
          onSubmit={(e) => {
            e.preventDefault();
            setError("");
            api<{ warning?: string; missing?: string[]; tools?: string[] }>(
              "/v1/settings/mcp_servers",
              "POST",
              {
                name,
                ...(url
                  ? { url }
                  : { command: command.split("\n").filter(Boolean) }),
              },
            )
              .then((r) => {
                setNotice(
                  [r.warning, ...(r.missing ?? [])]
                    .filter(Boolean)
                    .join(" · ") || t("saved"),
                );
                setName("");
                setUrl("");
                setCommand("");
                reload();
              })
              .catch((e) => setError(message(e)));
          }}
        >
          <label>
            {t("name")}
            <input
              required
              pattern="[A-Za-z0-9_-]+"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label>
            {t("url")}
            <input
              type="url"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
            />
          </label>
          <label>
            {t("command")}
            <textarea
              disabled={!!url}
              value={command}
              onChange={(e) => setCommand(e.target.value)}
            />
          </label>
          <button>{t("create")}</button>
        </form>
      </section>
      <section className="surface">
        <h2>{t("skills")}</h2>
        {data?.skills?.map((s) => (
          <div className="list-row" key={s.name}>
            <div>
              <strong>{s.name}</strong>
              <p>{s.description}</p>
            </div>
            <button
              onClick={() => {
                if (confirm(t("deleteConfirm")))
                  api(`/v1/settings/skills/${enc(s.name)}`, "DELETE")
                    .then(reload)
                    .catch((e) => setError(message(e)));
              }}
            >
              {t("delete")}
            </button>
          </div>
        ))}
        <form
          className="stack"
          onSubmit={(e) => {
            e.preventDefault();
            api("/v1/settings/skills", "POST", { url: skill })
              .then(() => {
                setSkill("");
                reload();
              })
              .catch((e) => setError(message(e)));
          }}
        >
          <label>
            {t("url")}
            <input
              type="url"
              required
              value={skill}
              onChange={(e) => setSkill(e.target.value)}
            />
          </label>
          <button>{t("install")}</button>
        </form>
      </section>
      <RemoteAgents />
      <p className="muted">{t("restartHelp")}</p>
      <RestartControl onRestart={reload} />
    </>
  );
}
