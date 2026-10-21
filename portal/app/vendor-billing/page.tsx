"use client";

import { useEffect, useState } from "react";
import { get, qs, send } from "@/lib/api";
import { count, istDate, money, when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { CsvImport } from "@/components/CsvImport";
import { Gate, Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Loading, Notice, Select, Stat, Table, Tabs, useAction } from "@/components/ui";

type Invoice = { id: string; vendor_id: string; reference: string; period_from: string; period_to: string; total_segments: number; total_amount: string; imported_by: string; imported_at: string; dispute_status: string };
type Line = { day: string; mcc: string; mnc: string; vendor_segments: number; our_segments: number; vendor_amount: string; our_amount: string; overbilled: string; status: string };
type Recon = { invoice_id: string; vendor_id: string; reference: string; tolerance_pct: string; summary: { vendor_segments: number; our_segments: number; vendor_amount: string; our_amount: string; disputable: string; mismatched_lines: number }; lines: Line[] };
type VDispute = { id: string; invoice_id: string; vendor_id: string; invoice_reference: string; status: string; disputed_amount: string; settled_amount?: string; lines: Line[]; notes: string; created_by: string; created_at: string; updated_at: string };

const lineTone: Record<string, string> = { ok: "good", mismatch: "bad", not_in_ours: "bad", not_in_vendor: "warn" };
const lineLabel: Record<string, string> = { ok: "ok", mismatch: "mismatch", not_in_ours: "not in our records", not_in_vendor: "not on invoice" };

function VendorBilling() {
  const [tab, setTab] = useState("invoices");
  return (
    <Shell title="Vendor billing" roles={["platform"]}>
      <Tabs value={tab} onChange={setTab} tabs={[{ id: "invoices", label: "Invoices" }, { id: "disputes", label: "Disputes" }]} />
      {tab === "invoices" ? <Invoices /> : <Disputes />}
    </Shell>
  );
}

function Invoices() {
  const { me } = useSession();
  const vendors = useData(() => get<{ id: string; name: string }[]>("/v1/vendors"), []);
  const [vendor, setVendor] = useState("");
  useEffect(() => { setVendor(new URLSearchParams(window.location.search).get("vendor") ?? ""); }, []);
  const list = useData(() => get<Invoice[]>(`/v1/vendor-invoices${qs({ vendor })}`), [vendor]);
  const [open, setOpen] = useState<string | null>(null);
  const [imp, setImp] = useState({ vendor: "", reference: "", from: istDate(-30), to: istDate(-1) });
  const finance = can(me, "finance");
  return (
    <div className="stack">
      <p className="small muted">Import a vendor&apos;s invoice as daily lines and compare it with what we sent through that vendor (from the message records). Lines over the tolerance can be disputed.</p>
      <Card pad={false}>
        <div className="form-row pad">
          <Field label="Vendor">
            <Select value={vendor} onChange={(e) => setVendor(e.target.value)}>
              <option value="">All</option>{(vendors.data ?? []).map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
            </Select>
          </Field>
        </div>
        <Notice>{list.error}</Notice>
        <Table rows={list.data ?? []} empty="No invoices imported" onRow={(i) => setOpen(open === i.id ? null : i.id)} columns={[
          { key: "reference", label: "Invoice", render: (i) => <strong>{i.reference}</strong> },
          { key: "vendor_id", label: "Vendor" },
          { key: "period", label: "Period", render: (i) => `${i.period_from} – ${i.period_to}` },
          { key: "total_segments", label: "Parts", align: "right", render: (i) => count(i.total_segments) },
          { key: "total_amount", label: "Amount", align: "right", render: (i) => money(i.total_amount) },
          { key: "imported_at", label: "Imported", render: (i) => <span className="small">{when(i.imported_at)}</span> },
          { key: "dispute_status", label: "Dispute", render: (i) => i.dispute_status ? <Badge>{i.dispute_status}</Badge> : "—" },
        ]} />
      </Card>
      {open && <Reconciliation id={open} finance={finance} done={list.reload} />}
      {finance && (
        <CsvImport title="Import an invoice" done={() => list.reload()}
          path={() => imp.vendor && imp.reference ? `/v1/vendors/${imp.vendor}/invoices${qs({ reference: imp.reference, from: imp.from, to: imp.to })}` : null}
          columns={<>day (YYYY-MM-DD, inside the period), mcc, mnc, segments, amount — one row per day and network.</>}
          example={"day,mcc,mnc,segments,amount\n2026-10-01,404,45,120000,11400.00"}
          extra={
            <div className="form-row">
              <Field label="Vendor">
                <Select value={imp.vendor} onChange={(e) => setImp({ ...imp, vendor: e.target.value })} required>
                  <option value="">Choose…</option>{(vendors.data ?? []).map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
                </Select>
              </Field>
              <Field label="Invoice number"><Input value={imp.reference} onChange={(e) => setImp({ ...imp, reference: e.target.value })} required /></Field>
              <Field label="Period from"><Input type="date" value={imp.from} onChange={(e) => setImp({ ...imp, from: e.target.value })} /></Field>
              <Field label="to"><Input type="date" value={imp.to} onChange={(e) => setImp({ ...imp, to: e.target.value })} /></Field>
            </div>
          } />
      )}
    </div>
  );
}

function Reconciliation({ id, finance, done }: { id: string; finance: boolean; done: () => void }) {
  const [tol, setTol] = useState("0.5");
  const [only, setOnly] = useState(true);
  const { data: r, error } = useData(() => get<Recon>(`/v1/vendor-invoices/${id}/reconciliation${qs({ tolerance_pct: tol })}`), [id, tol]);
  const [notes, setNotes] = useState("");
  const a = useAction();
  const lines = (r?.lines ?? []).filter((l) => !only || l.status !== "ok");
  return (
    <Card title={r ? `Reconciliation · ${r.reference}` : "Reconciliation"}>
      <Notice>{error}</Notice>
      {!r ? !error && <Loading /> : (
        <div className="stack">
          <div className="grid cols-4">
            <Stat label="Vendor billed" value={money(r.summary.vendor_amount)} hint={`${count(r.summary.vendor_segments)} parts`} />
            <Stat label="Our records" value={money(r.summary.our_amount)} hint={`${count(r.summary.our_segments)} parts`} />
            <Stat label="Disputable" value={money(r.summary.disputable)} tone={Number(r.summary.disputable) > 0 ? "warn" : "good"} />
            <Stat label="Lines off" value={count(r.summary.mismatched_lines)} hint={`tolerance ${r.tolerance_pct}%`} />
          </div>
          <div className="form-row">
            <Field label="Tolerance (%)"><Input inputMode="decimal" value={tol} onChange={(e) => setTol(e.target.value)} size={5} /></Field>
            <label className="small"><input type="checkbox" checked={only} onChange={(e) => setOnly(e.target.checked)} /> only lines that differ</label>
          </div>
          <Table rows={lines} empty="Every line matches" columns={[
            { key: "day", label: "Day" },
            { key: "net", label: "MCC/MNC", render: (l) => <code>{l.mcc}/{l.mnc}</code> },
            { key: "vendor_segments", label: "Vendor parts", align: "right", render: (l) => count(l.vendor_segments) },
            { key: "our_segments", label: "Our parts", align: "right", render: (l) => count(l.our_segments) },
            { key: "vendor_amount", label: "Vendor ₹", align: "right", render: (l) => money(l.vendor_amount) },
            { key: "our_amount", label: "Our ₹", align: "right", render: (l) => money(l.our_amount) },
            { key: "overbilled", label: "Overbilled", align: "right", render: (l) => Number(l.overbilled) > 0 ? money(l.overbilled) : "—" },
            { key: "status", label: "", render: (l) => <Badge tone={lineTone[l.status]}>{lineLabel[l.status] ?? l.status}</Badge> },
          ]} />
          {finance && Number(r.summary.disputable) > 0 && (
            <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
              await send("POST", `/v1/vendor-invoices/${id}/disputes${qs({ tolerance_pct: tol })}`, { notes }); done();
            }, "Dispute opened: see the Disputes tab"); }}>
              <Field label="Notes for the vendor"><Input value={notes} onChange={(e) => setNotes(e.target.value)} /></Field>
              <Button type="submit" busy={a.busy}>Dispute {money(r.summary.disputable)}</Button>
            </form>
          )}
          <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
        </div>
      )}
    </Card>
  );
}

const next: Record<string, string[]> = { open: ["sent", "accepted", "rejected"], sent: ["accepted", "rejected"], accepted: ["settled"], rejected: ["settled"] };

function Disputes() {
  const { me } = useSession();
  const [status, setStatus] = useState("");
  const { data, error, reload } = useData(() => get<VDispute[]>(`/v1/vendor-disputes${qs({ status })}`), [status]);
  const [open, setOpen] = useState<string | null>(null);
  const d = data?.find((x) => x.id === open);
  return (
    <div className="stack">
      <p className="small muted">Open → sent to the vendor → accepted or rejected → settled (with the amount the vendor credits).</p>
      <Card pad={false}>
        <div className="form-row pad">
          <Field label="Status">
            <Select value={status} onChange={(e) => setStatus(e.target.value)}>
              <option value="">All</option>{["open", "sent", "accepted", "rejected", "settled"].map((s) => <option key={s}>{s}</option>)}
            </Select>
          </Field>
        </div>
        <Notice>{error}</Notice>
        <Table rows={data ?? []} empty="No vendor disputes" onRow={(x) => setOpen(open === x.id ? null : x.id)} columns={[
          { key: "invoice_reference", label: "Invoice", render: (x) => <strong>{x.invoice_reference}</strong> },
          { key: "vendor_id", label: "Vendor" },
          { key: "disputed_amount", label: "Disputed", align: "right", render: (x) => money(x.disputed_amount) },
          { key: "settled_amount", label: "Settled", align: "right", render: (x) => x.settled_amount ? money(x.settled_amount) : "—" },
          { key: "status", label: "Status", render: (x) => <Badge tone={x.status === "settled" ? "good" : x.status === "rejected" ? "bad" : "warn"}>{x.status}</Badge> },
          { key: "lines", label: "Lines", align: "right", render: (x) => count(x.lines.length) },
          { key: "updated_at", label: "Updated", render: (x) => <span className="small">{when(x.updated_at)}</span> },
        ]} />
      </Card>
      {d && <DisputeView d={d} finance={can(me, "finance")} done={reload} />}
    </div>
  );
}

function DisputeView({ d, finance, done }: { d: VDispute; finance: boolean; done: () => void }) {
  const [to, setTo] = useState("");
  const [amount, setAmount] = useState("");
  const [note, setNote] = useState("");
  const a = useAction();
  const options = next[d.status] ?? [];
  return (
    <Card title={`Dispute · ${d.invoice_reference}`}>
      <div className="stack">
        {d.notes && <p className="small" style={{ whiteSpace: "pre-wrap" }}>{d.notes}</p>}
        <Table rows={d.lines} columns={[
          { key: "day", label: "Day" },
          { key: "net", label: "MCC/MNC", render: (l) => <code>{l.mcc}/{l.mnc}</code> },
          { key: "vendor_segments", label: "Vendor parts", align: "right", render: (l) => count(l.vendor_segments) },
          { key: "our_segments", label: "Our parts", align: "right", render: (l) => count(l.our_segments) },
          { key: "overbilled", label: "Overbilled", align: "right", render: (l) => money(l.overbilled) },
          { key: "status", label: "", render: (l) => <Badge tone={lineTone[l.status]}>{lineLabel[l.status] ?? l.status}</Badge> },
        ]} />
        {finance && options.length > 0 && (
          <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
            await send("PATCH", `/v1/vendor-disputes/${d.id}`, { status: to, note, ...(to === "settled" ? { settled_amount: amount } : {}) });
            setTo(""); setNote(""); done();
          }); }}>
            <Field label="Move to">
              <Select value={to} onChange={(e) => setTo(e.target.value)} required>
                <option value="">Choose…</option>{options.map((s) => <option key={s}>{s}</option>)}
              </Select>
            </Field>
            {to === "settled" && <Field label="Settled amount (₹)" hint={`up to ${money(d.disputed_amount)}`}><Input inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} required /></Field>}
            <Field label="Note"><Input value={note} onChange={(e) => setNote(e.target.value)} /></Field>
            <Button type="submit" busy={a.busy}>Update</Button>
          </form>
        )}
        <Notice>{a.error}</Notice>
      </div>
    </Card>
  );
}

export default function Page() {
  return <Gate roles={["platform"]} title="Vendor billing"><VendorBilling /></Gate>;
}
