"use client";

import { useEffect, useState } from "react";
import { get, qs, send } from "@/lib/api";
import { count } from "@/lib/format";
import { can, useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { AccountPicker, useAccount } from "@/components/AccountPicker";
import { CsvImport } from "@/components/CsvImport";
import { Shell } from "@/components/Shell";
import { Badge, Button, Card, Field, Input, Loading, Notice, Select, Table, Tabs, TextArea, useAction } from "@/components/ui";

type Entity = { pe_id: string; name: string; status: string; headers: number; active_templates: number; tm_chain: string };
type Summary = { account_id: string; dlt_mode: string; entities: Entity[] };
type Header = { header: string; category: string; status: string; active_templates: number };
type Template = { template_id: string; name: string; category: string; status: string; body: string; headers: string[]; whitelist: string[] };

const cat = (c: string) => c.replace("_", " ");

export default function DLT() {
  const { me } = useSession();
  const [acct, setAcct] = useAccount();
  const { data, error, loading, reload } = useData(() => (acct ? get<Summary>(`/v1/accounts/${acct}/dlt`) : Promise.resolve(null)), [acct]);
  const [pe, setPe] = useState("");
  const [adding, setAdding] = useState(false);
  useEffect(() => {
    const list = data?.entities ?? [];
    if (!list.some((e) => e.pe_id === pe)) setPe(list[0]?.pe_id ?? "");
  }, [data, pe]);
  const admin = can(me, "admin");
  const entity = data?.entities.find((e) => e.pe_id === pe);
  return (
    <Shell title="DLT registrations" actions={admin && <Button onClick={() => setAdding(!adding)}>{adding ? "Close" : "Add entity"}</Button>}>
      <div className="stack">
        {me?.account_role !== "customer" && <Card><div className="form-row"><AccountPicker value={acct} onChange={(id) => { setAcct(id); setPe(""); }} /></div></Card>}
        <Notice>{error}</Notice>
        {loading && !data && <Loading />}
        {data && (
          <>
            <Mode acct={acct} mode={data.dlt_mode} done={reload} />
            {adding && <AddEntity acct={acct} done={() => { setAdding(false); reload(); }} />}
            <Card title="Principal entities (PE IDs)" pad={false}>
              <Table rows={data.entities} onRow={(e) => setPe(e.pe_id)} empty="No entity registered: add the PE ID from the operator DLT portal." columns={[
                { key: "pe_id", label: "PE ID", render: (e) => <><code>{e.pe_id}</code>{e.pe_id === pe && <> <Badge tone="info">selected</Badge></>}</> },
                { key: "name", label: "Name" },
                { key: "status", label: "Status", render: (e) => <Badge>{e.status}</Badge> },
                { key: "headers", label: "Headers", align: "right", render: (e) => count(e.headers) },
                { key: "active_templates", label: "Active templates", align: "right", render: (e) => count(e.active_templates) },
                { key: "tm_chain", label: "TM chain", render: (e) => e.tm_chain || <span className="muted">not set</span> },
              ]} />
            </Card>
            {entity && <EntityView acct={acct} entity={entity} admin={admin} done={reload} />}
            <Check acct={acct} entities={data.entities} />
          </>
        )}
      </div>
    </Shell>
  );
}

function Mode({ acct, mode, done }: { acct: string; mode: string; done: () => void }) {
  const { me } = useSession();
  const [reason, setReason] = useState("");
  const a = useAction();
  const platform = me?.account_role === "platform" && can(me, "admin") && acct !== me.account_id;
  if (mode !== "off" && !platform) return null;
  const next = mode === "off" ? "enforce" : "off";
  return (
    <Card title="DLT checks" actions={mode === "off" ? <Badge tone="warn">off for this account</Badge> : <Badge tone="good">enforced</Badge>}>
      {mode === "off" && <p className="small">Messages from this account are not checked against its registrations. Operators still scrub them, so unregistered traffic fails at the operator.</p>}
      {platform && (
        <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => { await send("PUT", `/v1/accounts/${acct}/dlt-mode`, { mode: next, reason }); setReason(""); done(); }); }}>
          <Field label="Reason (audited)"><Input value={reason} onChange={(e) => setReason(e.target.value)} required /></Field>
          <Button type="submit" kind={next === "off" ? "danger" : "primary"} busy={a.busy}>{next === "off" ? "Turn checks off" : "Enforce checks"}</Button>
        </form>
      )}
      <Notice>{a.error}</Notice>
    </Card>
  );
}

function AddEntity({ acct, done }: { acct: string; done: () => void }) {
  const [f, setF] = useState({ pe_id: "", name: "", tm_chain: "" });
  const a = useAction();
  return (
    <Card title="Add or update a principal entity">
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => { await send("POST", `/v1/accounts/${acct}/dlt/entities`, f); done(); }); }}>
        <Field label="PE ID" hint="as registered on the DLT portal"><Input value={f.pe_id} inputMode="numeric" onChange={(e) => setF({ ...f, pe_id: e.target.value.trim() })} required /></Field>
        <Field label="Entity name"><Input value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} required /></Field>
        <Field label="TM chain" hint="the PE–TM chain registered on the DLT portal"><Input value={f.tm_chain} onChange={(e) => setF({ ...f, tm_chain: e.target.value })} /></Field>
        <Button type="submit" busy={a.busy}>Save</Button>
      </form>
      <Notice>{a.error}</Notice>
    </Card>
  );
}

function EntityView({ acct, entity, admin, done }: { acct: string; entity: Entity; admin: boolean; done: () => void }) {
  const [tab, setTab] = useState("headers");
  const [header, setHeader] = useState("");
  const base = `/v1/accounts/${acct}/dlt/entities/${entity.pe_id}`;
  const headers = useData(() => get<Header[]>(`${base}/headers`), [base]);
  const templates = useData(() => (tab === "templates" ? get<Template[]>(`${base}/templates${qs({ header })}`) : Promise.resolve(null)), [base, header, tab]);
  const refresh = () => { headers.reload(); templates.reload(); done(); };
  return (
    <Card title={`${entity.name} · ${entity.pe_id}`}>
      <Tabs value={tab} onChange={setTab} tabs={[
        { id: "headers", label: `Headers (${entity.headers})` },
        { id: "templates", label: `Templates (${entity.active_templates} active)` },
        { id: "chain", label: "TM chain" },
        ...(admin ? [{ id: "import", label: "Import" }] : []),
      ]} />
      {tab === "headers" && (
        <>
          <Notice>{headers.error}</Notice>
          <Table rows={headers.data ?? []} empty="No headers yet: import them from the DLT portal export." onRow={(h) => { setHeader(h.header); setTab("templates"); }} columns={[
            { key: "header", label: "Header", render: (h) => <code>{h.header}</code> },
            { key: "category", label: "Category", render: (h) => cat(h.category) },
            { key: "status", label: "Status", render: (h) => <Badge>{h.status}</Badge> },
            { key: "active_templates", label: "Active templates", align: "right", render: (h) => count(h.active_templates) },
          ]} />
        </>
      )}
      {tab === "templates" && (
        <div className="stack">
          <div className="form-row">
            <Field label="Header">
              <Select value={header} onChange={(e) => setHeader(e.target.value)}>
                <option value="">All</option>
                {(headers.data ?? []).map((h) => <option key={h.header}>{h.header}</option>)}
              </Select>
            </Field>
          </div>
          <Notice>{templates.error}</Notice>
          {templates.loading && !templates.data ? <Loading /> : (
            <Table rows={templates.data ?? []} empty="No templates" columns={[
              { key: "template_id", label: "Template ID", render: (t) => <code>{t.template_id}</code> },
              { key: "name", label: "Name" },
              { key: "category", label: "Category", render: (t) => cat(t.category) },
              { key: "status", label: "Status", render: (t) => <Badge>{t.status}</Badge> },
              { key: "headers", label: "Headers", render: (t) => t.headers.join(", ") },
              { key: "body", label: "Body", render: (t) => <span className="small">{t.body}</span> },
              { key: "whitelist", label: "Whitelisted", render: (t) => <span className="small">{t.whitelist.join(", ")}</span> },
            ]} />
          )}
        </div>
      )}
      {tab === "chain" && <Chain base={base} chain={entity.tm_chain} admin={admin} done={done} />}
      {tab === "import" && admin && (
        <div className="stack">
          <CsvImport title="Headers" path={`${base}/headers/import`} done={refresh}
            columns={<>header, category[, status]. The DLT portal&apos;s header export works as is (Header Name, Header Type…).</>}
            example={"header,category\nAMZOTP,transactional\n567890,promotional"} />
          <CsvImport title="Content templates" path={`${base}/templates/import`} done={refresh}
            columns={<>template_id, category, header, body[, name, status, consent, urls, apks, callbacks]. The DLT portal&apos;s template export works as is; headers must be imported first.</>}
            example={"template_id,category,header,body\n1107161234567890123,transactional,AMZOTP,Your OTP is {#var#}. -Amaze"} />
        </div>
      )}
    </Card>
  );
}

function Chain({ base, chain, admin, done }: { base: string; chain: string; admin: boolean; done: () => void }) {
  const [v, setV] = useState(chain);
  const a = useAction();
  return (
    <div className="stack">
      <p className="small muted">The PE–TM chain registered on the DLT portal. It goes with every message of this PE ID that does not carry its own chain; without one, messages are refused (dlt_tm_chain).</p>
      <form className="form-row" onSubmit={(e) => { e.preventDefault(); a.run(async () => { await send("PUT", `${base}/chain`, { tm_chain: v }); done(); }, "Saved"); }}>
        <Field label="TM chain"><Input value={v} onChange={(e) => setV(e.target.value)} disabled={!admin} required /></Field>
        {admin && <Button type="submit" busy={a.busy}>Save</Button>}
      </form>
      <Notice>{a.error}</Notice><Notice kind="ok">{a.ok}</Notice>
    </div>
  );
}

type CheckResult = { ok: boolean; pe_id?: string; template_id?: string; category?: string; tm_chain?: string; reason?: string; detail?: string; receipt_code?: string; smpp_status?: string };

function Check({ acct, entities }: { acct: string; entities: Entity[] }) {
  const [f, setF] = useState({ sender: "", text: "", pe_id: "", template_id: "", tm_chain: "" });
  const [res, setRes] = useState<CheckResult | null>(null);
  const a = useAction();
  return (
    <Card title="Check a message">
      <p className="small muted">Runs the same checks as a real submit, without sending anything.</p>
      <form className="stack" onSubmit={(e) => { e.preventDefault(); a.run(async () => setRes(await send<CheckResult>("POST", `/v1/accounts/${acct}/dlt/check`, f))); }}>
        <div className="form-row">
          <Field label="Sender (header)" hint="as sent, e.g. AMZOTP"><Input value={f.sender} onChange={(e) => setF({ ...f, sender: e.target.value })} required /></Field>
          <Field label="PE ID" hint="optional">
            <Select value={f.pe_id} onChange={(e) => setF({ ...f, pe_id: e.target.value })}>
              <option value="">From the header</option>
              {entities.map((x) => <option key={x.pe_id} value={x.pe_id}>{x.pe_id}</option>)}
            </Select>
          </Field>
          <Field label="Template ID" hint="optional; matched by text when empty"><Input value={f.template_id} onChange={(e) => setF({ ...f, template_id: e.target.value })} /></Field>
          <Field label="TM chain" hint="optional"><Input value={f.tm_chain} onChange={(e) => setF({ ...f, tm_chain: e.target.value })} /></Field>
        </div>
        <Field label="Text"><TextArea rows={3} value={f.text} onChange={(e) => setF({ ...f, text: e.target.value })} required /></Field>
        <div className="row"><Button type="submit" busy={a.busy}>Check</Button></div>
      </form>
      <Notice>{a.error}</Notice>
      {res && (
        <div className={`result ${res.ok ? "check-ok" : "check-bad"}`} style={{ marginTop: 12 }}>
          {res.ok ? (
            <dl className="kv">
              <dt>Result</dt><dd><Badge tone="good">would be accepted</Badge></dd>
              <dt>Template</dt><dd><code>{res.template_id}</code> ({cat(res.category ?? "")})</dd>
              <dt>PE ID</dt><dd><code>{res.pe_id}</code></dd>
              <dt>TM chain sent</dt><dd><code>{res.tm_chain || "—"}</code></dd>
            </dl>
          ) : (
            <dl className="kv">
              <dt>Result</dt><dd><Badge tone="bad">would be refused</Badge> <code>{res.reason}</code></dd>
              <dt>Why</dt><dd>{res.detail}</dd>
              <dt>Codes</dt><dd>SMPP <code>{res.smpp_status}</code> · receipt <code>{res.receipt_code}</code></dd>
            </dl>
          )}
        </div>
      )}
    </Card>
  );
}
