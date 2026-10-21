"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { Suspense, useState } from "react";
import { get, send } from "@/lib/api";
import { money, when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import type { AccountRow } from "@/components/AccountPicker";
import { ApiKeys, SmppCredentials, Users, Webhook } from "@/components/AccountAdmin";
import { Gate, Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Loading, Notice, Select, Stat, Tabs, useAction } from "@/components/ui";

function View() {
  const { me } = useSession();
  const id = useSearchParams().get("id") ?? "";
  const { data, error, reload } = useData(() => get<{ account: AccountRow; sub_accounts: number }>(`/v1/accounts/${id}`), [id]);
  const [tab, setTab] = useState("overview");
  const a = useAction();
  const x = data?.account;
  const admin = can(me, "admin");
  return (
    <Shell title={x?.name ?? "Account"} roles={["platform", "reseller"]}
      actions={x && <>
        <Link href={`/wallet/?account=${id}`}>Wallet</Link>
        {admin && <Button kind={x.status === "active" ? "danger" : "primary"} busy={a.busy} onClick={() =>
          confirm(x.status === "active" ? `Suspend ${x.name}? It stops sending within a minute.` : `Reactivate ${x.name}?`) &&
          a.run(async () => { await send("PUT", `/v1/accounts/${id}/status`, { status: x.status === "active" ? "suspended" : "active" }); reload(); })
        }>{x.status === "active" ? "Suspend" : "Reactivate"}</Button>}
      </>}>
      <Notice>{error ?? a.error}</Notice>
      {!x ? <Loading /> : (
        <>
          <Tabs value={tab} onChange={setTab} tabs={[
            { id: "overview", label: "Overview" }, { id: "people", label: "People" }, { id: "api", label: "API & SMPP" },
          ]} />
          {tab === "overview" && (
            <div className="stack">
              <div className="grid cols-4">
                <Stat label="Available" value={money(x.available)} />
                <Stat label="Balance" value={money(x.balance)} />
                <Stat label="Credit limit" value={money(x.credit_limit)} />
                <Stat label="Status" value={<Badge>{x.status}</Badge>} hint={`created ${when(x.created_at)}`} />
              </div>
              <Card title="Details">
                <dl className="kv">
                  <dt>Type</dt><dd>{x.role}{data.sub_accounts ? ` · ${data.sub_accounts} sub-accounts` : ""}</dd>
                  <dt>Under</dt><dd>{x.parent_name || "—"}</dd>
                  <dt>Rate card</dt><dd>{x.rate_card || "—"}</dd>
                  <dt>DLT checks</dt><dd>{x.dlt_mode === "off" ? <Badge tone="warn">off</Badge> : "enforced"}</dd>
                  <dt>Account ID</dt><dd><code>{x.id}</code></dd>
                </dl>
              </Card>
              {admin && <RateCard id={id} done={reload} />}
              <p className="small"><Link href={`/reports/?account=${id}`}>Usage and statement</Link></p>
            </div>
          )}
          {tab === "people" && <Users acct={id} />}
          {tab === "api" && (
            <div className="stack">
              <SmppCredentials acct={id} manage />
              <ApiKeys acct={id} />
              <Webhook acct={id} />
            </div>
          )}
        </>
      )}
    </Shell>
  );
}

function RateCard({ id, done }: { id: string; done: () => void }) {
  const cards = useData(() => get<{ id: string; name: string }[]>("/v1/rate-cards"), []);
  const [card, setCard] = useState("");
  const a = useAction();
  return (
    <Card title="Change rate card">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => { await send("PUT", `/v1/accounts/${id}/rate-card`, { rate_card_id: card }); done(); }, "Rate card changed (prices apply within a minute)"); }}>
        <Field label="Rate card">
          <Select value={card} onChange={(e) => setCard(e.target.value)} required>
            <option value="">Choose…</option>
            {(cards.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
          </Select>
        </Field>
        <Button type="submit" busy={a.busy}>Save</Button>
      </form>
      <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
    </Card>
  );
}

export default function Page() {
  return <Gate roles={["platform", "reseller"]} title="Account"><Suspense fallback={<Loading />}><View /></Suspense></Gate>;
}
