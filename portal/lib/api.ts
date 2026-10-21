// API client: every call goes to /v1 on the same origin, signed in with the
// OIDC access token (or, in dev, the X-Actor headers of the picked user).

import { accessToken, devSession, signOut } from "./auth";

export class ApiError extends Error {
  constructor(public status: number, message: string, public lines?: string[]) {
    super(message);
  }
}

async function headers(extra?: HeadersInit): Promise<Headers> {
  const h = new Headers(extra);
  const dev = devSession();
  if (dev) {
    h.set("X-Actor-Account", dev.account_id);
    h.set("X-Actor-User", dev.email);
  } else {
    const t = await accessToken();
    if (t) h.set("Authorization", `Bearer ${t}`);
  }
  return h;
}

export async function api<T = unknown>(path: string, init: RequestInit = {}): Promise<T> {
  const h = await headers(init.headers);
  if (init.body && typeof init.body === "string" && !h.has("Content-Type")) h.set("Content-Type", "application/json");
  const res = await fetch(path, { ...init, headers: h });
  if (res.status === 401) {
    await signOut();
    throw new ApiError(401, "Your session ended. Please sign in again.");
  }
  const text = await res.text();
  let body: any = undefined;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    body = text;
  }
  if (!res.ok) {
    throw new ApiError(res.status, body?.error ?? body?.message ?? `Request failed (${res.status})`, body?.lines);
  }
  return body as T;
}

export const get = <T = unknown>(path: string) => api<T>(path);
export const send = <T = unknown>(method: string, path: string, body?: unknown, headers?: Record<string, string>) =>
  api<T>(path, {
    method,
    body: body === undefined ? undefined : typeof body === "string" ? body : JSON.stringify(body),
    headers,
  });

// newKey makes an idempotency key: keep one per form and replace it only
// after a success, so a retried submit is never booked twice.
export const newKey = () => (typeof crypto !== "undefined" && "randomUUID" in crypto ? crypto.randomUUID() : `${Date.now()}-${Math.random()}`);

// download fetches with credentials and saves the response as a file.
export async function download(path: string, filename: string) {
  const res = await fetch(path, { headers: await headers() });
  if (!res.ok) throw new ApiError(res.status, `Download failed (${res.status})`);
  const url = URL.createObjectURL(await res.blob());
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}

export function qs(params: Record<string, string | number | undefined | null>): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== "") p.set(k, String(v));
  const s = p.toString();
  return s ? `?${s}` : "";
}
