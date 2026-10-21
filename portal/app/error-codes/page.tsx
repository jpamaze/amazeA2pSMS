"use client";

import { useState } from "react";
import { get, qs } from "@/lib/api";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { CsvImport } from "@/components/CsvImport";
import { Gate, Shell } from "@/components/Shell";
import { Card, Field, Input, Notice, Select, Table } from "@/components/ui";

type Code = { vendor: string; code: string; reason: string; customer_code: string; description: string };

function ErrorCodes() {
  const { me } = useSession();
  const { data, error, reload } = useData(() => get<Code[]>("/v1/vendor-error-codes"), []);
  const vendors = useData(() => get<{ id: string; name: string }[]>("/v1/vendors"), []);
  const [vendor, setVendor] = useState("");
  const [q, setQ] = useState("");
  const [impVendor, setImpVendor] = useState("");
  const rows = (data ?? []).filter((c) => (!vendor || c.vendor === vendor) && (!q || `${c.code} ${c.reason} ${c.customer_code} ${c.description}`.toLowerCase().includes(q.toLowerCase())));
  return (
    <Shell title="Vendor error codes" roles={["platform"]}>
      <div className="stack">
        <p className="small muted">Maps the error codes vendors put in receipts to one reason and the 3-digit code customers see, so receipts read the same whichever vendor carried the message. A vendor&apos;s own mapping wins over the one for all vendors (*).</p>
        <Notice>{error ?? vendors.error}</Notice>
        <Card pad={false}>
          <div className="form-row pad">
            <Field label="Vendor">
              <Select value={vendor} onChange={(e) => setVendor(e.target.value)}>
                <option value="">All</option><option value="*">All vendors (*)</option>
                {(vendors.data ?? []).map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
              </Select>
            </Field>
            <Field label="Find"><Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="code, reason or text" /></Field>
          </div>
          <Table rows={rows} empty="No mappings" columns={[
            { key: "vendor", label: "Vendor", render: (c) => c.vendor === "*" ? <span className="muted">all</span> : c.vendor },
            { key: "code", label: "Vendor code", render: (c) => <code>{c.code}</code> },
            { key: "reason", label: "Reason", render: (c) => <code>{c.reason}</code> },
            { key: "customer_code", label: "Customer code", render: (c) => <code>{c.customer_code}</code> },
            { key: "description", label: "Description" },
          ]} />
        </Card>
        {can(me, "admin") && (
          <CsvImport title="Import mappings" path={`/v1/vendor-error-codes/import${qs({ vendor: impVendor })}`} done={reload}
            columns={<>code, reason (a-z, 0-9, _), customer_code (3 digits)[, description]. Existing codes are updated.</>}
            example={"code,reason,customer_code,description\n0x0000000B,invalid_destination,201,Invalid destination number"}
            extra={
              <div className="form-row">
                <Field label="For vendor">
                  <Select value={impVendor} onChange={(e) => setImpVendor(e.target.value)}>
                    <option value="">All vendors (*)</option>
                    {(vendors.data ?? []).map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
                  </Select>
                </Field>
              </div>
            } />
        )}
      </div>
    </Shell>
  );
}

export default function Page() {
  return <Gate roles={["platform"]} title="Vendor error codes"><ErrorCodes /></Gate>;
}
