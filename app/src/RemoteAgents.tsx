import { useEffect, useState } from "react";
import { api, enc, message } from "./api";
import type { Obj } from "./types";
import { useI18n } from "./i18n";
import { ErrorBox } from "./ui";
// RemoteAgents are the agents of other servers this one calls over A2A,
// and whether it offers its own agent there.
export function RemoteAgents() {
  const { t } = useI18n();
  const [remote, setRemote] = useState<({ name: string; url: string } & Obj)[]>(
    [],
  );
  const [expose, setExpose] = useState(false);
  const [error, setError] = useState("");
  const [remoteName, setRemoteName] = useState("");
  const [remoteUrl, setRemoteUrl] = useState("");
  const [remoteHeaders, setRemoteHeaders] = useState("{}");
  const [remoteExtra, setRemoteExtra] = useState<Obj>({});
  const [remoteKey, setRemoteKey] = useState("");
  async function saveRemote() {
    try {
      const headers = JSON.parse(remoteHeaders) as Record<string, string>;
      if (remoteKey) {
        const secretName =
          "RINGIL_REMOTE_" +
          remoteName.toUpperCase().replace(/[^A-Z0-9_]/g, "_") +
          "_TOKEN";
        await api(`/v1/settings/secrets/${enc(secretName)}`, "PUT", {
          value: remoteKey,
        });
        setRemoteKey("");
        headers.Authorization = "Bearer ${" + secretName + "}";
      }
      await api(`/v1/settings/remote_agents/${enc(remoteName)}`, "PUT", {
        ...remoteExtra,
        url: remoteUrl,
        headers,
      });
      setRemoteName("");
      setRemoteUrl("");
      setRemoteHeaders("{}");
      setRemoteExtra({});
      reload();
    } catch (e) {
      setError(message(e));
    }
  }

  function reload() {
    api<{ agents: typeof remote; expose: boolean }>(
      "/v1/settings/remote_agents",
    )
      .then((r) => {
        setRemote(r.agents ?? []);
        setExpose(r.expose);
      })
      .catch((e) => setError(message(e)));
  }
  useEffect(reload, []);
  return (
    <section className="surface">
      <h2>{t("remote")}</h2>
      <ErrorBox error={error} />
      {remote.map((a) => (
        <div className="list-row" key={a.name}>
          <p>
            {a.name}
            <br />
            <span className="muted">{a.url}</span>
          </p>
          <div className="actions">
            <button
              onClick={() => {
                const { name, url, headers, ...rest } = a;
                setRemoteName(name);
                setRemoteUrl(url);
                setRemoteHeaders(JSON.stringify(headers ?? {}, null, 2));
                setRemoteExtra(rest);
                setRemoteKey("");
              }}
            >
              {t("edit")}
            </button>
            <button
              onClick={() =>
                api(`/v1/settings/remote_agents/${enc(a.name)}`, "PUT", {
                  ...a,
                  disabled: !a.disabled,
                })
                  .then(reload)
                  .catch((e) => setError(message(e)))
              }
            >
              {t(a.disabled ? "resume" : "pause")}
            </button>
            <button
              onClick={() => {
                if (confirm(t("deleteConfirm")))
                  api(`/v1/settings/remote_agents/${enc(a.name)}`, "DELETE")
                    .then(reload)
                    .catch((e) => setError(message(e)));
              }}
            >
              {t("delete")}
            </button>
          </div>
        </div>
      ))}
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          void saveRemote();
        }}
      >
        <label>
          {t("name")}
          <input
            required
            value={remoteName}
            onChange={(e) => setRemoteName(e.target.value)}
          />
        </label>
        <label>
          {t("url")}
          <input
            type="url"
            required
            value={remoteUrl}
            onChange={(e) => setRemoteUrl(e.target.value)}
          />
        </label>
        <label>
          {t("key")}
          <input
            type="password"
            autoComplete="off"
            value={remoteKey}
            onChange={(e) => setRemoteKey(e.target.value)}
          />
        </label>
        <details>
          <summary>{t("headersHelp")}</summary>
          <label>
            {t("jsonHelp")}
            <textarea
              className="json"
              value={remoteHeaders}
              onChange={(e) => setRemoteHeaders(e.target.value)}
            />
          </label>
        </details>
        <button>{t("save")}</button>
      </form>
      <label>
        <input
          type="checkbox"
          checked={expose}
          onChange={(e) =>
            api("/v1/settings/a2a", "PUT", { expose: e.target.checked })
              .then(reload)
              .catch((e) => setError(message(e)))
          }
        />
        {t("expose")}
      </label>
    </section>
  );
}
