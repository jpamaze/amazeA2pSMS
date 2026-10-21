"use client";

import { useState } from "react";
import { get, send } from "@/lib/api";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import type { AccountRow } from "@/components/AccountPicker";
import { Gate, Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Notice, Select, Table, useAction } from "@/components/ui";

type Route = { id: string; account_id: string; mcc: string; mnc: string; mode: string; vendor_id: string; priority: number; weight: number; hlr: boolean; hlr_reject: boolean; enabled: boolean };
const modes: Record<string, string> = { lcr: "least cost", priority: "priority", weighted: "weighted" };

function Routes() {
  const { me } = useSession();
  const routes = useData(() => get<Route[]>("/v1/routes"), []);
  const accounts = useData(() => get<AccountRow[]>("/v1/accounts?limit=500"), []);
  const vendors = useData(() => get<{ id: string; name: string; status: string }[]>("/v1/vendors"), []);
  const [disabled, setDisabled] = useState(false);
  const a = useAction();
  const admin = can(me, "admin");
  const acctName = new Map((accounts.data ?? []).map((x) => [x.id, x.name]));
  const rows = (routes.data ?? []).filter((r) => disabled || r.enabled);
  return (
    <Shell title="Routes" roles={["platform"]}>
      <div className="stack">
        <p className="small muted">
          An account&apos;s own routes are tried before the defaults. Among the routes matching the destination (its network, or the whole country with MNC *), lower priority numbers go first;
          routes with the same priority form a group: least cost picks the cheapest healthy vendor with a cost for the destination, priority takes the route&apos;s vendor, weighted splits by weight.
          HLR looks the number up first (ported numbers; with &quot;refuse&quot;, invalid or absent numbers are not sent).
        </p>
        {admin && <NewRoute accounts={accounts.data ?? []} vendors={vendors.data ?? []} done={routes.reload} />}
        <Notice>{routes.error ?? a.error}</Notice>
        <Card pad={false} title="Routes" actions={<label className="small"><input type="checkbox" checked={disabled} onChange={(e) => setDisabled(e.target.checked)} /> show disabled</label>}>
          <Table rows={rows} empty="No routes: messages to a destination without a route are refused." columns={[
            { key: "account_id", label: "Account", render: (r) => r.account_id ? acctName.get(r.account_id) ?? <code>{r.account_id.slice(0, 8)}</code> : <Badge tone="info">default</Badge> },
            { key: "dest", label: "MCC/MNC", render: (r) => <code>{r.mcc}/{r.mnc}</code> },
            { key: "mode", label: "Mode", render: (r) => modes[r.mode] ?? r.mode },
            { key: "vendor_id", label: "Vendor", render: (r) => r.vendor_id || <span className="muted">cheapest</span> },
            { key: "priority", label: "Priority", align: "right" },
            { key: "weight", label: "Weight", align: "right" },
            { key: "hlr", label: "HLR", render: (r) => r.hlr ? (r.hlr_reject ? "look up, refuse invalid/absent" : "look up") : "—" },
            { key: "enabled", label: "", render: (r) => !r.enabled ? <Badge>disabled</Badge> : admin && (
              <Button kind="link" onClick={() => confirm("Disable this route?") && a.run(async () => { await send("DELETE", `/v1/routes/${r.id}`); routes.reload(); })}>Disable</Button>) },
          ]} />
        </Card>
      </div>
    </Shell>
  );
}

function NewRoute({ accounts, vendors, done }: { accounts: AccountRow[]; vendors: { id: string; name: string }[]; done: () => void }) {
  const [f, setF] = useState({ account_id: "", mcc: "404", mnc: "*", mode: "lcr", vendor_id: "", priority: "100", weight: "1", hlr: "" });
  const a = useAction();
  return (
    <Card title="New route">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        await send("POST", "/v1/routes", {
          account_id: f.account_id || undefined, mcc: f.mcc, mnc: f.mnc || "*", mode: f.mode, vendor_id: f.mode === "lcr" ? undefined : f.vendor_id,
          priority: Number(f.priority), weight: Number(f.weight), hlr: f.hlr !== "", hlr_reject: f.hlr === "reject",
        });
        done();
      }, "Route added; routers apply it within 15 seconds"); }}>
        <Field label="Account">
          <Select value={f.account_id} onChange={(e) => setF({ ...f, account_id: e.target.value })}>
            <option value="">Default (everyone)</option>
            {accounts.filter((x) => x.role !== "platform").map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}
          </Select>
        </Field>
        <Field label="MCC"><Input value={f.mcc} onChange={(e) => setF({ ...f, mcc: e.target.value })} required size={4} /></Field>
        <Field label="MNC" hint="* = whole country"><Input value={f.mnc} onChange={(e) => setF({ ...f, mnc: e.target.value })} size={4} /></Field>
        <Field label="Mode">
          <Select value={f.mode} onChange={(e) => setF({ ...f, mode: e.target.value })}>
            <option value="lcr">Least cost</option><option value="priority">Priority</option><option value="weighted">Weighted</option>
          </Select>
        </Field>
        {f.mode !== "lcr" && (
          <Field label="Vendor">
            <Select value={f.vendor_id} onChange={(e) => setF({ ...f, vendor_id: e.target.value })} required>
              <option value="">Choose…</option>
              {vendors.map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
            </Select>
          </Field>
        )}
        <Field label="Priority" hint="lower first"><Input inputMode="numeric" value={f.priority} onChange={(e) => setF({ ...f, priority: e.target.value })} size={5} /></Field>
        {f.mode === "weighted" && <Field label="Weight"><Input inputMode="numeric" value={f.weight} onChange={(e) => setF({ ...f, weight: e.target.value })} size={5} /></Field>}
        <Field label="HLR">
          <Select value={f.hlr} onChange={(e) => setF({ ...f, hlr: e.target.value })}>
            <option value="">No lookup</option><option value="lookup">Look up</option><option value="reject">Look up, refuse invalid or absent</option>
          </Select>
        </Field>
        <Button type="submit" busy={a.busy}>Add route</Button>
      </form>
      <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
    </Card>
  );
}

export default function Page() {
  return <Gate roles={["platform"]} title="Routes"><Routes /></Gate>;
}
