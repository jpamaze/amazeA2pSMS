"use client";

// The signed-in frame: brand, navigation for the user's role, account
// switcher line and sign-out. Pages render inside it.

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { ReactNode, useEffect } from "react";
import { signOut } from "@/lib/auth";
import { Me, useSession } from "@/lib/session";
import { Loading, Notice } from "./ui";

type Item = { href: string; label: string; roles?: Me["account_role"][] };

const nav: Item[] = [
  { href: "/", label: "Dashboard" },
  { href: "/messages/", label: "Messages" },
  { href: "/reports/", label: "Reports" },
  { href: "/wallet/", label: "Wallet" },
  { href: "/accounts/", label: "Accounts", roles: ["platform", "reseller"] },
  { href: "/monitor/", label: "Live monitor", roles: ["platform"] },
  { href: "/developer/", label: "API & SMPP" },
  { href: "/team/", label: "Team" },
  { href: "/alerts/", label: "Alerts" },
  { href: "/branding/", label: "Branding", roles: ["platform", "reseller"] },
];

export function Shell({ title, actions, children, roles }: { title: string; actions?: ReactNode; children: ReactNode; roles?: Me["account_role"][] }) {
  const { me, loading, error, config } = useSession();
  const router = useRouter();
  const path = usePathname();

  useEffect(() => {
    if (!loading && !me && !error) router.replace(`/login/?next=${encodeURIComponent(path)}`);
  }, [loading, me, error, router, path]);

  if (loading || (!me && !error)) return <Loading />;
  if (error || !me) return <div className="pad"><Notice>{error ?? "Not signed in"}</Notice></div>;

  const brand = me.brand ?? config?.brand;
  const allowed = !roles || roles.includes(me.account_role);
  return (
    <div className="shell">
      <aside className="side">
        <div className="brand">
          {brand?.logo ? <img src={brand.logo} alt="" /> : <span className="brand-mark">{brand?.name?.[0] ?? "A"}</span>}
          <span>{brand?.name}</span>
        </div>
        <nav>
          {nav
            .filter((i) => !i.roles || i.roles.includes(me.account_role))
            .map((i) => (
              <Link key={i.href} href={i.href} className={(i.href === "/" ? path === "/" : path.startsWith(i.href)) ? "on" : ""}>
                {i.label}
              </Link>
            ))}
        </nav>
        <div className="side-foot">
          <div className="small">{me.account_name}</div>
          <div className="muted small">{me.email} · {me.user_role}</div>
          <button className="btn btn-link" onClick={() => signOut()}>Sign out</button>
          {brand?.support_email && <a className="small" href={`mailto:${brand.support_email}`}>Support</a>}
        </div>
      </aside>
      <main className="main">
        <header className="page-head">
          <h1>{title}</h1>
          <div className="row">{actions}</div>
        </header>
        {allowed ? children : <Notice>This page is not available for your account.</Notice>}
      </main>
    </div>
  );
}
