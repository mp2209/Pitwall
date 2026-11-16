# One image holds both binaries; Compose picks one with `entrypoint`.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mockprovider ./cmd/mockprovider

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY config/ /etc/pitwall/
ENV PITWALL_CONFIG=/etc/pitwall/pitwall.yaml
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=3 CMD ["/usr/local/bin/gateway", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/gateway"]
