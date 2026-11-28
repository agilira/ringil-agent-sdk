import { useState } from "react";
import type { Approval } from "./types";
import { useI18n } from "./i18n";
import { ApprovalDetails } from "./ApprovalDetails";
export function ApprovalCard({
  approval: a,
  onDecide,
}: {
  approval: Approval;
  onDecide: (
    a: Approval,
    allow: boolean,
    remember?: "session" | "always",
    message?: string,
  ) => Promise<void>;
}) {
  const { t } = useI18n();
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  async function decide(allow: boolean, remember?: "session" | "always") {
    setBusy(true);
    try {
      await onDecide(
        a,
        allow,
        remember,
        allow ? undefined : reason || undefined,
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <section
      className="approval"
      aria-label={t("approval")}
      aria-live="assertive"
    >
      <h3>
        {t("approval")} · {a.tool}
      </h3>
      <ApprovalDetails approval={a} />
      {a.rule && (
        <p className="muted">
          {t("permissionRule")}: <code>{a.rule}</code>
        </p>
      )}
      {a.explain && (
        <label>
          {t("denyReason")}
          <textarea
            maxLength={1000}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </label>
      )}
      <div className="actions">
        <button disabled={busy} onClick={() => decide(true)}>
          {t("yes")}
        </button>
        {a.rule && (
          <button disabled={busy} onClick={() => decide(true, "session")}>
            {t("yesSession")}
          </button>
        )}
        <button
          disabled={busy || !a.rule}
          title={!a.rule ? t("alwaysLimit") : undefined}
          onClick={() => decide(true, "always")}
        >
          {t("always")}
        </button>
        <button disabled={busy} onClick={() => decide(false)}>
          {t("no")}
        </button>
      </div>
      {!a.rule && <p className="muted">{t("alwaysLimit")}</p>}
    </section>
  );
}
