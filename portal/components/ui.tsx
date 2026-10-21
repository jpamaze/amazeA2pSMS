"use client";

// Small shared component library: every portal page is built from these.

import { ReactNode, useState } from "react";

export function Card({ title, actions, children, pad = true }: { title?: ReactNode; actions?: ReactNode; children: ReactNode; pad?: boolean }) {
  return (
    <section className="card">
      {(title || actions) && (
        <header className="card-head">
          <h2>{title}</h2>
          <div className="row">{actions}</div>
        </header>
      )}
      <div className={pad ? "card-body" : ""}>{children}</div>
    </section>
  );
}

export function Stat({ label, value, hint, tone }: { label: string; value: ReactNode; hint?: ReactNode; tone?: "good" | "bad" | "warn" }) {
  return (
    <div className={`stat ${tone ?? ""}`}>
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value}</div>
      {hint && <div className="stat-hint">{hint}</div>}
    </div>
  );
}

export function Button({ kind = "primary", busy, children, ...p }: React.ButtonHTMLAttributes<HTMLButtonElement> & { kind?: "primary" | "secondary" | "danger" | "link"; busy?: boolean }) {
  return (
    <button {...p} className={`btn btn-${kind} ${p.className ?? ""}`} disabled={p.disabled || busy}>
      {busy ? "Working…" : children}
    </button>
  );
}

export function Field({ label, hint, children }: { label: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="field">
      <span className="field-label">{label}</span>
      {children}
      {hint && <span className="field-hint">{hint}</span>}
    </label>
  );
}

export const Input = (p: React.InputHTMLAttributes<HTMLInputElement>) => <input {...p} className={`input ${p.className ?? ""}`} />;
export const TextArea = (p: React.TextareaHTMLAttributes<HTMLTextAreaElement>) => <textarea {...p} className={`input ${p.className ?? ""}`} />;
export const Select = (p: React.SelectHTMLAttributes<HTMLSelectElement>) => <select {...p} className={`input ${p.className ?? ""}`} />;

export function Notice({ kind = "error", children }: { kind?: "error" | "ok" | "info"; children: ReactNode }) {
  if (!children) return null;
  return <div className={`notice notice-${kind}`} role={kind === "error" ? "alert" : "status"}>{children}</div>;
}

export function Loading({ what = "Loading" }: { what?: string }) {
  return <div className="muted pad">{what}…</div>;
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}

const tones: Record<string, string> = {
  delivered: "good", active: "good", submitted: "info", accepted: "info", ok: "good", up: "good", approved: "good",
  failed: "bad", rejected: "bad", expired: "warn", suspended: "bad", down: "bad", revoked: "muted", disabled: "muted",
  open: "warn", firing: "bad",
};

export function Badge({ children, tone }: { children: ReactNode; tone?: string }) {
  const t = tone ?? tones[String(children).toLowerCase()] ?? "muted";
  return <span className={`badge badge-${t}`}>{children}</span>;
}

export type Column<T> = { key: string; label: ReactNode; render?: (row: T) => ReactNode; align?: "right" };

export function Table<T>({ rows, columns, empty = "Nothing to show", onRow }: { rows: T[]; columns: Column<T>[]; empty?: ReactNode; onRow?: (row: T) => void }) {
  if (rows.length === 0) return <Empty>{empty}</Empty>;
  return (
    <div className="table-wrap">
      <table className="table">
        <thead>
          <tr>
            {columns.map((c) => (
              <th key={c.key} className={c.align === "right" ? "num" : ""}>{c.label}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i} onClick={onRow ? () => onRow(r) : undefined} className={onRow ? "clickable" : ""}>
              {columns.map((c) => (
                <td key={c.key} className={c.align === "right" ? "num" : ""}>
                  {c.render ? c.render(r) : String((r as Record<string, unknown>)[c.key] ?? "")}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function Tabs({ tabs, value, onChange }: { tabs: { id: string; label: string }[]; value: string; onChange: (id: string) => void }) {
  return (
    <div className="tabs" role="tablist">
      {tabs.map((t) => (
        <button key={t.id} role="tab" aria-selected={t.id === value} className={t.id === value ? "tab on" : "tab"} onClick={() => onChange(t.id)}>
          {t.label}
        </button>
      ))}
    </div>
  );
}

// useAction runs a change and keeps its busy/error/ok state for the form.
export function useAction() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [ok, setOk] = useState<string | null>(null);
  async function run(fn: () => Promise<unknown>, success?: string) {
    setBusy(true);
    setError(null);
    setOk(null);
    try {
      await fn();
      if (success) setOk(success);
      return true;
    } catch (e: any) {
      setError(e?.lines?.length ? `${e.message}: ${e.lines.join("; ")}` : e?.message ?? String(e));
      return false;
    } finally {
      setBusy(false);
    }
  }
  return { busy, error, ok, run, setOk, setError };
}

export function CopyOnce({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="secret">
      <div className="muted small">{label} — shown once, store it now</div>
      <code>{value}</code>
      <Button kind="secondary" onClick={() => navigator.clipboard.writeText(value).then(() => setCopied(true))}>
        {copied ? "Copied" : "Copy"}
      </Button>
    </div>
  );
}
