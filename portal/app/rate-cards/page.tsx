"use client";

import { useEffect, useState } from "react";
import { get, send } from "@/lib/api";
import { when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { CsvImport } from "@/components/CsvImport";
import { Gate, Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Notice, Select, Table, useAction } from "@/components/ui";

type RateCard = { id: string; name: string; currency: string };
type Entry = { mcc: string; mnc: string; category: string; price: string; valid_from: string; scheduled: boolean };

const categories = ["*", "transactional", "service_implicit", "service_explicit", "promotional"];

function RateCards() {
  const { me } = useSession();
  const cards = useData(() => get<RateCard[]>("/v1/rate-cards"), []);
  const [card, setCard] = useState("");
  const [q, setQ] = useState("");
  useEffect(() => { if (!card && cards.data?.length) setCard(cards.data[0].id); }, [cards.data, card]);
  const entries = useData(() => (card ? get<Entry[]>(`/v1/rate-cards/${card}/entries`) : Promise.resolve(null)), [card]);
  const admin = can(me, "admin");
  const rows = (entries.data ?? []).filter((e) => !q || `${e.mcc}/${e.mnc}`.startsWith(q) || e.category.includes(q));
  return (
    <Shell title="Rate cards" roles={["platform", "reseller"]}>
      <div className="stack">
        <p className="small muted">
          The price per part an account pays, by destination (MCC, MNC or * for the whole country) and DLT category (* for any).
          {me?.account_role === "reseller" ? " Your cards price your sub-accounts; you pay the platform's price for the same traffic." : " Resellers keep their own cards for their sub-accounts."}
          {" "}A future date schedules a price change; deals override the card while they run.
        </p>
        {admin && <NewCard done={(id) => { cards.reload(); setCard(id); }} />}
        <Notice>{cards.error}</Notice>
        <Card pad={false}>
          <div className="form-row pad">
            <Field label="Rate card">
              <Select value={card} onChange={(e) => setCard(e.target.value)}>
                {(cards.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name} ({c.currency})</option>)}
              </Select>
            </Field>
            <Field label="Find"><Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="404/45 or category" /></Field>
          </div>
          <Notice>{entries.error}</Notice>
          <Table rows={rows} empty={cards.data?.length ? "No prices on this card" : "No rate cards yet"} columns={[
            { key: "mcc", label: "MCC" },
            { key: "mnc", label: "MNC" },
            { key: "category", label: "Category", render: (e) => e.category === "*" ? "any" : e.category.replace("_", " ") },
            { key: "price", label: "Price / part", align: "right", render: (e) => `₹${e.price}` },
            { key: "valid_from", label: "From", render: (e) => when(e.valid_from) },
            { key: "scheduled", label: "", render: (e) => e.scheduled ? <Badge tone="info">scheduled</Badge> : <Badge tone="good">current</Badge> },
          ]} />
        </Card>
        {admin && card && <SetPrice card={card} done={entries.reload} />}
        {admin && card && <CsvImport title="Import prices" path={`/v1/rate-cards/${card}/entries/import`} done={entries.reload}
          columns={<>mcc, price (required), mnc, category, valid_from (RFC 3339). A header row names the columns, in any order.</>}
          example={"mcc,mnc,category,price\n404,*,*,0.1400\n404,*,promotional,0.1200"} />}
      </div>
    </Shell>
  );
}

function NewCard({ done }: { done: (id: string) => void }) {
  const [name, setName] = useState("");
  const a = useAction();
  return (
    <Card title="New rate card">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => { const r = await send<{ id: string }>("POST", "/v1/rate-cards", { name }); setName(""); done(r.id); }); }}>
        <Field label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} required /></Field>
        <Button type="submit" busy={a.busy}>Create</Button>
      </form>
      <Notice>{a.error}</Notice>
    </Card>
  );
}

function SetPrice({ card, done }: { card: string; done: () => void }) {
  const [f, setF] = useState({ mcc: "404", mnc: "*", category: "*", price: "", from: "" });
  const a = useAction();
  return (
    <Card title="Set a price">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        await send("PUT", `/v1/rate-cards/${card}/entries`, [{ mcc: f.mcc, mnc: f.mnc || "*", category: f.category, price: f.price, ...(f.from ? { valid_from: new Date(f.from).toISOString() } : {}) }]);
        setF({ ...f, price: "" }); done();
      }, f.from ? "Price change scheduled" : "Price set; applies within a minute"); }}>
        <Field label="MCC"><Input value={f.mcc} onChange={(e) => setF({ ...f, mcc: e.target.value })} required size={4} /></Field>
        <Field label="MNC"><Input value={f.mnc} onChange={(e) => setF({ ...f, mnc: e.target.value })} size={4} /></Field>
        <Field label="Category">
          <Select value={f.category} onChange={(e) => setF({ ...f, category: e.target.value })}>
            {categories.map((c) => <option key={c} value={c}>{c === "*" ? "any" : c.replace("_", " ")}</option>)}
          </Select>
        </Field>
        <Field label="Price per part (₹)"><Input inputMode="decimal" value={f.price} onChange={(e) => setF({ ...f, price: e.target.value })} required /></Field>
        <Field label="From" hint="empty = now"><Input type="datetime-local" value={f.from} onChange={(e) => setF({ ...f, from: e.target.value })} /></Field>
        <Button type="submit" busy={a.busy}>Save</Button>
      </form>
      <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
    </Card>
  );
}

export default function Page() {
  return <Gate roles={["platform", "reseller"]} title="Rate cards"><RateCards /></Gate>;
}
