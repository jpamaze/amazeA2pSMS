"use client";

import { zones } from "@/lib/tz";
import { Field, Select } from "@/components/ui";

// TimeZoneField picks the zone that date filters and shown times use.
export function TimeZoneField({ value, onChange }: { value: string; onChange: (tz: string) => void }) {
  return (
    <Field label="Time zone">
      <Select value={value} onChange={(e) => onChange(e.target.value)}>
        {zones(value).map((z) => <option key={z.id} value={z.id}>{z.label}</option>)}
      </Select>
    </Field>
  );
}
