import type { Approval, RunEvent, Card } from "./types";
export function approvalsAfter(current: Approval[], event: RunEvent) {
  if (event.kind === "approval" && event.approval)
    return [
      ...current.filter((a) => a.id !== event.approval!.id),
      event.approval,
    ];
  if (event.kind === "approval_decided")
    return current.filter((a) => a.id !== event.decision?.id);
  if (event.kind === "run_end") return [];
  return current;
}
export function boardMove(
  cards: Card[],
  id: string,
  column: string,
  index: number,
): Card[] {
  const card = cards.find((c) => c.id === id);
  if (!card) throw new Error("Unknown card");
  const rest = cards.filter((c) => c.id !== id);
  const target = rest.filter((c) => c.column === column);
  const position = index < 0 ? target.length : Math.min(index, target.length);
  target.splice(position, 0, { ...card, column });
  return [...rest.filter((c) => c.column !== column), ...target];
}

export function decisionBody(
  approval: Approval,
  allow: boolean,
  remember?: "session" | "always",
  message?: string,
) {
  if (remember && (!allow || !approval.rule))
    throw new Error("This approval cannot remember a rule");
  return {
    allow,
    ...(remember ? { remember } : {}),
    ...(!allow && approval.explain && message ? { message } : {}),
  };
}
// agentTitle is the name people read for an agent: its title, else
// "Main agent" for default and the id for the others.
export function agentTitle(
  p: { name: string; title?: string },
  t: (k: "mainAgent") => string,
) {
  return p.title || (p.name === "default" ? t("mainAgent") : p.name);
}
// agentID makes the id of a new agent from its title: letters and digits
// without accents, words joined by -, at most 64; never "default" nor one
// of taken (in any case).
export function agentID(title: string, taken: string[]) {
  const base =
    title
      .normalize("NFKD")
      .replace(/[̀-ͯ]/g, "")
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 56)
      .replace(/-+$/, "") || "agent";
  const used = new Set([...taken, "default"].map((n) => n.toLowerCase()));
  let id = base;
  for (let i = 2; used.has(id); i++) id = `${base}-${i}`;
  return id;
}
