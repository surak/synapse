FROM docker.io/library/golang:1.23 AS build

WORKDIR /go/src/app
COPY . .

RUN go mod download
RUN CGO_ENABLED=0 go build -o /go/bin/server -trimpath ./cmd/server/main.go

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /go/bin/server /
ENTRYPOINT ["/server"]
