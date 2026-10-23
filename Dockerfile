# syntax=docker/dockerfile:1

# ---------- 构建：静态编译，产物单文件 ----------
FROM golang:1.24-alpine AS build
WORKDIR /src

# TARGETARCH 只在 buildx 多架构构建时注入；直接 docker build 时为空，即用本机架构
ARG TARGETOS
ARG TARGETARCH

COPY go.mod ./
COPY ./*.go ./
COPY console.html ./

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/checkin .

# ---------- 只取 CA 证书 ----------
FROM alpine:3.20 AS certs
RUN apk add --no-cache ca-certificates

# ---------- 运行：scratch，镜像里只有二进制 + 证书 ----------
FROM scratch

LABEL org.opencontainers.image.title="workbuddy-daily" \
      org.opencontainers.image.description="Trae + WorkBuddy 每日签到（自托管版，扫码自动入库）" \
      org.opencontainers.image.source="https://github.com/Kyson-666/workbuddy-daily" \
      org.opencontainers.image.url="https://github.com/ggzhzc/workbuddy-trae-checkin"

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/checkin /checkin

# 时区库已编进二进制（time/tzdata），scratch 里也能按 TZ=Asia/Shanghai 计算日期
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
    TZ=Asia/Shanghai \
    DATA_DIR=/data \
    PORT=17388 \
    CRON_SCHEDULE=08:10,00:20

EXPOSE 17388
VOLUME ["/data"]

ENTRYPOINT ["/checkin"]
