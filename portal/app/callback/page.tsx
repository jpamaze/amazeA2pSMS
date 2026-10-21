"use client";

import { useEffect, useState } from "react";
import { completeSignIn } from "@/lib/auth";
import { useSession } from "@/lib/session";
import { Loading, Notice } from "@/components/ui";

export default function Callback() {
  const { config } = useSession();
  const [err, setErr] = useState<string | null>(null);
  useEffect(() => {
    if (!config) return;
    completeSignIn()
      .then((to) => window.location.replace(to))
      .catch((e) => setErr(e.message));
  }, [config]);
  return err ? <div className="pad"><Notice>{err}</Notice><a href="/login/">Try again</a></div> : <Loading what="Signing in" />;
}
