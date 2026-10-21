"use client";

// The signed-in frame: brand, navigation for the user's role, account
// switcher line and sign-out. Pages render inside it.

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { Fragment, ReactNode, useEffect } from "react";
import { signOut } from "@/lib/auth";
import { Me, useSession } from "@/lib/session";
import { Loading, Notice } from "./ui";

type Item = { href: string; label: string; roles?: Me["account_role"][]; group?: string };

const P: Me["account_role"][] = ["platform"];
const nav: Item[] = [
  { href: "/", label: "Dashboard" },
  { href: "/messages/", label: "Messages" },
  { href: "/reports/", label: "Reports" },
  { href: "/wallet/", label: "Wallet" },
  { href: "/accounts/", label: "Accounts", roles: ["platform", "reseller"] },
  { href: "/monitor/", label: "Live monitor", roles: P },
  { href: "/dlt/", label: "DLT" },
  { href: "/deals/", label: "Deals" },
  { href: "/disputes/", label: "Disputes" },
  { href: "/rate-cards/", label: "Rate cards", roles: ["platform", "reseller"] },
  { href: "/developer/", label: "API & SMPP", group: "Settings" },
  { href: "/team/", label: "Team", group: "Settings" },
  { href: "/alerts/", label: "Alerts", group: "Settings" },
  { href: "/branding/", label: "Branding", roles: ["platform", "reseller"], group: "Settings" },
  { href: "/vendors/", label: "Vendors", roles: P, group: "Operations" },
  { href: "/routes/", label: "Routes", roles: P, group: "Operations" },
  { href: "/firewall/", label: "Firewall", roles: P, group: "Operations" },
  { href: "/vendor-billing/", label: "Vendor billing", roles: P, group: "Operations" },
  { href: "/numbers/", label: "Number data", roles: P, group: "Operations" },
  { href: "/error-codes/", label: "Error codes", roles: P, group: "Operations" },
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
            .map((i, n, all) => (
              <Fragment key={i.href}>
                {i.group && i.group !== all[n - 1]?.group && <div className="nav-group">{i.group}</div>}
                <Link href={i.href} className={(i.href === "/" ? path === "/" : path.startsWith(i.href)) ? "on" : ""}>
                  {i.label}
                </Link>
              </Fragment>
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

// Gate renders a page only for the given account roles, so a page's data
// is never requested by an account that may not see it; others get the
// frame with a "not available" notice.
export function Gate({ roles, title, children }: { roles: Me["account_role"][]; title: string; children: ReactNode }) {
  const { me } = useSession();
  if (!me || !roles.includes(me.account_role)) return <Shell title={title} roles={roles}>{null}</Shell>;
  return <>{children}</>;
}
