"use client";

import { useEffect, useState } from "react";
import { get, send } from "@/lib/api";
import { Brand } from "@/lib/auth";
import { applyBrand, can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { Gate, Shell } from "@/components/Shell";
import type { AccountRow } from "@/components/AccountPicker";
import { Button, Card, Field, Input, Loading, Notice, Select, useAction } from "@/components/ui";

function Branding() {
  const { me, reload: reloadSession } = useSession();
  const platform = me?.account_role === "platform";
  const [acct, setAcct] = useState("");
  useEffect(() => { if (me && !acct) setAcct(new URLSearchParams(window.location.search).get("account") ?? me.account_id); }, [me, acct]);
  const own = acct === me?.account_id;
  // The platform also edits resellers' brands: it alone sets portal domains.
  const resellers = useData(() => (platform ? get<AccountRow[]>("/v1/accounts?role=reseller&limit=500") : Promise.resolve([])), [platform]);
  const { data, error } = useData(() => (acct ? get<{ own: Brand | null; effective: Brand }>(`/v1/accounts/${acct}/branding`) : Promise.resolve(null)), [acct]);
  const [f, setF] = useState({ name: "", logo: "", primary_color: "#1f6feb", support_email: "", domain: "" });
  useEffect(() => {
    const b = data?.own ?? data?.effective;
    if (b) setF({ name: b.name, logo: b.logo, primary_color: b.primary_color, support_email: b.support_email, domain: data?.own?.domain ?? "" });
  }, [data]);
  const a = useAction();

  function onLogo(file: File | undefined) {
    if (!file) return;
    // Sent inline in the request, which the server caps at 64 KB.
    if (file.size > 45_000) return a.setError("Logo must be under 45 KB");
    const r = new FileReader();
    r.onload = () => setF((x) => ({ ...x, logo: String(r.result) }));
    r.readAsDataURL(file);
  }

  return (
    <Shell title="Branding" roles={["platform", "reseller"]}>
      {!data && !error && <Loading />}
      <Notice>{error}</Notice>
      <div className="grid cols-2">
        <Card title={own ? "Your brand" : "Reseller's brand"}>
          {platform && (
            <div className="form-row" style={{ marginBottom: 12 }}>
              <Field label="Brand of">
                <Select value={acct} onChange={(e) => setAcct(e.target.value)}>
                  <option value={me!.account_id}>{me!.account_name} (platform)</option>
                  {(resellers.data ?? []).map((r) => <option key={r.id} value={r.id}>{r.name}</option>)}
                </Select>
              </Field>
            </div>
          )}
          <p className="muted">{!platform
            ? "Your customers see this name, logo and colour, on your own domain if you have one. They never see the platform's brand."
            : own ? "The platform brand: shown on the main domain and to accounts without a reseller brand."
            : "This reseller's customers see its brand. Set its portal domain once its CNAME, TLS certificate and sign-in redirect are in place."}</p>
          <form className="form" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
            await send("PUT", `/v1/accounts/${acct}/branding`, f);
            if (own) reloadSession();
          }, "Saved"); }}>
            <Field label="Display name"><Input value={f.name} maxLength={60} onChange={(e) => setF({ ...f, name: e.target.value })} required /></Field>
            <Field label="Logo" hint="PNG, SVG, JPEG or WebP under 45 KB">
              <Input type="file" accept="image/png,image/svg+xml,image/jpeg,image/webp" onChange={(e) => onLogo(e.target.files?.[0])} />
            </Field>
            <Field label="Colour"><Input type="color" value={f.primary_color} onChange={(e) => { setF({ ...f, primary_color: e.target.value }); if (own) applyBrand({ ...f, primary_color: e.target.value }); }} /></Field>
            <Field label="Support e-mail"><Input type="email" value={f.support_email} onChange={(e) => setF({ ...f, support_email: e.target.value })} /></Field>
            <Field label="Portal domain" hint={platform
              ? "e.g. sms.reseller.com: add its TLS certificate and sign-in redirect first (docs/phase4-portals.md)"
              : "Set by the platform: point a CNAME for your domain at the portal host and ask the platform team to add it"}>
              <Input value={f.domain} onChange={(e) => setF({ ...f, domain: e.target.value.trim().toLowerCase() })} placeholder={platform ? "optional" : "none"} disabled={!platform} />
            </Field>
            {can(me, "admin") && <Button type="submit" busy={a.busy}>Save</Button>}
          </form>
          <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
        </Card>
        <Card title="Preview">
          <div className="login-brand">
            {f.logo ? <img src={f.logo} alt="" /> : <span className="brand-mark" style={{ background: f.primary_color }}>{f.name[0] ?? "?"}</span>}
            <span>{f.name || "Your brand"}</span>
          </div>
          <p className="muted">Sign in to send messages, follow delivery and manage your account.</p>
          <button className="btn" style={{ background: f.primary_color, color: "#fff" }}>Sign in</button>
        </Card>
      </div>
    </Shell>
  );
}

export default function Page() {
  return <Gate roles={["platform", "reseller"]} title="Branding"><Branding /></Gate>;
}
