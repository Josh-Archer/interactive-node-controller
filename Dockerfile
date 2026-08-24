FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/interactive-node-controller ./cmd/interactive-node-controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/interactive-node-controller /interactive-node-controller
USER 65532:65532
ENTRYPOINT ["/interactive-node-controller"]
