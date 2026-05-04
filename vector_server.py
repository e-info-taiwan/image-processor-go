import io
import os
import uvicorn
from fastapi import FastAPI, Request, HTTPException
from fastapi.responses import JSONResponse
from sentence_transformers import SentenceTransformer
from PIL import Image, ImageOps
import torch

app = FastAPI(title="Vector Server")

_model = None


def env_int(name, default):
    raw = os.environ.get(name, str(default))
    try:
        value = int(raw)
    except ValueError:
        return default
    if value <= 0:
        return default
    return value


torch.set_num_threads(env_int("TORCH_NUM_THREADS", 1))


def vector_image_max_size():
    return env_int("VECTOR_IMAGE_MAX_SIZE", 384)


def prepare_image_for_vector(body):
    image = Image.open(io.BytesIO(body))
    image = ImageOps.exif_transpose(image)
    image = image.convert("RGB")
    max_size = vector_image_max_size()
    image.thumbnail((max_size, max_size), Image.Resampling.LANCZOS)
    return image


@app.on_event("startup")
def load_model():
    global _model
    # Load the model on startup so it's ready in memory
    print("Loading clip-ViT-B-32 model...")
    _model = SentenceTransformer('clip-ViT-B-32')
    _model.eval()
    print("Model loaded successfully.")

@app.post("/vectorize")
async def vectorize_image(request: Request):
    global _model
    if _model is None:
        raise HTTPException(status_code=503, detail="Model not loaded yet")
    
    try:
        body = await request.body()
        if not body:
            raise HTTPException(status_code=400, detail="Empty request body")

        image = prepare_image_for_vector(body)

        with torch.inference_mode():
            vector = _model.encode(image, show_progress_bar=False, convert_to_numpy=True)
        return JSONResponse(content={"vector": vector.tolist()})

    except Exception as e:
        print(f"Error vectorizing image: {e}")
        raise HTTPException(status_code=500, detail=str(e))

if __name__ == "__main__":
    port = int(os.environ.get("VECTOR_PORT", "8081"))
    uvicorn.run(app, host="127.0.0.1", port=port)
