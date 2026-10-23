"use client";

import { useEffect, useState } from "react";
import { download, get, qs } from "@/lib/api";
import { count, money, pct } from "@/lib/format";
import { dateIn, shortZone, useTimeZone, whenIn } from "@/lib/tz";
import { useSession } from "@/lib/session";
import { AccountPicker } from "@/components/AccountPicker";
import { BarChart } from "@/components/Chart";
import { Shell } from "@/components/Shell";
import { TimeZoneField } from "@/components/TimeZone";
import { Badge, Button, Card, Field, Input, Notice, Select, Stat, Table, Tabs, useAction } from "@/components/ui";

type UsageDay = { day: string; sent: string; segments: string; delivered: string; not_delivered: string; via_sub_accounts: string; charged: string };
type Statement = { opening_balance: string; closing_balance: string; totals: Record<string, string>; entries: { at: string; kind: string; amount: string; balance_after: string; reference?: string; messages?: number; counterparty?: string }[] };
type DLTRow = { key: string; messages: string; segments: string; delivered: string; not_delivered: string; dlt_failures: string; top_dlt_reason: string };

type Entry = Statement["entries"][number];

// fillDays returns every day from..to (exclusive), empty ones included, so
// the chart keeps its time scale; capped at 92 days.
function fillDays(rows: UsageDay[], from: string, to: string): UsageDay[] {
  const by = new Map(rows.map((r) => [r.day, r]));
  const out: UsageDay[] = [];
  const end = new Date(`${to}T00:00:00Z`).getTime();
  for (let t = new Date(`${from}T00:00:00Z`).getTime(); t < end && out.length < 92; t += 86400_000) {
    const day = new Date(t).toISOString().slice(0, 10);
    out.push(by.get(day) ?? { day, sent: "0", segments: "0", delivered: "0", not_delivered: "0", via_sub_accounts: "0", charged: "0" });
  }
  return out.length ? out : rows;
}

// statementRows keeps top-ups, transfers and refunds as they are and folds
// charges (one ledger entry per rater batch, thousands a day) into one row
// per day in tz. The CSV download keeps every entry.
function statementRows(entries: Entry[], tz: string): Entry[] {
  const out: Entry[] = [];
  const day = (at: string) => new Intl.DateTimeFormat("en-CA", { timeZone: tz }).format(new Date(at));
  let cur: Entry | null = null;
  let curDay = "";
  for (const e of entries) {
    if (e.kind !== "charge") {
      if (cur) { out.push(cur); cur = null; }
      out.push(e);
      continue;
    }
    const d = day(e.at);
    if (cur && curDay === d) {
      cur.amount = (Number(cur.amount) + Number(e.amount)).toFixed(4);
      cur.messages = (cur.messages ?? 0) + (e.messages ?? 0);
      cur.balance_after = e.balance_after;
      cur.at = e.at;
    } else {
      if (cur) out.push(cur);
      cur = { ...e, reference: `charges on ${d}` };
      curDay = d;
    }
  }
  if (cur) out.push(cur);
  return out;
}

export default function Reports() {
  const { me } = useSession();
  const [tab, setTab] = useState("usage");
  const [acct, setAcct] = useState("");
  const [tz, setTz] = useTimeZone();
  const [from, setFrom] = useState(dateIn("UTC", -30));
  const [to, setTo] = useState(dateIn("UTC", 1));
  const [touched, setTouched] = useState(false); // dates follow the zone until edited
  const [by, setBy] = useState("template");
  const [usage, setUsage] = useState<UsageDay[] | null>(null);
  const [stmt, setStmt] = useState<Statement | null>(null);
  const [dlt, setDlt] = useState<DLTRow[] | null>(null);
  const act = useAction();
  useEffect(() => { if (me && !acct) setAcct(new URLSearchParams(window.location.search).get("account") ?? me.account_id); }, [me, acct]);

  async function run(from_ = from, to_ = to) {
    if (!acct) return;
    const [from, to] = [from_, to_];
    const p = qs({ from, to, tz });
    await act.run(async () => {
      if (tab === "usage") setUsage((await get<{ days: UsageDay[] }>(`/v1/accounts/${acct}/usage${p}`)).days);
      if (tab === "statement") setStmt(await get<Statement>(`/v1/accounts/${acct}/statement${p}`));
      if (tab === "dlt") setDlt((await get<{ rows: DLTRow[] }>(`/v1/accounts/${acct}/dlt/report${qs({ from, to, tz, by })}`)).rows);
    });
  }
  useEffect(() => {
    if (touched) { run(); return; }
    const [f, t] = [dateIn(tz, -30), dateIn(tz, 1)];
    setFrom(f); setTo(t); run(f, t);
    /* eslint-disable-next-line react-hooks/exhaustive-deps */
  }, [tab, acct, tz]);

  const sum = (k: keyof UsageDay) => (usage ?? []).reduce((s, d) => s + Number(d[k]), 0);
  return (
    <Shell title="Reports">
      <Tabs value={tab} onChange={setTab} tabs={[{ id: "usage", label: "Usage" }, { id: "statement", label: "Wallet statement" }, { id: "dlt", label: "DLT" }]} />
      <Card>
        <form className="form-row" onSubmit={(e) => { e.preventDefault(); run(); }}>
          <AccountPicker value={acct} onChange={setAcct} />
          <Field label="From"><Input type="date" value={from} onChange={(e) => { setFrom(e.target.value); setTouched(true); }} /></Field>
          <Field label="To (exclusive)"><Input type="date" value={to} onChange={(e) => { setTo(e.target.value); setTouched(true); }} /></Field>
          <TimeZoneField value={tz} onChange={setTz} />
          {tab === "dlt" && (
            <Field label="Group by">
              <Select value={by} onChange={(e) => setBy(e.target.value)}>
                <option value="template">Template</option>
                <option value="header">Header</option>
                <option value="pe_id">PE ID</option>
                <option value="category">Category</option>
              </Select>
            </Field>
          )}
          <Button type="submit" busy={act.busy}>Show</Button>
          {tab === "statement" && (
            <Button type="button" kind="secondary" onClick={() => act.run(() => download(`/v1/accounts/${acct}/statement${qs({ from, to, tz, format: "csv" })}`, `statement-${from}.csv`))}>
              Download CSV
            </Button>
          )}
        </form>
      </Card>
      <Notice>{act.error}</Notice>

      {tab === "usage" && usage && (
        <div className="stack" style={{ marginTop: 16 }}>
          <div className="grid cols-4">
            <Stat label="Messages" value={count(sum("sent"))} hint={`${count(sum("segments"))} parts`} />
            <Stat label="Delivered" value={count(sum("delivered"))} hint={pct(sum("delivered"), sum("sent"))} tone="good" />
            <Stat label="Not delivered" value={count(sum("not_delivered"))} hint={pct(sum("not_delivered"), sum("sent"))} />
            <Stat label="Charged" value={money(sum("charged"))} hint={sum("via_sub_accounts") ? `${count(sum("via_sub_accounts"))} via sub-accounts` : undefined} />
          </div>
          <Card title="Per day">
            <BarChart
              bars={fillDays(usage, from, to).map((d) => ({ label: d.day.slice(5), values: { delivered: +d.delivered, failed: +d.not_delivered, other: Math.max(+d.sent - +d.delivered - +d.not_delivered, 0), sub: +d.via_sub_accounts } }))}
              keys={[
                { key: "delivered", name: "Delivered", color: "var(--good)" },
                { key: "failed", name: "Not delivered", color: "var(--bad)" },
                { key: "other", name: "Awaiting receipt", color: "var(--brand)" },
                { key: "sub", name: "Via sub-accounts", color: "var(--muted)" },
              ]}
            />
          </Card>
          <Card pad={false}>
            <Table rows={usage} columns={[
              { key: "day", label: `Day (${shortZone(tz)})` },
              { key: "sent", label: "Messages", align: "right", render: (d) => count(d.sent) },
              { key: "segments", label: "Parts", align: "right", render: (d) => count(d.segments) },
              { key: "delivered", label: "Delivered", align: "right", render: (d) => `${count(d.delivered)} (${pct(+d.delivered, +d.sent)})` },
              { key: "not_delivered", label: "Not delivered", align: "right", render: (d) => count(d.not_delivered) },
              { key: "via_sub_accounts", label: "Via sub-accounts", align: "right", render: (d) => count(d.via_sub_accounts) },
              { key: "charged", label: "Charged", align: "right", render: (d) => money(d.charged) },
            ]} />
          </Card>
        </div>
      )}

      {tab === "statement" && stmt && (
        <div className="stack" style={{ marginTop: 16 }}>
          <div className="grid cols-4">
            <Stat label="Opening balance" value={money(stmt.opening_balance)} />
            <Stat label="Closing balance" value={money(stmt.closing_balance)} />
            {Object.entries(stmt.totals).map(([k, v]) => <Stat key={k} label={k.replace("_", " ")} value={money(v)} />)}
          </div>
          <Card pad={false}>
            <Table rows={statementRows(stmt.entries, tz)} empty="No entries in this period" columns={[
              { key: "at", label: `When (${shortZone(tz)})`, render: (e) => whenIn(e.at, tz) },
              { key: "kind", label: "Kind", render: (e) => <Badge tone={Number(e.amount) >= 0 ? "good" : "muted"}>{e.kind}</Badge> },
              { key: "amount", label: "Amount", align: "right", render: (e) => money(e.amount) },
              { key: "balance_after", label: "Balance", align: "right", render: (e) => money(e.balance_after) },
              { key: "messages", label: "Messages", align: "right", render: (e) => (e.messages ? count(e.messages) : "") },
              { key: "reference", label: "Reference" },
            ]} />
            <p className="muted small pad">Charges are shown per day; the CSV has every ledger entry.</p>
          </Card>
        </div>
      )}

      {tab === "dlt" && dlt && (
        <Card pad={false}>
          <Table rows={dlt} empty="No traffic in this period" columns={[
            { key: "key", label: by === "pe_id" ? "PE ID" : by[0].toUpperCase() + by.slice(1), render: (r) => <code>{r.key || "—"}</code> },
            { key: "messages", label: "Messages", align: "right", render: (r) => count(r.messages) },
            { key: "delivered", label: "Delivered", align: "right", render: (r) => `${count(r.delivered)} (${pct(+r.delivered, +r.messages)})` },
            { key: "not_delivered", label: "Not delivered", align: "right", render: (r) => count(r.not_delivered) },
            { key: "dlt_failures", label: "Operator DLT failures", align: "right", render: (r) => count(r.dlt_failures) },
            { key: "top_dlt_reason", label: "Main DLT reason", render: (r) => (r.top_dlt_reason ? <Badge tone="bad">{r.top_dlt_reason}</Badge> : "") },
          ]} />
          <p className="muted small pad">Messages refused at submit for DLT never reach operators and are not counted here.</p>
        </Card>
      )}
    </Shell>
  );
}
