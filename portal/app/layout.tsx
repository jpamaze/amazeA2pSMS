import type { Metadata } from "next";
import { SessionProvider } from "@/lib/session";
import "./globals.css";

export const metadata: Metadata = { title: "SMS portal" };

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <SessionProvider>{children}</SessionProvider>
      </body>
    </html>
  );
}
