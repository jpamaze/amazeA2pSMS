"use client";

import { useSession } from "@/lib/session";
import { ApiKeys, SmppCredentials, Webhook } from "@/components/AccountAdmin";
import { Shell } from "@/components/Shell";
import { Card } from "@/components/ui";

export default function Developer() {
  const { me } = useSession();
  if (!me) return <Shell title="API & SMPP"><span /></Shell>;
  const origin = typeof window !== "undefined" ? window.location.origin : "";
  return (
    <Shell title="API & SMPP">
      <div className="stack">
        <Card title="Sending">
          <p>HTTP: <code>POST /v1/sms</code> with header <code>X-API-Key</code> — body <code>{`{"to":"+9198…","sender":"HEADER","text":"…","template_id":"…","pe_id":"…"}`}</code>. DLT template ID and PE ID are found automatically when omitted.</p>
          <p className="muted small">API host: ask your provider (it may differ from this portal, {origin}). SMPP: v3.4 or v5.0 transceiver; DLT fields in TLVs 0x1400 (PE ID), 0x1401 (template ID), 0x1402 (TM chain).</p>
        </Card>
        <ApiKeys acct={me.account_id} />
        <Webhook acct={me.account_id} />
        <SmppCredentials acct={me.account_id} manage={me.account_role === "platform"} />
      </div>
    </Shell>
  );
}
