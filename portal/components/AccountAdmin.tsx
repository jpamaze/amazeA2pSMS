"use client";

// Per-account administration blocks, used for your own account (API & SMPP,
// Team) and for accounts you manage (Accounts > account).

import { useState } from "react";
import { get, send } from "@/lib/api";
import { when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { Badge, Button, Card, CopyOnce, Field, Input, Notice, Select, Table, useAction } from "./ui";

type Key = { id: string; prefix: string; name: string; created_at: string; revoked_at: string | null };

export function ApiKeys({ acct }: { acct: string }) {
  const { me } = useSession();
  const { data, error, reload } = useData(() => get<Key[]>(`/v1/accounts/${acct}/api-keys`), [acct]);
  const [name, setName] = useState("");
  const [created, setCreated] = useState<string | null>(null);
  const a = useAction();
  const admin = can(me, "admin");
  return (
    <Card title="HTTP API keys">
      <Notice>{error ?? a.error}</Notice>
      {created && <CopyOnce label="New API key" value={created} />}
      {admin && (
        <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
          const r = await send<{ api_key: string }>("POST", `/v1/accounts/${acct}/api-keys`, { name });
          setCreated(r.api_key); setName(""); reload();
        }); }}>
          <Field label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. production server" required /></Field>
          <Button type="submit" busy={a.busy}>Create key</Button>
        </form>
      )}
      <Table rows={data ?? []} empty="No API keys yet" columns={[
        { key: "name", label: "Name" },
        { key: "prefix", label: "Starts with", render: (k) => <code>{k.prefix}…</code> },
        { key: "created_at", label: "Created", render: (k) => when(k.created_at) },
        { key: "state", label: "State", render: (k) => <Badge>{k.revoked_at ? "revoked" : "active"}</Badge> },
        { key: "x", label: "", render: (k) => admin && !k.revoked_at ? (
          <Button kind="link" onClick={() => confirm(`Revoke "${k.name}"? Requests with it will be refused.`) && a.run(async () => { await send("DELETE", `/v1/accounts/${acct}/api-keys/${k.id}`); reload(); })}>Revoke</Button>
        ) : null },
      ]} />
    </Card>
  );
}

type Cred = { id: string; system_id: string; allowed_ips: string; max_binds: number; tps_limit: number; created_at: string; disabled_at: string | null };

export function SmppCredentials({ acct, manage }: { acct: string; manage: boolean }) {
  const { me } = useSession();
  const { data, error, reload } = useData(() => get<Cred[]>(`/v1/accounts/${acct}/smpp-credentials`), [acct]);
  const [f, setF] = useState({ system_id: "", password: "", tps: "100", max_binds: "4", allowed_ips: "" });
  const a = useAction();
  const admin = can(me, "admin");
  return (
    <Card title="SMPP binds">
      <Notice>{error ?? a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
      {manage && admin && (
        <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
          await send("POST", `/v1/accounts/${acct}/smpp-credentials`, {
            system_id: f.system_id, password: f.password, tps: Number(f.tps), max_binds: Number(f.max_binds),
            allowed_ips: f.allowed_ips.split(/[\s,]+/).filter(Boolean),
          });
          setF({ ...f, system_id: "", password: "" }); reload();
        }, "Credential created"); }}>
          <Field label="system_id"><Input value={f.system_id} maxLength={15} onChange={(e) => setF({ ...f, system_id: e.target.value })} required /></Field>
          <Field label="Password" hint="6 to 8 characters"><Input value={f.password} minLength={6} maxLength={8} onChange={(e) => setF({ ...f, password: e.target.value })} required /></Field>
          <Field label="TPS per bind"><Input type="number" min={1} value={f.tps} onChange={(e) => setF({ ...f, tps: e.target.value })} /></Field>
          <Field label="Max binds"><Input type="number" min={1} value={f.max_binds} onChange={(e) => setF({ ...f, max_binds: e.target.value })} /></Field>
          <Field label="Allowed IPs" hint="CIDRs, comma-separated; empty = any"><Input value={f.allowed_ips} onChange={(e) => setF({ ...f, allowed_ips: e.target.value })} /></Field>
          <Button type="submit" busy={a.busy}>Create</Button>
        </form>
      )}
      <Table rows={data ?? []} empty={manage ? "No SMPP credentials yet" : "No SMPP credentials. Ask your provider for one."} columns={[
        { key: "system_id", label: "system_id", render: (c) => <code>{c.system_id}</code> },
        { key: "tps_limit", label: "TPS / bind", align: "right" },
        { key: "max_binds", label: "Max binds", align: "right" },
        { key: "allowed_ips", label: "Allowed IPs", render: (c) => c.allowed_ips || "any" },
        { key: "created_at", label: "Created", render: (c) => when(c.created_at) },
        { key: "state", label: "State", render: (c) => <Badge>{c.disabled_at ? "disabled" : "active"}</Badge> },
        { key: "x", label: "", render: (c) => manage && admin && !c.disabled_at ? (
          <Button kind="link" onClick={() => confirm(`Disable ${c.system_id}? New binds will be refused.`) && a.run(async () => { await send("DELETE", `/v1/accounts/${acct}/smpp-credentials/${c.id}`); reload(); })}>Disable</Button>
        ) : null },
      ]} />
    </Card>
  );
}

export function Webhook({ acct }: { acct: string }) {
  const { me } = useSession();
  const { data, error, reload } = useData(() => get<{ url: string | null }>(`/v1/accounts/${acct}/webhook`), [acct]);
  const [url, setUrl] = useState("");
  const [secret, setSecret] = useState<string | null>(null);
  const a = useAction();
  return (
    <Card title="Delivery receipt webhook">
      <p className="muted small">Final status of every HTTP message is POSTed here, signed with <code>X-Amaze-Signature</code> (HMAC-SHA256).</p>
      <Notice>{error ?? a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
      {secret && <CopyOnce label="Signing secret" value={secret} />}
      <p>Current: {data?.url ? <code>{data.url}</code> : <span className="muted">none</span>}</p>
      {can(me, "admin") && (
        <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
          const r = await send<{ secret?: string }>("PUT", `/v1/accounts/${acct}/webhook`, { url });
          setSecret(r?.secret ?? null); setUrl(""); reload();
        }, url ? "Webhook saved" : "Webhook removed"); }}>
          <Field label="HTTPS URL" hint="empty removes the webhook"><Input type="url" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://example.com/sms/receipts" /></Field>
          <Button type="submit" busy={a.busy}>{url ? "Save" : "Remove"}</Button>
        </form>
      )}
    </Card>
  );
}

type User = { id: string; email: string; role: string; created_at: string; last_login_at: string | null; disabled_at: string | null };

export function Users({ acct }: { acct: string }) {
  const { me } = useSession();
  const { data, error, reload } = useData(() => get<User[]>(`/v1/accounts/${acct}/users`), [acct]);
  const [f, setF] = useState({ email: "", role: "viewer" });
  const a = useAction();
  const admin = can(me, "admin");
  const roles = ["viewer", "support", "finance", "admin", ...(me?.user_role === "owner" ? ["owner"] : [])];
  return (
    <Card title="People who can sign in">
      <Notice>{error ?? a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
      {admin && (
        <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
          await send("POST", `/v1/accounts/${acct}/users`, f); setF({ ...f, email: "" }); reload();
        }, "Added. They sign in with single sign-on using this e-mail."); }}>
          <Field label="E-mail"><Input type="email" value={f.email} onChange={(e) => setF({ ...f, email: e.target.value })} required /></Field>
          <Field label="Role">
            <Select value={f.role} onChange={(e) => setF({ ...f, role: e.target.value })}>
              {roles.map((r) => <option key={r}>{r}</option>)}
            </Select>
          </Field>
          <Button type="submit" busy={a.busy}>Add person</Button>
        </form>
      )}
      <p className="muted small">viewer and support: read only · finance: payments and credit limits · admin and owner: everything.</p>
      <Table rows={data ?? []} empty="No users" columns={[
        { key: "email", label: "E-mail" },
        { key: "role", label: "Role", render: (u) => <Badge tone="info">{u.role}</Badge> },
        { key: "last_login_at", label: "Last sign-in", render: (u) => when(u.last_login_at) },
        { key: "state", label: "State", render: (u) => <Badge>{u.disabled_at ? "disabled" : "active"}</Badge> },
        { key: "x", label: "", render: (u) => admin && !u.disabled_at && u.email.toLowerCase() !== me?.email.toLowerCase() ? (
          <Button kind="link" onClick={() => confirm(`Remove ${u.email}?`) && a.run(async () => { await send("DELETE", `/v1/accounts/${acct}/users/${u.id}`); reload(); })}>Remove</Button>
        ) : null },
      ]} />
    </Card>
  );
}
