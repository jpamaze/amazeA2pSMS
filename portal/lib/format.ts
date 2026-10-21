// Formatting for India: rupees with Indian digit grouping, times in IST.

const inr = new Intl.NumberFormat("en-IN", { style: "currency", currency: "INR", minimumFractionDigits: 2, maximumFractionDigits: 2 });
const num = new Intl.NumberFormat("en-IN");
const dt = new Intl.DateTimeFormat("en-IN", { timeZone: "Asia/Kolkata", dateStyle: "medium", timeStyle: "medium" });

export const money = (v: string | number | null | undefined) => (v === null || v === undefined || v === "" ? "—" : inr.format(Number(v)));
export const count = (v: string | number | null | undefined) => (v === null || v === undefined || v === "" ? "—" : num.format(Number(v)));
export const when = (v: string | null | undefined) => {
  if (!v || v.startsWith("0001")) return "—";
  const d = new Date(v.includes("T") || v.includes("Z") ? v : v.replace(" ", "T") + "Z");
  return isNaN(d.getTime()) ? v : dt.format(d);
};
export const pct = (part: number, whole: number) => (whole > 0 ? `${((100 * part) / whole).toFixed(1)}%` : "—");

// istDate returns YYYY-MM-DD in IST, offset by days.
export function istDate(offsetDays = 0): string {
  const d = new Date(Date.now() + offsetDays * 86400000);
  return new Intl.DateTimeFormat("en-CA", { timeZone: "Asia/Kolkata" }).format(d);
}

// istStart is the start of an IST day (YYYY-MM-DD) as RFC 3339; istEnd is
// the start of the next day, so a from/to pair covers whole days.
export const istStart = (day: string) => `${day}T00:00:00+05:30`;
export function istEnd(day: string): string {
  const d = new Date(`${day}T00:00:00Z`);
  d.setUTCDate(d.getUTCDate() + 1);
  return istStart(d.toISOString().slice(0, 10));
}
