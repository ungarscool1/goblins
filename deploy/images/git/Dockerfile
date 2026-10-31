# Helper image for workspace operations: Git and a shell, nothing else.
FROM alpine:3.22
RUN apk add --no-cache git \
  && adduser -D -u 10001 goblins \
  && mkdir -p /workspace /scratch && chown 10001:10001 /workspace /scratch
USER 10001
WORKDIR /workspace
