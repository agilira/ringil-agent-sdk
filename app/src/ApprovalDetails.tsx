import type { Approval, Obj } from "./types";
import { useI18n } from "./i18n";
export function ApprovalDetails({ approval }: { approval: Approval }) {
  const { t } = useI18n();
  if (!approval.input || typeof approval.input !== "object")
    return <pre>{JSON.stringify(approval.input, null, 2)}</pre>;
  const input = approval.input as Obj;
  const command = typeof input.command === "string" ? input.command : undefined;
  const reason =
    typeof input.description === "string"
      ? input.description
      : typeof input.reason === "string"
        ? input.reason
        : undefined;
  const path = typeof input.path === "string" ? input.path : undefined;
  const before =
    typeof input.old_string === "string"
      ? input.old_string
      : typeof input.old_text === "string"
        ? input.old_text
        : undefined;
  const after =
    typeof input.new_string === "string"
      ? input.new_string
      : typeof input.new_text === "string"
        ? input.new_text
        : undefined;
  let domain = "";
  try {
    if (typeof input.url === "string") domain = new URL(input.url).host;
  } catch {
    /* show original input instead */
  }
  return (
    <>
      {path && (
        <p>
          <strong>{path}</strong>
        </p>
      )}
      {reason && <p>{reason}</p>}
      {command && (
        <pre>
          <code>{command}</code>
        </pre>
      )}
      {domain && (
        <p>
          {t("domain")}: <strong>{domain}</strong>
        </p>
      )}
      {before !== undefined && after !== undefined && (
        <div className="diff" aria-label={t("changes")}>
          <pre className="removed">
            {before
              .split("\n")
              .map((s) => "- " + s)
              .join("\n")}
          </pre>
          <pre className="added">
            {after
              .split("\n")
              .map((s) => "+ " + s)
              .join("\n")}
          </pre>
        </div>
      )}
      <details open={!command && !domain && before === undefined}>
        <summary>{t("details")}</summary>
        <pre>{JSON.stringify(input, null, 2)}</pre>
      </details>
    </>
  );
}
