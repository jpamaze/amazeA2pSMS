"use client";

import { useEffect, useState } from "react";
import { get, send } from "@/lib/api";
import { money, when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Loading, Notice, Table, useAction } from "@/components/ui";

type Settings = { low_balance: string; emails: string[]; sms_numbers: string[]; webhook_url: string; enabled: boolean };
type Ev = { kind: string; available: string; threshold: string; delivered: string[]; errors: string; at: string };

export default function Alerts() {
  const { me } = useSession();
  const acct = me?.account_id;
  const { data, error, loading, reload } = useData(
    () => (acct ? get<{ settings: Settings | null; firing?: boolean; events: Ev[] }>(`/v1/accounts/${acct}/alerts`) : Promise.resolve(null)), [acct]);
  const [f, setF] = useState({ low_balance: "", emails: "", sms_numbers: "", webhook_url: "", enabled: true });
  useEffect(() => {
    const s = data?.settings;
    if (s) setF({ low_balance: s.low_balance, emails: s.emails.join(", "), sms_numbers: s.sms_numbers.join(", "), webhook_url: s.webhook_url, enabled: s.enabled });
  }, [data]);
  const a = useAction();
  const list = (s: string) => s.split(/[\s,;]+/).filter(Boolean);
  return (
    <Shell title="Alerts">
      {loading && !data && <Loading />}
      <Notice>{error}</Notice>
      <Card title="Low balance" actions={data?.firing ? <Badge tone="bad">below level now</Badge> : data?.settings?.enabled ? <Badge tone="good">armed</Badge> : null}>
        <p className="muted">We tell you once when the amount available to send drops below this level, and again only after it has gone back above it.</p>
        <form className="form" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
          await send("PUT", `/v1/accounts/${acct}/alerts`, { low_balance: f.low_balance || "0", emails: list(f.emails), sms_numbers: list(f.sms_numbers), webhook_url: f.webhook_url, enabled: f.enabled });
          reload();
        }, "Saved"); }}>
          <Field label="Alert below (₹)"><Input inputMode="decimal" value={f.low_balance} onChange={(e) => setF({ ...f, low_balance: e.target.value })} required /></Field>
          <Field label="E-mail to" hint="comma-separated"><Input value={f.emails} onChange={(e) => setF({ ...f, emails: e.target.value })} /></Field>
          <Field label="SMS to" hint="Indian mobile numbers, comma-separated"><Input value={f.sms_numbers} onChange={(e) => setF({ ...f, sms_numbers: e.target.value })} /></Field>
          <Field label="Webhook (HTTPS)" hint="POST {event, account_id, available, threshold}, signed like delivery webhooks"><Input type="url" value={f.webhook_url} onChange={(e) => setF({ ...f, webhook_url: e.target.value })} /></Field>
          <label className="row"><input type="checkbox" checked={f.enabled} onChange={(e) => setF({ ...f, enabled: e.target.checked })} /> Enabled</label>
          {can(me, "finance") && <Button type="submit" busy={a.busy}>Save</Button>}
        </form>
        <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
      </Card>
      <div style={{ marginTop: 16 }}>
        <Card title="Recent alerts" pad={false}>
          <Table rows={data?.events ?? []} empty="No alerts sent yet" columns={[
            { key: "at", label: "When", render: (e) => when(e.at) },
            { key: "kind", label: "Event", render: (e) => <Badge tone={e.kind === "low_balance" ? "bad" : "good"}>{e.kind.replace("_", " ")}</Badge> },
            { key: "available", label: "Available", align: "right", render: (e) => money(e.available) },
            { key: "threshold", label: "Level", align: "right", render: (e) => money(e.threshold) },
            { key: "delivered", label: "Sent by", render: (e) => e.delivered.join(", ") || "—" },
            { key: "errors", label: "Problems", render: (e) => e.errors ? <span className="small" style={{ color: "var(--bad)" }}>{e.errors}</span> : "" },
          ]} />
        </Card>
      </div>
    </Shell>
  );
}
