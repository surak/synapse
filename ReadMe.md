# Synapse

Synapse is a lightweight, efficient reverse proxy system designed to unify access to multiple LLM (Large Language Model) API endpoints. It provides a centralized platform for model discovery and API forwarding, similar to FastChat's worker system but with improvements for local network deployments.

## Features

- **Unified Model Access**: Access multiple OpenAI-compatible API endpoints through a single entry point
- **Dynamic Model Discovery**: Models register themselves with the server dynamically
- **WebSocket Communication**: Clients connect via WebSockets, eliminating the need for public IP addresses
- **Automatic Failover**: Requests are automatically routed to available model instances
- **Load Balancing**: Multiple instances of the same model can be registered for load distribution
- **Model Status Monitoring**: Real-time tracking of model availability
- **Heartbeat Mechanism**: Ensures connections remain active
- **One-Click Client Installation**: Simple script to install and configure the client
- **Version Compatibility Check**: Ensures server and clients are running compatible versions

## Architecture

Synapse consists of two main components:

1. **Synapse Server**: Central endpoint that clients connect to and that receives external API requests
2. **Synapse Client**: Connects to both an upstream LLM API server and the Synapse Server

```mermaid
graph TD
    Client[API Client] -->|HTTP| Server[Synapse Server]
    Server -->|WebSocket| Client1[Synapse Client 1]
    Server -->|WebSocket| Client2[Synapse Client 2]
    Client1 -->|HTTP| LLM1[LLM API Server 1]
    Client2 -->|HTTP| LLM2[LLM API Server 2]
```

## Use Cases

Synapse excels in scenarios where:

- Multiple LLM API servers exist in a local network
- You want a unified endpoint for all models
- Some LLM servers lack public IP addresses
- You need to manage multiple instances of the same model
- You want to dynamically add/remove models without reconfiguring clients
- You're running experiments across multiple models and need a consistent interface

Unlike FastChat's worker system (where the OpenAI server needs direct access to the vllm_worker), Synapse uses WebSockets for bidirectional communication. This allows clients to be deployed anywhere only with outbound connections, without requiring inbound connectivity.

## Installation

### Building from Source

```bash
git clone https://github.com/zeyugao/synapse
cd synapse
make all
# or docker
make docker
```

### Docker

```bash
docker pull ghcr.io/zeyugao/synapse:latest
```

## Usage

### Server

```bash
./bin/server [options]
```

Or with docker

```bash
docker run -p 8080:8080 ghcr.io/zeyugao/synapse:latest /server [options]
```

Options:
- `--host`: Host to bind the server (default: "localhost")
- `--port`: Port to listen on (default: "8080")
- `--api-auth-key`: API authentication key for securing the API endpoint
- `--ws-auth-key`: WebSocket authentication key for client connections
- `--client-binary`: Path to the client binary for one-click installation (default: "./client")
- `--version`: Print version information



### Client

```bash
./bin/client [options]
```

Options:
- `--upstream`: URL of the upstream OpenAI-compatible API (default: "http://localhost:8081")
- `--server-url`: WebSocket URL of the Synapse server (default: "ws://localhost:8080/ws")
- `--ws-auth-key`: WebSocket authentication key (must match server's ws-auth-key)
- `--upstream-api-key`: API key for the upstream server
- `--version`: Print version information

### One-Click Client Installation

Synapse provides a simple installation script you can run on any machine:

```bash
curl http://your-synapse-server/run | bash -s -- --upstream "http://127.0.0.1:8081" --ws-auth-key 'server-ws-auth-key'
```

This will:
1. Download the client binary to `$HOME/.local/bin/synapse-client`
2. Configure it to connect to your server with parameters
3. Start running the client

**Some Notes:**
- The script automatically installs the client to `$HOME/.local/bin/synapse-client`
- The WebSocket URL (`--server-url`) is automatically derived from the server you access the script from
- Version compatibility between client and server is checked automatically
- You can pass additional parameters after `bash -s --` which will be forwarded to the client

## API Usage

Once your setup is running, you can use the Synapse server as a drop-in replacement for OpenAI's API:

```bash
curl http://synapse-server:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "model": "llama3.1",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

## License

MIT
