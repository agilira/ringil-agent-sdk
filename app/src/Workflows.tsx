import { WorkflowGraph } from "./WorkflowGraph";
import { useEffect, useState } from "react";
import { api, enc, message } from "./api";
import type { Profile, Obj } from "./types";
import { useI18n, type Key } from "./i18n";
import { ErrorBox, Markdown } from "./ui";
import { agentTitle } from "./state";
interface Step {
  name: string;
  worker: string;
  task: string;
  skill?: string;
  needs?: string[];
}
interface Saved {
  name: string;
  description: string;
  brief?: string;
  plan: { steps: Step[]; output: string };
}
interface WorkflowRun {
  id: string;
  state: string;
  error?: string;
  output?: string;
  plan?: Saved["plan"];
  steps?: Record<string, { status: string; report?: string }>;
  spent?: { usage?: { cost?: number } };
}
export function Workflows({ profiles }: { profiles: Profile[] }) {
  const { t } = useI18n();
  const [items, setItems] = useState<Saved[]>([]);
  const [runs, setRuns] = useState<WorkflowRun[]>([]);
  const [run, setRun] = useState<WorkflowRun>();
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [brief, setBrief] = useState("");
  const [steps, setSteps] = useState<Step[]>([]);
  const [output, setOutput] = useState("");
  const [error, setError] = useState("");
  const [report, setReport] = useState("");
  const [resolve, setResolve] = useState("");
  function reload() {
    Promise.all([
      api<Saved[]>("/v1/workflows"),
      api<WorkflowRun[]>("/v1/workflow-runs"),
    ])
      .then(([w, r]) => {
        setItems(w ?? []);
        setRuns(r ?? []);
      })
      .catch((e) => setError(message(e)));
  }
  useEffect(() => {
    reload();
    const timer = setInterval(() => {
      if (document.visibilityState === "visible") reload();
    }, 8000);
    return () => clearInterval(timer);
  }, []);
  useEffect(() => {
    if (!run) return;
    const timer = setInterval(() => {
      if (document.visibilityState === "visible")
        api<WorkflowRun>(`/v1/workflow-runs/${enc(run.id)}`)
          .then(setRun)
          .catch((e) => setError(message(e)));
    }, 5000);
    return () => clearInterval(timer);
  }, [run?.id]);
  function edit(w?: Saved) {
    setEditing(true);
    setName(w?.name ?? "");
    setDescription(w?.description ?? "");
    setBrief(w?.brief ?? "");
    setSteps(w?.plan.steps ?? [{ name: "", worker: "default", task: "" }]);
    setOutput(w?.plan.output ?? "");
  }
  function update(i: number, patch: Partial<Step>) {
    setSteps((ss) => ss.map((s, j) => (j === i ? { ...s, ...patch } : s)));
  }
  async function action(path: string, body?: Obj) {
    try {
      await api(`/v1/workflow-runs/${enc(run!.id)}/${path}`, "POST", body);
      setRun(await api<WorkflowRun>(`/v1/workflow-runs/${enc(run!.id)}`));
      setResolve("");
      reload();
    } catch (e) {
      setError(message(e));
    }
  }
  return (
    <>
      <div className="page-header">
        <h1>{t("workflows")}</h1>
        <button className="primary" onClick={() => edit()}>
          {t("create")}
        </button>
      </div>
      <ErrorBox error={error} />
      {editing && (
        <form
          className="surface stack"
          onSubmit={(e) => {
            e.preventDefault();
            api(`/v1/workflows/${enc(name)}`, "PUT", {
              description,
              brief,
              plan: { steps, output: output || steps.at(-1)?.name },
            })
              .then(() => {
                setEditing(false);
                reload();
              })
              .catch((e) => setError(message(e)));
          }}
        >
          <label>
            {t("name")}
            <input
              required
              pattern="[A-Za-z0-9][A-Za-z0-9_-]{0,39}"
              value={name}
              onChange={(e) => setName(e.target.value)}
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
            {t("brief")}
            <textarea
              value={brief}
              onChange={(e) => setBrief(e.target.value)}
            />
          </label>
          {steps.map((s, i) => (
            <fieldset className="surface stack" key={i}>
              <legend>
                {t("steps")} {i + 1}
              </legend>
              <label>
                {t("name")}
                <input
                  required
                  pattern="[a-z0-9][a-z0-9_-]{0,63}"
                  value={s.name}
                  onChange={(e) => update(i, { name: e.target.value })}
                />
              </label>
              <label>
                {t("agent")}
                <select
                  value={s.worker}
                  onChange={(e) => update(i, { worker: e.target.value })}
                >
                  {profiles.map((p) => (
                    <option key={p.name} value={p.name}>
                      {agentTitle(p, t)}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                {t("task")}
                <textarea
                  required
                  value={s.task}
                  onChange={(e) => update(i, { task: e.target.value })}
                />
              </label>
              <label>
                {t("skills")}
                <input
                  value={s.skill ?? ""}
                  onChange={(e) =>
                    update(i, { skill: e.target.value || undefined })
                  }
                />
              </label>
              <label>
                {t("needs")}
                <input
                  value={s.needs?.join(", ") ?? ""}
                  onChange={(e) =>
                    update(i, {
                      needs: e.target.value
                        .split(",")
                        .map((s) => s.trim())
                        .filter(Boolean),
                    })
                  }
                />
              </label>
              <button
                type="button"
                disabled={steps.length === 1}
                onClick={() => setSteps((ss) => ss.filter((_, j) => j !== i))}
              >
                {t("remove")}
              </button>
            </fieldset>
          ))}
          <button
            type="button"
            onClick={() =>
              setSteps((s) => [...s, { name: "", worker: "default", task: "" }])
            }
          >
            {t("addStep")}
          </button>
          <label>
            {t("output")}
            <select
              value={output || steps.at(-1)?.name || ""}
              onChange={(e) => setOutput(e.target.value)}
            >
              {steps.map((s, i) => (
                <option key={i} value={s.name}>
                  {s.name}
                </option>
              ))}
            </select>
          </label>
          <div className="actions">
            <button>{t("save")}</button>
            <button type="button" onClick={() => setEditing(false)}>
              {t("cancel")}
            </button>
          </div>
        </form>
      )}
      {items.map((w) => (
        <section className="surface" key={w.name}>
          <h2>{w.name}</h2>
          <p>{w.description}</p>
          <WorkflowGraph steps={w.plan.steps} />
          <div className="actions">
            <button
              onClick={() =>
                api<{ id: string }>(
                  `/v1/workflows/${enc(w.name)}/runs`,
                  "POST",
                  {},
                )
                  .then(async (r) => {
                    setRun(
                      await api<WorkflowRun>(`/v1/workflow-runs/${enc(r.id)}`),
                    );
                    reload();
                  })
                  .catch((e) => setError(message(e)))
              }
            >
              {t("runNow")}
            </button>
            <button onClick={() => edit(w)}>{t("edit")}</button>
            <button
              onClick={() => {
                if (confirm(t("deleteConfirm")))
                  api(`/v1/workflows/${enc(w.name)}`, "DELETE")
                    .then(reload)
                    .catch((e) => setError(message(e)));
              }}
            >
              {t("delete")}
            </button>
          </div>
        </section>
      ))}
      <h2>{t("runs")}</h2>
      <div className="surface">
        {runs.length === 0 && <p className="muted">{t("empty")}</p>}
        {runs.map((r) => (
          <div className="list-row" key={r.id}>
            <button
              className="quiet"
              onClick={() =>
                api<WorkflowRun>(`/v1/workflow-runs/${enc(r.id)}`)
                  .then(setRun)
                  .catch((e) => setError(message(e)))
              }
            >
              {r.id}
            </button>
            <span>{t(r.state as Key) || r.state}</span>
          </div>
        ))}
      </div>
      {run && (
        <section className="surface">
          <div className="page-header">
            <h2>{run.id}</h2>
            <button onClick={() => setRun(undefined)}>{t("close")}</button>
          </div>
          <p>{t(run.state as Key) || run.state}</p>
          <ErrorBox error={run.error ?? ""} />
          {run.output && <Markdown text={run.output} />}{" "}
          {run.plan?.steps?.map((s) => (
            <div className="surface" key={s.name}>
              <h3>
                {s.name} · {run.steps?.[s.name]?.status ?? t("waiting")}
              </h3>
              <p>{s.task}</p>
              {run.steps?.[s.name]?.report && (
                <Markdown text={run.steps[s.name].report!} />
              )}{" "}
              {run.state !== "running" &&
                run.steps?.[s.name]?.status === "pending" && (
                  <div className="actions">
                    <button
                      onClick={() => action(`steps/${enc(s.name)}/retry`)}
                    >
                      {t("retry")}
                    </button>
                    <button
                      onClick={() => {
                        setResolve(s.name);
                        setReport("");
                      }}
                    >
                      {t("resolve")}
                    </button>
                  </div>
                )}
            </div>
          ))}
          {resolve && (
            <form
              className="stack"
              onSubmit={(e) => {
                e.preventDefault();
                void action(`steps/${enc(resolve)}/resolve`, { report });
              }}
            >
              <label>
                {t("report")}
                <textarea
                  required
                  value={report}
                  onChange={(e) => setReport(e.target.value)}
                />
              </label>
              <button>{t("save")}</button>
            </form>
          )}
          {run.state !== "running" && (
            <button onClick={() => action("resume")}>{t("resume")}</button>
          )}
        </section>
      )}
    </>
  );
}
