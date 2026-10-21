"use client";

import { useState } from "react";
import { get, send } from "@/lib/api";
import { count, istDate, istEnd, istStart, money, when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { AccountPicker, useAccount } from "@/components/AccountPicker";
import { Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Loading, Notice, Select, Stat, Table, Tabs, useAction } from "@/components/ui";

type Deal = {
  id: string; name: string; mcc: string; mnc: string; price: string; cap_segments: number | null; charged_segments: number;
  live_used_segments?: number; remaining_segments?: number; starts_at: string; ends_at: string; state: string; created_by: string;
};
type VendorDeal = {
  id: string; vendor_id: string; name: string; mcc: string; mnc: string; cost: string; commit_segments: number; shortfall_rate: string;
  starts_at: string; ends_at: string; state: string; routed_segments: number;
};
type Progress = {
  committed_segments: number; remaining_segments: number; needed_per_day: number; billed_segments: number; percent_met: number;
  days_total: number; days_left: number; billed_cost: string; projected_segments?: number; projected_shortfall?: number; shortfall_charge_projected?: string;
};

const stateTone: Record<string, string> = { running: "good", scheduled: "info", ended: "muted", cancelled: "muted" };
const dest = (mcc: string, mnc: string) => `${mcc}/${mnc}`;

export default function Deals() {
  const { me } = useSession();
  const [tab, setTab] = useState("customer");
  return (
    <Shell title="Deals">
      {me?.account_role === "platform" && (
        <Tabs value={tab} onChange={setTab} tabs={[{ id: "customer", label: "Price deals for accounts" }, { id: "vendor", label: "Vendor volume commitments" }]} />
      )}
      {tab === "customer" ? <CustomerDeals /> : <VendorDeals />}
    </Shell>
  );
}

function CustomerDeals() {
  const { me } = useSession();
  const [acct, setAcct] = useAccount();
  const { data, error, loading, reload } = useData(() => (acct ? get<Deal[]>(`/v1/accounts/${acct}/deals`) : Promise.resolve(null)), [acct]);
  const own = acct === me?.account_id;
  const grant = !own && can(me, "admin");
  const c = useAction();
  return (
    <div className="stack">
      <p className="small muted">
        A deal is a special price on one destination for a period, optionally for a capped number of parts. While it runs it replaces the rate card price{own ? "" : "; only the account directly above may grant or cancel it"}.
      </p>
      {me?.account_role !== "customer" && <Card><div className="form-row"><AccountPicker value={acct} onChange={setAcct} /></div></Card>}
      {grant && <NewDeal acct={acct} done={reload} />}
      <Notice>{error ?? c.error}</Notice>
      {loading && !data ? <Loading /> : (
        <Card pad={false}>
          <Table rows={data ?? []} empty={own ? "No deals on your account" : "No deals for this account"} columns={[
            { key: "name", label: "Deal", render: (d) => <strong>{d.name}</strong> },
            { key: "dest", label: "Destination (MCC/MNC)", render: (d) => <code>{dest(d.mcc, d.mnc)}</code> },
            { key: "price", label: "Price / part", align: "right", render: (d) => `₹${d.price}` },
            { key: "cap", label: "Used / cap", align: "right", render: (d) => d.cap_segments ? `${count(d.live_used_segments ?? d.charged_segments)} / ${count(d.cap_segments)}` : `${count(d.charged_segments)} / no cap` },
            { key: "period", label: "Runs", render: (d) => <span className="small">{when(d.starts_at)} – {when(d.ends_at)}</span> },
            { key: "state", label: "State", render: (d) => <Badge tone={stateTone[d.state]}>{d.state}</Badge> },
            ...(grant ? [{ key: "x", label: "", render: (d: Deal) => (d.state === "running" || d.state === "scheduled") && (
              <Button kind="link" onClick={() => confirm(`Cancel ${d.name}? Prices go back to the rate card within a minute.`) &&
                c.run(async () => { await send("POST", `/v1/deals/${d.id}/cancel`); reload(); })}>Cancel</Button>) }] : []),
          ]} />
        </Card>
      )}
    </div>
  );
}

function NewDeal({ acct, done }: { acct: string; done: () => void }) {
  const [f, setF] = useState({ name: "", mcc: "404", mnc: "*", price: "", cap: "", from: istDate(), to: istDate(30) });
  const a = useAction();
  return (
    <Card title="Grant a deal">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        await send("POST", `/v1/accounts/${acct}/deals`, {
          name: f.name, mcc: f.mcc, mnc: f.mnc || "*", price: f.price, cap_segments: f.cap ? Number(f.cap) : null,
          starts_at: f.from === istDate() ? new Date().toISOString() : istStart(f.from), ends_at: istEnd(f.to),
        });
        setF({ ...f, name: "", price: "", cap: "" }); done();
      }, "Deal granted"); }}>
        <Field label="Name"><Input value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} required /></Field>
        <Field label="MCC"><Input value={f.mcc} onChange={(e) => setF({ ...f, mcc: e.target.value })} required size={4} /></Field>
        <Field label="MNC" hint="* = every network"><Input value={f.mnc} onChange={(e) => setF({ ...f, mnc: e.target.value })} size={4} /></Field>
        <Field label="Price per part (₹)"><Input inputMode="decimal" value={f.price} onChange={(e) => setF({ ...f, price: e.target.value })} required /></Field>
        <Field label="Cap (parts)" hint="empty = no cap"><Input inputMode="numeric" value={f.cap} onChange={(e) => setF({ ...f, cap: e.target.value })} /></Field>
        <Field label="From (IST)"><Input type="date" value={f.from} onChange={(e) => setF({ ...f, from: e.target.value })} required /></Field>
        <Field label="To (IST, inclusive)"><Input type="date" value={f.to} onChange={(e) => setF({ ...f, to: e.target.value })} required /></Field>
        <Button type="submit" busy={a.busy}>Grant</Button>
      </form>
      <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
    </Card>
  );
}

function VendorDeals() {
  const { me } = useSession();
  const { data, error, reload } = useData(() => get<VendorDeal[]>("/v1/vendor-deals"), []);
  const [open, setOpen] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const admin = can(me, "admin");
  return (
    <div className="stack">
      <p className="small muted">Committed volume bought from a vendor at a lower cost. Least-cost routing picks the vendor first on that destination until the commitment is met; the vendor may charge the shortfall rate per part we fall short.</p>
      {admin && <div className="row"><Button onClick={() => setAdding(!adding)}>{adding ? "Close" : "New commitment"}</Button></div>}
      {adding && <NewVendorDeal done={() => { setAdding(false); reload(); }} />}
      <Notice>{error}</Notice>
      <Card pad={false}>
        <Table rows={data ?? []} empty="No vendor commitments" onRow={(d) => setOpen(open === d.id ? null : d.id)} columns={[
          { key: "name", label: "Commitment", render: (d) => <strong>{d.name}</strong> },
          { key: "vendor_id", label: "Vendor" },
          { key: "dest", label: "Destination", render: (d) => <code>{dest(d.mcc, d.mnc)}</code> },
          { key: "cost", label: "Cost / part", align: "right", render: (d) => `₹${d.cost}` },
          { key: "commit", label: "Routed / committed", align: "right", render: (d) => `${count(d.routed_segments)} / ${count(d.commit_segments)}` },
          { key: "period", label: "Runs", render: (d) => <span className="small">{when(d.starts_at)} – {when(d.ends_at)}</span> },
          { key: "state", label: "State", render: (d) => <Badge tone={stateTone[d.state]}>{d.state}</Badge> },
        ]} />
      </Card>
      {open && <VendorDealReport id={open} admin={admin} done={() => { setOpen(null); reload(); }} />}
    </div>
  );
}

function VendorDealReport({ id, admin, done }: { id: string; admin: boolean; done: () => void }) {
  const { data, error } = useData(() => get<{ deal: VendorDeal; billed?: string; progress?: Progress }>(`/v1/vendor-deals/${id}`), [id]);
  const a = useAction();
  const d = data?.deal;
  const p = data?.progress;
  return (
    <Card title={d ? `${d.name} · progress` : "Progress"} actions={admin && d && (d.state === "running" || d.state === "scheduled") &&
      <Button kind="danger" busy={a.busy} onClick={() => confirm(`Cancel ${d.name}?`) && a.run(async () => { await send("POST", `/v1/vendor-deals/${id}/cancel`); done(); })}>Cancel commitment</Button>}>
      <Notice>{error ?? a.error}</Notice>
      {!data ? <Loading /> : !p ? <p className="muted">{data.billed}</p> : (
        <div className="grid cols-4">
          <Stat label="Met" value={`${p.percent_met}%`} hint={`${count(p.billed_segments)} of ${count(p.committed_segments)} parts`} tone={p.percent_met >= 100 ? "good" : undefined} />
          <Stat label="Still needed" value={count(p.remaining_segments)} hint={`${count(p.needed_per_day)} a day for ${p.days_left} days`} />
          <Stat label="Billed cost" value={money(p.billed_cost)} />
          <Stat label="Projected shortfall" value={p.projected_shortfall !== undefined ? count(p.projected_shortfall) : "—"}
            hint={p.shortfall_charge_projected ? `charge ${money(p.shortfall_charge_projected)}` : p.projected_segments === undefined ? "after the first day" : "none"}
            tone={p.projected_shortfall ? "warn" : undefined} />
        </div>
      )}
    </Card>
  );
}

function NewVendorDeal({ done }: { done: () => void }) {
  const vendors = useData(() => get<{ id: string; name: string }[]>("/v1/vendors"), []);
  const [f, setF] = useState({ vendor: "", name: "", mcc: "404", mnc: "*", cost: "", commit: "", shortfall: "", from: istDate(), to: istDate(30) });
  const a = useAction();
  return (
    <Card title="New vendor commitment">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        await send("POST", `/v1/vendors/${f.vendor}/deals`, {
          name: f.name, mcc: f.mcc, mnc: f.mnc || "*", cost: f.cost, commit_segments: Number(f.commit), shortfall_rate: f.shortfall,
          starts_at: f.from === istDate() ? new Date().toISOString() : istStart(f.from), ends_at: istEnd(f.to),
        });
        done();
      }); }}>
        <Field label="Vendor">
          <Select value={f.vendor} onChange={(e) => setF({ ...f, vendor: e.target.value })} required>
            <option value="">Choose…</option>
            {(vendors.data ?? []).map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
          </Select>
        </Field>
        <Field label="Name"><Input value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} required /></Field>
        <Field label="MCC"><Input value={f.mcc} onChange={(e) => setF({ ...f, mcc: e.target.value })} required size={4} /></Field>
        <Field label="MNC"><Input value={f.mnc} onChange={(e) => setF({ ...f, mnc: e.target.value })} size={4} /></Field>
        <Field label="Cost per part (₹)"><Input inputMode="decimal" value={f.cost} onChange={(e) => setF({ ...f, cost: e.target.value })} required /></Field>
        <Field label="Committed parts"><Input inputMode="numeric" value={f.commit} onChange={(e) => setF({ ...f, commit: e.target.value })} required /></Field>
        <Field label="Shortfall rate (₹/part)"><Input inputMode="decimal" value={f.shortfall} onChange={(e) => setF({ ...f, shortfall: e.target.value })} required /></Field>
        <Field label="From (IST)"><Input type="date" value={f.from} onChange={(e) => setF({ ...f, from: e.target.value })} required /></Field>
        <Field label="To (IST, inclusive)"><Input type="date" value={f.to} onChange={(e) => setF({ ...f, to: e.target.value })} required /></Field>
        <Button type="submit" busy={a.busy}>Create</Button>
      </form>
      <Notice>{a.error ?? vendors.error}</Notice>
    </Card>
  );
}
