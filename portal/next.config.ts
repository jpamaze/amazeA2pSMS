import type { NextConfig } from "next";

// Static export: admin-api serves portal/out at "/" (PORTAL_DIR) next to the
// API at /v1, so the browser talks to one origin and needs no CORS.
const config: NextConfig = {
  output: "export",
  trailingSlash: true,
  images: { unoptimized: true },
  reactStrictMode: true,
  // `next dev` proxies the API to a local admin-api.
  ...(process.env.NODE_ENV === "development"
    ? { rewrites: async () => [{ source: "/v1/:path*", destination: `${process.env.ADMIN_API ?? "http://localhost:8081"}/v1/:path*` }] }
    : {}),
};

export default config;
