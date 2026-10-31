# Worker image for the Docker acceptance test: the fake CLI, plus Git and a shell.
FROM alpine:3.22
RUN apk add --no-cache git \
  && adduser -D -u 10001 goblins \
  && mkdir -p /workspace /scratch /claude-config /remote \
  && chown 10001:10001 /workspace /scratch /claude-config /remote
COPY claude /usr/local/bin/claude
RUN chmod 755 /usr/local/bin/claude
USER 10001
