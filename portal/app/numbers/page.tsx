"use client";

import { useState } from "react";
import { get, qs } from "@/lib/api";
import { count, when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { CsvImport } from "@/components/CsvImport";
import { Gate, Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Notice, Table, Tabs, useAction } from "@/components/ui";

type Net = { mcc: string; mnc: string; source: string };
type Info = { e164: string; series?: Net; resolved?: Net; hlr_cached?: { mcc?: string; mnc?: string; status: string; at: number } };
type Series = { prefix: string; mcc: string; mnc: string; operator: string; circle: string };
type MNPImport = { rows: number; bad_lines: number; imported_by: string; imported_at: string };

const sources: Record<string, string> = { mnp: "ported (MNP)", series: "number series", default: "country default", hlr: "HLR" };

function Numbers() {
  const [tab, setTab] = useState("lookup");
  return (
    <Shell title="Number data" roles={["platform"]}>
      <p className="small muted">How a number&apos;s network is found: the MNP (ported numbers) list first, then the number series, then the country default. HLR answers, when a route asks for one, are cached and win over both.</p>
      <Tabs value={tab} onChange={setTab} tabs={[{ id: "lookup", label: "Look up" }, { id: "series", label: "Number series" }, { id: "mnp", label: "Ported numbers (MNP)" }]} />
      {tab === "lookup" && <Lookup />}
      {tab === "series" && <SeriesTab />}
      {tab === "mnp" && <MNP />}
    </Shell>
  );
}

function Lookup() {
  const [n, setN] = useState("");
  const [info, setInfo] = useState<Info | null>(null);
  const a = useAction();
  const net = (x?: Net) => x ? <><code>{x.mcc}/{x.mnc}</code> <span className="muted small">{sources[x.source] ?? x.source}</span></> : "—";
  return (
    <Card title="Look up a number">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => setInfo(await get<Info>(`/v1/numbers/${encodeURIComponent(n.replace(/[\s+-]/g, ""))}`))); }}>
        <Field label="Number" hint="10 digits, or with country code"><Input value={n} onChange={(e) => setN(e.target.value)} inputMode="tel" required /></Field>
        <Button type="submit" busy={a.busy}>Look up</Button>
      </form>
      <Notice>{a.error}</Notice>
      {info && (
        <dl className="kv" style={{ marginTop: 12 }}>
          <dt>Number</dt><dd><code>+{info.e164}</code></dd>
          <dt>Routed as</dt><dd>{net(info.resolved)}</dd>
          <dt>Number series</dt><dd>{net(info.series)}</dd>
          <dt>HLR (cached)</dt><dd>{info.hlr_cached ? <><Badge tone={info.hlr_cached.status === "ok" ? "good" : "warn"}>{info.hlr_cached.status}</Badge> {info.hlr_cached.mcc && <code>{info.hlr_cached.mcc}/{info.hlr_cached.mnc}</code>} <span className="muted small">{when(new Date(info.hlr_cached.at).toISOString())}</span></> : "—"}</dd>
        </dl>
      )}
    </Card>
  );
}

function SeriesTab() {
  const { me } = useSession();
  const [q, setQ] = useState("");
  const [replace, setReplace] = useState(false);
  const { data, error, reload } = useData(() => get<{ total: number; series: Series[] }>(`/v1/number-series${qs({ q })}`), [q]);
  return (
    <div className="stack">
      <Card pad={false} title={data ? `${count(data.total)} prefixes` : "Prefixes"}>
        <div className="form-row pad"><Field label="Find"><Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="prefix, operator or circle" /></Field></div>
        <Notice>{error}</Notice>
        <Table rows={data?.series ?? []} empty="No number series loaded: numbers fall back to the country default." columns={[
          { key: "prefix", label: "Prefix", render: (s) => <code>{s.prefix}</code> },
          { key: "net", label: "MCC/MNC", render: (s) => <code>{s.mcc}/{s.mnc}</code> },
          { key: "operator", label: "Operator" },
          { key: "circle", label: "Circle" },
        ]} />
        {data && data.series.length === 500 && <p className="muted small pad">Showing the first 500; narrow the search.</p>}
      </Card>
      {can(me, "admin") && (
        <CsvImport title="Import number series" path={`/v1/number-series/import${replace ? "?replace=true" : ""}`} done={reload}
          columns={<>prefix (2–9 digits, national), mcc, mnc[, operator, circle]. The longest matching prefix wins.</>}
          example={"prefix,mcc,mnc,operator,circle\n98100,404,10,Airtel,Delhi"}
          extra={<label className="small"><input type="checkbox" checked={replace} onChange={(e) => setReplace(e.target.checked)} /> replace the whole table (otherwise rows are added or updated)</label>} />
      )}
    </div>
  );
}

function MNP() {
  const { me } = useSession();
  const { data, error, reload } = useData(() => get<MNPImport[]>("/v1/mnp/imports"), []);
  return (
    <div className="stack">
      <Card pad={false} title="Imports">
        <Notice>{error}</Notice>
        <Table rows={data ?? []} empty="No MNP data imported" columns={[
          { key: "imported_at", label: "When", render: (m) => when(m.imported_at) },
          { key: "rows", label: "Numbers", align: "right", render: (m) => count(m.rows) },
          { key: "bad_lines", label: "Bad lines", align: "right", render: (m) => m.bad_lines ? <Badge tone="warn">{count(m.bad_lines)}</Badge> : "0" },
          { key: "imported_by", label: "By" },
        ]} />
      </Card>
      {can(me, "admin") && (
        <CsvImport title="Import ported numbers" path="/v1/mnp/import" done={reload}
          columns={<>msisdn, mcc, mnc. An empty mnc removes the number (ported back). Bad lines are skipped and listed; large files are better sent to the API directly.</>}
          example={"msisdn,mcc,mnc\n919810012345,405,861"} />
      )}
    </div>
  );
}

export default function Page() {
  return <Gate roles={["platform"]} title="Number data"><Numbers /></Gate>;
}
