"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { get, qs } from "@/lib/api";
import { count, istDate, money, when } from "@/lib/format";
import { useSession } from "@/lib/session";
import { Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Notice, Select, Table, useAction } from "@/components/ui";

type Msg = { id: string; accepted_at: string; to: string; sender: string; segments: string; status: string; error_code: string; vendor_id: string; template_id: string; client_ref: string; charged: string; channel: string };

export default function Messages() {
  const { me } = useSession();
  const router = useRouter();
  const [f, setF] = useState({ from: istDate(-1), to: "", status: "", to_number: "", sender: "", client_ref: "", message_id: "" });
  const [rows, setRows] = useState<Msg[]>([]);
  const [more, setMore] = useState(false);
  const [searched, setSearched] = useState(false);
  const act = useAction();
  const acct = me?.account_id;

  async function search(after?: Msg) {
    if (!acct) return;
    await act.run(async () => {
      const r = await get<{ messages: Msg[]; more: boolean }>(
        `/v1/accounts/${acct}/messages${qs({ ...f, before: after?.accepted_at, before_id: after?.id, limit: 100 })}`,
      );
      setRows(after ? [...rows, ...r.messages] : r.messages);
      setMore(r.more);
      setSearched(true);
    });
  }
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => setF({ ...f, [k]: e.target.value });

  return (
    <Shell title="Messages">
      <Card>
        <form className="form-row" onSubmit={(e) => { e.preventDefault(); search(); }}>
          <Field label="From (IST date)"><Input type="date" value={f.from} onChange={set("from")} /></Field>
          <Field label="To"><Input type="date" value={f.to} onChange={set("to")} /></Field>
          <Field label="Status">
            <Select value={f.status} onChange={set("status")}>
              <option value="">Any</option>
              {["accepted", "submitted", "delivered", "failed", "expired", "rejected"].map((s) => <option key={s}>{s}</option>)}
            </Select>
          </Field>
          <Field label="Mobile number"><Input value={f.to_number} onChange={set("to_number")} placeholder="98XXXXXXXX" /></Field>
          <Field label="Sender"><Input value={f.sender} onChange={set("sender")} /></Field>
          <Field label="Your reference"><Input value={f.client_ref} onChange={set("client_ref")} /></Field>
          <Field label="Message ID"><Input value={f.message_id} onChange={set("message_id")} /></Field>
          <Button type="submit" busy={act.busy}>Search</Button>
        </form>
      </Card>
      <Notice>{act.error}</Notice>
      {searched && (
        <Card title={`${count(rows.length)}${more ? "+" : ""} messages`} pad={false}>
          <Table
            rows={rows}
            empty="No messages match"
            onRow={(m) => router.push(`/messages/view/?id=${m.id}`)}
            columns={[
              { key: "accepted_at", label: "Sent", render: (m) => when(m.accepted_at) },
              { key: "to", label: "To" },
              { key: "sender", label: "Sender" },
              { key: "status", label: "Status", render: (m) => <Badge>{m.status}</Badge> },
              { key: "error_code", label: "Error", render: (m) => <code>{m.error_code}</code> },
              { key: "segments", label: "Parts", align: "right" },
              { key: "charged", label: "Charged", align: "right", render: (m) => money(m.charged) },
              { key: "template_id", label: "Template" },
              { key: "client_ref", label: "Reference" },
            ]}
          />
          {more && <div className="pad"><Button kind="secondary" busy={act.busy} onClick={() => search(rows[rows.length - 1])}>Load more</Button></div>}
        </Card>
      )}
    </Shell>
  );
}
