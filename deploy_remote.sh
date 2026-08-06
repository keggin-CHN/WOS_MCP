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

# Run the server in background with nohup
echo "Starting server on port 7861..."
# Kill any existing server on port 7861
echo "Zhou060423rls@" | sudo -S killall microsocks || true
echo "Zhou060423rls@" | sudo -S fuser -k 7861/tcp || true

nohup python3 wos_mcp/server.py > mcp_server.log 2>&1 &
echo "Server started successfully on port 7861!"
