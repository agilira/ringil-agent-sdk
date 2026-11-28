import { describe, it, expect } from "vitest";
import { approvalsAfter, boardMove, decisionBody } from "../src/state";
import type { Approval, Card } from "../src/types";
const a: Approval = {
  id: "a1",
  call_id: "c1",
  tool: "bash",
  input: { command: "pwd" },
};
describe("approval lifecycle", () => {
  it("uses approval id, deduplicates replay and closes decisions", () => {
    const pending = approvalsAfter([a], {
      id: 2,
      kind: "approval",
      approval: a,
    });
    expect(pending).toEqual([a]);
    expect(
      approvalsAfter(pending, {
        id: 3,
        kind: "approval_decided",
        decision: { id: "a1", allowed: false },
      }),
    ).toEqual([]);
  });
  it("clears all outstanding requests at run_end", () =>
    expect(approvalsAfter([a], { id: 5, kind: "run_end" })).toEqual([]));
  it("does not close on an unrelated tool result", () =>
    expect(approvalsAfter([a], { id: 3, kind: "tool_end" })).toEqual([a]));
});
describe("board moves", () => {
  const cards: Card[] = [
    { id: "c1", title: "A", column: "To do" },
    { id: "c2", title: "B", column: "To do" },
    { id: "c3", title: "C", column: "Done" },
  ];
  it("reorders without mutating the original", () => {
    expect(
      boardMove(cards, "c2", "To do", 0)
        .filter((c) => c.column === "To do")
        .map((c) => c.id),
    ).toEqual(["c2", "c1"]);
    expect(cards[0].id).toBe("c1");
  });
  it("moves across columns at a chosen position", () =>
    expect(
      boardMove(cards, "c1", "Done", 0)
        .filter((c) => c.column === "Done")
        .map((c) => c.id),
    ).toEqual(["c1", "c3"]));
  it("appends for negative or oversized index", () => {
    for (const i of [-1, 100])
      expect(
        boardMove(cards, "c1", "Done", i)
          .filter((c) => c.column === "Done")
          .map((c) => c.id),
      ).toEqual(["c3", "c1"]);
  });
  it("refuses an unknown card", () =>
    expect(() => boardMove(cards, "no", "Done", 0)).toThrow());
});

describe("approval request contract", () => {
  it("keeps one-call approval compatible with servers without rule support", () =>
    expect(decisionBody(a, true)).toEqual({ allow: true }));
  it("remembers only a permission rule advertised by the server", () => {
    expect(() => decisionBody(a, true, "always")).toThrow();
    expect(
      decisionBody({ ...a, rule: "write_file(notes/*)" }, true, "always"),
    ).toEqual({ allow: true, remember: "always" });
  });
  it("sends a refusal explanation only when supported", () => {
    expect(decisionBody(a, false, undefined, "Try reading first")).toEqual({
      allow: false,
    });
    expect(
      decisionBody(
        { ...a, explain: true },
        false,
        undefined,
        "Try reading first",
      ),
    ).toEqual({ allow: false, message: "Try reading first" });
    expect(() => decisionBody({ ...a, rule: "x" }, false, "session")).toThrow();
  });
});
