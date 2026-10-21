"use client";

// CsvImport uploads a CSV file (or pasted text) to an import endpoint and
// shows what the server made of it, line by line when it refuses lines.

import { ReactNode, useState } from "react";
import { ApiError, send } from "@/lib/api";
import { Button, Card, Field, Input, Notice, TextArea } from "./ui";

export function CsvImport({ title, path, columns, example, extra, done }: {
  title: string;
  path: string | (() => string | null); // null: not ready (extra fields missing)
  columns: ReactNode;
  example?: string;
  extra?: ReactNode;
  done?: (result: Record<string, unknown>) => void;
}) {
  const [text, setText] = useState("");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [lines, setLines] = useState<string[]>([]);
  const [ok, setOk] = useState<string | null>(null);

  async function pick(f: File | undefined) {
    if (!f) return;
    if (f.size > 20 << 20) { setError("Files over 20 MB: use the API"); return; }
    setName(f.name);
    setText(await f.text());
  }
  async function upload() {
    const p = typeof path === "function" ? path() : path;
    if (!p) { setError("Fill in the fields above first"); return; }
    setBusy(true); setError(null); setLines([]); setOk(null);
    try {
      const r = await send<Record<string, unknown>>("POST", p, text, { "Content-Type": "text/csv" });
      const n = [r.imported, r.applied, r.saved, r.lines].find((x) => typeof x === "number");
      const bad = (r.bad_lines as string[] | null) ?? [];
      setOk([n !== undefined ? `Imported ${n} rows.` : "Imported.", r.scheduled ? `${r.scheduled} scheduled.` : "", (r.note as string) ?? ""].filter(Boolean).join(" "));
      setLines(bad);
      setText(""); setName("");
      done?.(r);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      if (e instanceof ApiError && e.lines) setLines(e.lines);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Card title={title}>
      <div className="stack">
        <p className="muted small">Columns: {columns}</p>
        {extra}
        <div className="form-row">
          <Field label="CSV file"><Input type="file" accept=".csv,text/csv,text/plain" onChange={(e) => pick(e.target.files?.[0])} /></Field>
        </div>
        <Field label={name ? `From ${name} (you can edit it)` : "…or paste the rows"}>
          <TextArea rows={5} value={text} onChange={(e) => setText(e.target.value)} placeholder={example} spellCheck={false} className="mono" />
        </Field>
        <div className="row"><Button onClick={upload} busy={busy} disabled={!text.trim()}>Import</Button></div>
        <Notice>{error}</Notice>
        <Notice kind="ok">{ok}</Notice>
        {lines.length > 0 && (
          <ul className="lines">{lines.slice(0, 50).map((l, i) => <li key={i}>{l}</li>)}{lines.length > 50 && <li>…and {lines.length - 50} more</li>}</ul>
        )}
      </div>
    </Card>
  );
}
