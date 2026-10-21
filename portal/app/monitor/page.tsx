"use client";

import { get } from "@/lib/api";
import { useData, useInterval } from "@/lib/use";
import { LineChart } from "@/components/Chart";
import { Gate, Shell } from "@/components/Shell";
import { Badge, Card, Loading, Notice, Stat, Table } from "@/components/ui";

type S = { labels: Record<string, string>; points: [number, number][] };
type Live = Record<string, S[]>;

const last = (s?: S[]) => (s && s[0]?.points.length ? s[0].points[s[0].points.length - 1][1] : 0);
const pts = (s?: S[]) => (s?.[0]?.points ?? []).map(([t, v]) => [t * 1000, v] as [number, number]);
const fmt = (v: number) => (v >= 100 ? v.toFixed(0) : v.toFixed(1));
const palette = ["#1f6feb", "#8250df", "#bf8700", "#1a7f37", "#cf222e", "#0a7c86"];

function Monitor() {
  const { data, error, loading, reload } = useData(() => get<Live>("/v1/monitor/live"), []);
  useInterval(reload, 10000);
  const vendors = new Set<string>();
  for (const k of ["binds_up", "routable", "vendor_submit_tps"]) for (const s of data?.[k] ?? []) vendors.add(s.labels.vendor);
  const pick = (k: string, v: string) => data?.[k]?.find((s) => s.labels.vendor === v);
  return (
    <Shell title="Live monitor" roles={["platform"]} actions={<span className="muted small">refreshes every 10 s</span>}>
      {loading && !data && <Loading />}
      <Notice>{error}</Notice>
      {data && (
        <div className="stack">
          <div className="grid cols-4">
            <Stat label="Accepted" value={`${fmt(last(data.accepted_tps))} /s`} />
            <Stat label="Refused" value={`${fmt(last(data.refused_tps))} /s`} tone={last(data.refused_tps) > 0 ? "warn" : undefined} />
            <Stat label="Delivered" value={`${fmt(last(data.delivered_tps))} /s`} tone="good" />
            <Stat label="Accept p99" value={`${fmt(last(data.accept_p99_ms))} ms`} hint={`${last(data.customer_binds).toFixed(0)} customer binds`} />
          </div>
          <Card title="Throughput, last 30 minutes (messages / s)">
            <LineChart series={[
              { name: "Accepted", color: "var(--brand)", points: pts(data.accepted_tps) },
              { name: "Delivered", color: "var(--good)", points: pts(data.delivered_tps) },
              { name: "Failed", color: "var(--bad)", points: pts(data.failed_tps) },
              { name: "Refused", color: "var(--warn)", points: pts(data.refused_tps) },
            ]} />
          </Card>
          <div className="grid cols-2">
            <Card title="Vendors" pad={false}>
              <Table rows={[...vendors].sort()} empty="No vendors reporting" columns={[
                { key: "v", label: "Vendor", render: (v) => <strong>{v}</strong> },
                { key: "r", label: "Routing", render: (v) => { const r = pick("routable", v); return r ? <Badge>{last([r]) === 1 ? "up" : "down"}</Badge> : "—"; } },
                { key: "b", label: "Binds up", align: "right", render: (v) => { const b = pick("binds_up", v); return b ? fmt(last([b])) : "—"; } },
                { key: "t", label: "Submits /s", align: "right", render: (v) => fmt((data.vendor_submit_tps ?? []).filter((s) => s.labels.vendor === v).reduce((x, s) => x + last([s]), 0)) },
              ]} />
            </Card>
            <Card title="Submit rate by vendor">
              <LineChart series={(data.vendor_submit_tps ?? []).map((s, i) => ({ name: s.labels.vendor, color: palette[i % palette.length], points: s.points.map(([t, v]) => [t * 1000, v] as [number, number]) }))} />
            </Card>
          </div>
          <div className="grid cols-2">
            <Card title="Refusals, last 5 minutes" pad={false}>
              <Table rows={(data.refused_by_reason ?? []).filter((s) => last([s]) > 0)} empty="None" columns={[
                { key: "reason", label: "Reason", render: (s) => <code>{s.labels.reason}</code> },
                { key: "rate", label: "per second", align: "right", render: (s) => fmt(last([s])) },
              ]} />
            </Card>
            <Card title="Failover reroutes, last 15 minutes" pad={false}>
              <Table rows={(data.rerouted ?? []).filter((s) => last([s]) >= 1)} empty="None" columns={[
                { key: "from", label: "From vendor", render: (s) => s.labels.from_vendor },
                { key: "why", label: "Why", render: (s) => s.labels.reason },
                { key: "n", label: "Messages", align: "right", render: (s) => last([s]).toFixed(0) },
              ]} />
            </Card>
          </div>
        </div>
      )}
    </Shell>
  );
}

export default function Page() {
  return <Gate roles={["platform"]} title="Live monitor"><Monitor /></Gate>;
}
