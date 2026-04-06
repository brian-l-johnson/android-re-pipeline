FROM eclipse-temurin:21-jre-alpine

ARG APKTOOL_VERSION=2.10.0
RUN apk add --no-cache curl && \
    curl -fsSL "https://github.com/iBotPeaches/Apktool/releases/download/v${APKTOOL_VERSION}/apktool_${APKTOOL_VERSION}.jar" \
      -o /opt/apktool.jar && \
    apk del curl

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

USER 1000
ENTRYPOINT ["/entrypoint.sh"]
