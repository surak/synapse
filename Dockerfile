FROM docker.io/library/golang:1.23 AS build

WORKDIR /go/src/app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /go/bin/server -trimpath ./cmd/server/main.go \
    && CGO_ENABLED=0 go build -o /go/bin/client -trimpath ./cmd/client/main.go

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /go/bin/server /go/bin/client /
