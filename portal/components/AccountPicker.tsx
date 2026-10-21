"use client";

// For platform and reseller users: choose which account a page shows (their
// own or one they manage). Customers only ever see their own.

import { get } from "@/lib/api";
import { useSession } from "@/lib/session";
import { useData } from "@/lib/use";
import { Field, Select } from "./ui";

export type AccountRow = { id: string; name: string; role: string; status: string; parent_name?: string; balance: string; credit_limit: string; available?: string; rate_card?: string; created_at: string; dlt_mode: string };

export function AccountPicker({ value, onChange }: { value: string; onChange: (id: string) => void }) {
  const { me } = useSession();
  const managers = me && me.account_role !== "customer";
  const { data } = useData(() => (managers ? get<AccountRow[]>("/v1/accounts?limit=500") : Promise.resolve([])), [managers]);
  if (!managers || !me) return null;
  return (
    <Field label="Account">
      <Select value={value} onChange={(e) => onChange(e.target.value)}>
        <option value={me.account_id}>{me.account_name} (you)</option>
        {(data ?? [])
          .filter((a) => a.id !== me.account_id)
          .map((a) => (
            <option key={a.id} value={a.id}>
              {a.name}{a.parent_name && me.account_role === "platform" ? ` — ${a.parent_name}` : ""}
            </option>
          ))}
      </Select>
    </Field>
  );
}
