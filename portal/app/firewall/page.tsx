"use client";

import { useEffect, useState } from "react";
import { get, send } from "@/lib/api";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import type { AccountRow } from "@/components/AccountPicker";
import { Gate, Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Notice, Select, Table, Tabs, useAction } from "@/components/ui";

type Rule = { id: number; account_id: string; kind: string; pattern: string; action: string; note: string; enabled: boolean; created_by: string };
type Limits = { dest_per_hour: number; series_per_10min: number; series_block_minutes: number; custom: boolean };
type Block = { range: string; seconds_left: number };

const kinds: Record<string, { label: string; hint: string }> = {
  sender: { label: "Sender", hint: "glob, any case, e.g. *BANK" },
  content: { label: "Content", hint: "a phrase (whole words), or re:<regexp>" },
  url: { label: "Link domain", hint: "blocks the domain and its subdomains, e.g. bit.ly" },
  destination: { label: "Destination", hint: "glob on the number with country code, e.g. 9198100*" },
};

function Firewall() {
  const [tab, setTab] = useState("rules");
  const accounts = useData(() => get<AccountRow[]>("/v1/accounts?limit=500"), []);
  return (
    <Shell title="Firewall" roles={["platform"]}>
      <Tabs value={tab} onChange={setTab} tabs={[{ id: "rules", label: "Block lists" }, { id: "limits", label: "Pumping guard" }]} />
      {tab === "rules" ? <Rules accounts={accounts.data ?? []} /> : <Guard accounts={accounts.data ?? []} />}
    </Shell>
  );
}

function Rules({ accounts }: { accounts: AccountRow[] }) {
  const { me } = useSession();
  const { data, error, reload } = useData(() => get<Rule[]>("/v1/firewall/rules"), []);
  const [kind, setKind] = useState("");
  const [disabled, setDisabled] = useState(false);
  const a = useAction();
  const admin = can(me, "admin");
  const names = new Map(accounts.map((x) => [x.id, x.name]));
  const rows = (data ?? []).filter((r) => (disabled || r.enabled) && (!kind || r.kind === kind));
  return (
    <div className="stack">
      <p className="small muted">Checked at submit, after DLT. Reject refuses the message (fw_sender, fw_content, fw_url, fw_destination); flag only counts it. Changes apply within 30 seconds.</p>
      {admin && <NewRule accounts={accounts} done={reload} />}
      <Notice>{error ?? a.error}</Notice>
      <Card pad={false}>
        <div className="form-row pad">
          <Field label="Kind">
            <Select value={kind} onChange={(e) => setKind(e.target.value)}>
              <option value="">All</option>{Object.entries(kinds).map(([k, v]) => <option key={k} value={k}>{v.label}</option>)}
            </Select>
          </Field>
          <label className="small"><input type="checkbox" checked={disabled} onChange={(e) => setDisabled(e.target.checked)} /> show disabled</label>
        </div>
        <Table rows={rows} empty="No rules" columns={[
          { key: "kind", label: "Kind", render: (r) => kinds[r.kind]?.label ?? r.kind },
          { key: "pattern", label: "Pattern", render: (r) => <code>{r.pattern}</code> },
          { key: "action", label: "Action", render: (r) => <Badge tone={r.action === "reject" ? "bad" : "warn"}>{r.action}</Badge> },
          { key: "account_id", label: "Applies to", render: (r) => r.account_id ? names.get(r.account_id) ?? <code>{r.account_id.slice(0, 8)}</code> : "every account" },
          { key: "note", label: "Note", render: (r) => <span className="small">{r.note}</span> },
          { key: "created_by", label: "By", render: (r) => <span className="small muted">{r.created_by}</span> },
          { key: "x", label: "", render: (r) => !r.enabled ? <Badge>disabled</Badge> : admin && (
            <Button kind="link" onClick={() => confirm(`Remove the ${r.kind} rule ${r.pattern}?`) && a.run(async () => { await send("DELETE", `/v1/firewall/rules/${r.id}`); reload(); })}>Disable</Button>) },
        ]} />
      </Card>
    </div>
  );
}

function NewRule({ accounts, done }: { accounts: AccountRow[]; done: () => void }) {
  const [f, setF] = useState({ kind: "content", pattern: "", action: "reject", account_id: "", note: "" });
  const a = useAction();
  return (
    <Card title="New rule">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        await send("POST", "/v1/firewall/rules", { ...f, account_id: f.account_id || undefined });
        setF({ ...f, pattern: "", note: "" }); done();
      }, "Rule added"); }}>
        <Field label="Kind">
          <Select value={f.kind} onChange={(e) => setF({ ...f, kind: e.target.value })}>
            {Object.entries(kinds).map(([k, v]) => <option key={k} value={k}>{v.label}</option>)}
          </Select>
        </Field>
        <Field label="Pattern" hint={kinds[f.kind].hint}><Input value={f.pattern} onChange={(e) => setF({ ...f, pattern: e.target.value })} required /></Field>
        <Field label="Action">
          <Select value={f.action} onChange={(e) => setF({ ...f, action: e.target.value })}><option value="reject">Reject</option><option value="flag">Flag only</option></Select>
        </Field>
        <Field label="Applies to">
          <Select value={f.account_id} onChange={(e) => setF({ ...f, account_id: e.target.value })}>
            <option value="">Every account</option>
            {accounts.filter((x) => x.role !== "platform").map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}
          </Select>
        </Field>
        <Field label="Note"><Input value={f.note} onChange={(e) => setF({ ...f, note: e.target.value })} /></Field>
        <Button type="submit" busy={a.busy}>Add rule</Button>
      </form>
      <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
    </Card>
  );
}

function Guard({ accounts }: { accounts: AccountRow[] }) {
  const { me } = useSession();
  const others = accounts.filter((x) => x.role !== "platform");
  const [acct, setAcct] = useState("");
  useEffect(() => { if (!acct && others.length) setAcct(new URLSearchParams(window.location.search).get("account") ?? others[0].id); }, [acct, others]);
  const limits = useData(() => (acct ? get<Limits>(`/v1/accounts/${acct}/firewall-limits`) : Promise.resolve(null)), [acct]);
  const blocks = useData(() => (acct ? get<Block[]>(`/v1/accounts/${acct}/firewall/blocks`) : Promise.resolve(null)), [acct]);
  const [f, setF] = useState({ dest_per_hour: "", series_per_10min: "", series_block_minutes: "" });
  useEffect(() => { const l = limits.data; if (l) setF({ dest_per_hour: String(l.dest_per_hour), series_per_10min: String(l.series_per_10min), series_block_minutes: String(l.series_block_minutes) }); }, [limits.data]);
  const a = useAction();
  const u = useAction();
  const admin = can(me, "admin");
  return (
    <div className="stack">
      <p className="small muted">Against SMS pumping (OTPs triggered to number ranges on SIM farms) and floods. A 1,000-number range that trips is blocked for that account only (fw_series). 0 turns a limit off.</p>
      <Card>
        <div className="form-row">
          <Field label="Account">
            <Select value={acct} onChange={(e) => setAcct(e.target.value)}>
              {others.map((x) => <option key={x.id} value={x.id}>{x.name}{x.parent_name ? ` — ${x.parent_name}` : ""}</option>)}
            </Select>
          </Field>
        </div>
      </Card>
      <Card title="Limits" actions={limits.data && (limits.data.custom ? <Badge tone="info">custom</Badge> : <Badge>defaults</Badge>)}>
        <Notice>{limits.error}</Notice>
        <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
          await send("PUT", `/v1/accounts/${acct}/firewall-limits`, { dest_per_hour: Number(f.dest_per_hour), series_per_10min: Number(f.series_per_10min), series_block_minutes: Number(f.series_block_minutes) });
          limits.reload();
        }, "Saved; applies within 30 seconds"); }}>
          <Field label="Messages to one number per hour"><Input inputMode="numeric" value={f.dest_per_hour} onChange={(e) => setF({ ...f, dest_per_hour: e.target.value })} disabled={!admin} /></Field>
          <Field label="Into one 1,000-number range per 10 min"><Input inputMode="numeric" value={f.series_per_10min} onChange={(e) => setF({ ...f, series_per_10min: e.target.value })} disabled={!admin} /></Field>
          <Field label="Block a tripped range for (min)"><Input inputMode="numeric" value={f.series_block_minutes} onChange={(e) => setF({ ...f, series_block_minutes: e.target.value })} disabled={!admin} /></Field>
          {admin && <Button type="submit" busy={a.busy}>Save</Button>}
        </form>
        <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
      </Card>
      <Card title="Blocked ranges" actions={<Button kind="secondary" onClick={() => blocks.reload()}>Refresh</Button>}>
        <Notice>{blocks.error ?? u.error}</Notice>
        <Table rows={blocks.data ?? []} empty="No range is blocked for this account" columns={[
          { key: "range", label: "Range", render: (b) => <code>{b.range}</code> },
          { key: "seconds_left", label: "Blocked for", align: "right", render: (b) => `${Math.ceil(b.seconds_left / 60)} more min` },
          { key: "x", label: "", render: (b) => admin && <Button kind="link" onClick={() => confirm(`Reopen ${b.range} for this account? Do this only for genuine traffic.`) &&
            u.run(async () => { await send("DELETE", `/v1/accounts/${acct}/firewall/blocks/${b.range}`); blocks.reload(); })}>Unblock</Button> },
        ]} />
      </Card>
    </div>
  );
}

export default function Page() {
  return <Gate roles={["platform"]} title="Firewall"><Firewall /></Gate>;
}
