import { describe, it, expect } from "vitest";
import { agentID, agentTitle } from "../src/state";
const t = () => "Main agent";
describe("agent names", () => {
  it("makes the id from the title, without accents, never default or taken", () => {
    expect(agentID("Assistente Vendite", [])).toBe("assistente-vendite");
    expect(agentID("Più è già", [])).toBe("piu-e-gia");
    expect(agentID("Default", [])).toBe("default-2");
    expect(agentID("Scrittore", ["Scrittore", "scrittore-2"])).toBe(
      "scrittore-3",
    );
    expect(agentID("日本語", [])).toBe("agent");
    const long = agentID("x".repeat(200), []);
    expect(long.length).toBeLessThanOrEqual(64);
    for (const s of ["a b", "--x--", "ﬁle", "Ⅻ", "a".repeat(64) + " b"])
      expect(agentID(s, ["agent"])).toMatch(/^[a-z0-9-]{1,64}$/);
  });
  it("reads the title, else Main agent for default and the id", () => {
    expect(agentTitle({ name: "default" }, t)).toBe("Main agent");
    expect(agentTitle({ name: "default", title: "Vendite" }, t)).toBe(
      "Vendite",
    );
    expect(agentTitle({ name: "writer" }, t)).toBe("writer");
  });
});
