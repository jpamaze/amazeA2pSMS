# One image per service: docker build --build-arg SERVICE=router .
# admin-api also serves the portal: docker build --target admin-api --build-arg SERVICE=admin-api .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG SERVICE
RUN test -n "$SERVICE" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/$SERVICE

# Portal (Next.js static export), only built for the admin-api target.
FROM node:22-alpine AS portal
WORKDIR /portal
COPY portal/package.json portal/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY portal/ ./
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM gcr.io/distroless/static-debian12:nonroot AS admin-api
COPY --from=build /out/app /app
COPY --from=portal /portal/out /portal
ENV PORTAL_DIR=/portal
USER nonroot:nonroot
ENTRYPOINT ["/app"]

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
