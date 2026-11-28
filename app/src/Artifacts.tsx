import { useEffect, useState } from "react";
import { api, enc, message, response } from "./api";
import { useI18n } from "./i18n";
import type { Artifact } from "./types";
import { Markdown, Mermaid, ErrorBox } from "./ui";
export function Artifacts({
  conversation,
  revision,
  onClose,
}: {
  conversation: string;
  revision: number;
  onClose: () => void;
}) {
  const { t } = useI18n();
  const [items, setItems] = useState<Artifact[]>([]);
  const [selected, setSelected] = useState("");
  const [versions, setVersions] = useState<Artifact[]>([]);
  const [version, setVersion] = useState(0);
  const [content, setContent] = useState("");
  const [error, setError] = useState("");
  const base = `/v1/conversations/${enc(conversation)}/artifacts`;
  useEffect(() => {
    setError("");
    api<Artifact[]>(base)
      .then((x) => {
        setItems(x ?? []);
        setSelected((s) => s || x?.[0]?.name || "");
      })
      .catch((e) => setError(message(e)));
  }, [base, revision]);
  useEffect(() => {
    if (!selected) return;
    api<{ latest: Artifact; versions: Artifact[] }>(`${base}/${enc(selected)}`)
      .then((x) => {
        setVersions(x.versions);
        setVersion(x.latest.version);
      })
      .catch((e) => setError(message(e)));
  }, [base, selected, revision]);
  const item =
    versions.find((v) => v.version === version) ??
    items.find((i) => i.name === selected);
  const url = `${base}/${enc(selected)}/content?version=${version}`;
  useEffect(() => {
    if (
      !selected ||
      !version ||
      item?.kind === "file" ||
      (item?.kind !== "svg" && item?.media_type?.startsWith("image/"))
    )
      return;
    const abort = new AbortController();
    response(url, { signal: abort.signal })
      .then((r) => r.text())
      .then(setContent)
      .catch((e) => {
        if (!abort.signal.aborted) setError(message(e));
      });
    return () => abort.abort();
  }, [url, selected, version]);
  return (
    <aside className="artifacts" aria-label={t("artifacts")}>
      <header>
        <h2>{t("artifacts")}</h2>
        <button onClick={onClose} aria-label={t("close")}>
          ×
        </button>
      </header>
      <ErrorBox error={error} />
      {items.length === 0 ? (
        <p>{t("empty")}</p>
      ) : (
        <>
          <select
            aria-label={t("artifacts")}
            value={selected}
            onChange={(e) => setSelected(e.target.value)}
          >
            {items.map((i) => (
              <option key={i.name}>{i.name}</option>
            ))}
          </select>
          <select
            aria-label={t("versions")}
            value={version}
            onChange={(e) => setVersion(Number(e.target.value))}
          >
            {versions.map((v) => (
              <option key={v.version} value={v.version}>
                {t("versions")} {v.version}
              </option>
            ))}
          </select>
          {item?.kind === "markdown" ? (
            <Markdown text={content} />
          ) : item?.kind === "mermaid" ? (
            <Mermaid text={content} />
          ) : item?.kind === "svg" ? (
            <img
              alt={item.title}
              src={`data:image/svg+xml;charset=utf-8,${enc(content)}`}
            />
          ) : item?.kind !== "svg" && item?.media_type?.startsWith("image/") ? (
            <img src={url} alt={item.title} />
          ) : item?.kind === "file" ? null : (
            <Markdown text={fenced(content, item?.language)} />
          )}{" "}
          {(item?.kind === "html" || item?.kind === "react") && (
            <p className="muted">{t("codeLimit")}</p>
          )}
          <a className="button" href={url} download={selected}>
            {t("download")}
          </a>
        </>
      )}
    </aside>
  );
}
function fenced(content: string, language = "") {
  const longest = Math.max(
    2,
    ...(content.match(/`+/g) ?? []).map((s) => s.length),
  );
  const fence = "`".repeat(longest + 1);
  return `${fence}${language.replace(/[^a-z0-9_-]/gi, "")}\n${content}\n${fence}`;
}
