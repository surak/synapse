.PHONY: all clean

all:
	go build -ldflags='-s -w' -trimpath -o ./bin/server ./cmd/server/main.go
	go build -ldflags='-s -w' -trimpath -o ./bin/client ./cmd/client/main.go

clean:
	rm -rf ./bin

docker:
	docker build -t registry.vul337.team:5005/gzy/synapse:latest .
	docker push registry.vul337.team:5005/gzy/synapse:latest
