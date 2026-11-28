import { useEffect, useState } from "react";
import { api, enc, message } from "./api";
import type { Conversation, Profile, Project, Message } from "./types";
import { I18n, translate, type Language, type Key } from "./i18n";
import { Chat } from "./Chat";
import { Settings, Setup, settingsTabs } from "./Settings";
import { Projects } from "./Projects";
import { Connections } from "./Connections";
import { Automations } from "./Automations";
import { Agents } from "./Agents";
import { Workflows } from "./Workflows";
import { ErrorBox, Confirm } from "./ui";
import {
  Logo,
  NewChatIcon,
  PlugIcon,
  ClockIcon,
  WorkflowIcon,
  FolderIcon,
  BotIcon,
  SettingsIcon,
  PanelIcon,
  MoreIcon,
  PinIcon,
  BackIcon,
  ModelsIcon,
  KeyIcon,
  WalletIcon,
  InstructionsIcon,
  MicIcon,
  MemoryIcon,
  ChannelsIcon,
  ComposioIcon,
  HooksIcon,
  ArchiveIcon,
  PaletteIcon,
} from "./icons";
export function App() {
  const [language, setLanguage] = useState<Language>(
    (localStorage.getItem("ringil.language") ||
      navigator.language.slice(0, 2)) === "it"
      ? "it"
      : "en",
  );
  const t = (k: Key) => translate(language, k);
  const [theme, setTheme] = useState(
    localStorage.getItem("ringil.theme") || "system",
  );
  const [collapsed, setCollapsed] = useState(false);
  const [screen, setScreen] = useState<Key>("newChat");
  // Settings has its own sidebar view; back returns to the screen it was
  // opened from.
  const [settingsTab, setSettingsTab] = useState<Key>("models");
  const [back, setBack] = useState<Key>("newChat");
  const [conversation, setConversation] = useState("");
  const [chatKey, setChatKey] = useState(0);
  const [initialMessage, setInitialMessage] = useState<{
    id: string;
    message: Message;
  }>();
  const [chatProject, setChatProject] = useState("");
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [signedout, setSignedout] = useState(false);
  const [setup, setSetup] = useState<boolean | null>(null);
  const [error, setError] = useState("");
  const [deleting, setDeleting] = useState("");
  const [editing, setEditing] = useState<Conversation | null>(null);
  const [editTitle, setEditTitle] = useState("");
  const [moveProject, setMoveProject] = useState("");
  function refresh() {
    Promise.all([
      api<Conversation[]>("/v1/conversations"),
      api<Profile[]>("/v1/profiles"),
      api<Project[]>("/v1/projects"),
    ])
      .then(([c, p, j]) => {
        setError("");
        setConversations(c ?? []);
        // The main agent first, then the others as the server lists them.
        setProfiles(
          [...(p ?? [])].sort(
            (a, b) =>
              Number(b.name === "default") - Number(a.name === "default"),
          ),
        );
        setProjects(j ?? []);
      })
      .catch((e) => setError(message(e)));
  }
  useEffect(() => {
    const handler = () => setSignedout(true);
    window.addEventListener("signedout", handler);
    refresh();
    api<{ ready: boolean }>("/v1/setup")
      .then((s) => setSetup(!s.ready))
      .catch((e) => setError(message(e)));
    return () => window.removeEventListener("signedout", handler);
  }, []);
  useEffect(() => {
    document.documentElement.lang = language;
    localStorage.setItem("ringil.language", language);
  }, [language]);
  useEffect(() => {
    localStorage.setItem("ringil.theme", theme);
    const mq = matchMedia("(prefers-color-scheme: dark)");
    const apply = () =>
      (document.documentElement.dataset.theme =
        theme === "system" ? (mq.matches ? "dark" : "light") : theme);
    apply();
    mq.addEventListener("change", apply);
    return () => mq.removeEventListener("change", apply);
  }, [theme]);
  function openChat(id = "", project = "") {
    setChatKey((k) => k + 1);
    setConversation(id);
    setChatProject(project);
    setScreen("newChat");
  }
  const sections: Key[] = [
    "connections",
    "automations",
    "workflows",
    "projects",
    "agents",
  ];
  const icons = [
    <PlugIcon key="c" />,
    <ClockIcon key="a" />,
    <WorkflowIcon key="w" />,
    <FolderIcon key="p" />,
    <BotIcon key="g" />,
  ];
  const settingsIcons = [
    <ModelsIcon key="models" />,
    <KeyIcon key="keys" />,
    <WalletIcon key="limits" />,
    <InstructionsIcon key="instructions" />,
    <MicIcon key="voice" />,
    <MemoryIcon key="memory" />,
    <ChannelsIcon key="channels" />,
    <ComposioIcon key="composio" />,
    <HooksIcon key="hooks" />,
    <ArchiveIcon key="state" />,
    <PaletteIcon key="appearance" />,
  ];
  return (
    <I18n.Provider value={{ language, t }}>
      {signedout ? (
        <main className="signin">
          <div className="sigil">
            <Logo size={40} />
          </div>
          <h1>{t("signedout")}</h1>
          <p>{t("signedoutText")}</p>
          <pre>ringil serve link</pre>
        </main>
      ) : setup === true ? (
        <Setup
          onReady={() => {
            setSetup(false);
            refresh();
          }}
        />
      ) : (
        <div className={"shell " + (collapsed ? "collapsed" : "")}>
          <aside className="sidebar">
            <header>
              <span className="brand">
                <Logo size={22} />
                <span className="wordmark nav-label">Ringil Agent</span>
              </span>
              <button
                className="quiet icon-button"
                aria-label={t("collapse")}
                title={t("collapse")}
                onClick={() => setCollapsed((c) => !c)}
              >
                <PanelIcon />
              </button>
            </header>
            {screen === "settings" ? (
              <>
                <button
                  className="new-chat"
                  aria-label={t("back")}
                  title={t("back")}
                  onClick={() => setScreen(back)}
                >
                  <BackIcon />
                  <span className="nav-label">{t("back")}</span>
                </button>
                <nav aria-label={t("settings")}>
                  {settingsTabs.map((k, i) => (
                    <button
                      key={k}
                      aria-label={t(k)}
                      title={t(k)}
                      aria-current={settingsTab === k ? "page" : undefined}
                      className={settingsTab === k ? "selected" : ""}
                      onClick={() => setSettingsTab(k)}
                    >
                      {settingsIcons[i]}
                      <span className="nav-label">{t(k)}</span>
                    </button>
                  ))}
                </nav>
              </>
            ) : (
              <>
                <button
                  className="new-chat"
                  aria-label={t("newChat")}
                  onClick={() => openChat()}
                  title={t("newChat")}
                >
                  <NewChatIcon />
                  <span className="nav-label">{t("newChat")}</span>
                </button>
                <nav aria-label="Ringil">
                  {sections.map((s, i) => (
                    <button
                      key={s}
                      aria-label={t(s)}
                      title={t(s)}
                      className={screen === s ? "selected" : ""}
                      onClick={() => setScreen(s)}
                    >
                      {icons[i]}
                      <span className="nav-label">{t(s)}</span>
                    </button>
                  ))}
                </nav>
                <div className="conversation-list">
                  <h2 className="nav-label">{t("conversations")}</h2>
                  {conversations.map((c) => (
                    <div
                      key={c.id}
                      className={
                        "conversation-row " +
                        (conversation === c.id && screen === "newChat"
                          ? "selected"
                          : "")
                      }
                    >
                      <button
                        aria-label={c.title || c.id}
                        title={c.title || c.id}
                        onClick={() => openChat(c.id, c.project)}
                      >
                        <span className="nav-label">{c.title || c.id}</span>
                        {c.pinned && (
                          <span className="pin nav-label">
                            <PinIcon size={14} />
                          </span>
                        )}
                      </button>
                      <details className="conversation-menu">
                        <summary aria-label={t("edit")} title={t("edit")}>
                          <MoreIcon size={16} />
                        </summary>
                        <div className="menu">
                          <button
                            onClick={() => {
                              setEditing(c);
                              setEditTitle(c.title || "");
                              setMoveProject(c.project || "");
                            }}
                          >
                            {t("rename")} / {t("moveProject")}
                          </button>
                          <button
                            onClick={() =>
                              api(`/v1/conversations/${enc(c.id)}`, "PATCH", {
                                pinned: !c.pinned,
                              })
                                .then(refresh)
                                .catch((e) => setError(message(e)))
                            }
                          >
                            {t(c.pinned ? "unpin" : "pin")}
                          </button>
                          <button onClick={() => setDeleting(c.id)}>
                            {t("delete")}
                          </button>
                        </div>
                      </details>
                    </div>
                  ))}
                </div>
                <button
                  className="settings-nav"
                  aria-label={t("settings")}
                  title={t("settings")}
                  onClick={() => {
                    setBack(screen);
                    setScreen("settings");
                  }}
                >
                  <SettingsIcon />
                  <span className="nav-label">{t("settings")}</span>
                </button>
              </>
            )}
          </aside>
          <main className="main">
            <ErrorBox error={error} />
            {deleting && (
              <Confirm
                text={t("deleteConfirm")}
                onCancel={() => setDeleting("")}
                onConfirm={() =>
                  api(`/v1/conversations/${enc(deleting)}`, "DELETE")
                    .then(() => {
                      if (conversation === deleting) openChat();
                      setDeleting("");
                      refresh();
                    })
                    .catch((e) => setError(message(e)))
                }
              />
            )}{" "}
            {editing && (
              <form
                className="surface edit-conversation"
                onSubmit={(e) => {
                  e.preventDefault();
                  api(`/v1/conversations/${enc(editing.id)}`, "PATCH", {
                    title: editTitle,
                    project: moveProject,
                  })
                    .then(() => {
                      setEditing(null);
                      refresh();
                    })
                    .catch((e) => setError(message(e)));
                }}
              >
                <label>
                  {t("title")}
                  <input
                    value={editTitle}
                    onChange={(e) => setEditTitle(e.target.value)}
                  />
                </label>
                <label>
                  {t("moveProject")}
                  <select
                    value={moveProject}
                    onChange={(e) => setMoveProject(e.target.value)}
                  >
                    <option value="">{t("noProject")}</option>
                    {projects.map((p) => (
                      <option key={p.id} value={p.id}>
                        {p.name}
                      </option>
                    ))}
                  </select>
                </label>
                <button>{t("save")}</button>
                <button type="button" onClick={() => setEditing(null)}>
                  {t("cancel")}
                </button>
              </form>
            )}
            {screen === "newChat" ? (
              <Chat
                key={chatKey}
                conversation={conversation}
                project={chatProject}
                profiles={profiles}
                initialMessage={
                  initialMessage?.id === conversation
                    ? initialMessage.message
                    : undefined
                }
                onCreated={(id, m) => {
                  setInitialMessage({ id, message: m });
                  setConversation(id);
                }}
                onRefresh={refresh}
              />
            ) : (
              <div className="page">
                {screen === "settings" ? (
                  <Settings
                    tab={settingsTab}
                    language={language}
                    theme={theme}
                    onLanguage={setLanguage}
                    onTheme={setTheme}
                    onRefresh={refresh}
                  />
                ) : screen === "projects" ? (
                  <Projects
                    profiles={profiles}
                    onChat={openChat}
                    onRefresh={refresh}
                  />
                ) : screen === "connections" ? (
                  <Connections />
                ) : screen === "automations" ? (
                  <Automations profiles={profiles} onChat={openChat} />
                ) : screen === "agents" ? (
                  <Agents onRefresh={refresh} />
                ) : screen === "workflows" ? (
                  <Workflows profiles={profiles} />
                ) : null}
              </div>
            )}
          </main>
        </div>
      )}
    </I18n.Provider>
  );
}
