"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { get, send } from "@/lib/api";
import { count, when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData, useInterval } from "@/lib/use";
import { connectorSum, type Health, type VendorRow } from "@/lib/vendors";
import { Gate, Shell } from "@/components/Shell";
import { RoutingBadge } from "@/components/VendorState";
import { Badge, Button, Card, Field, Input, Notice, Select, Table, useAction } from "@/components/ui";

function Vendors() {
  const { me } = useSession();
  const router = useRouter();
  const list = useData(() => get<VendorRow[]>("/v1/vendors"), []);
  const health = useData(() => get<Health[]>("/v1/vendors/health"), []);
  useInterval(() => health.reload(), 10000);
  const [adding, setAdding] = useState(false);
  const byId = new Map((health.data ?? []).map((h) => [h.vendor_id, h]));
  return (
    <Shell title="Vendors" roles={["platform"]} actions={can(me, "admin") && <Button onClick={() => setAdding(!adding)}>{adding ? "Close" : "New vendor"}</Button>}>
      <div className="stack">
        {adding && <NewVendor done={(id) => router.push(`/vendors/view/?id=${id}`)} />}
        <Notice>{list.error ?? health.error}</Notice>
        <Card pad={false}>
          <Table rows={list.data ?? []} empty="No vendors yet" onRow={(v) => router.push(`/vendors/view/?id=${v.id}`)} columns={[
            { key: "name", label: "Vendor", render: (v) => <><strong>{v.name}</strong> <code className="muted">{v.id}</code></> },
            { key: "protocol", label: "Protocol", render: (v) => <Badge tone="info">{v.protocol.toUpperCase()}</Badge> },
            { key: "setup", label: "Set up", render: (v) => v.protocol === "smpp" ? `${v.binds} bind${v.binds === 1 ? "" : "s"}` : v.http_profile ? "profile set" : <Badge tone="warn">no profile</Badge> },
            { key: "routing", label: "Routing", render: (v) => <RoutingBadge r={byId.get(v.id)?.routing ?? v.routing} status={v.status} /> },
            { key: "binds_up", label: "Binds up", align: "right", render: (v) => { const s = connectorSum(byId.get(v.id)?.connectors); return s && v.protocol === "smpp" ? `${s.binds_up} / ${s.binds}` : "—"; } },
            { key: "rate", label: "Sent / failed (30 s)", align: "right", render: (v) => { const s = connectorSum(byId.get(v.id)?.connectors); return s ? `${count(s.ok)} / ${count(s.failed)}` : <span className="muted">no connector</span>; } },
            { key: "p95", label: "p95 submit", align: "right", render: (v) => { const s = connectorSum(byId.get(v.id)?.connectors); return s ? `${s.p95_ms} ms` : "—"; } },
            { key: "failover", label: "Failover", render: (v) => byId.get(v.id)?.policy.enabled ? "on" : <span className="muted">off</span> },
            { key: "created_at", label: "Added", render: (v) => when(v.created_at) },
          ]} />
        </Card>
      </div>
    </Shell>
  );
}

function NewVendor({ done }: { done: (id: string) => void }) {
  const [f, setF] = useState({ id: "", name: "", protocol: "smpp" });
  const a = useAction();
  return (
    <Card title="New vendor">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => { await send("POST", "/v1/vendors", f); done(f.id); }); }}>
        <Field label="Name"><Input value={f.name} onChange={(e) => setF({ ...f, name: e.target.value, id: f.id || "" })} required /></Field>
        <Field label="ID" hint="a-z, 0-9, -; names its queue. Cannot be changed.">
          <Input value={f.id} onChange={(e) => setF({ ...f, id: e.target.value.toLowerCase() })} pattern="[a-z0-9\-]{2,32}" required />
        </Field>
        <Field label="Protocol">
          <Select value={f.protocol} onChange={(e) => setF({ ...f, protocol: e.target.value })}>
            <option value="smpp">SMPP</option><option value="http">HTTP</option>
          </Select>
        </Field>
        <Button type="submit" busy={a.busy}>Create</Button>
      </form>
      <Notice>{a.error}</Notice>
      <p className="muted small">New vendors start paused. Add binds or the HTTP profile, import costs, start its connector, then activate it.</p>
    </Card>
  );
}

export default function Page() {
  return <Gate roles={["platform"]} title="Vendors"><Vendors /></Gate>;
}
