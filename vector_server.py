import io
import os
import uvicorn
from fastapi import FastAPI, Request, HTTPException
from fastapi.responses import JSONResponse
from sentence_transformers import SentenceTransformer
from PIL import Image

app = FastAPI(title="Vector Server")

_model = None

@app.on_event("startup")
def load_model():
    global _model
    # Load the model on startup so it's ready in memory
    print("Loading clip-ViT-B-32 model...")
    _model = SentenceTransformer('clip-ViT-B-32')
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
            
        image = Image.open(io.BytesIO(body))
        
        # sentence-transformers supports PIL Image directly
        vector = _model.encode(image)
        return JSONResponse(content={"vector": vector.tolist()})
        
    except Exception as e:
        print(f"Error vectorizing image: {e}")
        raise HTTPException(status_code=500, detail=str(e))

if __name__ == "__main__":
    port = int(os.environ.get("VECTOR_PORT", "8081"))
    uvicorn.run(app, host="127.0.0.1", port=port)
