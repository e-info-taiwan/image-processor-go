#!/bin/bash
set -e

if [ "$ENABLE_IMAGE_VECTOR" = "true" ] || [ "$ENABLE_IMAGE_VECTOR" = "1" ]; then
    echo "Starting Python Vector Server on port 8081..."
    export VECTOR_PORT=8081
    uvicorn vector_server:app --host 127.0.0.1 --port 8081 &
    # Give it a few seconds to load the model
    sleep 5
fi

echo "Starting Go Image Processor..."
exec /app/image-processor
