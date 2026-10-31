# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/ungarscool1/goblins/internal/app.Version=${VERSION}" -o /out/goblins ./cmd/goblins
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/goblins /usr/local/bin/goblins
COPY --from=build --chown=65532:65532 /out/data /var/lib/goblins
ENV GOBLINS_DATA_DIR=/var/lib/goblins
ENV GOBLINS_LISTEN=0.0.0.0:8080
EXPOSE 8080
VOLUME /var/lib/goblins
HEALTHCHECK --interval=15s --timeout=6s --start-period=5s CMD ["goblins", "healthcheck"]
ENTRYPOINT ["goblins"]
CMD ["serve", "--mock"]
