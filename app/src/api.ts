export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message);
  }
}
export async function response(path: string, init: RequestInit = {}) {
  const r = await fetch(path, { ...init, credentials: "same-origin" });
  if (!r.ok) {
    let e: { error?: { code: string; message: string } } = {};
    try {
      e = await r.json();
    } catch {
      /* non-JSON proxy failure */
    }
    if (r.status === 401) window.dispatchEvent(new Event("signedout"));
    throw new ApiError(
      r.status,
      e.error?.code ?? "NETWORK",
      e.error?.message ?? `${r.status} ${r.statusText}`,
    );
  }
  return r;
}
export async function api<T = void>(
  path: string,
  method = "GET",
  body?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  const r = await response(path, {
    method,
    signal,
    ...(body === undefined
      ? {}
      : {
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        }),
  });
  const text = await r.text();
  return text.trim() ? (JSON.parse(text) as T) : (undefined as T);
}
export const enc = encodeURIComponent;
export function message(e: unknown) {
  return e instanceof Error ? e.message : String(e);
}
