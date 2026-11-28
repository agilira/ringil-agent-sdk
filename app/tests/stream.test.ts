import { describe, it, expect } from "vitest";
import { SSEParser, type SSE } from "../src/stream";
describe("SSE parser", () => {
  it("handles arbitrary chunks, CRLF, comments and UTF-8 text", () => {
    const events: SSE[] = [];
    const p = new SSEParser((e) => events.push(e));
    for (const c of ': alive\r\nid: 12\r\ndata: {"kind":"text","text":"ciao è"}\r\n\r\n')
      p.feed(c);
    expect(events).toEqual([
      { id: "12", event: "message", data: '{"kind":"text","text":"ciao è"}' },
    ]);
  });
  it("handles dropped events, multiline data and final run_end", () => {
    const events: SSE[] = [];
    const p = new SSEParser((e) => events.push(e));
    p.feed(
      'event: dropped\ndata: {"missed":3}\n\nid: 5\ndata: one\ndata: two\n\nid: 6\ndata: {"kind":"run_end"}\n\n',
    );
    expect(events).toEqual([
      { event: "dropped", data: '{"missed":3}', id: undefined },
      { event: "message", data: "one\ntwo", id: "5" },
      { event: "message", data: '{"kind":"run_end"}', id: "6" },
    ]);
  });
  it("does not dispatch an unfinished event", () => {
    const events: SSE[] = [];
    new SSEParser((e) => events.push(e)).feed("data: incomplete\n");
    expect(events).toEqual([]);
  });
});

import { vi, afterEach } from "vitest";
import { streamRun } from "../src/stream";
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});
it("resumes with the last event ID, skips duplicates, and ends only at run_end", async () => {
  vi.useFakeTimers();
  const fetcher = vi
    .fn()
    .mockResolvedValueOnce(
      new Response(
        'id: 1\ndata: {"id":1,"kind":"done"}\n\nid: 2\ndata: {"id":2,"kind":"text","text":"next"}\n\n',
      ),
    )
    .mockResolvedValueOnce(
      new Response(
        'id: 2\ndata: {"id":2,"kind":"text","text":"next"}\n\nid: 3\ndata: {"id":3,"kind":"run_end"}\n\n',
      ),
    );
  vi.stubGlobal("fetch", fetcher);
  const events: number[] = [];
  const connection = vi.fn();
  const pending = streamRun(
    "r",
    (e) => events.push(e.id),
    connection,
    new AbortController().signal,
    vi.fn(),
  );
  await vi.advanceTimersByTimeAsync(1000);
  await pending;
  expect(events).toEqual([1, 2, 3]);
  expect(fetcher.mock.calls[1][0]).toBe("/v1/runs/r/events?after=2");
  expect(connection).toHaveBeenCalledWith(true);
  expect(fetcher).toHaveBeenCalledTimes(2);
});
