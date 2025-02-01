all:
	go build -ldflags='-s -w' -trimpath -o ./bin/server ./cmd/server/main.go
	go build -ldflags='-s -w' -trimpath -o ./bin/client ./cmd/client/main.go