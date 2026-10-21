"use client";

// Session context: portal config (brand, sign-in method) and the signed-in
// user (/v1/me). The brand colour is applied as a CSS variable, so a
// reseller's portal looks like the reseller.

import { createContext, useCallback, useContext, useEffect, useState } from "react";
import { get } from "./api";
import { Brand, PortalConfig, initOIDC, isSignedIn } from "./auth";

export type Me = {
  email: string;
  account_id: string;
  account_name: string;
  account_role: "platform" | "reseller" | "customer";
  user_role: string;
  permission: "read" | "finance" | "admin";
  brand: Brand;
};

type Session = {
  config: PortalConfig | null;
  me: Me | null;
  loading: boolean;
  error: string | null;
  reload: () => void;
};

const Ctx = createContext<Session>({ config: null, me: null, loading: true, error: null, reload: () => {} });

export function applyBrand(b: Brand | undefined) {
  if (!b || typeof document === "undefined") return;
  document.documentElement.style.setProperty("--brand", b.primary_color || "#1f6feb");
  document.title = b.name;
}

export function SessionProvider({ children }: { children: React.ReactNode }) {
  const [config, setConfig] = useState<PortalConfig | null>(null);
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const cfg = await get<PortalConfig>("/v1/portal/config");
      initOIDC(cfg);
      setConfig(cfg);
      applyBrand(cfg.brand);
      if (await isSignedIn()) {
        const m = await get<Me>("/v1/me");
        setMe(m);
        applyBrand(m.brand);
      } else {
        setMe(null);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  return <Ctx.Provider value={{ config, me, loading, error, reload: load }}>{children}</Ctx.Provider>;
}

export const useSession = () => useContext(Ctx);

// can reports whether the user's permission covers need.
export function can(me: Me | null, need: "read" | "finance" | "admin"): boolean {
  if (!me) return false;
  const rank = { read: 0, finance: 1, admin: 2 };
  return rank[me.permission] >= rank[need];
}
