import { useEffect, useState } from "react";
import { api, enc, message } from "./api";
import type { Profile, Obj } from "./types";
import { useI18n } from "./i18n";
import { ErrorBox, Markdown } from "./ui";
import { agentTitle, agentID } from "./state";
// Agents lists the user's agents, the main one first; one opened is edited
// in place: its name, description and instructions, and for the others
// their models, tools and memory under Advanced.
export function Agents({ onRefresh }: { onRefresh: () => void }) {
  const { t } = useI18n();
  const [items, setItems] = useState<Profile[]>([]);
  // open is the agent being edited: null for none, {} for a new one.
  const [open, setOpen] = useState<Profile | null>(null);
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [models, setModels] = useState("");
  const [instructions, setInstructions] = useState("");
  const [tools, setTools] = useState("");
  const [noTools, setNoTools] = useState(false);
  const [memory, setMemory] = useState("");
  const [extra, setExtra] = useState<Obj>({});
  const [preview, setPreview] = useState(false);
  const [error, setError] = useState("");
  function reload() {
    api<Profile[]>("/v1/settings/profiles")
      .then((p) => setItems(p ?? []))
      .catch((e) => setError(message(e)));
  }
  useEffect(reload, []);
  function edit(p: Profile | null) {
    setOpen(p);
    setError("");
    setPreview(false);
    setTitle(p?.title ?? (p?.main ? "" : (p?.name ?? "")));
    setDescription(p?.description ?? "");
    setModels(p?.models?.join("\n") ?? "");
    setInstructions(p?.instructions ?? "");
    setTools(p?.tools?.join("\n") ?? "");
    setNoTools(Array.isArray(p?.tools) && p.tools.length === 0);
    setMemory(p?.memory ?? "");
    const {
      name,
      title,
      main,
      description,
      models,
      instructions,
      instructions_path,
      tools,
      memory,
      ...rest
    } = (p ?? {}) as unknown as Obj;
    setExtra(rest);
  }
  function done() {
    setOpen(null);
    reload();
    onRefresh();
  }
  if (open) {
    const main = open.main === true;
    const id =
      open.name ||
      agentID(
        title,
        items.map((p) => p.name),
      );
    return (
      <>
        <div className="page-header">
          <h1>{open.name ? agentTitle(open, t) : t("newAgent")}</h1>
        </div>
        <ErrorBox error={error} />
        <form
          className="surface stack"
          onSubmit={(e) => {
            e.preventDefault();
            api(
              `/v1/settings/profiles/${enc(id)}`,
              "PUT",
              main
                ? { title: title.trim(), description, instructions }
                : {
                    ...extra,
                    title: title.trim(),
                    description,
                    models: models.split("\n").filter(Boolean),
                    instructions,
                    ...(noTools
                      ? { tools: [] }
                      : tools.trim()
                        ? { tools: tools.split("\n").filter(Boolean) }
                        : {}),
                    ...(memory ? { memory } : {}),
                  },
            )
              .then(done)
              .catch((e) => setError(message(e)));
          }}
        >
          <label>
            {t("name")}
            <input
              required={!main}
              maxLength={64}
              placeholder={main ? t("mainAgent") : undefined}
              value={title}
              onChange={(e) => setTitle(e.target.value)}
            />
          </label>
          <label>
            {t("description")}
            <input
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </label>
          <label>
            {t("agentInstructions")}
            <textarea
              rows={10}
              value={instructions}
              onChange={(e) => setInstructions(e.target.value)}
            />
          </label>
          <label>
            <input
              type="checkbox"
              checked={preview}
              onChange={(e) => setPreview(e.target.checked)}
            />
            {t("preview")}
          </label>
          {preview && <Markdown text={instructions} />}
          {main ? (
            <p className="muted">{t("mainAgentSettings")}</p>
          ) : (
            <details>
              <summary>{t("advanced")}</summary>
              <div className="stack">
                <label>
                  {t("models")}
                  <textarea
                    value={models}
                    onChange={(e) => setModels(e.target.value)}
                  />
                </label>
                <label>
                  {t("tools")}
                  <textarea
                    disabled={noTools}
                    value={tools}
                    onChange={(e) => setTools(e.target.value)}
                  />
                </label>
                <label>
                  <input
                    type="checkbox"
                    checked={noTools}
                    onChange={(e) => setNoTools(e.target.checked)}
                  />
                  {t("noTools")}
                </label>
                <label>
                  {t("memory")}
                  <select
                    value={memory}
                    onChange={(e) => setMemory(e.target.value)}
                  >
                    <option value="">{t("system")}</option>
                    <option value="chat">{t("conversations")}</option>
                    <option value="off">{t("off")}</option>
                  </select>
                </label>
              </div>
            </details>
          )}
          <div className="actions">
            <button className="primary">{t("save")}</button>
            <button type="button" onClick={() => setOpen(null)}>
              {t("cancel")}
            </button>
            {open.name && !main && (
              <button
                type="button"
                onClick={() => {
                  if (confirm(t("deleteConfirm")))
                    api(`/v1/settings/profiles/${enc(open.name)}`, "DELETE")
                      .then(done)
                      .catch((e) => setError(message(e)));
                }}
              >
                {t("delete")}
              </button>
            )}
          </div>
        </form>
      </>
    );
  }
  return (
    <>
      <div className="page-header">
        <h1>{t("agents")}</h1>
        <button className="primary" onClick={() => edit({} as Profile)}>
          {t("create")}
        </button>
      </div>
      <ErrorBox error={error} />
      <div className="agent-list">
        {items.map((p) => (
          <button key={p.name} className="agent-card" onClick={() => edit(p)}>
            <strong>{agentTitle(p, t)}</strong>
            {p.description && <span className="muted">{p.description}</span>}
          </button>
        ))}
      </div>
    </>
  );
}
