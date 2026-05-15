FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/proxy ./

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/proxy /proxy
USER 65532:65532
EXPOSE 7777
ENTRYPOINT ["/proxy"]
