// Sign-in. Production: Keycloak (OIDC authorization code with PKCE) via
// oidc-client-ts; tokens stay in session storage. Dev (no OIDC configured):
// pick a user and send X-Actor headers, which admin-api accepts only with
// APP_ENV=dev.

import { UserManager, WebStorageStateStore } from "oidc-client-ts";

export type PortalConfig = {
  brand: Brand;
  oidc?: { issuer: string; client_id: string };
  dev_sign_in?: boolean;
};

export type Brand = { name: string; logo: string; primary_color: string; support_email: string; domain?: string };

export type DevSession = { account_id: string; email: string };

let manager: UserManager | null = null;

export function initOIDC(cfg: PortalConfig) {
  if (!cfg.oidc || manager || typeof window === "undefined") return;
  const origin = window.location.origin;
  manager = new UserManager({
    authority: cfg.oidc.issuer,
    client_id: cfg.oidc.client_id,
    redirect_uri: `${origin}/callback/`,
    post_logout_redirect_uri: `${origin}/login/`,
    response_type: "code",
    scope: "openid email profile",
    automaticSilentRenew: true,
    userStore: new WebStorageStateStore({ store: window.sessionStorage }),
  });
}

export async function signIn(returnTo = "/") {
  if (!manager) throw new Error("sign-in is not configured");
  await manager.signinRedirect({ state: { returnTo } });
}

export async function completeSignIn(): Promise<string> {
  if (!manager) throw new Error("sign-in is not configured");
  const user = await manager.signinRedirectCallback();
  return (user.state as { returnTo?: string } | undefined)?.returnTo ?? "/";
}

export async function accessToken(): Promise<string | null> {
  if (!manager) return null;
  const user = await manager.getUser();
  if (!user || user.expired) return null;
  return user.access_token;
}

export function devSession(): DevSession | null {
  if (typeof window === "undefined") return null;
  const raw = window.sessionStorage.getItem("dev-session");
  return raw ? (JSON.parse(raw) as DevSession) : null;
}

export function setDevSession(s: DevSession) {
  window.sessionStorage.setItem("dev-session", JSON.stringify(s));
}

export async function isSignedIn(): Promise<boolean> {
  return devSession() !== null || (await accessToken()) !== null;
}

export async function signOut() {
  if (typeof window === "undefined") return;
  window.sessionStorage.removeItem("dev-session");
  if (manager && (await manager.getUser())) {
    await manager.removeUser();
    await manager.signoutRedirect().catch(() => (window.location.href = "/login/"));
    return;
  }
  if (!window.location.pathname.startsWith("/login")) window.location.href = "/login/";
}
