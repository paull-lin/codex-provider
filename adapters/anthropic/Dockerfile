FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY adapters/anthropic/ ./adapters/anthropic/
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/anthropic-adapter ./adapters/anthropic

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/anthropic-adapter /anthropic-adapter
USER nonroot:nonroot
EXPOSE 8400
ENTRYPOINT ["/anthropic-adapter"]
