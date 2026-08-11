#!/bin/bash

# Setup virtual environment if it doesn't exist
if [ ! -d "venv" ]; then
    echo "Creating virtual environment..."
    python3 -m venv venv
fi

# Activate virtual environment
source venv/bin/activate

# Install dependencies
echo "Installing dependencies..."
pip install -r wos_mcp/requirements.txt
pip install beautifulsoup4 opencv-python-headless mcp==1.29.0 anyio starlette httpx pycryptodome uvicorn requests

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

# Run the server in background with nohup
echo "Starting server on port 7861..."
nohup python3 wos_mcp/server.py > mcp_server.log 2>&1 &
echo $! > mcp_server.pid
echo "Server started successfully on port 7861! (PID $(cat mcp_server.pid))"
echo "Logs: mcp_server.log  |  Stop: kill \$(cat mcp_server.pid)"
