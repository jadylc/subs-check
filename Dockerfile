# 纯运行时镜像 — 不再编译 Go，二进制由 goreleaser 预编译后通过 release.yml 放入 binaries/ 目录。
# buildx 多平台构建时自动注入 TARGETARCH (amd64 / arm64 / arm)。
FROM alpine
ARG TARGETARCH
WORKDIR /app
ENV TZ=Asia/Shanghai
RUN apk add --no-cache alpine-conf ca-certificates nodejs && \
    /usr/sbin/setup-timezone -z Asia/Shanghai && \
    apk del alpine-conf && \
    rm -rf /var/cache/apk/*
COPY binaries/subs-check-${TARGETARCH} /app/subs-check
RUN chmod +x /app/subs-check
CMD ["/app/subs-check"]
EXPOSE 8199
EXPOSE 8299
