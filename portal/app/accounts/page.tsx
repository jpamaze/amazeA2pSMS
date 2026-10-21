"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { get, qs, send } from "@/lib/api";
import { money, when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import type { AccountRow } from "@/components/AccountPicker";
import { Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Notice, Select, Table, useAction } from "@/components/ui";

type RateCard = { id: string; name: string };

export default function Accounts() {
  const { me } = useSession();
  const router = useRouter();
  const [q, setQ] = useState("");
  const [role, setRole] = useState("");
  const { data, error, reload } = useData(() => get<AccountRow[]>(`/v1/accounts${qs({ q, role, limit: 500 })}`), [q, role]);
  const [open, setOpen] = useState(false);
  const rows = (data ?? []).filter((a) => a.id !== me?.account_id);
  return (
    <Shell title={me?.account_role === "reseller" ? "Sub-accounts" : "Accounts"} roles={["platform", "reseller"]}
      actions={can(me, "admin") && <Button onClick={() => setOpen(!open)}>{open ? "Close" : "New account"}</Button>}>
      {open && <NewAccount done={(id) => { setOpen(false); reload(); router.push(`/accounts/view/?id=${id}`); }} />}
      <Card pad={false}>
        <div className="form-row pad">
          <Field label="Search"><Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="name" /></Field>
          {me?.account_role === "platform" && (
            <Field label="Role">
              <Select value={role} onChange={(e) => setRole(e.target.value)}>
                <option value="">All</option><option value="reseller">Resellers</option><option value="customer">Customers</option>
              </Select>
            </Field>
          )}
        </div>
        <Notice>{error}</Notice>
        <Table rows={rows} empty="No accounts yet" onRow={(a) => router.push(`/accounts/view/?id=${a.id}`)} columns={[
          { key: "name", label: "Name", render: (a) => <strong>{a.name}</strong> },
          { key: "role", label: "Type", render: (a) => <Badge tone="info">{a.role}</Badge> },
          ...(me?.account_role === "platform" ? [{ key: "parent_name", label: "Under" }] : []),
          { key: "status", label: "Status", render: (a: AccountRow) => <Badge>{a.status}</Badge> },
          { key: "available", label: "Available", align: "right" as const, render: (a: AccountRow) => money(a.available) },
          { key: "balance", label: "Balance", align: "right" as const, render: (a: AccountRow) => money(a.balance) },
          { key: "credit_limit", label: "Credit", align: "right" as const, render: (a: AccountRow) => money(a.credit_limit) },
          { key: "rate_card", label: "Rate card" },
          { key: "created_at", label: "Created", render: (a: AccountRow) => when(a.created_at) },
        ]} />
      </Card>
    </Shell>
  );
}

function NewAccount({ done }: { done: (id: string) => void }) {
  const { me } = useSession();
  const cards = useData(() => get<RateCard[]>("/v1/rate-cards"), []);
  const [f, setF] = useState({ name: "", role: "customer", rate_card_id: "" });
  const a = useAction();
  return (
    <Card title="New account">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        const r = await send<{ id: string }>("POST", "/v1/accounts", f);
        done(r.id);
      }); }}>
        <Field label="Name"><Input value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} required /></Field>
        {me?.account_role === "platform" && (
          <Field label="Type">
            <Select value={f.role} onChange={(e) => setF({ ...f, role: e.target.value })}>
              <option value="customer">Customer</option><option value="reseller">Reseller</option>
            </Select>
          </Field>
        )}
        <Field label="Rate card">
          <Select value={f.rate_card_id} onChange={(e) => setF({ ...f, rate_card_id: e.target.value })} required>
            <option value="">Choose…</option>
            {(cards.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
          </Select>
        </Field>
        <Button type="submit" busy={a.busy}>Create</Button>
      </form>
      <Notice>{a.error ?? cards.error}</Notice>
      <p className="muted small">New accounts start prepaid (credit limit 0). Add funds and people after creating it.</p>
    </Card>
  );
}
