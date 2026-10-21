"use client";

import type { Routing } from "@/lib/vendors";
import { Badge } from "./ui";

const reasons: Record<string, string> = { down: "no bind up", slow: "slow submits", errors: "too many errors", dlr_rate: "low delivery rate" };

// RoutingBadge shows whether failover lets routing use the vendor.
export function RoutingBadge({ r, status }: { r?: Routing; status: string }) {
  if (status !== "active") return <Badge tone="muted">{status}</Badge>;
  if (!r || !r.since) return <Badge tone="muted">no data</Badge>;
  return r.up ? <Badge tone="good">in routing</Badge> : <span title={r.detail}><Badge tone="bad">{`out: ${reasons[r.reason ?? ""] ?? r.reason ?? "down"}`}</Badge></span>;
}
