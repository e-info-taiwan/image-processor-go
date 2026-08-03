#!/bin/bash
set -e

if [[ "${ENABLE_IMAGE_VECTOR,,}" == "true" || "$ENABLE_IMAGE_VECTOR" == "1" || "${ENABLE_IMAGE_VECTOR,,}" == "yes" || "${ENABLE_IMAGE_VECTOR,,}" == "on" ]]; then
    echo "Starting Python Vector Server on port 8081..."
    export VECTOR_PORT=8081
    uvicorn vector_server:app --host 127.0.0.1 --port 8081 &
    PYTHON_PID=$!
    
    echo "Waiting for Python Vector Server to bind to port 8081..."
    while ! (echo > /dev/tcp/127.0.0.1/8081) >/dev/null 2>&1; do
        # Check if Python process is still alive
        if ! kill -0 $PYTHON_PID 2>/dev/null; then
            echo "🚨 CRITICAL: Python server crashed during startup! Shutting down container..."
            exit 1
        fi
        sleep 0.5
    done
    echo "Python Vector Server is ready!"
fi

if [[ -n "${VECTOR_LAB_MODE:-}" ]]; then
    echo "Starting vector lab job: ${VECTOR_LAB_MODE}"
    exec /app/image-processor
fi

echo "Starting Go Image Processor..."
/app/image-processor &

# Wait for ANY background process to exit
wait -n

# If we reach here, it means either Python or Go has crashed.
echo "🚨 CRITICAL: A background process (Python or Go) has unexpectedly exited! Shutting down container..."
exit 1
