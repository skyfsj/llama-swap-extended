import { get } from "svelte/store";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { authSession, refreshAuthSession, signInWithAPIKey, signOut } from "./auth";

describe("browser authentication session", () => {
  const fetchMock = vi.fn<typeof fetch>();

  beforeEach(() => {
    vi.stubGlobal("fetch", fetchMock);
    fetchMock.mockReset();
    authSession.set({ loading: true, configured: false, authenticated: false, error: "" });
  });

  afterEach(() => vi.unstubAllGlobals());

  it("loads an unauthenticated configured session without starting an open shell", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ configured: true, authenticated: false }), { status: 200 }));

    await refreshAuthSession();

    expect(get(authSession)).toEqual({ loading: false, configured: true, authenticated: false, error: "" });
    expect(fetchMock).toHaveBeenCalledWith("/api/auth/session", { credentials: "same-origin" });
  });

  it("promotes a successful API key exchange to an authenticated session", async () => {
    fetchMock.mockResolvedValue(new Response(JSON.stringify({ authenticated: true }), { status: 200 }));

    await signInWithAPIKey("local-key");

    expect(get(authSession)).toEqual({ loading: false, configured: true, authenticated: true, error: "" });
    expect(fetchMock).toHaveBeenCalledWith("/api/auth/session", expect.objectContaining({
      method: "POST",
      body: JSON.stringify({ key: "local-key" }),
    }));
  });

  it("returns to a configured unauthenticated session on sign out", async () => {
    authSession.set({ loading: false, configured: true, authenticated: true, error: "" });
    fetchMock.mockResolvedValue(new Response(null, { status: 204 }));

    await signOut();

    expect(get(authSession)).toEqual({ loading: false, configured: true, authenticated: false, error: "" });
  });
});
