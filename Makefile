.PHONY: all clean

VERSION ?= $(shell git rev-parse --short=6 HEAD)$(if $(shell git status --porcelain),-dev)

all:
	CGO_ENABLED=0 go build -ldflags='-s -w -X main.version=$(VERSION)' -trimpath -o ./bin/server ./cmd/server/main.go
	CGO_ENABLED=0 go build -ldflags='-s -w -X main.version=$(VERSION)' -trimpath -o ./bin/client ./cmd/client/main.go

clean:
	rm -rf ./bin

docker:
	docker build -t registry.vul337.team:5005/gzy/synapse:latest . --build-arg VERSION=$(VERSION)
	docker tag registry.vul337.team:5005/gzy/synapse:latest registry.vul337.team:5005/ops/devcontainer/synapse:latest
	docker push registry.vul337.team:5005/gzy/synapse:latest
	docker push registry.vul337.team:5005/ops/devcontainer/synapse:latest
