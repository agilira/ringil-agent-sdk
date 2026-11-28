import { response, ApiError } from "./api";
import type { RunEvent } from "./types";
export interface SSE {
  id?: string;
  event: string;
  data: string;
}
export class SSEParser {
  private buffer = "";
  private data: string[] = [];
  private event = "message";
  private id: string | undefined;
  constructor(private emit: (event: SSE) => void) {}
  feed(chunk: string) {
    this.buffer += chunk;
    let m: RegExpExecArray | null;
    while ((m = /\r\n|\n|\r(?!$)/.exec(this.buffer))) {
      const line = this.buffer.slice(0, m.index);
      this.buffer = this.buffer.slice(m.index + m[0].length);
      this.line(line);
    }
  }
  private line(line: string) {
    if (!line) {
      if (this.data.length)
        this.emit({
          event: this.event,
          data: this.data.join("\n"),
          id: this.id,
        });
      this.data = [];
      this.event = "message";
      return;
    }
    if (line.startsWith(":")) return;
    const i = line.indexOf(":");
    const field = i < 0 ? line : line.slice(0, i);
    const value = i < 0 ? "" : line.slice(i + 1).replace(/^ /, "");
    if (field === "data") this.data.push(value);
    if (field === "event") this.event = value;
    if (field === "id" && !value.includes("\0")) this.id = value;
  }
}
export async function streamRun(
  id: string,
  onEvent: (event: RunEvent) => void,
  onConnection: (lost: boolean) => void,
  signal: AbortSignal,
  onDropped: () => void,
) {
  let after = 0;
  let ended = false;
  let delay = 500;
  while (!signal.aborted && !ended) {
    try {
      const r = await response(`/v1/runs/${id}/events?after=${after}`, {
        signal,
      });
      if (!r.body) throw new Error("Empty stream");
      onConnection(false);
      const reader = r.body.getReader();
      const decoder = new TextDecoder();
      const parser = new SSEParser((e) => {
        if (e.event === "dropped") {
          onDropped();
          return;
        }
        const data = JSON.parse(e.data) as RunEvent;
        if (data.id <= after) return;
        after = data.id;
        onEvent(data);
        if (data.kind === "run_end") ended = true;
      });
      try {
        while (!ended) {
          const { value, done } = await reader.read();
          if (done) break;
          parser.feed(decoder.decode(value, { stream: true }));
        }
      } finally {
        await reader.cancel();
      }
      delay = 500;
      if (!ended) throw new Error("Stream interrupted");
    } catch (e) {
      if (signal.aborted) return;
      if (e instanceof ApiError && e.status < 500 && e.status !== 429) throw e;
      onConnection(true);
      await new Promise<void>((resolve) => {
        const done = () => {
          clearTimeout(timer);
          signal.removeEventListener("abort", done);
          resolve();
        };
        const timer = setTimeout(done, delay);
        signal.addEventListener("abort", done, { once: true });
      });
      delay = Math.min(delay * 2, 10000);
    }
  }
}
