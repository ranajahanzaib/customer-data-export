FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/exportctl ./cmd/exportctl
FROM alpine:3.21
RUN addgroup -S app && adduser -S -G app app && mkdir -p /var/lib/customer-export && chown app:app /var/lib/customer-export
COPY --from=build /out/api /usr/local/bin/api
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/exportctl /usr/local/bin/exportctl
USER app
ENV ARTIFACT_DIR=/var/lib/customer-export
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/api"]
