FROM eclipse-temurin:21-jre-alpine

ARG JADX_VERSION=1.5.1
RUN apk add --no-cache curl unzip && \
    curl -fsSL "https://github.com/skylot/jadx/releases/download/v${JADX_VERSION}/jadx-${JADX_VERSION}.zip" \
      -o /tmp/jadx.zip && \
    unzip /tmp/jadx.zip -d /opt/jadx && \
    rm /tmp/jadx.zip && \
    chmod +x /opt/jadx/bin/jadx && \
    apk del curl unzip

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

USER 1000
ENTRYPOINT ["/entrypoint.sh"]
