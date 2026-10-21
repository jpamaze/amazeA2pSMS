"use client";

import { useSearchParams } from "next/navigation";
import { Suspense } from "react";
import { get } from "@/lib/api";
import { money, when } from "@/lib/format";
import { useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { Shell } from "@/components/Shell";
import { Badge, Card, Loading, Notice } from "@/components/ui";

function View() {
  const { me } = useSession();
  const id = useSearchParams().get("id") ?? "";
  const acct = useSearchParams().get("account") ?? me?.account_id;
  const { data, error, loading } = useData(
    () => (acct && id ? get<{ source: string; message: Record<string, any> }>(`/v1/accounts/${acct}/messages/${id}`) : Promise.resolve(null)),
    [acct, id],
  );
  const m = data?.message;
  const rows: [string, React.ReactNode][] = m
    ? [
        ["Message ID", <code key="id">{m.id}</code>],
        ["Status", <Badge key="s">{m.status}</Badge>],
        ["Error", m.error_code ? <code key="e">{m.error_code}</code> : "—"],
        ["To", m.to],
        ["Sender", m.sender],
        ["Parts", m.segments],
        ["Price", money(m.price)],
        ["Accepted", when(m.accepted_at)],
        ["Completed", when(m.done_at)],
        ["Network (MCC/MNC)", `${m.mcc ?? ""} / ${m.mnc ?? ""}`],
        ["DLT PE ID", m.pe_id || "—"],
        ["DLT template", m.template_id || "—"],
        ["DLT category", m.dlt_category || "—"],
        ["Vendor", m.vendor_id || "—"],
        ["Vendor message ID", m.vendor_msg_id || "—"],
        ["Your reference", m.client_ref || "—"],
        ["Channel", m.channel],
      ]
    : [];
  return (
    <Shell title="Message">
      {loading && <Loading />}
      <Notice>{error}</Notice>
      {m && (
        <Card title={data?.source === "live" ? "Live state" : "Message record"}>
          <dl className="kv">
            {rows.map(([k, v]) => (
              <div key={k} style={{ display: "contents" }}>
                <dt>{k}</dt>
                <dd>{v}</dd>
              </div>
            ))}
          </dl>
        </Card>
      )}
    </Shell>
  );
}

export default function Page() {
  return (
    <Suspense fallback={<Loading />}>
      <View />
    </Suspense>
  );
}
