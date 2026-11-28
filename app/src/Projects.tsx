import { useEffect, useState, useRef } from "react";
import { api, enc, message } from "./api";
import type { Card, Conversation, Profile, Project, Timeline } from "./types";
import { useI18n, type Key } from "./i18n";
import { boardMove, agentTitle } from "./state";
import { ErrorBox, Markdown, Confirm, tabKeys } from "./ui";
export function Projects({
  profiles,
  onChat,
  onRefresh,
}: {
  profiles: Profile[];
  onChat: (id?: string, project?: string) => void;
  onRefresh: () => void;
}) {
  const { t } = useI18n();
  const [items, setItems] = useState<Project[]>([]);
  const [id, setId] = useState("");
  const [project, setProject] = useState<Project>();
  const [tab, setTab] = useState("board");
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [brief, setBrief] = useState("");
  const [error, setError] = useState("");
  const [card, setCard] = useState<Partial<Card> | null>(null);
  const [timeline, setTimeline] = useState<Timeline[]>([]);
  const [hasOlder, setHasOlder] = useState(true);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const timelineView = useRef<HTMLDivElement>(null);
  const selectedProject = useRef(id);
  selectedProject.current = id;
  const [notes, setNotes] = useState<string[]>([]);
  const [note, setNote] = useState("");
  const [noteText, setNoteText] = useState("");
  const [preview, setPreview] = useState(false);
  const [chats, setChats] = useState<Conversation[]>([]);
  const [comment, setComment] = useState("");
  const [kind, setKind] = useState("comment");
  const [deleting, setDeleting] = useState(false);
  const [archived, setArchived] = useState(false);
  const [agents, setAgents] = useState<Profile[]>([]);
  const [moving, setMoving] = useState(false);
  const [saved, setSaved] = useState(false);
  const base = `/v1/projects/${enc(id)}`;
  function list() {
    api<Project[]>(`/v1/projects${archived ? "?archived=true" : ""}`)
      .then((x) => setItems(x ?? []))
      .catch((e) => setError(message(e)));
  }
  function reload() {
    if (!id) return;
    Promise.all([
      api<Project>(base),
      api<{ events: Timeline[] }>(`${base}/timeline?limit=100`),
      api<string[]>(`${base}/notes`),
      api<Conversation[]>(`/v1/conversations?project=${enc(id)}`),
    ])
      .then(([p, t, n, c]) => {
        if (selectedProject.current !== id) return;
        const el = timelineView.current;
        const bottom =
          !el || el.scrollHeight - el.scrollTop - el.clientHeight < 120;
        setProject(p);
        setTimeline((old) => mergeTimeline(old, t.events ?? []));
        if (bottom)
          requestAnimationFrame(() => {
            if (timelineView.current)
              timelineView.current.scrollTop =
                timelineView.current.scrollHeight;
          });
        setNotes(n ?? []);
        setChats(c ?? []);
      })
      .catch((e) => setError(message(e)));
  }
  useEffect(list, [archived]);
  useEffect(() => {
    api<Profile[]>("/v1/settings/profiles")
      .then((ps) =>
        setAgents([
          ...profiles.filter((p) => p.name === "default"),
          ...ps.filter((p) => !p.main && p.tools == null),
        ]),
      )
      .catch((e) => setError(message(e)));
  }, [profiles]);
  useEffect(() => {
    setProject(undefined);
    setTimeline([]);
    setHasOlder(true);
    setError("");
    setNote("");
    reload();
    if (!id) return;
    const timer = setInterval(() => {
      if (document.visibilityState === "visible") reload();
    }, 7000);
    return () => clearInterval(timer);
  }, [id]);
  useEffect(() => {
    if (tab === "timeline" && timelineView.current)
      timelineView.current.scrollTop = timelineView.current.scrollHeight;
  }, [tab]);
  async function loadOlder() {
    if (loadingOlder || !hasOlder || !timeline.length) return;
    const first = timeline[0].seq;
    if (first <= 1) {
      setHasOlder(false);
      return;
    }
    setLoadingOlder(true);
    const el = timelineView.current;
    const height = el?.scrollHeight ?? 0;
    const top = el?.scrollTop ?? 0;
    try {
      const from = Math.max(1, first - 100);
      const data = await api<{ events: Timeline[] }>(
        `${base}/timeline?from=${from}&limit=${first - from}`,
      );
      const older = (data.events ?? []).filter((e) => e.seq < first);
      setTimeline((old) => mergeTimeline(older, old));
      setHasOlder(older.length > 0 && older[0].seq > 1);
      requestAnimationFrame(() => {
        if (timelineView.current)
          timelineView.current.scrollTop =
            timelineView.current.scrollHeight - height + top;
      });
    } catch (e) {
      setError(message(e));
    } finally {
      setLoadingOlder(false);
    }
  }
  async function move(c: Card, column: string, index: number) {
    if (!project || moving) return;
    const previous = project;
    setMoving(true);
    setError("");
    setProject({
      ...project,
      cards: boardMove(project.cards ?? [], c.id, column, index),
    });
    try {
      await api(`${base}/cards/${enc(c.id)}`, "PATCH", { column, index });
      reload();
    } catch (e) {
      setProject(previous);
      setError(message(e));
    } finally {
      setMoving(false);
    }
  }
  async function openNote(path: string) {
    setNote(path);
    setSaved(false);
    try {
      const data = await api<{ text: string }>(
        `${base}/notes/${path.split("/").map(enc).join("/")}`,
      );
      setNoteText(data.text);
    } catch (e) {
      setError(message(e));
    }
  }
  if (!id)
    return (
      <>
        <div className="page-header">
          <h1>{t("projects")}</h1>
          <button className="primary" onClick={() => setCreating((c) => !c)}>
            {t("newProject")}
          </button>
        </div>
        <label>
          <input
            type="checkbox"
            checked={archived}
            onChange={(e) => setArchived(e.target.checked)}
          />
          {t("archived")}
        </label>
        <ErrorBox error={error} />
        {creating && (
          <form
            className="surface stack"
            onSubmit={(e) => {
              e.preventDefault();
              api<Project>("/v1/projects", "POST", { name, brief })
                .then((p) => {
                  setId(p.id);
                  setCreating(false);
                  setName("");
                  setBrief("");
                  list();
                  onRefresh();
                })
                .catch((e) => setError(message(e)));
            }}
          >
            <label>
              {t("name")}
              <input
                required
                maxLength={120}
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </label>
            <label>
              {t("brief")}
              <textarea
                value={brief}
                onChange={(e) => setBrief(e.target.value)}
              />
            </label>
            <button>{t("create")}</button>
          </form>
        )}
        <div className="grid">
          {items.map((p) => (
            <button
              className="surface project-tile"
              key={p.id}
              onClick={() => setId(p.id)}
            >
              <h2>{p.name}</h2>
              <p>{p.brief}</p>
            </button>
          ))}
        </div>
        {!items.length && <p className="muted">{t("empty")}</p>}
      </>
    );
  return (
    <>
      <div className="page-header">
        <div>
          <button
            className="quiet"
            onClick={() => {
              setId("");
              list();
            }}
          >
            {t("projects")}
          </button>
          <h1>{project?.name ?? t("loading")}</h1>
        </div>
        <div className="actions">
          <button onClick={() => onChat("", id)}>{t("newChat")}</button>
          <button
            onClick={() =>
              api(base, "PATCH", { archived: !project?.archived })
                .then(() => {
                  reload();
                  list();
                  onRefresh();
                })
                .catch((e) => setError(message(e)))
            }
          >
            {t(project?.archived ? "restore" : "archive")}
          </button>
          <button onClick={() => setDeleting(true)}>{t("delete")}</button>
        </div>
      </div>
      <ErrorBox error={error} />
      {deleting && (
        <Confirm
          text={t("deleteConfirm")}
          onCancel={() => setDeleting(false)}
          onConfirm={() =>
            api(base, "DELETE")
              .then(() => {
                setId("");
                setDeleting(false);
                list();
                onRefresh();
              })
              .catch((e) => setError(message(e)))
          }
        />
      )}
      <p className="muted">{project?.brief}</p>
      <div className="tabs" role="tablist" onKeyDown={tabKeys}>
        {(["board", "timeline", "notes", "projectChats"] as const).map((k) => (
          <button
            role="tab"
            aria-selected={tab === k}
            tabIndex={tab === k ? 0 : -1}
            key={k}
            onClick={() => setTab(k)}
          >
            {t(k)}
          </button>
        ))}
      </div>
      {tab === "board" ? (
        <>
          <div className="page-header">
            <p className="muted">{t("handoff")}</p>
            <button
              disabled={project?.archived}
              onClick={() =>
                setCard({
                  title: "",
                  text: "",
                  column: project?.columns[0]?.name,
                  agent: "",
                  person: "",
                })
              }
            >
              {t("newCard")}
            </button>
          </div>
          {card && (
            <form
              className="surface stack"
              onSubmit={(e) => {
                e.preventDefault();
                const { id: cardId, title, text, column, agent, person } = card;
                api(
                  `${base}/cards${cardId ? "/" + enc(cardId) : ""}`,
                  cardId ? "PATCH" : "POST",
                  { title, text, column, agent, person },
                )
                  .then(() => {
                    setCard(null);
                    reload();
                  })
                  .catch((e) => setError(message(e)));
              }}
            >
              <label>
                {t("title")}
                <input
                  required
                  maxLength={200}
                  value={card.title ?? ""}
                  onChange={(e) => setCard({ ...card, title: e.target.value })}
                />
              </label>
              <label>
                {t("content")}
                <textarea
                  value={card.text ?? ""}
                  onChange={(e) => setCard({ ...card, text: e.target.value })}
                />
              </label>
              <div className="grid">
                <label>
                  {t("column")}
                  <select
                    value={card.column}
                    onChange={(e) =>
                      setCard({ ...card, column: e.target.value })
                    }
                  >
                    {project?.columns.map((c) => (
                      <option key={c.name}>{c.name}</option>
                    ))}
                  </select>
                </label>
                <label>
                  {t("agent")}
                  <select
                    value={card.agent ?? ""}
                    onChange={(e) =>
                      setCard({ ...card, agent: e.target.value })
                    }
                  >
                    <option value="">{t("unassigned")}</option>
                    {agents.map((a) => (
                      <option key={a.name} value={a.name}>
                        {agentTitle(a, t)}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  {t("person")}
                  <input
                    value={card.person ?? ""}
                    onChange={(e) =>
                      setCard({ ...card, person: e.target.value })
                    }
                  />
                </label>
              </div>
              <div className="actions">
                <button>{t("save")}</button>
                <button type="button" onClick={() => setCard(null)}>
                  {t("cancel")}
                </button>
                {card.id && (
                  <button
                    type="button"
                    onClick={() => {
                      if (confirm(t("deleteConfirm")))
                        api(`${base}/cards/${enc(card.id!)}`, "DELETE")
                          .then(() => {
                            setCard(null);
                            reload();
                          })
                          .catch((e) => setError(message(e)));
                    }}
                  >
                    {t("delete")}
                  </button>
                )}
              </div>
            </form>
          )}
          <div className="board">
            {project?.columns.map((col) => (
              <section
                className="board-column"
                aria-label={col.name}
                key={col.name}
                onDragOver={(e) => e.preventDefault()}
                onDrop={(e) => {
                  e.preventDefault();
                  const c = project.cards?.find(
                    (c) => c.id === e.dataTransfer.getData("text/plain"),
                  );
                  if (c) void move(c, col.name, -1);
                }}
              >
                <h2>{col.name}</h2>
                {col.auto && <p className="muted">{t("starts")}</p>}
                {(project.cards ?? [])
                  .filter((c) => c.column === col.name)
                  .map((c, index) => (
                    <article
                      className="card"
                      key={c.id}
                      draggable={!project.archived}
                      onDragStart={(e) =>
                        e.dataTransfer.setData("text/plain", c.id)
                      }
                      onDragOver={(e) => e.preventDefault()}
                      onDrop={(e) => {
                        e.preventDefault();
                        e.stopPropagation();
                        const moving = project.cards?.find(
                          (c) => c.id === e.dataTransfer.getData("text/plain"),
                        );
                        if (moving) void move(moving, col.name, index);
                      }}
                    >
                      <button
                        className="quiet"
                        disabled={project.archived}
                        onClick={() => setCard(c)}
                      >
                        <h3>{c.title}</h3>
                      </button>
                      {c.text && <p>{c.text}</p>}
                      <p className="muted">
                        {[c.agent, c.person].filter(Boolean).join(" · ")}
                      </p>
                      <label>
                        {t("column")}
                        <select
                          disabled={moving || project.archived}
                          value={c.column}
                          onChange={(e) => move(c, e.target.value, -1)}
                        >
                          {project.columns.map((col) => (
                            <option key={col.name}>{col.name}</option>
                          ))}
                        </select>
                      </label>
                      <div className="actions">
                        <button
                          aria-label={`${t("up")}: ${c.title}`}
                          disabled={index === 0 || moving || project.archived}
                          onClick={() => move(c, c.column, index - 1)}
                        >
                          {t("up")}
                        </button>
                        <button
                          aria-label={`${t("down")}: ${c.title}`}
                          disabled={
                            index ===
                              (project.cards ?? []).filter(
                                (x) => x.column === col.name,
                              ).length -
                                1 ||
                            moving ||
                            project.archived
                          }
                          onClick={() => move(c, c.column, index + 1)}
                        >
                          {t("down")}
                        </button>
                      </div>
                      {c.conversations?.map((cid) => (
                        <button
                          className="quiet"
                          key={cid}
                          onClick={() => onChat(cid, id)}
                        >
                          {t("conversations")}
                        </button>
                      ))}
                    </article>
                  ))}
              </section>
            ))}
          </div>
        </>
      ) : tab === "timeline" ? (
        <>
          <button
            onClick={loadOlder}
            disabled={
              loadingOlder ||
              !hasOlder ||
              !timeline.length ||
              timeline[0].seq <= 1
            }
          >
            {t("older")}
          </button>
          <div
            className="timeline"
            ref={timelineView}
            onScroll={(e) => {
              if (e.currentTarget.scrollTop < 4) void loadOlder();
            }}
          >
            {timeline.map((e) => (
              <article key={e.seq}>
                <p>
                  <strong>{e.actor}</strong> · {t(e.kind as Key) || e.kind}{" "}
                  {e.card && (
                    <span>
                      {project?.cards?.find((c) => c.id === e.card)?.title ??
                        e.card}
                    </span>
                  )}
                </p>
                <p>{e.text}</p>
                <time className="muted" dateTime={e.at}>
                  {new Date(e.at).toLocaleString()}
                </time>
                {e.conversation && (
                  <button
                    className="quiet"
                    onClick={() => onChat(e.conversation, id)}
                  >
                    {t("conversations")}
                  </button>
                )}
              </article>
            ))}
          </div>
          <form
            className="surface stack"
            onSubmit={(e) => {
              e.preventDefault();
              api(`${base}/timeline`, "POST", { kind, text: comment })
                .then(() => {
                  setComment("");
                  reload();
                })
                .catch((e) => setError(message(e)));
            }}
          >
            <label>
              {t("kind")}
              <select value={kind} onChange={(e) => setKind(e.target.value)}>
                <option value="comment">{t("comment")}</option>
                <option value="decision">{t("decision")}</option>
              </select>
            </label>
            <label>
              {t("content")}
              <textarea
                required
                value={comment}
                onChange={(e) => setComment(e.target.value)}
              />
            </label>
            <button disabled={project?.archived}>{t("save")}</button>
          </form>
        </>
      ) : tab === "notes" ? (
        <div className="surface stack">
          <div className="actions">
            {notes.map((p) => (
              <button key={p} onClick={() => openNote(p)}>
                {p}
              </button>
            ))}
            <button
              onClick={() => {
                setNote("untitled.md");
                setNoteText("");
                setSaved(false);
              }}
            >
              {t("newNote")}
            </button>
          </div>
          {note && (
            <>
              <label>
                {t("path")}
                <input value={note} onChange={(e) => setNote(e.target.value)} />
              </label>
              <label>
                <input
                  type="checkbox"
                  checked={preview}
                  onChange={(e) => setPreview(e.target.checked)}
                />
                {t("preview")}
              </label>
              {preview ? (
                <Markdown text={noteText} />
              ) : (
                <label>
                  {t("content")}
                  <textarea
                    rows={15}
                    value={noteText}
                    onChange={(e) => {
                      setNoteText(e.target.value);
                      setSaved(false);
                    }}
                  />
                </label>
              )}
              <div className="actions">
                <button
                  disabled={project?.archived}
                  onClick={() =>
                    api(
                      `${base}/notes/${note.split("/").map(enc).join("/")}`,
                      "PUT",
                      { text: noteText },
                    )
                      .then(() => {
                        setSaved(true);
                        reload();
                      })
                      .catch((e) => setError(message(e)))
                  }
                >
                  {t("save")}
                </button>
                <button
                  disabled={project?.archived}
                  onClick={() => {
                    if (confirm(t("deleteConfirm")))
                      api(
                        `${base}/notes/${note.split("/").map(enc).join("/")}`,
                        "DELETE",
                      )
                        .then(() => {
                          setNote("");
                          reload();
                        })
                        .catch((e) => setError(message(e)));
                  }}
                >
                  {t("delete")}
                </button>
              </div>
              <p role="status">{saved ? t("saved") : ""}</p>
            </>
          )}
        </div>
      ) : (
        <div className="surface">
          {chats.map((c) => (
            <div className="list-row" key={c.id}>
              <button className="quiet" onClick={() => onChat(c.id, id)}>
                {c.title || c.id}
              </button>
            </div>
          ))}
        </div>
      )}
    </>
  );
}

function mergeTimeline(a: Timeline[], b: Timeline[]) {
  return [...new Map([...a, ...b].map((e) => [e.seq, e])).values()].sort(
    (a, b) => a.seq - b.seq,
  );
}
