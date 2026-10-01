import { writable } from "svelte/store";

export interface AuthSession {
  loading: boolean;
  configured: boolean;
  authenticated: boolean;
  error: string;
}

const initialSession: AuthSession = {
  loading: true,
  configured: false,
  authenticated: false,
  error: "",
};

export const authSession = writable<AuthSession>(initialSession);

let requestRevision = 0;

function responseError(payload: unknown, status: number): string {
  if (payload && typeof payload === "object" && "error" in payload) {
    const message = (payload as { error?: unknown }).error;
    if (typeof message === "string" && message.trim() !== "") return message;
  }
  return `HTTP ${status}`;
}

// Keep all browser-session reads in one store so the login screen and shell
// cannot race each other and start protected requests before authentication.
export async function refreshAuthSession(): Promise<void> {
  const revision = ++requestRevision;
  authSession.update((current) => ({ ...current, loading: true, error: "" }));
  try {
    const response = await fetch("/api/auth/session", { credentials: "same-origin" });
    const payload = await response.json().catch(() => ({})) as {
      configured?: boolean;
      authenticated?: boolean;
      error?: string;
    };
    if (!response.ok) throw new Error(responseError(payload, response.status));
    if (revision !== requestRevision) return;
    authSession.set({
      loading: false,
      configured: payload.configured === true,
      authenticated: payload.authenticated === true,
      error: "",
    });
  } catch (cause) {
    if (revision !== requestRevision) return;
    // Fail closed for the UI: without a successful probe we cannot establish
    // that an installation is intentionally open, so do not start API streams.
    authSession.set({
      loading: false,
      configured: true,
      authenticated: false,
      error: cause instanceof Error ? cause.message : String(cause),
    });
  }
}

export async function signInWithAPIKey(key: string): Promise<void> {
  const revision = ++requestRevision;
  const response = await fetch("/api/auth/session", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ key }),
  });
  const payload = await response.json().catch(() => ({})) as { authenticated?: boolean; error?: string };
  if (!response.ok) throw new Error(responseError(payload, response.status));
  if (revision !== requestRevision) return;
    authSession.set({ loading: false, configured: true, authenticated: payload.authenticated === true, error: "" });
}

export async function signOut(): Promise<void> {
  const revision = ++requestRevision;
  const response = await fetch("/api/auth/session", { method: "DELETE", credentials: "same-origin" });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  if (revision !== requestRevision) return;
  authSession.update((current) => ({ ...current, loading: false, authenticated: false, error: "" }));
}
