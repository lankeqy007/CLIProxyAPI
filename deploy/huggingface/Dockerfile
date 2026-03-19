FROM alpine:3.22.0

RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -u 1000 app

WORKDIR /app

COPY server /app/server
COPY entrypoint.sh /app/entrypoint.sh
COPY config.template.yaml /app/config.template.yaml
COPY config.example.yaml /app/config.example.yaml

ENV HOME=/tmp
ENV TZ=Asia/Shanghai
ENV PORT=7860

RUN chmod +x /app/server /app/entrypoint.sh
RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

EXPOSE 7860

USER 1000

CMD ["/app/entrypoint.sh"]
