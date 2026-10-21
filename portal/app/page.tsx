"use client";

import Link from "next/link";
import { get } from "@/lib/api";
import { count, money, pct } from "@/lib/format";
import { useSession } from "@/lib/session";
import { useData, useInterval } from "@/lib/use";
import { BarChart } from "@/components/Chart";
import { Shell } from "@/components/Shell";
import { Card, Loading, Notice, Stat } from "@/components/ui";

type Dash = {
  wallet: { balance: string; credit_limit: string; available?: string; held?: string; credit_used?: string };
  low_balance_alert?: { firing: boolean; threshold: string };
  today?: Record<string, number | string>;
  last_24h?: { hour: string; sent: string; delivered: string; failed: string }[];
};

type Hour = { hour: string; sent: string; delivered: string; failed: string };

// hours24 returns the last 24 IST hours, empty ones included, so the chart
// keeps its time scale however little traffic there is.
function hours24(rows: Hour[]): Hour[] {
  const byHour = new Map(rows.map((r) => [r.hour, r]));
  const out: Hour[] = [];
  const fmt = new Intl.DateTimeFormat("sv-SE", { timeZone: "Asia/Kolkata", year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", hourCycle: "h23" });
  for (let i = 23; i >= 0; i--) {
    const key = fmt.format(new Date(Date.now() - i * 3600_000)).replace(" ", "T") + ":00:00";
    out.push(byHour.get(key) ?? { hour: key, sent: "0", delivered: "0", failed: "0" });
  }
  return out;
}

export default function Dashboard() {
  const { me } = useSession();
  const { data, error, loading, reload } = useData(
    () => (me ? get<Dash>(`/v1/accounts/${me.account_id}/dashboard`) : Promise.resolve(null)),
    [me?.account_id],
  );
  useInterval(reload, 30000);
  const t = data?.today ?? {};
  const total = Number(t.total ?? 0);
  const delivered = Number(t.delivered ?? 0);
  const failed = Number(t.failed ?? 0) + Number(t.expired ?? 0) + Number(t.rejected ?? 0);
  const pending = Number(t.accepted ?? 0) + Number(t.submitted ?? 0);
  const w = data?.wallet;
  const lowAvail = data?.low_balance_alert?.firing;

  return (
    <Shell title={`Welcome, ${me?.account_name ?? ""}`}>
      {loading && !data && <Loading />}
      <Notice>{error}</Notice>
      {lowAvail && (
        <Notice>
          Available balance is below your alert level of {money(data?.low_balance_alert?.threshold)}. Messages are refused once nothing is available.
        </Notice>
      )}
      {data && (
        <div className="stack">
          <div className="grid cols-4">
            <Stat label="Available to send" value={money(w?.available)} hint="balance + credit limit − in flight" tone={Number(w?.available) <= 0 ? "bad" : lowAvail ? "warn" : undefined} />
            <Stat label="Balance" value={money(w?.balance)} hint={Number(w?.credit_used) > 0 ? `credit used ${money(w?.credit_used)}` : "settled"} />
            <Stat label="Credit limit" value={money(w?.credit_limit)} />
            <Stat label="In flight" value={money(w?.held)} hint="held for messages not yet charged" />
          </div>
          <div className="grid cols-4">
            <Stat label="Sent today" value={count(total)} hint={`charged ${money(t.charged as string)}`} />
            <Stat label="Delivered" value={count(delivered)} hint={pct(delivered, total)} tone="good" />
            <Stat label="Failed" value={count(failed)} hint={pct(failed, total)} tone={failed > 0 ? "bad" : undefined} />
            <Stat label="Awaiting receipt" value={count(pending)} />
          </div>
          <Card title="Last 24 hours" actions={<Link href="/reports/">Reports</Link>}>
            {data.last_24h ? (
              <BarChart
                bars={hours24(data.last_24h).map((h) => ({
                  label: h.hour.slice(11, 16),
                  values: { delivered: +h.delivered, failed: +h.failed, other: Math.max(+h.sent - +h.delivered - +h.failed, 0) },
                }))}
                keys={[
                  { key: "delivered", name: "Delivered", color: "var(--good)" },
                  { key: "failed", name: "Failed", color: "var(--bad)" },
                  { key: "other", name: "Awaiting receipt", color: "var(--brand)" },
                ]}
              />
            ) : (
              <p className="muted">Traffic charts need the message store (ClickHouse).</p>
            )}
          </Card>
        </div>
      )}
    </Shell>
  );
}
