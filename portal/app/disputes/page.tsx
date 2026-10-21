"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { get, qs, send } from "@/lib/api";
import { istDate, istEnd, istStart, money, when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Notice, Select, Table, TextArea, useAction } from "@/components/ui";

export type Dispute = {
  id: string; account_id: string; account_name?: string; resolver_id: string | null; from: string; to: string; message_ids: string[];
  claimed_amount: string; reason: string; status: string; approved_amount?: string; resolution?: string;
  raised_by: string; resolved_by?: string; created_at: string; resolved_at?: string;
};

export default function Disputes() {
  const { me } = useSession();
  const router = useRouter();
  const [status, setStatus] = useState("");
  const [raising, setRaising] = useState(false);
  const { data, error, reload } = useData(() => get<Dispute[]>(`/v1/disputes${qs({ status })}`), [status]);
  const mayRaise = me?.account_role !== "platform" && can(me, "finance");
  return (
    <Shell title="Disputes" actions={mayRaise && <Button onClick={() => setRaising(!raising)}>{raising ? "Close" : "Raise a dispute"}</Button>}>
      <div className="stack">
        {raising && <Raise done={(id) => router.push(`/disputes/view/?id=${id}`)} />}
        <p className="small muted">
          {me?.account_role === "customer" ? "Claims about charges go to your provider, who reviews them against the message records and can credit your wallet."
            : me?.account_role === "reseller" ? "Your sub-accounts' claims come to you to resolve; your own claims go to the platform. Approving credits the sub-account from your wallet."
            : "Claims from accounts directly under the platform. Approving credits their wallet."}
        </p>
        <Card pad={false}>
          <div className="form-row pad">
            <Field label="Status">
              <Select value={status} onChange={(e) => setStatus(e.target.value)}>
                <option value="">All</option><option value="open">Open</option><option value="approved">Approved</option>
                <option value="rejected">Rejected</option><option value="withdrawn">Withdrawn</option>
              </Select>
            </Field>
          </div>
          <Notice>{error}</Notice>
          <Table rows={data ?? []} empty="No disputes" onRow={(d) => router.push(`/disputes/view/?id=${d.id}`)} columns={[
            { key: "created_at", label: "Raised", render: (d) => when(d.created_at) },
            { key: "side", label: "", render: (d) => d.account_id === me?.account_id ? <Badge tone="info">yours</Badge> : <Badge tone="warn">to resolve</Badge> },
            { key: "account", label: "Account", render: (d) => d.account_name ?? <code>{d.account_id.slice(0, 8)}</code> },
            { key: "period", label: "Period", render: (d) => <span className="small">{when(d.from)} – {when(d.to)}</span> },
            { key: "claimed_amount", label: "Claimed", align: "right", render: (d) => money(d.claimed_amount) },
            { key: "approved_amount", label: "Credited", align: "right", render: (d) => d.approved_amount ? money(d.approved_amount) : "—" },
            { key: "status", label: "Status", render: (d) => <Badge>{d.status}</Badge> },
            { key: "reason", label: "Reason", render: (d) => <span className="small">{d.reason.length > 80 ? d.reason.slice(0, 80) + "…" : d.reason}</span> },
          ]} />
        </Card>
      </div>
    </Shell>
  );
}

function Raise({ done }: { done: (id: string) => void }) {
  const { me } = useSession();
  const [f, setF] = useState({ from: istDate(-7), to: istDate(-1), claimed_amount: "", reason: "", ids: "" });
  const a = useAction();
  return (
    <Card title="Raise a dispute">
      <form className="stack" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        const ids = f.ids.split(/[\s,;]+/).filter(Boolean);
        const r = await send<{ id: string }>("POST", `/v1/accounts/${me!.account_id}/disputes`, {
          from: istStart(f.from), to: istEnd(f.to), claimed_amount: f.claimed_amount, reason: f.reason, message_ids: ids,
        });
        done(r.id);
      }); }}>
        <div className="form-row">
          <Field label="From (IST)"><Input type="date" value={f.from} onChange={(e) => setF({ ...f, from: e.target.value })} required /></Field>
          <Field label="To (IST, inclusive)"><Input type="date" value={f.to} onChange={(e) => setF({ ...f, to: e.target.value })} required /></Field>
          <Field label="Amount claimed (₹)"><Input inputMode="decimal" value={f.claimed_amount} onChange={(e) => setF({ ...f, claimed_amount: e.target.value })} required /></Field>
        </div>
        <Field label="What is wrong"><TextArea rows={3} value={f.reason} onChange={(e) => setF({ ...f, reason: e.target.value })} placeholder="e.g. charged for messages that were never delivered" required /></Field>
        <Field label="Message IDs" hint="optional, up to 1,000; one per line or comma-separated (from Messages)">
          <TextArea rows={3} value={f.ids} onChange={(e) => setF({ ...f, ids: e.target.value })} />
        </Field>
        <div className="row"><Button type="submit" busy={a.busy}>Raise</Button></div>
      </form>
      <Notice>{a.error}</Notice>
    </Card>
  );
}
