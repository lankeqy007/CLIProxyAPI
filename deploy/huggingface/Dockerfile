FROM alpine:3.22.0

RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -u 1000 app

WORKDIR /app

COPY . /app

ENV HOME=/tmp
ENV TZ=Asia/Shanghai
ENV PORT=7860

RUN chmod +x /app/server /app/entrypoint.sh
RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

EXPOSE 7860

USER 1000

CMD ["/app/entrypoint.sh"]
