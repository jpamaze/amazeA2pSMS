"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { Suspense, useEffect, useState } from "react";
import { get, send } from "@/lib/api";
import { count, when } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData, useInterval } from "@/lib/use";
import type { Health, Policy, Report, Routing } from "@/lib/vendors";
import { CsvImport } from "@/components/CsvImport";
import { Gate, Shell } from "@/components/Shell";
import { RoutingBadge } from "@/components/VendorState";
import { Badge, Button, Card, Field, Input, Loading, Notice, Select, Table, Tabs, TextArea, useAction } from "@/components/ui";

type Bind = { id: string; host: string; port: number; tls: boolean; system_id: string; system_type: string; smpp_version: string; bind_type: string; window_size: number; tps_limit: number; tlv_profile: Record<string, string>; enabled: boolean };
type Profile = { base_url: string; auth: unknown; request_template: unknown; response_mapping: unknown; dlr_mapping: unknown; tps_limit: number; concurrency: number; dlr_path?: string; has_secret: boolean };
type Vendor = { id: string; name: string; protocol: string; status: string; created_at: string; binds: Bind[]; http_profile?: Profile; routing?: Routing; connectors?: Report[] | null };
type Cost = { mcc: string; mnc: string; category: string; cost: string; valid_from: string; state: string };

function View() {
  const { me } = useSession();
  const id = useSearchParams().get("id") ?? "";
  const { data: v, error, reload } = useData(() => get<Vendor>(`/v1/vendors/${id}`), [id]);
  useInterval(() => reload(), 15000);
  const [tab, setTab] = useState("setup");
  const st = useAction();
  const admin = can(me, "admin");
  const setStatus = (status: string) => st.run(async () => { await send("PUT", `/v1/vendors/${id}/status`, { status }); reload(); }, `Vendor ${status}; routers apply it within 15 seconds`);
  return (
    <Shell title={v?.name ?? "Vendor"} roles={["platform"]} actions={<Link href="/vendors/">All vendors</Link>}>
      <Notice>{error ?? st.error}</Notice><Notice kind="ok">{st.ok}</Notice>
      {!v ? <Loading /> : (
        <div className="stack">
          <Card title="Status" actions={admin && (
            <div className="inline-actions">
              {v.status !== "active" && <Button busy={st.busy} onClick={() => setStatus("active")}>Activate</Button>}
              {v.status === "active" && <Button kind="secondary" busy={st.busy} onClick={() => confirm(`Pause ${v.name}? Routing stops using it.`) && setStatus("paused")}>Pause</Button>}
              {v.status !== "disabled" && <Button kind="danger" busy={st.busy} onClick={() => confirm(`Disable ${v.name}?`) && setStatus("disabled")}>Disable</Button>}
            </div>
          )}>
            <dl className="kv">
              <dt>ID</dt><dd><code>{v.id}</code> · {v.protocol.toUpperCase()} · added {when(v.created_at)}</dd>
              <dt>Status</dt><dd><Badge>{v.status}</Badge></dd>
              <dt>Routing</dt><dd><RoutingBadge r={v.routing} status={v.status} />{v.routing?.detail && <span className="muted small"> {v.routing.detail}</span>}{v.routing?.since ? <span className="muted small"> since {when(new Date(v.routing.since).toISOString())}</span> : null}</dd>
            </dl>
            {v.connectors?.length ? (
              <div style={{ marginTop: 12 }}>
                <Table rows={v.connectors} columns={[
                  { key: "at", label: "Connector report", render: (r) => when(new Date(r.at).toISOString()) },
                  ...(v.protocol === "smpp" ? [{ key: "binds", label: "Binds up", align: "right" as const, render: (r: Report) => `${r.binds_up} / ${r.binds}` }] : []),
                  { key: "ok", label: "Sent (30 s)", align: "right", render: (r) => count(r.ok) },
                  { key: "failed", label: "Failed", align: "right", render: (r) => count(r.failed) },
                  { key: "p95_ms", label: "p95 submit", align: "right", render: (r) => `${r.p95_ms} ms` },
                  { key: "rerouted", label: "Rerouted", align: "right", render: (r) => count(r.rerouted) },
                ]} />
              </div>
            ) : <p className="muted small">No connector is reporting. Connectors start a vendor within 30 seconds once it has an enabled bind or an HTTP profile and is not disabled.</p>}
          </Card>
          <Tabs value={tab} onChange={setTab} tabs={[
            { id: "setup", label: v.protocol === "smpp" ? "SMPP binds" : "HTTP profile" },
            { id: "failover", label: "Failover" },
            { id: "costs", label: "Costs" },
          ]} />
          {tab === "setup" && (v.protocol === "smpp" ? <Binds v={v} admin={admin} done={reload} /> : <HTTPProfile v={v} admin={admin} done={reload} />)}
          {tab === "failover" && <Failover id={id} admin={admin} />}
          {tab === "costs" && <Costs id={id} admin={admin} />}
          <p className="small"><Link href="/deals/">Volume commitments</Link> · <Link href={`/vendor-billing/?vendor=${id}`}>Invoices</Link> · <Link href="/routes/">Routes</Link></p>
        </div>
      )}
    </Shell>
  );
}

function Binds({ v, admin, done }: { v: Vendor; admin: boolean; done: () => void }) {
  const a = useAction();
  const [adding, setAdding] = useState(false);
  return (
    <Card title="SMPP binds" actions={admin && <Button kind="secondary" onClick={() => setAdding(!adding)}>{adding ? "Close" : "Add bind"}</Button>}>
      <div className="stack">
        {adding && <AddBind vendor={v.id} done={() => { setAdding(false); done(); }} />}
        <Notice>{a.error}</Notice>
        <Table rows={v.binds} empty="No binds: add the vendor's SMSC host and credentials." columns={[
          { key: "host", label: "Host", render: (b) => <code>{b.host}:{b.port}{b.tls ? " (TLS)" : ""}</code> },
          { key: "system_id", label: "System ID" },
          { key: "smpp_version", label: "Version / type", render: (b) => `${b.smpp_version} · ${b.bind_type}` },
          { key: "window_size", label: "Window", align: "right" },
          { key: "tps_limit", label: "TPS", align: "right" },
          { key: "tlv", label: "DLT TLVs", render: (b) => Object.keys(b.tlv_profile ?? {}).length ? <span className="small">{Object.entries(b.tlv_profile).map(([k, t]) => `${k} ${t}`).join(", ")}</span> : <span className="muted">none</span> },
          { key: "enabled", label: "", render: (b) => b.enabled ? (admin ? <Button kind="link" onClick={() => confirm(`Disable the bind to ${b.host}?`) &&
            a.run(async () => { await send("DELETE", `/v1/vendors/${v.id}/binds/${b.id}`); done(); })}>Disable</Button> : <Badge>active</Badge>) : <Badge>disabled</Badge> },
        ]} />
        <p className="muted small">The connector reads binds when it starts: restart it after a change. Passwords are stored encrypted and never shown.</p>
      </div>
    </Card>
  );
}

function AddBind({ vendor, done }: { vendor: string; done: () => void }) {
  const [f, setF] = useState({ host: "", port: "2775", tls: false, system_id: "", password: "", system_type: "", smpp_version: "3.4", bind_type: "transceiver", window_size: "10", tps_limit: "100", pe_id: "", template_id: "", tm_chain: "" });
  const a = useAction();
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => setF({ ...f, [k]: e.target.value });
  return (
    <form className="stack" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
      const tlv: Record<string, string> = {};
      for (const k of ["pe_id", "template_id", "tm_chain"] as const) if (f[k]) tlv[k] = f[k];
      await send("POST", `/v1/vendors/${vendor}/binds`, {
        host: f.host, port: Number(f.port), tls: f.tls, system_id: f.system_id, password: f.password, system_type: f.system_type,
        smpp_version: f.smpp_version, bind_type: f.bind_type, window_size: Number(f.window_size), tps_limit: Number(f.tps_limit), tlv_profile: tlv,
      });
      done();
    }); }}>
      <div className="form-row">
        <Field label="Host"><Input value={f.host} onChange={set("host")} required /></Field>
        <Field label="Port"><Input inputMode="numeric" value={f.port} onChange={set("port")} required size={6} /></Field>
        <Field label="TLS"><Select value={f.tls ? "1" : ""} onChange={(e) => setF({ ...f, tls: !!e.target.value })}><option value="">No</option><option value="1">Yes</option></Select></Field>
        <Field label="System ID"><Input value={f.system_id} onChange={set("system_id")} maxLength={16} required /></Field>
        <Field label="Password"><Input type="password" value={f.password} onChange={set("password")} maxLength={9} required autoComplete="new-password" /></Field>
        <Field label="System type"><Input value={f.system_type} onChange={set("system_type")} size={8} /></Field>
      </div>
      <div className="form-row">
        <Field label="SMPP version"><Select value={f.smpp_version} onChange={set("smpp_version")}><option>3.4</option><option>3.3</option><option>5.0</option></Select></Field>
        <Field label="Bind type"><Select value={f.bind_type} onChange={set("bind_type")}><option>transceiver</option><option>transmitter</option><option>receiver</option></Select></Field>
        <Field label="Window"><Input inputMode="numeric" value={f.window_size} onChange={set("window_size")} size={5} /></Field>
        <Field label="TPS"><Input inputMode="numeric" value={f.tps_limit} onChange={set("tps_limit")} size={6} /></Field>
      </div>
      <div className="form-row">
        <Field label="PE ID TLV" hint="e.g. 0x1400; ask the vendor"><Input value={f.pe_id} onChange={set("pe_id")} size={8} /></Field>
        <Field label="Template ID TLV"><Input value={f.template_id} onChange={set("template_id")} size={8} /></Field>
        <Field label="TM chain TLV"><Input value={f.tm_chain} onChange={set("tm_chain")} size={8} /></Field>
      </div>
      <div className="row"><Button type="submit" busy={a.busy}>Add bind</Button></div>
      <Notice>{a.error}</Notice>
    </form>
  );
}

const pretty = (v: unknown) => JSON.stringify(v ?? {}, null, 2);

function HTTPProfile({ v, admin, done }: { v: Vendor; admin: boolean; done: () => void }) {
  const p = v.http_profile;
  const [f, setF] = useState({ base_url: "", auth: "{}", request_template: "", response_mapping: "", dlr_mapping: "", tps_limit: "100", concurrency: "50", secret: "" });
  useEffect(() => {
    if (p) setF({ base_url: p.base_url, auth: pretty(p.auth), request_template: pretty(p.request_template), response_mapping: pretty(p.response_mapping), dlr_mapping: pretty(p.dlr_mapping), tps_limit: String(p.tps_limit), concurrency: String(p.concurrency), secret: "" });
    else setF((x) => ({ ...x, request_template: pretty({ method: "POST", path: "/send", encoding: "json", params: { to: "{{to}}", from: "{{sender}}", text: "{{text}}", pe_id: "{{pe_id}}", template_id: "{{template_id}}" } }),
      response_mapping: pretty({ format: "json", success_path: "status", success_values: ["ok"], id_path: "message_id", error_path: "error" }),
      dlr_mapping: pretty({ id_param: "message_id", status_param: "status", error_param: "error", delivered_values: ["DELIVRD"], expired_values: ["EXPIRED"] }) }));
  }, [p]);
  const a = useAction();
  const json = (name: string, s: string) => { try { return JSON.parse(s); } catch { throw new Error(`${name} is not valid JSON`); } };
  const area = (k: "auth" | "request_template" | "response_mapping" | "dlr_mapping", label: string, hint: string) => (
    <Field label={label} hint={hint}><TextArea rows={k === "auth" ? 3 : 8} value={f[k]} onChange={(e) => setF({ ...f, [k]: e.target.value })} disabled={!admin} spellCheck={false} /></Field>
  );
  return (
    <Card title="HTTP profile">
      <form className="stack" onSubmit={(e) => { e.preventDefault(); a.run(async () => {
        await send("PUT", `/v1/vendors/${v.id}/http-profile`, {
          base_url: f.base_url, auth: json("Auth", f.auth), request_template: json("Request", f.request_template), response_mapping: json("Response", f.response_mapping),
          dlr_mapping: json("Receipts", f.dlr_mapping), tps_limit: Number(f.tps_limit), concurrency: Number(f.concurrency), ...(f.secret ? { secret: f.secret } : {}),
        });
        setF({ ...f, secret: "" }); done();
      }, "Saved. Restart the vendor's connector to use it."); }}>
        <div className="form-row">
          <Field label="Base URL"><Input value={f.base_url} onChange={(e) => setF({ ...f, base_url: e.target.value })} placeholder="https://api.vendor.example" required disabled={!admin} /></Field>
          <Field label="TPS"><Input inputMode="numeric" value={f.tps_limit} onChange={(e) => setF({ ...f, tps_limit: e.target.value })} size={6} disabled={!admin} /></Field>
          <Field label="Concurrency"><Input inputMode="numeric" value={f.concurrency} onChange={(e) => setF({ ...f, concurrency: e.target.value })} size={6} disabled={!admin} /></Field>
          <Field label="Secret" hint={p?.has_secret ? "stored; leave empty to keep it" : "API key or password"}>
            <Input type="password" value={f.secret} onChange={(e) => setF({ ...f, secret: e.target.value })} autoComplete="new-password" disabled={!admin} />
          </Field>
        </div>
        <div className="grid cols-2">
          {area("auth", "Auth", "type: none | bearer | basic | header | query; name; username")}
          {area("request_template", "Request", "method, path, encoding (json | form | query), params, headers. Placeholders: {{to}} {{sender}} {{text}} {{pe_id}} {{template_id}} {{tm_chain}} {{secret}} …")}
          {area("response_mapping", "Response", "format json: success_path, success_values, id_path, error_path; format text: id_regex")}
          {area("dlr_mapping", "Receipts", "id_param, status_param, error_param, delivered_values, expired_values, intermediate_values")}
        </div>
        {admin && <div className="row"><Button type="submit" busy={a.busy}>Save profile</Button></div>}
        <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
      </form>
      {p && (p.dlr_path
        ? <p className="small">Receipt URL to give the vendor: the connector&apos;s DLR listener (<code>DLR_ADDR</code>) at <code>{p.dlr_path}</code></p>
        : <p className="small muted">The receipt URL (it carries the vendor&apos;s token) is shown to admins.</p>)}
    </Card>
  );
}

function Failover({ id, admin }: { id: string; admin: boolean }) {
  const { data, error } = useData(() => get<Health[]>("/v1/vendors/health"), [id]);
  const [f, setF] = useState<Policy | null>(null);
  useEffect(() => { const h = data?.find((x) => x.vendor_id === id); if (h) setF(h.policy); }, [data, id]);
  const a = useAction();
  if (!f) return <Card title="Failover"><Notice>{error}</Notice>{!error && <Loading />}</Card>;
  const num = (k: keyof Policy, label: string, hint: string) => (
    <Field label={label} hint={hint}><Input inputMode="numeric" value={String(f[k])} onChange={(e) => setF({ ...f, [k]: Number(e.target.value) || 0 })} size={6} disabled={!admin} /></Field>
  );
  return (
    <Card title="Failover">
      <p className="small muted">Takes the vendor out of routing when no bind is up or a limit is crossed and brings it back after the cool-down once healthy; queued messages are rerouted. 0 = not checked.</p>
      <form className="stack" onSubmit={(e) => { e.preventDefault(); a.run(() => send("PUT", `/v1/vendors/${id}/failover`, f), "Saved; applies within seconds"); }}>
        <div className="form-row">
          <Field label="Failover"><Select value={f.enabled ? "1" : ""} onChange={(e) => setF({ ...f, enabled: !!e.target.value })} disabled={!admin}><option value="1">On</option><option value="">Off</option></Select></Field>
          {num("max_submit_ms", "Max p95 submit (ms)", "slow")}
          {num("max_error_pct", "Max errors (%)", "submit errors")}
          {num("min_dlr_pct", "Min delivered (%)", "of receipts")}
          {num("dlr_window_min", "Receipt window (min)", "")}
          {num("cooldown_s", "Cool-down (s)", "before coming back")}
        </div>
        {admin && <div className="row"><Button type="submit" busy={a.busy}>Save</Button></div>}
      </form>
      <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
    </Card>
  );
}

function Costs({ id, admin }: { id: string; admin: boolean }) {
  const { data, error, reload } = useData(() => get<Cost[]>(`/v1/vendors/${id}/costs`), [id]);
  const [all, setAll] = useState(false);
  const rows = (data ?? []).filter((c) => all || c.state !== "superseded");
  return (
    <div className="stack">
      <Card title="Costs" actions={<label className="small"><input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} /> show superseded</label>}>
        <Notice>{error}</Notice>
        <Table rows={rows} empty="No costs: least-cost routing skips a vendor without a cost for the destination." columns={[
          { key: "mcc", label: "MCC" },
          { key: "mnc", label: "MNC" },
          { key: "category", label: "Category" },
          { key: "cost", label: "Cost / part", align: "right", render: (c) => `₹${c.cost}` },
          { key: "valid_from", label: "From", render: (c) => when(c.valid_from) },
          { key: "state", label: "", render: (c) => <Badge tone={c.state === "current" ? "good" : c.state === "scheduled" ? "info" : "muted"}>{c.state}</Badge> },
        ]} />
      </Card>
      {admin && <CsvImport title="Import costs" path={`/v1/vendors/${id}/costs/import`} done={reload}
        columns={<>mcc, cost (required), mnc, category, valid_from (RFC 3339; a future date schedules the change). A header row names the columns.</>}
        example={"mcc,mnc,category,cost\n404,*,*,0.0950\n405,861,*,0.0900"} />}
    </div>
  );
}

export default function Page() {
  return <Gate roles={["platform"]} title="Vendor"><Suspense fallback={<Loading />}><View /></Suspense></Gate>;
}
