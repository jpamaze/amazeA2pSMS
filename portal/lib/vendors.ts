// Vendor shapes shared by the vendor pages.

export type Routing = { up: boolean; reason?: string; since: number; detail?: string };
export type Report = { at: number; binds_up: number; binds: number; ok: number; failed: number; p95_ms: number; rerouted: number };
export type Policy = { enabled: boolean; max_submit_ms: number; max_error_pct: number; min_dlr_pct: number; dlr_window_min: number; cooldown_s: number };
export type Health = { vendor_id: string; status: string; routing: Routing; connectors: Report[] | null; policy: Policy };
export type VendorRow = { id: string; name: string; protocol: string; status: string; created_at: string; binds: number; http_profile: boolean; routing?: Routing };

// connectorSum adds up the fresh reports of a vendor's connector instances.
export function connectorSum(r: Report[] | null | undefined) {
  if (!r?.length) return null;
  return r.reduce((s, x) => ({ instances: s.instances + 1, binds_up: s.binds_up + x.binds_up, binds: s.binds + x.binds, ok: s.ok + x.ok, failed: s.failed + x.failed, p95_ms: Math.max(s.p95_ms, x.p95_ms), rerouted: s.rerouted + x.rerouted }),
    { instances: 0, binds_up: 0, binds: 0, ok: 0, failed: 0, p95_ms: 0, rerouted: 0 });
}
