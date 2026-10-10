# Clusterheart dashboard server, packaged for the in-cluster ("web") deployment.
# This is the same Go server the desktop app spawns on localhost — here it runs
# as a pod, reads its own cluster via the mounted ServiceAccount, and serves the
# UI to a browser. Build context is this app/ directory:
#
#   docker build -t clusterheart:latest app/
#
# --- build stage ---
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off → a static binary that runs on the slim runtime image. The frontend in
# ./static is embedded into the binary at build time (go:embed).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/clusterheart .

# --- runtime stage ---
FROM debian:bookworm-slim
# kubectl backs the "view YAML", port-forward and load-test features. Inside a
# pod it authenticates automatically via the mounted ServiceAccount token, so no
# kubeconfig is needed. aws/jmeter are intentionally omitted (the cost and
# load-test panels just report they're unavailable if the tool is missing).
ARG KUBECTL_VERSION=v1.31.0
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && curl -fsSLo /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/$(dpkg --print-architecture)/kubectl" \
 && chmod +x /usr/local/bin/kubectl \
 && apt-get purge -y curl && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/clusterheart /usr/local/bin/clusterheart
EXPOSE 8088
# Bind all interfaces so the Service can reach it (the desktop build binds
# 127.0.0.1 only). With no kubeconfig file present the server falls back to the
# in-cluster ServiceAccount automatically.
ENTRYPOINT ["clusterheart", "-addr", "0.0.0.0:8088"]
