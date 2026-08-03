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
- `ENABLE_IMAGE_LABEL`: 是否啟用 Google Cloud Vision 圖片標籤偵測，預設 `false`
- `IMAGE_LABEL_MIN_SCORE`: 寫入建議標籤的最低 Vision score，預設 `0.75`
- `IMAGE_LABEL_MAX_RESULTS`: Cloud Vision `LABEL_DETECTION` 最大回傳數，預設 `10`；同一個請求也會執行 `FACE_DETECTION`、`LOGO_DETECTION`、`LANDMARK_DETECTION`

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
- 可使用 Google Cloud Vision API；專案需啟用 Cloud Vision API，服務帳號需能透過 ADC 呼叫 `vision.googleapis.com`

### Isolated vector-lab jobs

`VECTOR_LAB_MODE` 會將同一個 image 改為一次性工作，不會啟動 HTTP server、Eventarc 或 Pub/Sub 圖片處理器：

- `seed`：唯讀列舉 `IMAGE_BUCKET` 的 `images/*-w480.*`，寫入 lab DB manifest。manifest 以原始 file ID 去重；有原格式縮圖時優先使用它，否則才使用 `.webP`。可用 `VECTOR_LAB_OBJECT_START_OFFSET`／`VECTOR_LAB_OBJECT_END_OFFSET` 分割 listing。
- `vectorize`：以 `FOR UPDATE SKIP LOCKED` 分片 claim manifest，唯讀下載縮圖、計算 CLIP 向量，再只寫入 lab DB。
- `candidates`：以相同的可續跑 claim 機制，對每張圖取 Top-N cosine 近鄰並寫入候選 pair。執行前必須先建立 HNSW index，程式會拒絕沒有索引的全量掃描。

lab Job 會自動建立 `docs/vector-lab-schema.sql` 中的 extension、tables 與一般 claim indexes；向量完成後，再於該資料庫手動建立註解中的 HNSW index，最後才執行 `candidates`。以新的 `VECTOR_LAB_CANDIDATE_RUN` 重跑候選時，所有已成功的來源都會安全地重新計算。`VECTOR_LAB_MAX_ITEMS_PER_TASK`（預設 500）會讓 vectorize/candidates task 提早成功結束；重複執行 Job 即可從 checkpoint 接續，避免單一 task timeout。Job 的 service account 只需要 prod bucket 的 `storage.objects.list`／`storage.objects.get`，以及 lab DB 的連線權限；絕不可授予 prod GCS 寫入或 prod DB credential。

## 行為說明

- 只處理副檔名為 `jpg`、`jpeg`、`png`、`gif`、`tif`、`tiff`、`webp`
- 會略過系統產生的混合大小寫 `.webP`，避免重複處理
- 已經帶有 `-w###` 的檔名會直接略過，避免無限遞迴
- 若 GCS event 帶有 source object generation，輸出物件會寫入 `sourceGeneration` metadata；同一個 source generation 重送時，服務會用最後一個 resize target 的 `.webP` 當完成 sentinel 直接略過，避免重複 resize
- 每個 resize target 會輸出原副檔名版本，例如 `images/foo-w800.jpg`
- 每個 resize target 也會輸出 WebP 版本，例如 `images/foo-w800.webP`
- `possibleDuplicates` 只寫入 pHash 判定的高度重複圖片，64-bit Hamming distance 門檻為 `<= 2`
- `ENABLE_IMAGE_VECTOR` 只負責計算並寫入 `imageVector`，不會把 vector 相似結果混入 `possibleDuplicates`
- `ENABLE_IMAGE_LABEL=true` 時會用 w480 圖呼叫 Google Cloud Vision `LABEL_DETECTION`、`FACE_DETECTION`、`LOGO_DETECTION`、`LANDMARK_DETECTION`，並寫入 `imageLabelRawResult`、`imageLabelSuggestions`、`imageLabelStatus`、`imageLabelFailReason`、`imageLabelUpdatedAt`。Logo 與 landmark 結果會併入既有標籤／建議標籤。只要偵測到至少一張臉，就會額外寫入 canonical 建議標籤「人物」，即使 Label Detection 僅回傳 Furniture、Table 等場景標籤；欄位範例在 `docs/image-label-schema.sql`
