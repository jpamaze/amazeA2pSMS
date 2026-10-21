"use client";

import { useSession } from "@/lib/session";
import { Users } from "@/components/AccountAdmin";
import { Shell } from "@/components/Shell";

export default function Team() {
  const { me } = useSession();
  return <Shell title="Team">{me && <Users acct={me.account_id} />}</Shell>;
}
