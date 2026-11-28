import { useEffect, useState } from "react";
import { api, enc, message } from "./api";
import { useI18n } from "./i18n";
import type { Obj, Profile } from "./types";
import { ErrorBox } from "./ui";
import { agentTitle } from "./state";
type Channel = {
  owners: string[];
  members: string[];
  token_set?: boolean;
  running?: boolean;
  company_group?: string;
  member_phones?: string[];
};
interface ChannelsData extends Obj {
  telegram: Channel;
  slack: Channel;
  member_profile?: string;
  public_profile?: string;
}
export function Channels() {
  const { t } = useI18n();
  const [data, setData] = useState<ChannelsData>();
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const [tokens, setTokens] = useState<Record<string, string>>({});
  function load() {
    Promise.all([
      api<ChannelsData>("/v1/settings/channels"),
      api<Profile[]>("/v1/profiles"),
    ])
      .then(([d, p]) => {
        setData(d);
        setProfiles(p);
      })
      .catch((e) => setError(message(e)));
  }
  useEffect(load, []);
  async function save() {
    if (!data) return;
    setError("");
    try {
      for (const [name, value] of Object.entries(tokens)) {
        if (value)
          await api(`/v1/settings/secrets/${enc(name)}`, "PUT", { value });
      }
      setTokens({});
      const clean = {
        ...data,
        telegram: { ...data.telegram },
        slack: { ...data.slack },
      };
      for (const k of ["telegram", "slack"] as const) {
        delete clean[k].token_set;
        delete clean[k].running;
      }
      await api("/v1/settings/channels", "PUT", clean);
      setSaved(true);
      load();
    } catch (e) {
      setError(message(e));
    }
  }
  return (
    <form
      className="stack"
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      <ErrorBox error={error} />
      {data && (
        <>
          {(["telegram", "slack"] as const).map((kind) => (
            <section className="surface stack" key={kind}>
              <h2>{kind === "telegram" ? "Telegram" : "Slack"}</h2>
              <p>
                {t(data[kind].token_set ? "keySet" : "keyMissing")} ·{" "}
                {t(data[kind].running ? "running" : "off")}
              </p>
              {(["owners", "members"] as const).map((field) => (
                <label key={field}>
                  {t(field)}
                  <textarea
                    value={data[kind][field].join("\n")}
                    onChange={(e) => {
                      setData({
                        ...data,
                        [kind]: {
                          ...data[kind],
                          [field]: e.target.value.split("\n").filter(Boolean),
                        },
                      });
                      setSaved(false);
                    }}
                  />
                </label>
              ))}
              {(kind === "telegram"
                ? ["RINGIL_TELEGRAM_TOKEN"]
                : ["SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"]
              ).map((name) => (
                <label key={name}>
                  {name}
                  <input
                    type="password"
                    autoComplete="off"
                    value={tokens[name] ?? ""}
                    onChange={(e) =>
                      setTokens({ ...tokens, [name]: e.target.value })
                    }
                  />
                </label>
              ))}
            </section>
          ))}
          <section className="surface stack">
            {(["member_profile", "public_profile"] as const).map((field) => (
              <label key={field}>
                {t(field === "member_profile" ? "memberAgent" : "publicAgent")}
                <select
                  value={data[field] ?? ""}
                  onChange={(e) =>
                    setData({ ...data, [field]: e.target.value })
                  }
                >
                  <option value="">{t("unassigned")}</option>
                  {profiles
                    .filter((p) => p.name !== "default")
                    .map((p) => (
                      <option key={p.name} value={p.name}>
                        {agentTitle(p, t)}
                      </option>
                    ))}
                </select>
              </label>
            ))}
          </section>
          <button>{t("save")}</button>
          <p role="status">{saved ? t("saved") : ""}</p>
          <p className="muted">{t("restartHelp")}</p>
        </>
      )}
    </form>
  );
}
