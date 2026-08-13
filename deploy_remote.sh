#!/bin/bash

# Build Go application
echo "Building Go application..."
cd wos_mcp_go
go mod tidy
go build -o ../wos_mcp
cd ..

# Set port to 7861
if [ -f "config.json" ]; then
    python3 -c "import json; d=json.load(open('config.json')); d['port']=7861; d['listen_public']=True; json.dump(d, open('config.json','w'), indent=4)"
fi

# Stop any existing instance of this server (no sudo needed for user-owned process)
if [ -f "mcp_server.pid" ]; then
    OLD_PID=$(cat mcp_server.pid 2>/dev/null)
    if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
        echo "Stopping existing server (PID $OLD_PID)..."
        kill "$OLD_PID"
        sleep 1
    fi
    rm -f mcp_server.pid
fi

# If the port is still occupied by a previous orphan process, try to free it (best effort, no sudo)
if command -v fuser >/dev/null 2>&1; then
    fuser -k 7861/tcp 2>/dev/null || true
fi

# Run in background
echo "Starting server in background..."
nohup ./wos_mcp > server.log 2>&1 &
echo $! > mcp_server.pid
echo "Server started successfully on port 7861! (PID $(cat mcp_server.pid))"
echo "Logs: mcp_server.log  |  Stop: kill \$(cat mcp_server.pid)"
