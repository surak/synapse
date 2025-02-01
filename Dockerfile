FROM docker.io/library/golang:1.23 AS build

WORKDIR /go/src/app
COPY . .

RUN go mod download
RUN CGO_ENABLED=0 go build -o /go/bin/server -trimpath ./cmd/server/main.go
RUN CGO_ENABLED=0 go build -o /go/bin/client -trimpath ./cmd/client/main.go

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /go/bin/server /
COPY --from=build /go/bin/client /
