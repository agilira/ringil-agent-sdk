import { ApprovalCard } from "./ApprovalCard";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { api, ApiError, enc, message, response } from "./api";
import { streamRun } from "./stream";
import { approvalsAfter, decisionBody, agentTitle } from "./state";
import type {
  Approval,
  Artifact,
  Call,
  Message,
  Profile,
  Run,
  RunEvent,
} from "./types";
import { useI18n } from "./i18n";
import { Logo } from "./icons";
import { ErrorBox, Markdown } from "./ui";
import { Artifacts } from "./Artifacts";
type Activity = {
  call: Call;
  output?: string;
  ms?: number;
  error?: boolean;
  worker?: string;
};
export function Chat({
  conversation,
  project,
  profiles,
  onCreated,
  onRefresh,
  initialMessage,
}: {
  conversation: string;
  project?: string;
  profiles: Profile[];
  onCreated: (id: string, message: Message) => void;
  onRefresh: () => void;
  initialMessage?: Message;
}) {
  const { t, language } = useI18n();
  const [messages, setMessages] = useState<Message[]>([]);
  const [from, setFrom] = useState(0);
  const [text, setText] = useState("");
  const [draft, setDraft] = useState("");
  const [profile, setProfile] = useState("default");
  const [run, setRun] = useState<Run | null>(null);
  const [approvals, setApprovals] = useState<Approval[]>([]);
  const [activity, setActivity] = useState<Activity[]>([]);
  const [error, setError] = useState("");
  const [lost, setLost] = useState(false);
  const [cost, setCost] = useState<number>();
  const [sending, setSending] = useState(false);
  const [files, setFiles] = useState<File[]>([]);
  const [audio, setAudio] = useState({ transcribe: false, speech: false });
  const [recording, setRecording] = useState(false);
  const [voice, setVoice] = useState(false);
  const [artifacts, setArtifacts] = useState(false);
  const [revision, setRevision] = useState(0);
  const stream = useRef<AbortController | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const input = useRef<HTMLTextAreaElement>(null);
  const recorder = useRef<MediaRecorder | null>(null);
  const media = useRef<MediaStream | null>(null);
  const mounted = useRef(true);
  const scroller = useRef<HTMLDivElement>(null);
  const voiceRef = useRef(false);
  const sendRef = useRef(send);
  sendRef.current = send;
  const recordingRef = useRef(startRecording);
  recordingRef.current = startRecording;
  const playback = useRef<HTMLAudioElement | null>(null);
  const active = !!run && ["running", "waiting"].includes(run.state);
  async function load(id: string, older = false) {
    const page = await api<{ messages: Message[]; from: number }>(
      `/v1/conversations/${enc(id)}/messages${older ? `?from=${Math.max(0, from - 200)}&limit=${Math.min(200, from)}` : ""}`,
    );
    setMessages((m) =>
      older ? [...(page.messages ?? []), ...m] : (page.messages ?? []),
    );
    setFrom(page.from);
  }
  function attach(snapshot: Run) {
    stream.current?.abort();
    const controller = new AbortController();
    stream.current = controller;
    setRun(snapshot);
    setApprovals(snapshot.pending ?? []);
    setProfile(snapshot.profile);
    let final = "";
    streamRun(
      snapshot.id,
      (e: RunEvent) => {
        setApprovals((a) => approvalsAfter(a, e));
        if (e.kind === "text" && !e.parent) {
          final += e.text ?? "";
          setText(final);
        }
        if (e.kind === "tool_start" && e.call)
          setActivity((a) => [
            ...a.filter((x) => x.call.id !== e.call!.id),
            { call: e.call!, worker: e.worker },
          ]);
        if (e.kind === "tool_end" && e.call)
          setActivity((a) =>
            a.map((x) =>
              x.call.id === e.call!.id
                ? {
                    ...x,
                    output:
                      typeof e.output?.content === "string"
                        ? e.output.content
                        : JSON.stringify(e.output?.content),
                    ms: e.ms,
                    error: e.output?.is_error || !!e.error,
                  }
                : x,
            ),
          );
        if (e.kind === "failed") setError(e.error?.message ?? t("error"));
        if (e.kind === "artifact") {
          setRevision((r) => r + 1);
          setArtifacts(true);
        }
        if (e.kind === "run_end") {
          api<Run>(`/v1/runs/${snapshot.id}`)
            .then(async (r) => {
              if (controller.signal.aborted) return;
              setRun(r);
              setCost(r.usage ? (r.usage.cost ?? 0) : undefined);
              if (r.error && r.state !== "cancelled") setError(r.error.message);
              if (snapshot.conversation) {
                await load(snapshot.conversation);
                setText("");
              }
              onRefresh();
              if (voiceRef.current && r.text) await speak(r.text);
              if (voiceRef.current) recordingRef.current();
            })
            .catch((e) => setError(message(e)));
        }
      },
      setLost,
      controller.signal,
      () => setRevision((r) => r + 1),
    ).catch((e) => {
      setError(message(e));
      setRun(null);
    });
  }
  useEffect(() => {
    mounted.current = true;
    setMessages(initialMessage ? [initialMessage] : []);
    setText("");
    setActivity([]);
    setApprovals([]);
    setRun(null);
    setError("");
    setCost(undefined);
    setArtifacts(false);
    setProfile("default");
    stream.current?.abort();
    const abort = new AbortController();
    if (conversation) {
      api<Run[]>("/v1/runs", "GET", undefined, abort.signal)
        .then(async (runs) => {
          if (abort.signal.aborted) return;
          const running = runs.find(
            (r) =>
              r.conversation === conversation &&
              ["running", "waiting"].includes(r.state),
          );
          if (running) attach(running);
          else await load(conversation);
        })
        .catch((e) => {
          if (!abort.signal.aborted) setError(message(e));
        });
      api<{ profile?: string }[]>("/v1/conversations")
        .then((cs) => {
          const c = cs.find((c) => (c as { id?: string }).id === conversation);
          if (c?.profile) setProfile(c.profile);
        })
        .catch((e) => setError(message(e)));
    }
    return () => {
      abort.abort();
      stream.current?.abort();
      mounted.current = false;
    };
  }, [conversation]);
  useEffect(() => {
    api<typeof audio>("/v1/audio")
      .then(setAudio)
      .catch((e) => setError(message(e)));
    return () => {
      media.current?.getTracks().forEach((x) => x.stop());
      voiceRef.current = false;
      playback.current?.pause();
    };
  }, []);
  useEffect(() => {
    const el = scroller.current;
    if (el && el.scrollHeight - el.scrollTop - el.clientHeight < 250)
      el.scrollTop = el.scrollHeight;
  }, [text, activity, approvals, messages]);
  useEffect(() => {
    if (input.current) {
      input.current.style.height = "auto";
      input.current.style.height =
        Math.min(input.current.scrollHeight, 220) + "px";
    }
  }, [draft]);
  function addFiles(incoming: File[]) {
    const valid = incoming.every(
      (f) =>
        [
          "image/png",
          "image/jpeg",
          "image/gif",
          "image/webp",
          "application/pdf",
        ].includes(f.type) &&
        (f.type !== "application/pdf" || f.size <= 20 * 1024 * 1024),
    );
    if (!valid || files.length + incoming.length > 20) {
      setError(t("attachmentLimit"));
      return;
    }
    setFiles((f) => [...f, ...incoming]);
  }
  async function send(event?: FormEvent, spoken?: string) {
    event?.preventDefault();
    const value = spoken ?? draft;
    if (active || sending || (!value.trim() && !files.length)) return;
    setSending(true);
    setError("");
    try {
      const attachments = await Promise.all(
        files.map(async (f) => ({
          mime: f.type,
          data: await new Promise<string>((resolve, reject) => {
            const reader = new FileReader();
            reader.onload = () => resolve(String(reader.result).split(",")[1]);
            reader.onerror = reject;
            reader.readAsDataURL(f);
          }),
        })),
      );
      const id =
        conversation ||
        "c-" + crypto.randomUUID().replaceAll("-", "").slice(0, 20);
      const r = await api<Run>("/v1/runs", "POST", {
        profile,
        input: value,
        conversation: id,
        ...(project ? { project } : {}),
        ...(attachments.length ? { attachments } : {}),
      });
      const userMessage: Message = {
        role: "user",
        text: value,
        attachments: files.map((f) => ({ mime: f.type, bytes: f.size })),
      };
      setMessages((m) => [...m, userMessage]);
      setDraft("");
      setFiles([]);
      setText("");
      setActivity([]);
      setCost(undefined);
      if (!conversation) {
        onCreated(id, userMessage);
      } else attach(r);
      onRefresh();
    } catch (e) {
      setError(message(e));
    } finally {
      setSending(false);
    }
  }
  async function decide(
    a: Approval,
    allow: boolean,
    remember?: "session" | "always",
    explanation?: string,
  ) {
    try {
      await api(
        `/v1/runs/${run!.id}/approvals/${enc(a.id)}`,
        "POST",
        decisionBody(a, allow, remember, explanation),
      );
      setApprovals((as) => as.filter((x) => x.id !== a.id));
    } catch (e) {
      setError(message(e));
      if (e instanceof ApiError && e.status === 404 && run) {
        const snapshot = await api<Run>(`/v1/runs/${run.id}`);
        setApprovals(snapshot.pending ?? []);
      }
    }
  }
  async function speak(value: string) {
    try {
      for (let at = 0; at < value.length; at += 4096) {
        const r = await response("/v1/audio/speech", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ text: value.slice(at, at + 4096) }),
        });
        const url = URL.createObjectURL(await r.blob());
        try {
          await new Promise<void>((resolve, reject) => {
            const a = new Audio(url);
            playback.current = a;
            a.onended = () => resolve();
            a.onerror = () => reject(new Error(t("error")));
            a.play().catch(reject);
          });
        } finally {
          URL.revokeObjectURL(url);
        }
      }
    } catch (e) {
      setError(message(e));
      voiceRef.current = false;
      setVoice(false);
    }
  }
  async function startRecording() {
    try {
      if (!navigator.mediaDevices || !window.MediaRecorder)
        throw new Error(t("notSupported"));
      const ms = await navigator.mediaDevices.getUserMedia({ audio: true });
      media.current = ms;
      const rec = new MediaRecorder(ms);
      recorder.current = rec;
      const chunks: BlobPart[] = [];
      rec.ondataavailable = (e) => chunks.push(e.data);
      rec.onstop = async () => {
        ms.getTracks().forEach((x) => x.stop());
        setRecording(false);
        try {
          const blob = new Blob(chunks, { type: rec.mimeType });
          const r = await response("/v1/audio/transcriptions", {
            method: "POST",
            headers: { "Content-Type": blob.type },
            body: blob,
          });
          const { text } = (await r.json()) as { text: string };
          if (voiceRef.current) await sendRef.current(undefined, text);
          else setDraft((d) => d + (d ? " " : "") + text);
        } catch (e) {
          setError(message(e));
        }
      };
      rec.start();
      setRecording(true);
      if (voiceRef.current) {
        const context = new AudioContext();
        const analyser = context.createAnalyser();
        context.createMediaStreamSource(ms).connect(analyser);
        const samples = new Uint8Array(analyser.fftSize);
        let speaking = false;
        let lastSound = performance.now();
        const timer = setInterval(() => {
          if (rec.state !== "recording") {
            clearInterval(timer);
            void context.close();
            return;
          }
          analyser.getByteTimeDomainData(samples);
          const volume = Math.sqrt(
            samples.reduce((s, x) => s + ((x - 128) / 128) ** 2, 0) /
              samples.length,
          );
          if (volume > 0.015) {
            speaking = true;
            lastSound = performance.now();
          }
          if (speaking && performance.now() - lastSound > 1400) rec.stop();
        }, 100);
      }
    } catch (e) {
      setError(message(e));
      voiceRef.current = false;
      setVoice(false);
    }
  }
  return (
    <div className="chat-layout">
      <section className="chat">
        <header className="chat-header">
          <label className="agent-picker">
            {t("agent")}{" "}
            <select
              value={profile}
              disabled={active}
              onChange={(e) => setProfile(e.target.value)}
            >
              {profiles.map((p) => (
                <option key={p.name} value={p.name}>
                  {agentTitle(p, t)}
                </option>
              ))}
            </select>
          </label>
          <button className="quiet" onClick={() => setArtifacts((a) => !a)}>
            {t("artifacts")}
          </button>
        </header>
        {lost && (
          <div className="banner" role="status">
            {t("reconnecting")}
          </div>
        )}
        <div className="chat-scroll" ref={scroller}>
          {from > 0 && (
            <button
              onClick={() =>
                load(conversation, true).catch((e) => setError(message(e)))
              }
            >
              {t("history")}
            </button>
          )}
          {messages.length === 0 && !active && !sending ? (
            <div className="welcome">
              <div className="sigil" aria-hidden="true">
                <Logo size={40} />
              </div>
              <h1>{t("welcome")}</h1>
              <p>{t("welcomeText")}</p>
              <div className="suggestions">
                {(["suggest1", "suggest2", "suggest3"] as const).map((k) => (
                  <button
                    key={k}
                    onClick={() => {
                      setDraft(t(k));
                      input.current?.focus();
                    }}
                  >
                    {t(k)}
                  </button>
                ))}
              </div>
            </div>
          ) : (
            <div className="messages">
              {messages.map((m, i) => (
                <article key={i} className={"message " + m.role}>
                  <p className="speaker">
                    {m.role === "user"
                      ? t("you")
                      : m.role === "tool"
                        ? t("activity")
                        : profile}
                  </p>
                  {m.text && <Markdown text={m.text} />}{" "}
                  {m.attachments?.map((a, j) => (
                    <span key={j} className="chip">
                      {a.mime} · {Math.round(a.bytes / 1024)} KB
                    </span>
                  ))}
                  {m.calls?.map((c) => (
                    <details key={c.id}>
                      <summary>{c.name}</summary>
                      <pre>{JSON.stringify(c.input, null, 2)}</pre>
                    </details>
                  ))}
                  {m.result && (
                    <details>
                      <summary>
                        {m.result.is_error ? t("error") : t("activity")}
                      </summary>
                      <pre>{m.result.content}</pre>
                    </details>
                  )}
                  {audio.speech && m.role === "assistant" && m.text && (
                    <button className="quiet" onClick={() => speak(m.text!)}>
                      {t("read")}
                    </button>
                  )}
                </article>
              ))}
              {activity.length > 0 && (
                <div className="activity">
                  {activity.map((a) => (
                    <details key={a.call.id}>
                      <summary>
                        {a.worker && `${a.worker} · `}
                        {a.call.name}{" "}
                        <span className="muted">
                          {a.ms === undefined ? "…" : `${a.ms} ms`}
                          {a.error ? " !" : ""}
                        </span>
                      </summary>
                      <pre>{JSON.stringify(a.call.input, null, 2)}</pre>
                      {a.output && <pre>{a.output}</pre>}
                    </details>
                  ))}
                </div>
              )}
              <div aria-live="polite" aria-atomic="false">
                {text && (
                  <article className="message assistant">
                    <p className="speaker">{profile}</p>
                    <Markdown text={text} />
                  </article>
                )}
              </div>
              {approvals.map((a) => (
                <ApprovalCard key={a.id} approval={a} onDecide={decide} />
              ))}
              {cost !== undefined && <p className="cost">${cost.toFixed(4)}</p>}
            </div>
          )}
        </div>
        <div className="composer-wrap">
          <ErrorBox error={error} />
          <form
            className="composer"
            onSubmit={send}
            onDragOver={(e) => e.preventDefault()}
            onDrop={(e) => {
              e.preventDefault();
              addFiles(Array.from(e.dataTransfer.files));
            }}
          >
            {files.length > 0 && (
              <div className="attachments">
                {files.map((f, i) => (
                  <span className="chip" key={i}>
                    {f.name}
                    <button
                      type="button"
                      aria-label={`${t("remove")} ${f.name}`}
                      onClick={() =>
                        setFiles((fs) => fs.filter((_, j) => i !== j))
                      }
                    >
                      ×
                    </button>
                  </span>
                ))}
              </div>
            )}
            <textarea
              ref={input}
              aria-label={t("input")}
              placeholder={t("input")}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => {
                if (
                  e.key === "Enter" &&
                  !e.shiftKey &&
                  !e.nativeEvent.isComposing
                ) {
                  e.preventDefault();
                  void send();
                }
              }}
            />
            <div className="composer-actions">
              <input
                ref={fileInput}
                type="file"
                accept="image/png,image/jpeg,image/gif,image/webp,application/pdf"
                multiple
                hidden
                onChange={(e) => {
                  addFiles(Array.from(e.target.files ?? []));
                  e.target.value = "";
                }}
              />
              <button
                type="button"
                className="icon"
                aria-label={t("attach")}
                onClick={() => fileInput.current?.click()}
              >
                +
              </button>
              <div className="spacer" />
              {audio.transcribe && (
                <button
                  type="button"
                  aria-pressed={recording}
                  onClick={() =>
                    recording ? recorder.current?.stop() : startRecording()
                  }
                >
                  {t(recording ? "recording" : "mic")}
                </button>
              )}
              {audio.transcribe && audio.speech && (
                <button
                  type="button"
                  aria-pressed={voice}
                  onClick={() => {
                    const next = !voice;
                    voiceRef.current = next;
                    setVoice(next);
                    if (next) void startRecording();
                    else {
                      playback.current?.pause();
                      if (recording) recorder.current?.stop();
                    }
                  }}
                >
                  {t("voice")}
                </button>
              )}
              {active ? (
                <button
                  type="button"
                  onClick={() =>
                    api(`/v1/runs/${run.id}/cancel`, "POST").catch((e) =>
                      setError(message(e)),
                    )
                  }
                >
                  {t("stop")}
                </button>
              ) : (
                <button
                  className="primary"
                  type="submit"
                  disabled={sending || (!draft.trim() && !files.length)}
                >
                  {sending ? "…" : t("send")}
                </button>
              )}
            </div>
          </form>
          <p className="composer-caption">Ringil · {t("localAgent")}</p>
        </div>
      </section>
      {artifacts && conversation && (
        <Artifacts
          conversation={conversation}
          revision={revision}
          onClose={() => setArtifacts(false)}
        />
      )}
    </div>
  );
}
