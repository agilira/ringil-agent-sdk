import { waitForServer } from "./restart";
import { useEffect, useId, useState, type ReactNode } from "react";
import ReactMarkdown from "react-markdown";
import sanitize from "rehype-sanitize";
import highlight from "rehype-highlight";
import { useI18n } from "./i18n";
import { api, message } from "./api";
export function ErrorBox({ error }: { error: string }) {
  return error ? (
    <div className="error" role="alert">
      {error}
    </div>
  ) : null;
}
export function Field({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  const id = useId();
  return (
    <div className="field">
      <label id={id}>{label}</label>
      <div className="control" aria-labelledby={id}>
        {children}
      </div>
    </div>
  );
}
export function Copy({ text }: { text: string }) {
  const { t } = useI18n();
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState("");
  return (
    <>
      <button
        className="quiet"
        onClick={() => {
          navigator.clipboard
            .writeText(text)
            .then(() => {
              setCopied(true);
              setTimeout(() => setCopied(false), 1800);
            })
            .catch((e) => setError(message(e)));
        }}
      >
        {t(copied ? "copied" : "copy")}
      </button>
      <ErrorBox error={error} />
    </>
  );
}
export function Markdown({ text }: { text: string }) {
  return (
    <div className="markdown">
      <ReactMarkdown
        rehypePlugins={[sanitize, highlight]}
        components={{
          img: ({ src, alt }) => {
            if (!src) return null;
            try {
              const url = new URL(src, window.location.origin);
              if (
                url.origin !== window.location.origin &&
                !["data:", "blob:"].includes(url.protocol)
              )
                return <span>{alt}</span>;
            } catch {
              return <span>{alt}</span>;
            }
            return <img src={src} alt={alt ?? ""} />;
          },
          a: ({ children, ...p }) => (
            <a {...p} rel="noreferrer" target="_blank">
              {children}
            </a>
          ),
          pre: ({ children }) => (
            <div className="code-block">
              <pre>{children}</pre>
              <Copy text={extract(children)} />
            </div>
          ),
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
function extract(node: ReactNode): string {
  if (typeof node === "string") return node;
  if (Array.isArray(node)) return node.map(extract).join("");
  if (node && typeof node === "object" && "props" in node)
    return extract((node.props as { children: ReactNode }).children);
  return "";
}
export function Mermaid({ text }: { text: string }) {
  const [svg, setSvg] = useState("");
  const [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    import("mermaid")
      .then(async ({ default: m }) => {
        m.initialize({
          startOnLoad: false,
          securityLevel: "strict",
          theme: "neutral",
        });
        const { svg } = await m.render("diagram-" + crypto.randomUUID(), text);
        if (active) setSvg(svg);
      })
      .catch((e) => active && setError(message(e)));
    return () => {
      active = false;
    };
  }, [text]);
  return error ? (
    <>
      <ErrorBox error={error} />
      <pre>{text}</pre>
    </>
  ) : (
    <img
      className="diagram"
      alt={text}
      src={`data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`}
    />
  );
}
export function JsonEditor({
  path,
  title,
  heading = true,
  clean,
}: {
  path: string;
  title: string;
  // heading is false when the editor is the whole page and the page title
  // already names it.
  heading?: boolean;
  clean?: (data: Record<string, unknown>) => unknown;
}) {
  const { t } = useI18n();
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    api<Record<string, unknown>>(path)
      .then((x) => setValue(JSON.stringify(clean ? clean(x) : x, null, 2)))
      .catch((e) => setError(message(e)));
  }, [path]);
  return (
    <section className="surface">
      {heading && <h2>{title}</h2>}
      <p className="muted">{t("jsonHelp")}</p>
      <label>
        <span className="sr-only">{title}</span>
        <textarea
          className="json"
          value={value}
          onChange={(e) => {
            setValue(e.target.value);
            setSaved(false);
          }}
        />
      </label>
      <ErrorBox error={error} />
      <button
        onClick={() => {
          setError("");
          try {
            const body = JSON.parse(value);
            api(path, "PUT", body)
              .then(() => setSaved(true))
              .catch((e) => setError(message(e)));
          } catch (e) {
            setError(message(e));
          }
        }}
      >
        {t("save")}
      </button>
      <span role="status">{saved ? t("saved") : ""}</span>
    </section>
  );
}
export function Confirm({
  text,
  onConfirm,
  onCancel,
}: {
  text: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { t } = useI18n();
  return (
    <div
      className="confirm"
      role="alertdialog"
      aria-modal="false"
      aria-label={t("delete")}
    >
      <p>{text}</p>
      <button className="danger" onClick={onConfirm}>
        {t("delete")}
      </button>
      <button onClick={onCancel}>{t("cancel")}</button>
    </div>
  );
}

export function tabKeys(event: import("react").KeyboardEvent<HTMLDivElement>) {
  const tabs = Array.from(
    event.currentTarget.querySelectorAll<HTMLButtonElement>("[role=tab]"),
  );
  const current = tabs.indexOf(document.activeElement as HTMLButtonElement);
  let next: number;
  switch (event.key) {
    case "ArrowRight":
      next = (current + 1) % tabs.length;
      break;
    case "ArrowLeft":
      next = (current - 1 + tabs.length) % tabs.length;
      break;
    case "Home":
      next = 0;
      break;
    case "End":
      next = tabs.length - 1;
      break;
    default:
      return;
  }
  event.preventDefault();
  tabs[next]?.focus();
  tabs[next]?.click();
}

export function RestartControl({ onRestart }: { onRestart: () => void }) {
  const { t } = useI18n();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <>
      <button
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          setError("");
          try {
            await api("/v1/restart", "POST");
            await waitForServer(t("serverTimeout"));
            onRestart();
          } catch (e) {
            setError(message(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        {t("restart")}
      </button>
      <span role="status">{busy ? t("restarting") : ""}</span>
      <ErrorBox error={error} />
    </>
  );
}
