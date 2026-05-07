# Go Image Processor

這個版本用 Go 重新實作原本 Python `image-processor` 的核心能力：

- 接收 GCS object create event
- 從 GCS 下載原圖並產出多組 resize 圖
- 上傳原尺寸 `.webP`、resize 後的原格式檔與 resize 後的 `.webP`
- 透過 env variables 決定是否加上 watermark

## Event 格式

服務會接受兩種常見 payload：

1. 直接的 GCS event JSON
2. Pub/Sub push envelope，`message.data` 內是 base64 編碼的 GCS event

HTTP endpoint:

- `POST /image_processor`
- `GET /`
- `GET /healthz`

## 環境變數

- `PORT`: 預設 `8080`
- `RESIZE_TARGETS`: 例如 `w480,w800,w1200,w1600,w2400`
- `ENABLE_WATERMARK`: `true` 或 `false`
- `WATERMARK_PATH`: watermark 圖檔本機路徑；當 `ENABLE_WATERMARK=true` 時必填
- `WATERMARK_SCALE`: watermark 寬度相對於輸出圖寬度的比例，預設 `0.15`
- `WATERMARK_MARGIN_RATIO`: watermark 與邊界距離比例，預設 `0.025`
- `WATERMARK_OPACITY`: `0` 到 `1`，預設 `1.0`
- `CACHE_CONTROL`: 上傳到 GCS 時寫入的 cache control，預設 `public, max-age=31536000`
- `MAX_SOURCE_PIXELS`: 原圖 decode 前允許的最高像素數，預設 `60000000`；設為 `0` 可關閉限制
- `ENABLE_IMAGE_VECTOR`: 是否啟用 CLIP image vector sidecar，預設 `false`
- `VECTOR_IMAGE_MAX_SIZE`: vector sidecar 送進 CLIP 前的最長邊，預設 `384`
- `TORCH_NUM_THREADS`: vector sidecar 的 Torch CPU thread 數，預設 `1`

## 本機執行

```bash
go run .
```

如果要本機測試並啟用 watermark：

```bash
ENABLE_WATERMARK=true \
WATERMARK_PATH=./static/watermark.png \
RESIZE_TARGETS=w480,w800,w1200 \
go run .
```

## 部署

這個服務適合部署到 Cloud Run，並搭配：

- Eventarc 直接轉 GCS finalized event 到 HTTP
- 或 Pub/Sub push subscription 導到 `/image_processor`

若使用 Cloud Run，請確保執行身份有：

- `roles/storage.objectViewer`
- `roles/storage.objectCreator`

## 行為說明

- 只處理副檔名為 `jpg`、`jpeg`、`png`、`gif`、`tif`、`tiff`、`webp`
- 會略過系統產生的混合大小寫 `.webP`，避免重複處理
- 已經帶有 `-w###` 的檔名會直接略過，避免無限遞迴
- 若 GCS event 帶有 source object generation，輸出物件會寫入 `sourceGeneration` metadata；同一個 source generation 重送時，服務會用最後一個 resize target 的 `.webP` 當完成 sentinel 直接略過，避免重複 resize
- 每個 resize target 會輸出原副檔名版本，例如 `images/foo-w800.jpg`
- 每個 resize target 也會輸出 WebP 版本，例如 `images/foo-w800.webP`
