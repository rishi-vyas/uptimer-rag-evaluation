# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/uptimer ./cmd/uptimer

# distroless/static ships CA certificates (needed for HTTPS probes) and runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/uptimer /uptimer
COPY config.example.json /etc/uptimer/config.json
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/uptimer"]
CMD ["-config", "/etc/uptimer/config.json"]
