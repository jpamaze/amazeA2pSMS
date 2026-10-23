"use client";

// Time zone for date filters and times on the Messages and Reports pages.
// The default is UTC; the choice is remembered per browser.

import { useEffect, useState } from "react";

export const ZONES: { id: string; label: string }[] = [
  { id: "UTC", label: "UTC" },
  { id: "Asia/Kolkata", label: "India (IST)" },
  { id: "Asia/Dubai", label: "Dubai (GST)" },
  { id: "Asia/Singapore", label: "Singapore (SGT)" },
  { id: "Europe/London", label: "London" },
  { id: "America/New_York", label: "New York" },
];

const KEY = "amaze.tz";

export function useTimeZone(): [string, (tz: string) => void] {
  const [tz, setTz] = useState("UTC");
  useEffect(() => {
    try {
      const v = localStorage.getItem(KEY);
      if (v) setTz(v);
    } catch { /* storage blocked: stay on UTC */ }
  }, []);
  const set = (v: string) => {
    setTz(v);
    try { localStorage.setItem(KEY, v); } catch { /* not remembered */ }
  };
  return [tz, set];
}

// zones lists ZONES plus the browser's own zone and the current one if missing.
export function zones(current: string): { id: string; label: string }[] {
  const out = [...ZONES];
  let local = "";
  try { local = Intl.DateTimeFormat().resolvedOptions().timeZone; } catch { /* old browser */ }
  for (const z of [local, current]) {
    if (z && !out.some((o) => o.id === z)) out.push({ id: z, label: z === local ? `${z} (this device)` : z });
  }
  return out;
}

// dateIn returns YYYY-MM-DD in tz, offset by days.
export function dateIn(tz: string, offsetDays = 0): string {
  return new Intl.DateTimeFormat("en-CA", { timeZone: tz }).format(new Date(Date.now() + offsetDays * 86400000));
}

// whenIn formats a time (RFC 3339, or "YYYY-MM-DD hh:mm:ss" in UTC) in tz.
export function whenIn(v: string | null | undefined, tz: string): string {
  if (!v || v.startsWith("0001")) return "—";
  const d = new Date(v.includes("T") || v.includes("Z") ? v : v.replace(" ", "T") + "Z");
  return isNaN(d.getTime()) ? v : new Intl.DateTimeFormat("en-IN", { timeZone: tz, dateStyle: "medium", timeStyle: "medium" }).format(d);
}

// shortZone is the label shown next to dates, e.g. "UTC" or "IST".
export function shortZone(tz: string): string {
  if (tz === "UTC") return "UTC";
  if (tz === "Asia/Kolkata") return "IST";
  try {
    return new Intl.DateTimeFormat("en-US", { timeZone: tz, timeZoneName: "short" }).formatToParts(new Date()).find((p) => p.type === "timeZoneName")?.value ?? tz;
  } catch {
    return tz;
  }
}
