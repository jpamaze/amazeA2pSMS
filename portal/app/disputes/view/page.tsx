"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { Suspense, useState } from "react";
import { get, send } from "@/lib/api";
import { count, money, when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Loading, Notice, Select, Table, useAction } from "@/components/ui";
import type { Dispute } from "../page";

type Evidence = {
  period_by_status: { status: string; messages: string; segments: string; charged: string }[];
  messages?: { message_id: string; status: string; error_code: string; vendor_id: string; segments: string; charged: string; accepted_at: string }[];
  not_found?: string[];
};

function View() {
  const { me } = useSession();
  const id = useSearchParams().get("id") ?? "";
  const { data, error, reload } = useData(() => get<{ dispute: Dispute; evidence: Evidence | string }>(`/v1/disputes/${id}`), [id]);
  const d = data?.dispute;
  const ev = data?.evidence;
  const w = useAction();
  const own = d?.account_id === me?.account_id;
  const resolver = d && !own && (d.resolver_id === me?.account_id || (d.resolver_id === null && me?.account_role === "platform"));
  return (
    <Shell title="Dispute" actions={<Link href="/disputes/">All disputes</Link>}>
      <Notice>{error ?? w.error}</Notice>
      {!d ? <Loading /> : (
        <div className="stack">
          <Card title="Claim" actions={<Badge>{d.status}</Badge>}>
            <dl className="kv">
              <dt>Account</dt><dd>{own ? "yours" : <Link href={`/accounts/view/?id=${d.account_id}`}>{d.account_name ?? d.account_id}</Link>}</dd>
              <dt>Period</dt><dd>{when(d.from)} – {when(d.to)}</dd>
              <dt>Claimed</dt><dd>{money(d.claimed_amount)}</dd>
              <dt>Reason</dt><dd>{d.reason}</dd>
              <dt>Raised</dt><dd>{when(d.created_at)} by {d.raised_by}</dd>
              {d.message_ids.length > 0 && <><dt>Messages</dt><dd>{count(d.message_ids.length)} listed</dd></>}
              {d.status !== "open" && d.resolved_at && <><dt>Decided</dt><dd>{when(d.resolved_at)}{d.resolved_by ? ` by ${d.resolved_by}` : ""}</dd></>}
              {d.approved_amount && <><dt>Credited</dt><dd>{money(d.approved_amount)}</dd></>}
              {d.resolution && <><dt>Note</dt><dd>{d.resolution}</dd></>}
            </dl>
            {own && d.status === "open" && can(me, "finance") && (
              <div className="row" style={{ marginTop: 12 }}>
                <Button kind="secondary" busy={w.busy} onClick={() => confirm("Withdraw this dispute?") && w.run(async () => { await send("POST", `/v1/disputes/${id}/withdraw`); reload(); })}>Withdraw</Button>
              </div>
            )}
          </Card>
          {resolver && d.status === "open" && can(me, "finance") && <Resolve d={d} done={reload} />}
          <Card title="Our records for the period">
            {typeof ev === "string" || !ev ? <p className="muted">{ev || "No evidence"}</p> : (
              <div className="stack">
                <Table rows={ev.period_by_status} empty="No messages in the period" columns={[
                  { key: "status", label: "Status", render: (r) => <Badge>{r.status}</Badge> },
                  { key: "messages", label: "Messages", align: "right", render: (r) => count(r.messages) },
                  { key: "segments", label: "Parts", align: "right", render: (r) => count(r.segments) },
                  { key: "charged", label: "Charged", align: "right", render: (r) => money(r.charged) },
                ]} />
                {ev.messages && (
                  <Table rows={ev.messages} columns={[
                    { key: "message_id", label: "Message", render: (m) => <Link href={`/messages/view/?id=${m.message_id}&account=${d.account_id}`}><code>{m.message_id.slice(0, 8)}</code></Link> },
                    { key: "accepted_at", label: "Accepted", render: (m) => when(m.accepted_at) },
                    { key: "status", label: "Status", render: (m) => <Badge>{m.status}</Badge> },
                    { key: "error_code", label: "Error" },
                    ...(me?.account_role === "platform" ? [{ key: "vendor_id", label: "Vendor" }] : []),
                    { key: "segments", label: "Parts", align: "right" as const },
                    { key: "charged", label: "Charged", align: "right" as const, render: (m: NonNullable<Evidence["messages"]>[number]) => money(m.charged) },
                  ]} />
                )}
                {ev.not_found && ev.not_found.length > 0 && <Notice kind="info">Not in our records for this account: {ev.not_found.join(", ")}</Notice>}
              </div>
            )}
          </Card>
        </div>
      )}
    </Shell>
  );
}

function Resolve({ d, done }: { d: Dispute; done: () => void }) {
  const [f, setF] = useState({ decision: "approve", amount: d.claimed_amount, note: "" });
  const a = useAction();
  return (
    <Card title="Decide">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        await send("POST", `/v1/disputes/${d.id}/resolve`, f.decision === "approve" ? f : { decision: "reject", note: f.note });
        done();
      }); }}>
        <Field label="Decision">
          <Select value={f.decision} onChange={(e) => setF({ ...f, decision: e.target.value })}>
            <option value="approve">Approve and credit</option><option value="reject">Reject</option>
          </Select>
        </Field>
        {f.decision === "approve" && <Field label="Credit (₹)" hint={`up to ${money(d.claimed_amount)}`}><Input inputMode="decimal" value={f.amount} onChange={(e) => setF({ ...f, amount: e.target.value })} required /></Field>}
        <Field label="Note to the account"><Input value={f.note} onChange={(e) => setF({ ...f, note: e.target.value })} required /></Field>
        <Button type="submit" kind={f.decision === "reject" ? "danger" : "primary"} busy={a.busy}>{f.decision === "approve" ? "Approve" : "Reject"}</Button>
      </form>
      <Notice>{a.error}</Notice>
    </Card>
  );
}

export default function Page() {
  return <Suspense fallback={<Loading />}><View /></Suspense>;
}
