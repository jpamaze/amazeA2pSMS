"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { get, newKey, send } from "@/lib/api";
import { money } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { AccountPicker } from "@/components/AccountPicker";
import { Shell } from "@/components/Shell";
import { Button, Card, Field, Input, Loading, Notice, Stat, useAction } from "@/components/ui";

type Wallet = { ledger_balance: string; live_balance: string; credit_limit: string; held: string; available: string };

export default function WalletPage() {
  const { me } = useSession();
  const [acct, setAcct] = useState("");
  useEffect(() => { if (me && !acct) setAcct(new URLSearchParams(window.location.search).get("account") ?? me.account_id); }, [me, acct]);
  const { data: w, error, loading, reload } = useData(() => (acct ? get<Wallet>(`/v1/accounts/${acct}/wallet`) : Promise.resolve(null)), [acct]);
  const own = acct === me?.account_id;
  const finance = can(me, "finance");

  return (
    <Shell title="Wallet" actions={<Link href="/reports/">Statement</Link>}>
      {me?.account_role !== "customer" && (
        <Card><div className="form-row"><AccountPicker value={acct} onChange={setAcct} /></div></Card>
      )}
      {loading && !w && <Loading />}
      <Notice>{error}</Notice>
      {w && (
        <div className="stack" style={{ marginTop: 16 }}>
          <div className="grid cols-4">
            <Stat label="Available to send" value={money(w.available)} tone={Number(w.available) <= 0 ? "bad" : undefined} hint="balance + credit limit − in flight" />
            <Stat label="Balance" value={money(w.live_balance)} hint={`settled ${money(w.ledger_balance)}`} />
            <Stat label="Credit limit" value={money(w.credit_limit)} hint={Number(w.live_balance) < 0 ? `credit used ${money(-Number(w.live_balance))}` : "none used"} />
            <Stat label="In flight" value={money(w.held)} />
          </div>
          {!own && finance && me?.account_role === "platform" && <TopUp acct={acct} done={reload} />}
          {!own && finance && me?.account_role === "reseller" && <Transfer acct={acct} done={reload} />}
          {!own && finance && <CreditLimit acct={acct} current={w.credit_limit} done={reload} />}
          {own && me?.account_role !== "platform" && (
            <Notice kind="info">To add funds, pay {me?.account_role === "customer" ? "your provider" : "the platform"} and your balance is credited here. Credit limits are set by your provider.</Notice>
          )}
        </div>
      )}
    </Shell>
  );
}

function TopUp({ acct, done }: { acct: string; done: () => void }) {
  const [key, setKey] = useState(newKey);
  const [amount, setAmount] = useState("");
  const [ref, setRef] = useState("");
  const a = useAction();
  return (
    <Card title="Record a payment (top-up)">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        await send("POST", `/v1/accounts/${acct}/topups`, { amount, reference: ref }, { "Idempotency-Key": key });
        setAmount(""); setRef(""); setKey(newKey()); done();
      }, "Top-up booked"); }}>
        <Field label="Amount (₹)"><Input inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} required /></Field>
        <Field label="Payment reference"><Input value={ref} onChange={(e) => setRef(e.target.value)} placeholder="UTR / invoice no." required /></Field>
        <Button type="submit" busy={a.busy}>Add funds</Button>
      </form>
      <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
    </Card>
  );
}

function Transfer({ acct, done }: { acct: string; done: () => void }) {
  const [key, setKey] = useState(newKey);
  const [amount, setAmount] = useState("");
  const [ref, setRef] = useState("");
  const a = useAction();
  return (
    <Card title="Transfer from your wallet">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        await send("POST", `/v1/accounts/${acct}/transfers`, { amount, reference: ref }, { "Idempotency-Key": key });
        setAmount(""); setRef(""); setKey(newKey()); done();
      }, "Transferred"); }}>
        <Field label="Amount (₹)"><Input inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} required /></Field>
        <Field label="Reference"><Input value={ref} onChange={(e) => setRef(e.target.value)} /></Field>
        <Button type="submit" busy={a.busy}>Transfer</Button>
      </form>
      <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
    </Card>
  );
}

function CreditLimit({ acct, current, done }: { acct: string; current: string; done: () => void }) {
  const [limit, setLimit] = useState(current);
  const [reason, setReason] = useState("");
  const a = useAction();
  return (
    <Card title="Credit limit">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        await send("PUT", `/v1/accounts/${acct}/credit-limit`, { credit_limit: limit, reason });
        setReason(""); done();
      }, "Credit limit changed"); }}>
        <Field label="New limit (₹)" hint="0 = prepaid only"><Input inputMode="decimal" value={limit} onChange={(e) => setLimit(e.target.value)} required /></Field>
        <Field label="Reason (audited)"><Input value={reason} onChange={(e) => setReason(e.target.value)} required /></Field>
        <Button type="submit" busy={a.busy}>Save</Button>
      </form>
      <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
    </Card>
  );
}
