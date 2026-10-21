"use client";

import { useEffect, useState } from "react";
import { get, send } from "@/lib/api";
import { Brand } from "@/lib/auth";
import { applyBrand, can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { Gate, Shell } from "@/components/Shell";
import { Button, Card, Field, Input, Loading, Notice, useAction } from "@/components/ui";

function Branding() {
  const { me, reload: reloadSession } = useSession();
  const acct = me?.account_id;
  const { data, error } = useData(() => (acct ? get<{ own: Brand | null; effective: Brand }>(`/v1/accounts/${acct}/branding`) : Promise.resolve(null)), [acct]);
  const [f, setF] = useState({ name: "", logo: "", primary_color: "#1f6feb", support_email: "", domain: "" });
  useEffect(() => {
    const b = data?.own ?? data?.effective;
    if (b) setF({ name: b.name, logo: b.logo, primary_color: b.primary_color, support_email: b.support_email, domain: data?.own?.domain ?? "" });
  }, [data]);
  const a = useAction();

  function onLogo(file: File | undefined) {
    if (!file) return;
    if (file.size > 200_000) return a.setError("Logo must be under 200 KB");
    const r = new FileReader();
    r.onload = () => setF((x) => ({ ...x, logo: String(r.result) }));
    r.readAsDataURL(file);
  }

  return (
    <Shell title="Branding" roles={["platform", "reseller"]}>
      {!data && !error && <Loading />}
      <Notice>{error}</Notice>
      <div className="grid cols-2">
        <Card title="Your brand">
          <p className="muted">{me?.account_role === "reseller"
            ? "Your customers see this name, logo and colour, on your own domain if you set one. They never see the platform's brand."
            : "The platform brand: shown on the main domain and to accounts without a reseller brand."}</p>
          <form className="form" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
            await send("PUT", `/v1/accounts/${acct}/branding`, f);
            reloadSession();
          }, "Saved"); }}>
            <Field label="Display name"><Input value={f.name} maxLength={60} onChange={(e) => setF({ ...f, name: e.target.value })} required /></Field>
            <Field label="Logo" hint="PNG/SVG under 200 KB, or an https:// URL">
              <Input type="file" accept="image/png,image/svg+xml,image/jpeg,image/webp" onChange={(e) => onLogo(e.target.files?.[0])} />
            </Field>
            <Field label="Colour"><Input type="color" value={f.primary_color} onChange={(e) => { setF({ ...f, primary_color: e.target.value }); applyBrand({ ...f, primary_color: e.target.value }); }} /></Field>
            <Field label="Support e-mail"><Input type="email" value={f.support_email} onChange={(e) => setF({ ...f, support_email: e.target.value })} /></Field>
            <Field label="Portal domain" hint="e.g. sms.yourcompany.com — point a CNAME at the platform's portal host; HTTPS is set up by the platform team">
              <Input value={f.domain} onChange={(e) => setF({ ...f, domain: e.target.value.trim().toLowerCase() })} placeholder="optional" />
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
