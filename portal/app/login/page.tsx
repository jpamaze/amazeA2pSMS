"use client";

import { useEffect, useState } from "react";
import { get } from "@/lib/api";
import { safePath, setDevSession, signIn } from "@/lib/auth";
import { useSession } from "@/lib/session";
import { Button, Card, Loading, Notice } from "@/components/ui";

type DevUser = { email: string; user_role: string; account_id: string; account_name: string; account_role: string };

export default function Login() {
  const { config, me, loading, error, reload } = useSession();
  const [users, setUsers] = useState<DevUser[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const next = typeof window !== "undefined" ? safePath(new URLSearchParams(window.location.search).get("next")) : "/";

  useEffect(() => {
    if (me) window.location.replace(next);
  }, [me, next]);
  useEffect(() => {
    if (config?.dev_sign_in) get<DevUser[]>("/v1/portal/dev-users").then(setUsers).catch((e) => setErr(e.message));
  }, [config]);

  if (loading) return <Loading />;
  const brand = config?.brand;
  return (
    <div className="login">
      <Card>
        <div className="login-brand">
          {brand?.logo && <img src={brand.logo} alt="" />}
          <span>{brand?.name ?? "SMS portal"}</span>
        </div>
        <p className="muted">Sign in to send messages, follow delivery and manage your account.</p>
        <Notice>{error ?? err}</Notice>
        {config?.oidc && (
          <Button onClick={() => signIn(next).catch((e) => setErr(e.message))}>Sign in</Button>
        )}
        {config?.dev_sign_in && (
          <>
            <Notice kind="info">Development sign-in: pick a user. Production uses single sign-on.</Notice>
            {!users ? (
              <Loading what="Loading users" />
            ) : users.length === 0 ? (
              <p className="muted">No users yet. Create one with <code>admin-user</code> or run devseed.</p>
            ) : (
              <div className="user-pick">
                {users.map((u) => (
                  <button
                    key={u.email}
                    onClick={() => {
                      setDevSession({ account_id: u.account_id, email: u.email });
                      reload();
                    }}
                  >
                    <strong>{u.account_name}</strong> <span className="muted">({u.account_role})</span>
                    <div className="small muted">{u.email} · {u.user_role}</div>
                  </button>
                ))}
              </div>
            )}
          </>
        )}
        {brand?.support_email && (
          <p className="small muted">Need access? <a href={`mailto:${brand.support_email}`}>{brand.support_email}</a></p>
        )}
      </Card>
    </div>
  );
}
